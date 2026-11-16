package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gshepptech/keep/internal/model"
)

// itemColumns is the explicit, ordered column list every query returning a
// model.Item selects, in model.Item's own field order. It exists because the
// ready view is defined as `SELECT i.* FROM items i ...` and that `i.*` is not
// frozen at CREATE VIEW time: it re-resolves against items' current shape on
// every query (a live ALTER TABLE changes its width immediately). A
// fixed-arity rows.Scan against `*` would break at query time on the next
// migration that adds a column, far from the change that caused it. Every item
// query in this package reuses this constant rather than re-listing the
// columns.
const itemColumns = `id, parent_id, ordinal, stage, type, title, body, acceptance, status, value, confidence, effort, score, why, repo, paths, claimed_by_session, claimed_at, assignee, external_ref, created_at, updated_at, closed_at`

// scanItems scans rows produced by a query that selects itemColumns, in that
// exact order, into a []model.Item. This is the one place the rows.Scan
// destinations are written; Ready and every other item query in this
// package call this instead of each writing its own Scan call.
//
// An empty result is []model.Item{}, never nil. This is the store's
// contract, not a courtesy to one surface: a nil slice marshals to JSON
// `null` and an empty one to `[]`, and if each surface normalised that for
// itself, `keep ready --json` and the `ready` MCP tool could disagree about
// the same empty store. Fixing it here fixes it for the CLI, the MCP
// server, the dashboard, and whatever reads the store next.
func scanItems(rows *sql.Rows) ([]model.Item, error) {
	out := []model.Item{}
	for rows.Next() {
		var it model.Item
		if err := rows.Scan(
			&it.ID, &it.ParentID, &it.Ordinal, &it.Stage, &it.Type, &it.Title,
			&it.Body, &it.Acceptance, &it.Status, &it.Value, &it.Confidence,
			&it.Effort, &it.Score, &it.Why, &it.Repo, &it.Paths,
			&it.ClaimedBySession, &it.ClaimedAt, &it.Assignee, &it.ExternalRef,
			&it.CreatedAt, &it.UpdatedAt, &it.ClosedAt,
		); err != nil {
			return nil, fmt.Errorf("scanItems: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanItems: %w", err)
	}
	return out, nil
}

// blockedTreeCTE is the single definition of keep's blocking recursion
// that lives in Go. It mirrors the ready view's own
// blocked_directly/blocked_tree CTE (internal/store/schema.go), extended
// with blocker_id and nearest_blocked_id so it can report the blocker
// instead of only filtering on it. Blocked() and IsBlocked() both embed this
// constant; neither carries its own copy of the SQL text, so `blocked` and
// `ready` cannot disagree. TestBlockedNamesTheBlocker cross-checks the two
// on a 5-item/3-level fixture and confirms they partition the open tickets
// exactly. A change to the view's CTE needs the same change here, in the
// same commit, so the two never become independently maintained tree walks.
//
// blocked_directly's SELECT DISTINCT matters: without it, an item
// blocked by two different `blocks` edges to the same target would appear
// twice. blocked_tree is intentionally not deduplicated across multiple
// independent blocking ancestors, so that the query stays a faithful
// mirror of the view's. Callers that need a set (the partition test,
// IsBlocked's LIMIT 1) tolerate or collapse duplicates themselves; Blocked()
// returns whatever distinct-by-construction rows blocked_tree produces for
// the simple/common case.
var blockedTreeCTE = expandDepthSQL(`
WITH RECURSIVE
  blocked_directly AS (
    SELECT DISTINCT d.item_id, d.depends_on_id AS blocker_id
    FROM deps d
    WHERE d.type = 'blocks'
      AND EXISTS (
        SELECT 1 FROM items b
        WHERE b.id = d.depends_on_id AND b.status NOT IN ('closed', 'dropped')
      )
  ),
  blocked_tree AS (
    SELECT item_id, item_id AS nearest_blocked_id, blocker_id, 0 AS depth
    FROM blocked_directly
    UNION ALL
    SELECT d.item_id, bt.nearest_blocked_id, bt.blocker_id, bt.depth + 1
    FROM blocked_tree bt
    JOIN deps d ON d.depends_on_id = bt.item_id
    WHERE d.type = 'parent-child' AND bt.depth < $MAXDEPTH$
  )
`)

// Ready returns the open, unblocked tickets the ready view already
// computes, in the view's own order (score DESC, created_at ASC), honouring
// limit when it is > 0 (limit <= 0 means no limit).
//
// This query has no WHERE clause and re-filters nothing the view already
// filters. stage='ticket', status='open', and the blocked-tree exclusion all
// live inside the view, which is the single place those predicates exist.
// Repeating any of them here would be a second place they could drift from
// the view. Ready selects named columns from the view and adds LIMIT.
func (s *Store) Ready(ctx context.Context, limit int) ([]model.Item, error) {
	query := `SELECT ` + itemColumns + ` FROM ready`
	var args []any
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("Ready: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

// Blocked returns every open ticket ready omits, each annotated with the id
// of the nearest item blocking it. It embeds blockedTreeCTE rather than a
// second copy of the tree-walk SQL, and applies the exact same base
// predicate the ready view applies (stage='ticket' AND status='open'). That
// is not a defensive re-filter: Blocked does not read from the view at all.
// It is the view's structural inverse, and this predicate is what "open
// ticket" means for both queries. Ordered i.score DESC, i.created_at ASC to
// match ready's own ordering, since people read both lists side by side.
//
// Blocked names one blocker per item, the nearest, which the recursion's
// own traversal produces via nearest_blocked_id/blocker_id. Showing all
// blockers could be added compatibly as a JSON array instead of a scalar.
// A multi-row-per-item result would break the one-line-per-item output
// shape of `keep blocked`.
func (s *Store) Blocked(ctx context.Context) ([]model.BlockedItem, error) {
	query := blockedTreeCTE + `
SELECT ` + itemColumns + `,
       CASE WHEN bt.depth = 0 THEN bt.blocker_id ELSE bt.nearest_blocked_id END AS blocked_by
FROM blocked_tree bt
JOIN items i ON i.id = bt.item_id
WHERE i.stage = 'ticket' AND i.status = 'open'
ORDER BY i.score DESC, i.created_at ASC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("Blocked: %w", err)
	}
	defer rows.Close()

	out := []model.BlockedItem{}
	for rows.Next() {
		var it model.Item
		var blockedBy string
		if err := rows.Scan(
			&it.ID, &it.ParentID, &it.Ordinal, &it.Stage, &it.Type, &it.Title,
			&it.Body, &it.Acceptance, &it.Status, &it.Value, &it.Confidence,
			&it.Effort, &it.Score, &it.Why, &it.Repo, &it.Paths,
			&it.ClaimedBySession, &it.ClaimedAt, &it.Assignee, &it.ExternalRef,
			&it.CreatedAt, &it.UpdatedAt, &it.ClosedAt,
			&blockedBy,
		); err != nil {
			return nil, fmt.Errorf("Blocked: scan: %w", err)
		}
		out = append(out, model.BlockedItem{Item: it, BlockedBy: blockedBy})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Blocked: rows: %w", err)
	}
	return out, nil
}

// IsBlocked reports whether id appears anywhere in blockedTreeCTE's
// blocked_tree, returning the nearest blocking item's id when it does. It
// applies no stage/status predicate of its own. Unlike Blocked, it answers
// a narrower question ("is this specific item currently blocked?") for any
// item, ticket or not, which is what StartItem needs for its refusal check
// (the ErrInvalidTransition message must name the blocker). It embeds the
// same blockedTreeCTE constant Blocked does, so the two can never disagree
// about which items are blocked.
func (s *Store) IsBlocked(ctx context.Context, id string) (string, bool, error) {
	query := blockedTreeCTE + `
SELECT CASE WHEN bt.depth = 0 THEN bt.blocker_id ELSE bt.nearest_blocked_id END AS blocked_by
FROM blocked_tree bt
WHERE bt.item_id = ?
LIMIT 1`

	var blockedBy string
	err := s.db.QueryRowContext(ctx, query, id).Scan(&blockedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("IsBlocked: %w", err)
	}
	return blockedBy, true, nil
}
