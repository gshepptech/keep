package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/gshepptech/keep/internal/model"
)

// GetItem returns id's full detail: the item itself, its dependency edges in
// both directions, and its complete event history oldest first. This is the
// read surface `show` (ITEM-04) is built on.
//
// Direction, fixed in plan 02 and unchanged here (02-05-PLAN.md
// <interfaces>): deps(item_id = A, depends_on_id = B) reads "A depends on
// B". So for item X:
//   - DependsOn = rows WHERE item_id = X -- what X is waiting on, what X is
//     a child of, what X was derived from.
//   - DependedOnBy = rows WHERE depends_on_id = X -- what is waiting on X,
//     X's children, what was derived from X.
//
// An unknown id returns ErrNotFound wrapped with the id, not a zero
// ItemDetail and a nil error -- the latter is the failure mode that would
// make `show` print an empty block and exit 0.
func (s *Store) GetItem(ctx context.Context, id string) (model.ItemDetail, error) {
	var out model.ItemDetail

	itemRows, err := s.db.QueryContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id = ?`, id)
	if err != nil {
		return model.ItemDetail{}, fmt.Errorf("GetItem: %w", err)
	}
	items, err := scanItems(itemRows)
	itemRows.Close()
	if err != nil {
		return model.ItemDetail{}, fmt.Errorf("GetItem: %w", err)
	}
	if len(items) == 0 {
		return model.ItemDetail{}, fmt.Errorf("item %q: %w", id, ErrNotFound)
	}
	out.Item = items[0]

	// DependsOn: what this item is item_id for -- what it is waiting on,
	// what it is a child of, what it was derived from.
	dependsOn, err := queryDeps(ctx, s.db,
		`SELECT item_id, depends_on_id, type, created_at, created_by FROM deps WHERE item_id = ? ORDER BY type, depends_on_id`, id)
	if err != nil {
		return model.ItemDetail{}, fmt.Errorf("GetItem: DependsOn: %w", err)
	}
	out.DependsOn = dependsOn

	// DependedOnBy: what this item is depends_on_id for -- what is waiting
	// on it, its children, what was derived from it.
	dependedOnBy, err := queryDeps(ctx, s.db,
		`SELECT item_id, depends_on_id, type, created_at, created_by FROM deps WHERE depends_on_id = ? ORDER BY type, item_id`, id)
	if err != nil {
		return model.ItemDetail{}, fmt.Errorf("GetItem: DependedOnBy: %w", err)
	}
	out.DependedOnBy = dependedOnBy

	// Ordered by id, not by at. id is INTEGER PRIMARY KEY AUTOINCREMENT and
	// is monotonic within a store; at is a TEXT column that can tie when two
	// events land in the same transaction (nothing in this phase writes two
	// events in one transaction, but a later phase might), and a tied sort
	// on at would make the history's order between those two rows
	// non-deterministic -- id has no such tie, ever.
	events, err := queryEvents(ctx, s.db, id)
	if err != nil {
		return model.ItemDetail{}, fmt.Errorf("GetItem: Events: %w", err)
	}
	out.Events = events

	return out, nil
}

// queryDeps runs one of GetItem's two direction-specific deps queries and
// scans the results into []model.Dep. Both directions share this scanner
// rather than each writing its own rows.Scan call.
func queryDeps(ctx context.Context, db *sql.DB, query, id string) ([]model.Dep, error) {
	rows, err := db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Dep{}
	for rows.Next() {
		var d model.Dep
		if err := rows.Scan(&d.ItemID, &d.DependsOnID, &d.Type, &d.CreatedAt, &d.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// queryEvents returns itemID's full event history, ordered oldest first by
// id ascending (see GetItem's doc comment for why id, not at).
func queryEvents(ctx context.Context, db *sql.DB, itemID string) ([]model.Event, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, item_id, kind, actor, session_id, at, payload FROM events WHERE item_id = ? ORDER BY id ASC`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Event{}
	for rows.Next() {
		var e model.Event
		if err := rows.Scan(&e.ID, &e.ItemID, &e.Kind, &e.Actor, &e.SessionID, &e.At, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListFilter's zero value means "no filter" -- an empty field is omitted
// from the WHERE clause entirely rather than bound as ”. Filters compose as
// AND (02-CONTEXT.md "Claude's Discretion": yes, the obvious reading).
//
// json tags (04-02): this struct doubles as the `list` MCP tool's wire type
// directly (internal/mcp). Every field is optional -- a bare `list` with no
// arguments is legal and matches every existing caller's zero-value use.
// Tags are additive and behaviourally inert for Go callers, all of which
// construct this struct with named fields.
type ListFilter struct {
	Stage    string `json:"stage,omitempty"`
	Status   string `json:"status,omitempty"`
	Type     string `json:"type,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
	Repo     string `json:"repo,omitempty"`

	// ExcludeClosed adds `status NOT IN ('closed', 'dropped')` to the WHERE
	// clause when true -- CAP-03's caller (backlog, cmd/keep/backlog.go)
	// is the first ListItems caller that needs an exclusion rather than an
	// equality filter: "every raw item still parked" means "not explicitly
	// discarded", which Status (a single `col = ?`) cannot express. The zero
	// value (false) changes nothing for every existing caller.
	//
	// This is the exact literal already shipped in schema.go's `ready` view
	// (`blocked_directly`'s `b.status NOT IN ('closed', 'dropped')`) --
	// copied verbatim, not re-derived, so the two predicates cannot drift
	// apart unnoticed (03-RESEARCH.md Pitfall 5).
	ExcludeClosed bool `json:"exclude_closed,omitempty"`

	Limit int `json:"limit,omitempty"`
}

// ListItems queries items directly -- never the ready view -- so list (ITEM-
// 05) can show raw items and closed/dropped items alike, which ready/blocked
// deliberately cannot. Each of Stage/Status/Type/ParentID/Repo, when
// non-empty, is bound as one `col = ?` condition; all present conditions
// join with AND. Filter values are never validated against the closed
// vocabularies here -- that belongs at the CLI boundary (plan 08); an
// unrecognized value here is simply a filter that matches nothing, which is
// the honest answer to the question asked.
//
// Results are ordered score DESC, created_at ASC, id ASC. The trailing id
// ASC matters: two children added inside one transaction share a created_at
// (plan 02's single-nowFunc()-per-transaction rule), and without a final
// tiebreak SQLite's row order for equal (score, created_at) pairs is
// unspecified -- that would make list's output differ between runs.
func (s *Store) ListItems(ctx context.Context, f ListFilter) ([]model.Item, error) {
	var conds []string
	var args []any

	if f.Stage != "" {
		conds = append(conds, "stage = ?")
		args = append(args, f.Stage)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Type != "" {
		conds = append(conds, "type = ?")
		args = append(args, f.Type)
	}
	if f.ParentID != "" {
		conds = append(conds, "parent_id = ?")
		args = append(args, f.ParentID)
	}
	if f.Repo != "" {
		conds = append(conds, "repo = ?")
		args = append(args, f.Repo)
	}
	if f.ExcludeClosed {
		// A constant predicate with no bind parameter -- appends to conds
		// only, never to args.
		conds = append(conds, "status NOT IN ('closed', 'dropped')")
	}

	query := `SELECT ` + itemColumns + ` FROM items`
	for i, c := range conds {
		if i == 0 {
			query += " WHERE "
		} else {
			query += " AND "
		}
		query += c
	}
	query += " ORDER BY score DESC, created_at ASC, id ASC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListItems: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}
