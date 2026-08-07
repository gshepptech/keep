package main

import (
	"context"
	"flag"
	"fmt"
	"io"

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
// dash.NewServer and prints the URL as stdout's FIRST line, then serves.
//
// This first cut serves synchronously with no signal handling -- task 2
// hardens it with signal.NotifyContext + graceful Shutdown so Ctrl-C exits
// 0 rather than leaving the process to be killed. Placeholder only for the
// span between this commit and task 2's; not a shipped end state.
func serveWeb(st *store.Store, port int, stdout, stderr io.Writer) int {
	srv, err := dash.NewServer(st, fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Fprintf(stderr, "rounce board: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, srv.URL())
	if err := srv.Serve(); err != nil {
		fmt.Fprintf(stderr, "rounce board: %v\n", err)
		return 1
	}
	return 0
}
