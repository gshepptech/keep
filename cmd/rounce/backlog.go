package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/rounce/internal/model"
)

// runBacklog implements the `backlog` subcommand (CAP-03) -- the pen: every
// raw item still parked, grouped by type in schema-enum order, newest first
// within each group, filterable by --type.
//
// Grouping and ordering now live entirely in store.Backlog (04-01-PLAN.md
// task 2/3), not here: `rounce backlog` and the MCP `backlog` tool both call
// the same store method, so the two surfaces walk one order by construction
// rather than by two implementations agreeing -- Phase 3 wave 5 already
// shipped and fixed exactly this class of disagreement once, between
// backlog's own rendered and --json output, where it escaped notice until
// two types interleaved in time. renderBacklog still derives its group
// boundaries from Type changing between consecutive rows, which continues
// to work only because store.Backlog guarantees each type's rows are
// contiguous.
func runBacklog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backlog", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	typeFlag := fs.String("type", "", "filter by item type ("+validTypesList()+")")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: rounce backlog [flags]")
		return 2
	}

	// Validated against the closed vocabulary before the store is opened,
	// matching list's/add's own pre-validation discipline.
	if *typeFlag != "" && !model.ValidTypes[*typeFlag] {
		fmt.Fprintf(stderr, "rounce: backlog: --type %q is not one of %s\n", *typeFlag, validTypesList())
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
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
			fmt.Fprintf(stderr, "rounce: backlog: %v\n", err)
			return 1
		}
		return 0
	}

	renderBacklog(stdout, ordered)
	return 0
}

// renderBacklog writes the locked output shape (03-CONTEXT.md decision 4):
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
// would only add a timezone-conversion class of bug that cannot occur here
// (03-RESEARCH.md Open Question 2). The length guard protects against a
// malformed or truncated CreatedAt value rather than assuming the schema's
// NOT NULL guarantee always holds in every future caller.
func createdDate(createdAt string) string {
	if len(createdAt) < 10 {
		return createdAt
	}
	return createdAt[:10]
}
