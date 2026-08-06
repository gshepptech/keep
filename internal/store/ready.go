package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gshepptech/rounce/internal/model"
)

// itemColumns is the explicit, ordered column list every query returning a
// model.Item selects, in model.Item's own field order. It exists because the
// ready view is defined as `SELECT i.* FROM items i ...` and that `i.*` is
// NOT frozen at CREATE VIEW time -- it re-resolves against items' current
// shape on every query (confirmed: 23 columns became 24 immediately after a
// live ALTER TABLE, see 02-RESEARCH.md Pattern 6). A fixed-arity rows.Scan
// against `*` would break at query time on the first future migration, far
// from the change that caused it. Every later plan's item query in this
// package reuses this constant rather than re-listing the 23 columns.
const itemColumns = `id, parent_id, ordinal, stage, type, title, body, acceptance, status, value, confidence, effort, score, why, repo, paths, claimed_by_session, claimed_at, assignee, external_ref, created_at, updated_at, closed_at`

// scanItems scans rows produced by a query that selects itemColumns, in that
// exact order, into a []model.Item. This is the one place the 23
// rows.Scan destinations are written -- Ready and every later plan's item
// query in this package call this instead of each writing its own Scan call.
func scanItems(rows *sql.Rows) ([]model.Item, error) {
	var out []model.Item
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

// blockedTreeCTE is the single definition of rounce's blocking recursion
// that lives in Go. It is a deliberate mirror of the shipped ready view's
// own blocked_directly/blocked_tree CTE (internal/store/schema.go),
// extended with blocker_id and nearest_blocked_id so it can REPORT the
// blocker instead of only filtering on it. Blocked() and IsBlocked() both
// embed this constant; neither carries its own copy of the SQL text. That is
// what makes `blocked` and `ready` structurally incapable of disagreeing --
// see 02-RESEARCH.md Pattern 5, which derived this exact query, executed it
// against a real 5-item/3-level fixture, and cross-checked it against `ready`
// on the same fixture, confirming the two partition the open tickets exactly.
// If the view's CTE ever changes, this constant changes with it, in the same
// commit -- do not let the two drift into two independently-maintained tree
// walks.
//
// blocked_directly's SELECT DISTINCT is load-bearing: without it, an item
// blocked by two different `blocks` edges to the same target would appear
// twice. blocked_tree is intentionally NOT deduplicated across multiple
// independent blocking ancestors -- 02-RESEARCH.md's fixture never exercised
// that shape, and the query is used verbatim, not re-derived, per this
// plan's constraint. Callers that need a set (the partition test, IsBlocked's
// LIMIT 1) tolerate or collapse duplicates themselves; Blocked() returns
// whatever distinct-by-construction rows blocked_tree produces for the
// simple/common case this phase's fixtures exercise.
const blockedTreeCTE = `
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
    WHERE d.type = 'parent-child' AND bt.depth < 50
  )
`

// Ready returns the open, unblocked tickets the shipped ready view already
// computes, in the view's own order (score DESC, created_at ASC), honouring
// limit when it is > 0 (limit <= 0 means no limit).
//
// This query has NO WHERE clause and re-filters nothing the view already
// filters. stage='ticket', status='open', and the blocked-tree exclusion all
// live inside the view (READY-01/READY-02/READY-03) on purpose -- the view
// is the single place those predicates are allowed to exist. Adding any of
// them again here "for clarity" would be a second place they could drift
// from the view (docs/design/phase-1.md's "Specific Ideas" names this exact
// trap; 02-CONTEXT.md's carried-forward decisions repeat it). Ready's job is
// to select named columns from the view and add LIMIT. Nothing else.
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
// of the nearest item blocking it (READY-04). It embeds blockedTreeCTE
// rather than a second copy of the tree-walk SQL, and applies the exact same
// base predicate the ready view applies (stage='ticket' AND status='open')
// -- not as a defensive re-filter of something else already filters, but
// because Blocked does not read from the view at all: it is the view's
// structural inverse, and this predicate is what "open ticket" means for
// both queries. Ordered i.score DESC, i.created_at ASC to match ready's own
// ordering, since the exit gate reads both lists side by side.
//
// Settled here (02-RESEARCH.md Open Question 2): Blocked names ONE blocker
// per item -- the nearest, which the recursion's own traversal produces via
// nearest_blocked_id/blocker_id. READY-04 requires only that a blocker be
// named. Showing all blockers is a backward-compatible later addition (a
// JSON array instead of a scalar) -- do not "improve" this into a
// multi-row-per-item result; that breaks the one-line-per-item output shape
// CONTEXT.md decision 2 specifies.
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

	var out []model.BlockedItem
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
// applies no stage/status predicate of its own -- unlike Blocked, it answers
// a narrower question ("is this specific item currently blocked?") for any
// item, ticket or not, which is what plan 06's StartItem needs for its
// refusal check (02-CONTEXT.md decision 3: the ErrInvalidTransition message
// must name the blocker). It embeds the same blockedTreeCTE constant Blocked
// does, so the two can never disagree about which items are blocked.
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
