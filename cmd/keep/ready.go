package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/registry"
)

// runReady implements the `ready` subcommand (READY-01): the ranked queue of
// workable items, exactly as the shipped `ready` view computes it.
//
// cmd/keep may not contain SQL or filtering/sorting logic (STORE-10,
// READY-03) -- runReady makes exactly one store.Ready call and renders its
// result unmodified. It does not re-sort, does not drop raw items, does not
// check status: every predicate lives in the view, per 02-CONTEXT.md's
// carried-forward decision and docs/design/phase-1.md's warning against
// re-adding a filter in a caller "for clarity".
func runReady(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	limitFlag := fs.Int("limit", 0, "maximum number of items to return (default: no limit)")
	allFlag := fs.Bool("all", false, "merge the ready queues of every registered store (see keep repos)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep ready [flags]")
		return 2
	}

	// --db names exactly one store; --all reads the registry. Silently
	// picking one would be worse than refusing, the same reasoning init.go
	// applies to --db and --global.
	if *allFlag && *dbFlag != "" {
		fmt.Fprintln(stderr, "keep: ready: --db and --all are mutually exclusive (--db names one store; --all reads the registry)")
		return 2
	}
	if *allFlag {
		return runReadyAll(*limitFlag, *jsonFlag, stdout, stderr)
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	// UNLIMITED, then filtered, then sliced. Applying --limit in SQL and
	// filtering afterwards would return fewer rows than asked for whenever
	// Interlock held anything -- the same count-versus-rows rule runBoard
	// and mcpInstructions each state at length, for the same reason.
	ctx := context.Background()
	items, err := s.Ready(ctx, 0)
	if err != nil {
		return exitFor(stderr, err)
	}

	// The second of the two joins both roadmaps describe: `ready`
	// additionally excludes items whose paths collide with a lease
	// Interlock currently holds. Absent Interlock, this is exactly what it
	// was before -- Fetch reports unusable rather than failing.
	//
	// The empty dir means "inherit this process's working directory", which
	// is the right one: `interlock status` resolves its own repository root
	// by walking up from where it runs, exactly as `keep ready` resolves
	// its store. Passing the store's path instead would ask Interlock about
	// whichever repository --db pointed at, which is not where the agent is
	// working.
	if st, ok := interlock.Fetch(ctx, ""); ok {
		var held []interlock.Collision
		items, held = interlock.Exclude(items, st)
		// Diagnostics on stderr, never stdout: stdout is the queue, and an
		// agent piping it must not have to strip prose. Naming the holder
		// is the point -- an item that merely vanished would be
		// indistinguishable from one that was closed.
		for _, c := range held {
			fmt.Fprintf(stderr, "keep: ready: %s held back -- %s is held by %s (session %s)\n",
				c.Item.ID, c.Path, c.Lease.Agent, c.Lease.SessionID)
		}
	}

	if *limitFlag > 0 && *limitFlag < len(items) {
		items = items[:*limitFlag]
	}

	if *jsonFlag {
		// A nil []model.Item marshals to `null`, and an agent doing
		// `for item in $(jq '.[]')` breaks on null. An empty queue is
		// exit 0, not an error (RANK-02/CLI-01), so the JSON shape must
		// say so unambiguously: substitute an empty, non-nil slice
		// before encoding.
		if items == nil {
			items = []model.Item{}
		}
		if err := emitJSON(stdout, items); err != nil {
			fmt.Fprintf(stderr, "keep: ready: %v\n", err)
			return 1
		}
		return 0
	}

	// renderItems is a no-op on an empty slice -- an empty queue prints
	// nothing and still exits 0.
	renderItems(stdout, items, renderOpts{})
	return 0
}

// readyAllJSON is `ready --all --json`'s whole stdout payload. The two-field
// top-level object mirrors board.go's {counts, items} precedent.
//
// Errors are a first-class field, not an omission: an agent consuming this
// needs to know it saw a PARTIAL view. A silent partial result is the
// failure mode this phase is shaped against, so both arrays are always
// present and always non-nil -- an empty errors array is a positive
// statement that every registered store was read.
type readyAllJSON struct {
	Items  []registry.MergedItem `json:"items"`
	Errors []readyAllError       `json:"errors"`
}

