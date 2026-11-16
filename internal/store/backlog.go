package store

import (
	"context"
	"fmt"

	"github.com/gshepptech/keep/internal/model"
)

// Backlog returns every raw, non-discarded item in canonical backlog order,
// optionally narrowed to one type ("" means every type).
//
// The ordering lives here, in SQL, and not in a caller, just as `ready`'s
// ordering lives in the view's own ORDER BY. Two surfaces render this list,
// `keep backlog` and the MCP `backlog` tool, and they can only be sure to
// agree if they walk one order rather than two implementations of it. Two
// orderings drifting apart is easy to miss: it only shows once items of
// two types interleave in time.
//
// typeFilter follows this package's existing optional-string-filter
// convention (ListFilter's fields): "" means no filter, never a bound ”.
//
// The trailing `id ASC` is not decoration. Two items added inside one
// transaction share a created_at (this package's one-nowFunc-per-
// transaction rule), and SQLite's row order for a tie is unspecified.
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
