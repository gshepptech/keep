package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/registry"
)

// runReady implements the `ready` subcommand: the ranked queue of
// workable items, exactly as the `ready` view computes it.
//
// cmd/keep contains no SQL or filtering/sorting logic: runReady makes
// exactly one store.Ready call and renders its result unmodified. It does
// not re-sort, does not drop raw items, does not check status: every
// predicate lives in the view, and re-adding a filter in a caller "for
// clarity" would give it a second place to drift.
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

	// Unlimited, then filtered, then sliced. Applying --limit in SQL and
	// filtering afterwards would return fewer rows than asked for whenever
	// Interlock held anything, the same count-versus-rows rule runBoard and
	// mcpInstructions each state.
	ctx := context.Background()
	items, err := s.Ready(ctx, 0)
	if err != nil {
		return exitFor(stderr, err)
	}

	// `ready` also excludes items whose paths collide with a lease
	// Interlock currently holds. Absent Interlock, the queue is unchanged,
	// because Fetch reports unusable rather than failing.
	//
	// The empty dir means "inherit this process's working directory", which
	// is the right one: `interlock status` resolves its own repository root
	// by walking up from where it runs, exactly as `keep ready` resolves
	// its store. Passing the store's path instead would ask Interlock about
	// whichever repository --db pointed at, which is not where the agent is
	// working.
	var held []interlock.Collision
	items, held = heldBack(ctx, items)
	explainHolds(stderr, "ready", held)

	if *limitFlag > 0 && *limitFlag < len(items) {
		items = items[:*limitFlag]
	}

	if *jsonFlag {
		// A nil []model.Item marshals to `null`, and an agent doing
		// `for item in $(jq '.[]')` breaks on null. An empty queue is exit
		// 0, not an error, so the JSON shape must say so unambiguously:
		// substitute an empty, non-nil slice before encoding.
		if items == nil {
			items = []model.Item{}
		}
		if err := emitJSON(stdout, items); err != nil {
			fmt.Fprintf(stderr, "keep: ready: %v\n", err)
			return 1
		}
		return 0
	}

	// renderItems is a no-op on an empty slice, so an empty queue prints
	// nothing and still exits 0.
	renderItems(stdout, items, renderOpts{})
	return 0
}

// readyAllJSON is `ready --all --json`'s whole stdout payload. The two-field
// top-level object mirrors board.go's {counts, items} precedent.
//
// Errors are a first-class field, not an omission: an agent consuming this
// needs to know it saw a partial view. A silent partial result is the
// failure this design avoids, so both arrays are always present and always
// non-nil, and an empty errors array is a positive statement that every
// registered store was read.
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

// runReadyAll implements `ready --all`. It reads the registry, fans out
// read-only through internal/registry, merges by score across every store,
// and renders.
//
// It never opens a store itself and never re-implements the ready query:
// the per-store rows come from store.Ready via registry.FetchReady, so
// ready --all's view of a repository cannot disagree with that repository's
// own `keep ready`.
//
// --limit slices the merged result, and each store is fetched unlimited. A
// per-store limit would let each store's own created_at tiebreak decide
// which rows even reach the cross-store ranking, and created_at is the key
// that is not comparable across stores. board.go follows the same "one
// unlimited call feeds everything" rule.
func runReadyAll(limit int, asJSON bool, stdout, stderr io.Writer) int {
	payload, err := readyAllPayload(context.Background(), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}

	merged := payload.Items
	if limit > 0 && limit < len(merged) {
		merged = merged[:limit]
	}

	if asJSON {
		if err := emitJSON(stdout, readyAllJSON{Items: merged, Errors: payload.Errors}); err != nil {
			fmt.Fprintf(stderr, "keep: ready --all: %v\n", err)
			return 1
		}
		return 0
	}

	// Errors go to stderr, never to stdout, which carries the queue only
	// Exit 0 even when some repos failed, matching `repos`.
	//
	// One failure is named in full, because it is probably the thing the
	// person needs to see. Several are summarised to a single line: `init`
	// registers automatically and nothing unregisters until `repos forget
	// --gone`, so a registry with a dozen dead scratch repositories would
	// otherwise print a dozen paragraphs above every queue, which is how a
	// person learns to stop reading stderr.
	switch len(payload.Errors) {
	case 0:
	case 1:
		fmt.Fprintf(stderr, "keep: repo %s: %s\n", payload.Errors[0].Repository, payload.Errors[0].Error)
	default:
		names := make([]string, len(payload.Errors))
		for i, e := range payload.Errors {
			names[i] = e.Repository
		}
		fmt.Fprintf(stderr, "keep: %d registered stores could not be read (%s). `keep repos` names the errors; `keep repos forget --gone` drops the ones that are gone.\n",
			len(payload.Errors), strings.Join(names, ", "))
	}

	renderMerged(stdout, merged)
	return 0
}

// renderMerged prints one line per merged item (repository, id, score,
// type, title) with every column aligned to the widest value in this result
// set, matching renderItems' conventions (space padding, no colour, no box
// drawing, title truncated to the remaining budget via
// truncateTitle/titleWidth).
//
// It does not reuse renderOpts's annotation mechanism, because
// renderOpts.blockedBy is a map keyed by item id, and item ids are not
// unique across stores. Two repositories can independently mint the
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

// readyAllPayload is the cross-repository queue as data, with no rendering
// and no exit code: the same {items, errors} shape `ready --all --json`
// prints, built by the same registry calls.
//
// It exists so the `ready_all` MCP tool and the `--all` flag cannot answer
// differently. internal/mcp holds one *store.Store and has no business
// knowing about the registry, so cmd/keep injects this as
// mcp.Options.ReadyAll, and the CLI calls the same function.
//
// A malformed registry is never fatal: registry.Load hands back both a
// usable empty envelope and the error, so this reports and continues.
// Refusing the whole read because one line of JSON is wrong would be
// worse than a warning.
//
// It is reported to warn rather than returned, because it is a warning about
// the machine's configuration and not a property of the queue, so it does not
// belong in the payload a consumer reads rows out of. The CLI passes stderr;
// `keep mcp` passes a writer onto its own logger, because there stdout is
// protocol bytes and nothing else may touch it.
func readyAllPayload(ctx context.Context, warn io.Writer) (readyAllJSON, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return readyAllJSON{}, err
	}
	env, loadErr := registry.Load(home)
	if loadErr != nil {
		fmt.Fprintf(warn, "keep: warning: %v\n", loadErr)
	}

	rows := registry.FetchReady(ctx, env.Repos)
	out := readyAllJSON{Items: registry.Merge(rows), Errors: make([]readyAllError, 0)}
	for _, r := range rows {
		if r.Err != nil {
			out.Errors = append(out.Errors, readyAllError{Repository: r.Repo, Path: r.Path, Error: r.Err.Error()})
		}
	}
	return out, nil
}