// readyAllError is one registered store's fan-out failure, named by
// repository and path so a consumer can tell WHICH view is incomplete.
type readyAllError struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Error      string `json:"error"`
}

// runReadyAll implements REPO-03. It reads the registry, fans out read-only
// through internal/registry, merges by score across every store, and
// renders.
//
// It never opens a store itself and never re-implements the ready query: the
// per-store rows come from store.Ready via registry.FetchReady, which is
// what makes ready --all's view of a repository structurally unable to
// disagree with that repository's own `keep ready` (Anti-Pattern 1 in
// .planning/research/ARCHITECTURE.md).
//
// --limit slices the MERGED result. Each store is fetched unlimited. A
// per-store limit would let each store's own created_at tiebreak decide
// which rows even reach the cross-store ranking, and created_at is exactly
// the key that is not comparable across stores -- the same "one unlimited
// call feeds everything" discipline board.go already applies.
func runReadyAll(limit int, asJSON bool, stdout, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}

	// A malformed registry is never fatal (06-CONTEXT.md decision 2): Load
	// hands back BOTH a usable empty envelope and the error, so this reports
	// and continues. Returning here would be the failure that decision
	// forbids.
	env, err := registry.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "keep: warning: %v\n", err)
	}

	rows := registry.FetchReady(context.Background(), env.Repos)
	merged := registry.Merge(rows)

	if limit > 0 && limit < len(merged) {
		merged = merged[:limit]
	}

	if asJSON {
		out := readyAllJSON{Items: merged, Errors: make([]readyAllError, 0)}
		for _, r := range rows {
			if r.Err != nil {
				out.Errors = append(out.Errors, readyAllError{
					Repository: r.Repo,
					Path:       r.Path,
					Error:      r.Err.Error(),
				})
			}
		}
		if err := emitJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "keep: ready --all: %v\n", err)
			return 1
		}
		return 0
	}

	// Errors on the rendered path go to stderr, one line each -- never to
	// stdout, which carries the queue only (CLI-01). Exit 0 even when some
	// repos failed, matching `repos`.
	for _, r := range rows {
		if r.Err != nil {
			fmt.Fprintf(stderr, "keep: repo %s: %v\n", r.Repo, r.Err)
		}
	}
	renderMerged(stdout, merged)
	return 0
}

// renderMerged prints one line per merged item -- repository, id, score,
// type, title -- with every column aligned to the widest value in THIS
// result set, matching renderItems' conventions (space padding, no colour,
// no box drawing, title truncated to the remaining budget via
// truncateTitle/titleWidth).
//
// It does NOT reuse renderOpts's annotation mechanism, and the reason is not
// stylistic: renderOpts.blockedBy is a map keyed by item id, and item ids
// are NOT unique across stores. Two repositories can independently mint the
// same rnc-<6 hex> id, so any id-keyed map would silently attribute one
// repository's row to another. renderMerged works off the slice, index by
// index, where the repository name travels with its own row.
func renderMerged(w io.Writer, items []registry.MergedItem) {
	if len(items) == 0 {
		return
	}

	repoW, idW, scoreW, typeW := 0, 0, 0, 0
	scores := make([]string, len(items))
	for i, it := range items {
		if l := len(it.Repository); l > repoW {
			repoW = l
		}
		if l := len(it.ID); l > idW {
			idW = l
		}
		scores[i] = strconv.Itoa(it.Score)
		if l := len(scores[i]); l > scoreW {
			scoreW = l
		}
		if l := len(it.Type); l > typeW {
			typeW = l
		}
	}

	used := repoW + 2 + idW + 2 + scoreW + 2 + typeW + 2
	titleBudget := titleWidth(w) - used
	if titleBudget < minTitleBudget {
		titleBudget = minTitleBudget
	}

	for i, it := range items {
		fmt.Fprintf(w, "%-*s  %-*s  %*s  %-*s  %s\n",
			repoW, it.Repository, idW, it.ID, scoreW, scores[i], typeW, it.Type, truncateTitle(it.Title, titleBudget))
	}
}
