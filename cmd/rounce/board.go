package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gshepptech/rounce/internal/dash"
	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// defaultBoardLimit is the number of queue rows the terminal board prints
// after its counts line, absent --limit (05-CONTEXT.md decision 2: "a
// summary plus the top ten").
const defaultBoardLimit = 10

// boardCounts is board --json's counts object -- this shape settles
// 05-RESEARCH.md's open question 3 ("one object with the counts and the
// items"; field names left to discretion) and becomes the standard the
// same way every other --json shape in this codebase has: written once,
// consistently.
type boardCounts struct {
	Ready      int `json:"ready"`
	InProgress int `json:"in_progress"`
	Blocked    int `json:"blocked"`
	Backlog    int `json:"backlog"`
}

// boardJSON is board --json's whole (and only) stdout payload.
type boardJSON struct {
	Counts boardCounts  `json:"counts"`
	Items  []model.Item `json:"items"`
}

// runBoard implements `rounce board` (DASH-01, DASH-02): a terminal status
// glance -- counts, then the top of the ready queue -- or, with --web, the
// loopback dashboard server. It mutates nothing, so it registers no --actor
// and writes no events row.
func runBoard(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("board", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	limitFlag := fs.Int("limit", defaultBoardLimit, "queue rows after the counts (0 = all)")
	webFlag := fs.Bool("web", false, "serve the dashboard on 127.0.0.1 and print the URL")
	portFlag := fs.Int("port", 0, "a fixed port instead of an OS-assigned one (requires --web)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}

	var portSupplied bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSupplied = true
		}
	})

	if *jsonFlag && *webFlag {
		fmt.Fprintln(stderr, "rounce board: --json and --web cannot be combined")
		return 2
	}
	if portSupplied && !*webFlag {
		fmt.Fprintln(stderr, "rounce board: --port requires --web")
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: rounce board [flags]")
		return 2
	}

	s, err := openStoreReadOnly(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
	defer s.Close()

	if *webFlag {
		return serveWeb(s, *portFlag, stdout, stderr)
	}

	ctx := context.Background()

	// ONE unlimited store.Ready call feeds both the count and the printed
	// rows -- --limit slices the already-fetched slice in Go. This is
	// deliberate and load-bearing, not laziness (05-CONTEXT.md decision 2):
	// the count must be exact regardless of --limit, and the rows must be a
	// prefix of the same list. Calling Ready twice with different limits,
	// or calling it once with *limitFlag for both, would each be a way for
	// the count to silently disagree with the queue plan 06's DASH-07 test
	// exists to catch -- do not "optimise" this into two calls.
	ready, err := s.Ready(ctx, 0)
	if err != nil {
		return exitFor(stderr, err)
	}
	inProgress, err := s.ListItems(ctx, store.ListFilter{Status: model.StatusInProgress})
	if err != nil {
		return exitFor(stderr, err)
	}
	blocked, err := s.Blocked(ctx)
	if err != nil {
		return exitFor(stderr, err)
	}
	backlog, err := s.Backlog(ctx, "")
	if err != nil {
		return exitFor(stderr, err)
	}

	limit := *limitFlag
	top := ready
	if limit > 0 && limit < len(ready) {
		top = ready[:limit]
	}

	if *jsonFlag {
		items := top
		if items == nil {
			items = []model.Item{}
		}
		out := boardJSON{
			Counts: boardCounts{
				Ready:      len(ready),
				InProgress: len(inProgress),
				Blocked:    len(blocked),
				Backlog:    len(backlog),
			},
			Items: items,
		}
		if err := emitJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "rounce: board: %v\n", err)
			return 1
		}
		return 0
	}

	// No colour, no box drawing, ever, in this phase (05-CONTEXT.md
	// decision 3): the terminal already has the better signal --
	// renderItems' "<- blocked by <id>" names the blocker, which colour
	// cannot. Three spaces between count pairs.
	fmt.Fprintf(stdout, "ready %d   in progress %d   blocked %d   backlog %d\n",
		len(ready), len(inProgress), len(blocked), len(backlog))
	fmt.Fprintln(stdout)
	renderItems(stdout, top, renderOpts{})
	return 0
}

// serveWeb binds hostport 127.0.0.1:port (port 0 means OS-assigned) via
// dash.NewServer, prints the URL as stdout's FIRST line, then serves until
// SIGINT/SIGTERM. The host is the literal string "127.0.0.1" here and
// nowhere else -- there is no flag between the user and this constant;
// dash.NewServer's loopback validation is the backstop for a future flag,
// not the primary gate today (05-CONTEXT.md decision 4).
func serveWeb(st *store.Store, port int, stdout, stderr io.Writer) int {
	// A registry that cannot be located is not fatal to the board: the
	// per-repository views still work, and /all renders its own empty
	// state. Same rule runInit follows when it registers best-effort.
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		fmt.Fprintf(stderr, "rounce board: cannot locate the registry: %v\n", homeErr)
	}

	srv, err := dash.NewServer(st, fmt.Sprintf("127.0.0.1:%d", port), home)
	if err != nil {
		fmt.Fprintf(stderr, "rounce board: %v\n", err)
		return 1
	}

	// The URL is the first line of stdout, before anything else is written
	// there -- the demo script and board_test.go's --web tests both read
	// exactly one line and expect a working URL. No "serving..." banner, no
	// request log, ever on stdout: diagnostics go to stderr.
	fmt.Fprintln(stdout, srv.URL())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// return 0 here is the whole point: a clean shutdown is not an
		// error. rounce mcp shipped exiting 1 on an ordinary client
		// disconnect once already (codeServerClosing, cmd/rounce/mcp.go) --
		// this is the second application of the identical fix, applied to
		// a different mechanism (http.Server.Shutdown rather than a
		// JSON-RPC close code) for the identical shape of mistake.
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "rounce board: shutdown: %v\n", err)
			return 1
		}
		return 0
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "rounce board: %v\n", err)
			return 1
		}
		return 0
	}
}
