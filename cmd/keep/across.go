package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gshepptech/keep/internal/registry"
	"github.com/gshepptech/keep/internal/store"
)

// acrossRepos runs one store query against every registered store and
// returns the merged rows, ordered by less.
//
// It is the shared half of every `--all`: the registry load, the fan-out,
// the merge and the failure reporting, written once so no command copies
// them and gets the reporting wrong -- a partial view reported as a
// complete one is the failure this whole surface is shaped against.
//
// A malformed registry is reported and survived, never fatal:
// registry.Load hands back both a usable empty envelope and the error.
func acrossRepos[T any](
	ctx context.Context,
	stderr io.Writer,
	verb string,
	query func(context.Context, *store.Store) ([]T, error),
	less func(a, b registry.Tagged[T]) bool,
) ([]registry.Tagged[T], []registry.Fanout[T], error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, err
	}

	env, err := registry.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "keep: warning: %v\n", err)
	}

	results := registry.FetchEach(ctx, env.Repos, query)
	reportRepoFailures(stderr, verb, registry.Failures(results))
	return registry.Flatten(results, less), results, nil
}

// reportRepoFailures writes the unreadable repositories to stderr, never to
// stdout, which carries the rows only. Exit code is unaffected --
// a partial view is reported, not fatal, matching `repos`.
//
// One failure is named in full, because it is probably the thing the person
// needs to see. Several are summarised to a single line: `init` registers
// automatically and nothing unregisters until `repos forget --gone`, so a
// registry with a dozen dead scratch repositories would otherwise print a
// dozen paragraphs above every queue, which is how a person learns to stop
// reading stderr.
func reportRepoFailures[T any](stderr io.Writer, verb string, failed []registry.Fanout[T]) {
	switch len(failed) {
	case 0:
	case 1:
		fmt.Fprintf(stderr, "keep: %s: repo %s: %v\n", verb, failed[0].Repo, failed[0].Err)
	default:
		names := make([]string, len(failed))
		for i, f := range failed {
			names[i] = f.Repo
		}
		fmt.Fprintf(stderr, "keep: %s: %d registered stores could not be read (%s). `keep repos` names the errors; `keep repos forget --gone` drops the ones that are gone.\n",
			verb, len(failed), strings.Join(names, ", "))
	}
}

// repoErrorsJSON renders the unreadable repositories for a --json payload.
//
// Errors are a first-class field, not an omission: a consumer needs to know
// it saw a PARTIAL view. The slice is always present and never nil, so an
// empty array is a positive statement that every registered store was read.
func repoErrorsJSON[T any](results []registry.Fanout[T]) []readyAllError {
	out := make([]readyAllError, 0)
	for _, r := range results {
		if r.Err != nil {
			out = append(out, readyAllError{Repository: r.Repo, Path: r.Path, Error: r.Err.Error()})
		}
	}
	return out
}

// byNewest orders rows by a timestamp, newest first, then by repository and
// a stable key.
//
// Ordering ACROSS stores by a timestamp is exactly what merge.go's own
// comment warns against for the ready queue -- and the distinction is worth
// stating rather than looking like an oversight. For ranked WORK, two
// stores' created_at values are wall-clock strings from unrelated
// histories and ordering by them produces something meaningless. For "what
// did I park recently" and "what happened", time IS the question being
// asked, and every registered store is a file on this one machine, read by
// this one process, stamped from this one clock.
//
// The repository and key tiebreaks make the order total, which is what
// keeps output identical between runs -- sort.Slice is documented as
// unstable, and a comparison that is false in both directions for two rows
// is how that instability becomes visible.
func byNewest[T any](at func(T) string, key func(T) string) func(a, b registry.Tagged[T]) bool {
	return func(a, b registry.Tagged[T]) bool {
		if at(a.Row) != at(b.Row) {
			return at(a.Row) > at(b.Row)
		}
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		return key(a.Row) < key(b.Row)
	}
}

// renderTagged prints one line per merged row: the repository name, then
// whatever columns cols produces, every column padded to the widest value in
// THIS result set.
//
// One renderer for every `--all`, rather than one per command, and it works
// off the slice index by index. That is not a convenience -- renderMerged's
// own comment records why: item ids are NOT unique across stores, two
// repositories can independently mint the same rnc-<6 hex>, and any
// id-keyed map would silently attribute one repository's row to another.
//
// The last column is the title, and it absorbs whatever width is left, so a
// long title is truncated rather than wrapping into an unreadable block.
func renderTagged[T any](w io.Writer, rows []registry.Tagged[T], cols func(T) []string) {
	if len(rows) == 0 {
		return
	}

	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = append([]string{r.Repository}, cols(r.Row)...)
	}

	widths := make([]int, len(cells[0]))
	for _, row := range cells {
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}

	// Everything but the last column, plus the single spaces between them.
	fixed := 0
	for _, wd := range widths[:len(widths)-1] {
		fixed += wd + 2
	}
	titleBudget := titleWidth(w) - fixed

	for _, row := range cells {
		last := len(row) - 1
		for i, c := range row[:last] {
			fmt.Fprintf(w, "%-*s  ", widths[i], c)
		}
		fmt.Fprintln(w, truncateTitle(row[last], titleBudget))
	}
}
