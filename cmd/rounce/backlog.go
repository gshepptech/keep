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

	// orderBacklog groups by type in schema-enum order, sorting each group
	// newest first with a deterministic tiebreak, and flattens the result
	// into ONE ordered slice -- the single source of order for both the
	// rendered text (which re-derives its group boundaries from
	// consecutive equal Types, since orderBacklog already grouped them) and
	// --json's flat array. --json must be "the same order the rendered
	// output uses" (the plan's own <behavior>/<constraints>): a plain
	// CreatedAt-only sort across all types would NOT match the rendered
	// order once two different types interleave in time, so both outputs
	// share this one ordering instead of computing it twice and risking
	// drift between them.
	ordered := orderBacklog(items)

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

// orderBacklog groups items by Type in backlogTypeOrder (schema-enum order,
// never alphabetical or map-iteration order), sorts each group newest first
// -- CreatedAt descending, ID ascending as a deterministic tiebreak, since
// sort.Slice is not stable and a missing tiebreak would make output differ
// between runs -- and returns the concatenation of the non-empty groups in
// that order. This single ordering is what both the rendered text and
// --json's flat array walk.
func orderBacklog(items []model.Item) []model.Item {
	byType := make(map[string][]model.Item, len(backlogTypeOrder))
	for _, it := range items {
		byType[it.Type] = append(byType[it.Type], it)
	}

	var out []model.Item
	for _, typ := range backlogTypeOrder {
		group := byType[typ]
		if len(group) == 0 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			if group[i].CreatedAt != group[j].CreatedAt {
				return group[i].CreatedAt > group[j].CreatedAt
			}
			return group[i].ID < group[j].ID
		})
		out = append(out, group...)
	}
	return out
}

// renderBacklog writes the locked output shape (03-CONTEXT.md decision 4):
// a bare, unadorned header line per non-empty type group in schema-enum
// order, a blank line BETWEEN groups (never after the last one), and one
// "id  created  title" line per item, newest first within its group. Empty
// groups print nothing at all -- not even an empty header. ordered must
// already be grouped-then-sorted (orderBacklog's output) -- a new group
// boundary is detected by Type changing between consecutive items, since
// backlogTypeOrder guarantees each type's rows are contiguous.
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
