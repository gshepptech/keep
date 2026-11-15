package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/registry"
	"github.com/gshepptech/keep/internal/store"
)

// runBacklog implements the `backlog` subcommand -- the pen: every
// raw item still parked, grouped by type in schema-enum order, newest first
// within each group, filterable by --type.
//
// Grouping and ordering live entirely in store.Backlog, not here: `keep
// backlog` and the MCP `backlog` tool both call the same store method, so
// the two surfaces walk one order by construction rather than by two
// implementations agreeing. renderBacklog derives its group boundaries
// from Type changing between consecutive rows, which works only because
// store.Backlog guarantees each type's rows are contiguous.
func runBacklog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backlog", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	typeFlag := fs.String("type", "", "filter by item type ("+validTypesList()+")")
	allFlag := fs.Bool("all", false, "merge the backlogs of every registered store (see keep repos)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep backlog [flags]")
		return 2
	}
	// --db names exactly one store; --all reads the registry. Silently
	// picking one would be worse than refusing, the same reasoning
	// ready.go and init.go apply.
	if *allFlag && *dbFlag != "" {
		fmt.Fprintln(stderr, "keep: backlog: --db and --all are mutually exclusive (--db names one store; --all reads the registry)")
		return 2
	}

	// Validated against the closed vocabulary before the store is opened,
	// matching list's/add's own pre-validation discipline.
	if *typeFlag != "" && !model.ValidTypes[*typeFlag] {
		fmt.Fprintf(stderr, "keep: backlog: --type %q is not one of %s\n", *typeFlag, validTypesList())
		return 2
	}

	if *allFlag {
		return runBacklogAll(*typeFlag, *jsonFlag, stdout, stderr)
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	ordered, err := s.Backlog(context.Background(), *typeFlag)
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if ordered == nil {
			ordered = []model.Item{}
		}
		if err := emitJSON(stdout, ordered); err != nil {
			fmt.Fprintf(stderr, "keep: backlog: %v\n", err)
			return 1
		}
		return 0
	}

	renderBacklog(stdout, ordered)
	return 0
}

// renderBacklog writes a fixed output shape:
// a bare, unadorned header line per non-empty type group in schema-enum
// order, a blank line BETWEEN groups (never after the last one), and one
// "id  created  title" line per item, newest first within its group. Empty
// groups print nothing at all -- not even an empty header. ordered must
// already be grouped-then-sorted (store.Backlog's output) -- a new group
// boundary is detected by Type changing between consecutive items, since
// store.Backlog guarantees each type's rows are contiguous.
func renderBacklog(w io.Writer, ordered []model.Item) {
	if len(ordered) == 0 {
		return
	}

	// Id column width computed across ALL groups, not per group, so the
	// columns line up down the whole page.
	idW := 0
	for _, it := range ordered {
		if l := len(it.ID); l > idW {
			idW = l
		}
	}

	budget := titleWidth(w)
	currentType := ""
	for i, it := range ordered {
		if it.Type != currentType {
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, it.Type)
			currentType = it.Type
		}
		fmt.Fprintf(w, "%-*s  %s  %s\n", idW, it.ID, createdDate(it.CreatedAt), truncateTitle(it.Title, budget))
	}
}

// createdDate extracts the date-only portion of a created_at timestamp: a
// direct 10-rune slice of CreatedAt, not a time.Parse/Format round trip.
// model.TimeFormat ("2006-01-02T15:04:05.000000000Z") is fixed-width and
// always UTC by construction (internal/model/model.go), so the first ten
// characters are always exactly YYYY-MM-DD -- a parse/format round trip
// would only add a timezone-conversion class of bug that cannot occur
// here. The length guard protects against a
// malformed or truncated CreatedAt value rather than assuming the schema's
// NOT NULL guarantee always holds in every future caller.
func createdDate(createdAt string) string {
	if len(createdAt) < 10 {
		return createdAt
	}
	return createdAt[:10]
}

// backlogAllJSON is `backlog --all --json`'s payload, shaped like
// readyAllJSON's: rows and errors, both always present and never nil, so an
// empty errors array is a positive statement that every registered store was
// read rather than the absence of a field.
type backlogAllJSON struct {
	Items  []registry.Tagged[model.Item] `json:"items"`
	Errors []readyAllError               `json:"errors"`
}

// runBacklogAll merges every registered store's pen.
//
// Ordering is newest first across all of them, which is a different decision
// from `ready --all`'s and deliberately so -- see byNewest. `backlog` is
// "what have I parked", and the answer to that is chronological; the pen is
// not ranked, and a raw item has no score to rank it by in the first place.
//
// This does NOT preserve single-store backlog's grouping by type. Grouping
// depends on rows of one type being contiguous, and interleaving several
// stores breaks that; --type narrows to one type when the grouping is what
// you wanted.
func runBacklogAll(typeFilter string, asJSON bool, stdout, stderr io.Writer) int {
	rows, results, err := acrossRepos(context.Background(), stderr, "backlog",
		func(ctx context.Context, st *store.Store) ([]model.Item, error) {
			return st.Backlog(ctx, typeFilter)
		},
		byNewest(func(it model.Item) string { return it.CreatedAt }, func(it model.Item) string { return it.ID }),
	)
	if err != nil {
		fmt.Fprintf(stderr, "keep: backlog: %v\n", err)
		return 1
	}

	if asJSON {
		if err := emitJSON(stdout, backlogAllJSON{Items: rows, Errors: repoErrorsJSON(results)}); err != nil {
			fmt.Fprintf(stderr, "keep: backlog --all: %v\n", err)
			return 1
		}
		return 0
	}

	renderTagged(stdout, rows, func(it model.Item) []string {
		return []string{it.ID, it.Type, it.Title}
	})
	return 0
}
