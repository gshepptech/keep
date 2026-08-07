package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// backlogTypeOrder is the schema's type enum, in schema order -- NOT
// alphabetical, and NEVER map-iteration order (03-CONTEXT.md decision 4).
// This is the same order validTypesList() (add.go) already encodes for the
// identical reason: map iteration would make a demo transcript flaky.
var backlogTypeOrder = []string{model.TypeRoadmap, model.TypeEpic, model.TypeTask, model.TypeBug, model.TypeNote}

// runBacklog implements the `backlog` subcommand (CAP-03) -- the pen: every
// raw item still parked, grouped by type in schema-enum order, newest first
// within each group, filterable by --type.
//
// One ListItems call -- Stage: model.StageRaw, Type: *typeFlag,
// ExcludeClosed: true -- exactly the scope 03-CONTEXT.md decision 4 locks:
// "stage = 'raw' AND status NOT IN ('closed', 'dropped')". Grouping and
// within-group ordering are CLI-side concerns over that one flat result
// (03-RESEARCH.md Pattern 4), not a new store method: raw items are never
// scored, so ListItems' own `ORDER BY score DESC, created_at ASC, id ASC` is
// irrelevant here and the CLI re-sorts locally.
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

	items, err := s.ListItems(context.Background(), store.ListFilter{
		Stage:         model.StageRaw,
		Type:          *typeFlag,
		ExcludeClosed: true,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	// Newest first, with a deterministic tiebreak: sort.Slice is not
	// stable, and two items created in one second are not the tie risk a
	// missing tiebreak making output differ between runs is (03-CONTEXT.md
	// decision 4 / the plan's own <constraints>). CreatedAt sorts lexically
	// because model.TimeFormat is fixed-width UTC.
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return items[i].ID < items[j].ID
	})

	if *jsonFlag {
		if items == nil {
			items = []model.Item{}
		}
		if err := emitJSON(stdout, items); err != nil {
			fmt.Fprintf(stderr, "rounce: backlog: %v\n", err)
			return 1
		}
		return 0
	}

	renderBacklog(stdout, items)
	return 0
}

// renderBacklog writes the locked output shape (03-CONTEXT.md decision 4):
// a bare, unadorned header line per non-empty type group in schema-enum
// order, a blank line BETWEEN groups (never after the last one), and one
// "id  created  title" line per item, newest first within its group. Empty
// groups print nothing at all -- not even an empty header.
func renderBacklog(w io.Writer, items []model.Item) {
	if len(items) == 0 {
		return
	}

	// Id column width computed across ALL groups, not per group, so the
	// columns line up down the whole page.
	idW := 0
	for _, it := range items {
		if l := len(it.ID); l > idW {
			idW = l
		}
	}

	byType := make(map[string][]model.Item, len(backlogTypeOrder))
	for _, it := range items {
		byType[it.Type] = append(byType[it.Type], it)
	}

	budget := titleWidth(w)
	first := true
	for _, typ := range backlogTypeOrder {
		group := byType[typ]
		if len(group) == 0 {
			continue
		}
		if !first {
			fmt.Fprintln(w)
		}
		first = false

		fmt.Fprintln(w, typ)
		for _, it := range group {
			fmt.Fprintf(w, "%-*s  %s  %s\n", idW, it.ID, createdDate(it.CreatedAt), truncateTitle(it.Title, budget))
		}
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
