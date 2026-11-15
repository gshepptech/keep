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

	"github.com/gshepptech/keep/internal/dash"
	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/registry"
	"github.com/gshepptech/keep/internal/store"
)

// defaultBoardLimit is the number of queue rows the terminal board prints
// after its counts line, absent --limit: a summary plus the top ten.
const defaultBoardLimit = 10

// boardCounts is board --json's counts object: one object with the counts
// and the items.
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

// runBoard implements `keep board`: a terminal status
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
	allFlag := fs.Bool("all", false, "counts and queue across every registered store (see keep repos)")

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
		fmt.Fprintln(stderr, "keep board: --json and --web cannot be combined")
		return 2
	}
	if portSupplied && !*webFlag {
		fmt.Fprintln(stderr, "keep board: --port requires --web")
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep board [flags]")
		return 2
	}
	if *allFlag && *dbFlag != "" {
		fmt.Fprintln(stderr, "keep: board: --db and --all are mutually exclusive (--db names one store; --all reads the registry)")
		return 2
	}
	if *allFlag && *webFlag {
		// The dashboard already has its own cross-repository page, served
		// from the registry (internal/dash/projects.go). Two answers to
		// "show me everything" reachable from one command is how they drift.
		fmt.Fprintln(stderr, "keep: board: --all and --web cannot be combined; the dashboard's own projects page is the cross-repository view")
		return 2
	}

	if *allFlag {
		return runBoardAll(*limitFlag, *jsonFlag, stdout, stderr)
	}

	s, err := openStoreReadOnly(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	if *webFlag {
		return serveWeb(s, *portFlag, *dbFlag, stdout, stderr)
	}

	ctx := context.Background()

	// ONE unlimited store.Ready call feeds both the count and the printed
	// rows -- --limit slices the already-fetched slice in Go. This is
	// deliberate and load-bearing, not laziness: the count must be exact
	// regardless of --limit, and the rows must be a prefix of the same
	// list. Calling Ready twice with different limits, or calling it once
	// with *limitFlag for both, would each be a way for the count to
	// silently disagree with the queue -- do not "optimise" this into two
	// calls.
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
			fmt.Fprintf(stderr, "keep: board: %v\n", err)
			return 1
		}
		return 0
	}

	// No colour, no box drawing: the terminal already has the better signal --
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
// not the primary gate today.
func serveWeb(st *store.Store, port int, dbFlag string, stdout, stderr io.Writer) int {
	// A registry that cannot be located is not fatal to the board: the
	// per-repository views still work, and /all renders its own empty
	// state. Same rule runInit follows when it registers best-effort.
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		fmt.Fprintf(stderr, "keep board: cannot locate the registry: %v\n", homeErr)
	}

	// The board acts on the store, so it needs the path to reopen for
	// writing. resolveStorePath is the same four-rule resolver every other
	// command uses -- the board must not invent a fifth.
	dbPath, pathErr := store.Resolve(dbFlag)
	if pathErr != nil {
		fmt.Fprintf(stderr, "keep board: %v\n", pathErr)
	}
	actor, _ := resolveActor("")

	srv, err := dash.NewServer(st, fmt.Sprintf("127.0.0.1:%d", port), home,
		dash.Writable(dbPath), dash.ActingAs(actor))
	if err != nil {
		fmt.Fprintf(stderr, "keep board: %v\n", err)
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
		// A clean shutdown is not an error, so this returns 0. keep mcp
		// treats an ordinary client disconnect the same way
		// (codeServerClosing, cmd/keep/mcp.go).
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "keep board: shutdown: %v\n", err)
			return 1
		}
		return 0
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "keep board: %v\n", err)
			return 1
		}
		return 0
	}
}

// boardAllJSON is `board --all --json`'s payload: the same {counts, items}
// shape single-store board uses, summed across every registered store, plus
// the errors field every cross-repository command carries so a consumer
// knows whether it saw everything.
type boardAllJSON struct {
	Counts boardCounts                   `json:"counts"`
	Items  []registry.Tagged[model.Item] `json:"items"`
	Errors []readyAllError               `json:"errors"`
}

// runBoardAll is the status glance across every registered store.
//
// The counts are summed from four cross-store reads rather than from
// registry.FetchCounts, which reports per-repository totals for `keep
// repos` and has no notion of in-progress or blocked. Four fan-outs is four
// passes over the same files; that is the same "correct before fast" trade
// single-store board already makes when it calls Ready, ListItems, Blocked
// and Backlog purely to take len() of each.
//
// The queue is ranked by score across all stores, exactly as ready --all
// ranks it, because it IS ready --all's ranking -- the same registry.Merge
// ordering, so the number in the counts line and the rows beneath it can
// never come from two different ideas of what is workable.
func runBoardAll(limit int, asJSON bool, stdout, stderr io.Writer) int {
	ctx := context.Background()

	ready, results, err := acrossRepos(ctx, stderr, "board",
		func(ctx context.Context, st *store.Store) ([]model.Item, error) { return st.Ready(ctx, 0) },
		func(a, b registry.Tagged[model.Item]) bool {
			if a.Row.Score != b.Row.Score {
				return a.Row.Score > b.Row.Score
			}
			if a.Repository != b.Repository {
				return a.Repository < b.Repository
			}
			return a.Row.ID < b.Row.ID
		},
	)
	if err != nil {
		fmt.Fprintf(stderr, "keep: board: %v\n", err)
		return 1
	}

	// The remaining three counts. Failures are already reported once, above,
	// so these fan-outs report nothing -- a store that will not open would
	// otherwise be named four times for one glance.
	inProgress := countAcross(ctx, func(ctx context.Context, st *store.Store) ([]model.Item, error) {
		return st.ListItems(ctx, store.ListFilter{Status: model.StatusInProgress})
	})
	blocked := countAcross(ctx, func(ctx context.Context, st *store.Store) ([]model.BlockedItem, error) {
		return st.Blocked(ctx)
	})
	backlog := countAcross(ctx, func(ctx context.Context, st *store.Store) ([]model.Item, error) {
		return st.Backlog(ctx, "")
	})

	counts := boardCounts{Ready: len(ready), InProgress: inProgress, Blocked: blocked, Backlog: backlog}

	rows := ready
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}

	if asJSON {
		if err := emitJSON(stdout, boardAllJSON{Counts: counts, Items: rows, Errors: repoErrorsJSON(results)}); err != nil {
			fmt.Fprintf(stderr, "keep: board --all: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "ready %d   in progress %d   blocked %d   backlog %d   across %d repositories\n\n",
		counts.Ready, counts.InProgress, counts.Blocked, counts.Backlog, len(results))
	renderTagged(stdout, rows, func(it model.Item) []string {
		return []string{it.ID, fmt.Sprint(it.Score), it.Type, it.Title}
	})
	return 0
}

// countAcross sums one query's row count over every registered store.
//
// It reports no failures of its own: runBoardAll has already named them
// once from its first fan-out, and a store that will not open would
// otherwise be named four times for a single glance. The count is therefore
// a count of what could be READ, which is the same thing every other number
// on this line is.
func countAcross[T any](ctx context.Context, query func(context.Context, *store.Store) ([]T, error)) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	env, _ := registry.Load(home)
	n := 0
	for _, r := range registry.FetchEach(ctx, env.Repos, query) {
		n += len(r.Rows)
	}
	return n
}
