package store

import (
	"context"
	"fmt"

	"github.com/gshepptech/rounce/internal/model"
)

// Backlog returns every raw, non-discarded item in canonical backlog order,
// optionally narrowed to one type ("" means every type).
//
// The ordering lives HERE, in SQL, and not in a caller. `ready`'s ordering
// has always lived in the store (inside the view's own ORDER BY);
// backlog's living in cmd/rounce was the anomaly. Two surfaces render this
// list -- `rounce backlog` and the MCP `backlog` tool -- and the only way
// they cannot disagree is if they walk ONE order by construction rather
// than two implementations agreeing. Phase 3 wave 5 shipped and fixed
// exactly that disagreement once already, between backlog's rendered and
// --json output; it escaped notice until two types interleaved in time.
//
// typeFilter follows this package's existing optional-string-filter
// convention (ListFilter's fields): "" means no filter, never a bound ''.
//
// The trailing `id ASC` is not decoration. Two items added inside one
// transaction share a created_at (this package's one-nowFunc-per-
// transaction rule), and SQLite's row order for a tie is unspecified --
// cmd/rounce/backlog.go's now-removed CLI-side grouping helper carried an
// ID-ascending tiebreak for this exact reason and it must not be lost on
// the way down here.
func (s *Store) Backlog(ctx context.Context, typeFilter string) ([]model.Item, error) {
	query := `SELECT ` + itemColumns + ` FROM items
WHERE stage = 'raw' AND status NOT IN ('closed', 'dropped')`
	var args []any
	if typeFilter != "" {
		query += ` AND type = ?`
		args = append(args, typeFilter)
	}
	query += ` ORDER BY CASE type WHEN 'roadmap' THEN 0 WHEN 'epic' THEN 1
                   WHEN 'task' THEN 2 WHEN 'bug' THEN 3
                   WHEN 'note' THEN 4 END,
         created_at DESC, id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("Backlog: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}
