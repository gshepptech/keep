package dash

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	"github.com/gshepptech/keep/internal/model"
)

// This file's only non-stdlib import is internal/model -- the store package
// is deliberately absent, checkable directly by grepping this file for that
// package's own import path (a literal this comment avoids spelling out, so
// the check does not trip on its own description). That is what makes every
// render function below fixturable from struct literals alone, which is
// what makes the tree view's golden test possible without
// a live database or a wall clock: rnc- ids are
// sha256(title, createdAt, attempt)-derived, and createdAt is wall-clock
// time, so a golden file built by round-tripping through a live
// store.AddItem would pass only on the machine and moment it was generated.

// renderReady renders the ready-queue view: the id (linked to /item/{{.ID}}),
// score, type, and title, in the order items is already in. No sort --
// ordering is store.Ready's job, not this package's.
func renderReady(items []model.Item) ([]byte, error) {
	return execute("board", boardData{Ready: items, Counts: boardCounts{Ready: len(items)}})
}

// backlogRow pairs an item with whether it opens a new type group in the
// backlog view. This is presentation detection over the slice's own
// existing order -- items is already grouped-then-sorted by store.Backlog
// (contiguous per type, in model.TypesOrdered order); a row's NewGroup is
// true exactly when its Type differs from the previous row's Type, matching
// cmd/keep/backlog.go's renderBacklog precedent. renderBacklog does not
// sort or re-group; ranging over model.TypesOrdered and filtering here would
// silently drop any type the slice's own order did not anticipate.
type backlogRow struct {
	model.Item
	NewGroup bool
}

// renderBacklog renders the raw backlog, grouped by type via backlogRow
// above -- a group header row is emitted in backlog.html whenever NewGroup
// is true.
func renderBacklog(items []model.Item) ([]byte, error) {
	return execute("backlog", backlogRows(items))
}

// backlogRows is the grouping itself, split out so a handler can build the
// rows without also rendering them -- the page wrapper needs the data, not
// the bytes.
func backlogRows(items []model.Item) []backlogRow {
	rows := make([]backlogRow, len(items))
	prevType := ""
	for i, it := range items {
		rows[i] = backlogRow{Item: it, NewGroup: i == 0 || it.Type != prevType}
		prevType = it.Type
	}
	return rows
}

// renderItem renders one item's full detail: header fields (show's own
// field order -- id, stage, type, status, score, parent, repo, paths, why,
// acceptance, created, updated), dependencies in both directions, event
// history oldest-first (already ordered by store.GetItem), then the body.
// The body renders inside <pre> as plain text -- never markdown-to-HTML,
// never template.HTML. A parent id and each dep's other end are links to
// /item/{{...}}, built from the id field alone.
// The item view carries the lease holding it back, if any. renderItem takes
// a bare detail and wraps it with no hold, because its callers -- the
// golden-file smoke test and any future non-HTTP renderer -- have no
// Interlock to ask. The handler builds the view itself.
func renderItem(d model.ItemDetail) ([]byte, error) {
	return execute("item", itemView{ItemDetail: d})
}

// renderTree renders one row per model.TreeNode, indented by Depth via the
// depthClass FuncMap helper (templates.go) rather than an inline style.
// nodes is already depth-first ordered by store.Tree's own `ORDER BY path`;
// this function does not sort.
func renderTree(nodes []model.TreeNode) ([]byte, error) {
	return execute("tree", nodes)
}

// execute runs the named top-level view template (each of which composes
// templates/layout.html's shared "head"/"foot" blocks itself -- see that
// file's header comment for why there is no single shared "layout" template
// dispatching by name) into a buffer and returns its bytes. No io.Writer
// parameter: a returned []byte is what both the golden test and the
// rendered-output scan want; handlers write it themselves.
// execute renders one view from data alone, with no server behind it.
//
// It wraps data in the same page envelope a request would, minus the parts
// only a server can supply: no CSRF token, so no mutating control renders,
// and Writable false, so the templates skip every action. That is what keeps
// these functions fixturable from struct literals -- the property render.go
// documents about itself and the golden file depends on.
func execute(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, page{Data: data}); err != nil {
		return nil, fmt.Errorf("dash: render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// relativeTime renders an RFC3339 instant as the distance from now: "in 4m",
// "12s ago", "expired".
//
// It exists because a lease's expiry is the one field on /who that a person
// reads to make a decision -- wait, or pick something else -- and
// "2026-08-14T03:56:17Z" does not answer that without arithmetic. Printing
// the raw field is the machine's convenience, not the reader's.
//
// Presentation only, and the third such function in this package after state
// and depthClass. It decides how a value is spelled, never what it means:
// the instant itself comes from Interlock, and a value this cannot parse is
// passed through untouched rather than guessed at or hidden.
func relativeTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	return "in " + shortDuration(d)
}

// shortDuration is the compact spelling: one unit, no decimals, rounded
// down. "in 1m" rather than "in 1 minute 4 seconds" -- the reader is
// deciding whether to wait, and the second unit never changes that answer.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// agoTime renders an RFC3339 instant as how long ago it was: "12s ago".
//
// Separate from relativeTime rather than one function branching on sign,
// because the two spell a past instant differently on purpose. A lease that
// has passed its expiry is "expired" -- the fact worth reporting is that it
// no longer holds anything, not how long ago that became true. A session
// last seen in the past is "12s ago", where the elapsed time is the whole
// point. One function would have to guess which of those a caller meant.
func agoTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Since(t)
	if d < 0 {
		return "just now"
	}
	return shortDuration(d) + " ago"
}
