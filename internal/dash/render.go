package dash

import (
	"bytes"
	"fmt"

	"github.com/gshepptech/rounce/internal/model"
)

// This file's only non-stdlib import is internal/model -- the store package
// is deliberately absent, checkable directly by grepping this file for that
// package's own import path (a literal this comment avoids spelling out, so
// the check does not trip on its own description). That is what makes every
// render function below fixturable from struct literals alone, which is
// what makes DASH-06's golden test possible without
// a live database or a wall clock: rnc- ids are
// sha256(title, createdAt, attempt)-derived, and createdAt is wall-clock
// time, so a golden file built by round-tripping through a live
// store.AddItem would pass only on the machine and moment it was generated.

// renderReady renders the ready-queue view: the id (linked to /item/{{.ID}}),
// score, type, and title, in the order items is already in. No sort --
// ordering is store.Ready's job, not this package's.
func renderReady(items []model.Item) ([]byte, error) {
	return execute("ready", items)
}

// backlogRow pairs an item with whether it opens a new type group in the
// backlog view. This is presentation detection over the slice's own
// existing order -- items is already grouped-then-sorted by store.Backlog
// (contiguous per type, in model.TypesOrdered order); a row's NewGroup is
// true exactly when its Type differs from the previous row's Type, matching
// cmd/rounce/backlog.go's renderBacklog precedent. renderBacklog does not
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
	rows := make([]backlogRow, len(items))
	prevType := ""
	for i, it := range items {
		rows[i] = backlogRow{Item: it, NewGroup: i == 0 || it.Type != prevType}
		prevType = it.Type
	}
	return execute("backlog", rows)
}

// renderItem renders one item's full detail: header fields (show's own
// field order -- id, stage, type, status, score, parent, repo, paths, why,
// acceptance, created, updated), dependencies in both directions, event
// history oldest-first (already ordered by store.GetItem), then the body.
// The body renders inside <pre> as plain text -- never markdown-to-HTML,
// never template.HTML. A parent id and each dep's other end are links to
// /item/{{...}}, built from the id field alone.
func renderItem(d model.ItemDetail) ([]byte, error) {
	return execute("item", d)
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
// parameter: a returned []byte is what both the DASH-06 golden test and the
// DASH-05 rendered-output scan want; handlers (plan 03) write it themselves.
func execute(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("dash: render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
