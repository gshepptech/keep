package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/gshepptech/rounce/internal/model"
)

// treeCTE walks DOWN the parent-child tree from a bound root id. It is
// appended to blockedTreeCTE (which already opens `WITH RECURSIVE`), so
// Tree carries no second copy of the blocking recursion -- Tree is
// blockedTreeCTE's fifth consumer, after Ready's view, Blocked, IsBlocked
// and isBlockedTx. That shared constant is the whole reason the tree view
// and `rounce ready` cannot disagree about which items are blocked
// (DASH-07); a second, independently written recursion here would be the
// one way to make that claim false.
//
// THE DIRECTION TRAP. parent-child rows are (item_id = child,
// depends_on_id = parent). Walking DOWN therefore joins the recursive step
// on `d.depends_on_id = t.item_id` -- the OPPOSITE of
// parentChildCycleCheckSQL in items.go, which joins on `d.item_id = anc.id`
// because it walks UP. items.go is the only descending/ascending CTE
// precedent in this package, so copying its join direction here is the
// natural mistake. It does not error. Verified live against this repository
// during 05 planning: the wrong-direction query on a real three-item tree
// returned exactly ONE row (the root), silently -- a tree view rendering a
// single node looks like an empty roadmap, not a defect. tree_test.go
// asserts descendant COUNT for exactly this reason.
//
// path is a materialised depth-first sort key: the parent's path plus the
// child's zero-padded ordinal. ORDER BY path (a plain TEXT sort) reproduces
// exact tree reading order, so no caller sorts anything. depth < 50 mirrors
// the ready view's own bound: `dep` refuses to create a parent-child cycle
// at write time, but the cost of being wrong is an unbounded loop inside a
// browser request.
//
// blocked_nearest is not optional and is not decoration. blocked_tree
// (blockedTreeCTE, above) is deliberately NOT deduplicated across multiple
// independent blocking ancestors -- its own doc comment says so. A plain
// `LEFT JOIN blocked_tree` therefore MULTIPLIES tree rows: verified live
// against this repository during 05 planning, a three-item tree where the
// epic and its child were each blocked by a different open item produced 5
// rows for a 3-item tree via a naive join, and exactly 3 (one per item, each
// naming the nearer ancestor) via the ROW_NUMBER()-per-item dedup below.
// Window functions are confirmed working on this project's pinned
// modernc.org/sqlite v1.56.0. Do NOT substitute `GROUP BY item_id` +
// bare-column `MIN(depth)`: it depends on SQLite's single-`min()`
// bare-column special case, and this query names `MIN(depth)` twice, which
// that special case does not cover.
const treeCTE = `,
  tree AS (
    SELECT id AS item_id, 0 AS depth, '' AS path
    FROM items WHERE id = ?
    UNION ALL
    SELECT d.item_id, t.depth + 1,
           t.path || '.' || printf('%08d', COALESCE(c.ordinal, 0))
    FROM tree t
    JOIN deps d ON d.depends_on_id = t.item_id AND d.type = 'parent-child'
    JOIN items c ON c.id = d.item_id
    WHERE t.depth < 50
  ),
  blocked_nearest AS (
    SELECT item_id,
           CASE WHEN depth = 0 THEN blocker_id ELSE nearest_blocked_id END AS blocked_by,
           ROW_NUMBER() OVER (PARTITION BY item_id ORDER BY depth ASC, blocker_id ASC) AS rn
    FROM blocked_tree
  )
`

// Tree returns rootID and every descendant beneath it in the parent-child
// tree, depth-first, each annotated with its depth and blocked state.
//
// It walks `deps` where type = 'parent-child', NEVER items.parent_id. The
// two are written together in one transaction (items.go's dual write) and
// the shipped ready view reads only deps for structure -- a tree built from
// parent_id would agree with every parent_id assertion while disagreeing
// with `ready` about the same hierarchy, which DASH-07 makes a requirement
// violation.
//
// Closed and dropped descendants are INCLUDED. A tree that hides closed
// children misrepresents the shape of the work; the renderer may dim them,
// the query does not decide they do not exist (05-CONTEXT.md decision 1).
//
// An unknown rootID returns ErrNotFound wrapped with the id, matching
// GetItem's precedent rather than returning an empty slice and a nil error
// -- confirmed live during planning that the query itself simply produces
// zero rows for an unknown root, so the check must be explicit here. The
// alternative would make a typo'd id render an empty page and exit 0.
func (s *Store) Tree(ctx context.Context, rootID string) ([]model.TreeNode, error) {
	query := blockedTreeCTE + treeCTE + `
SELECT t.depth, ` + itemColumns + `,
       CASE WHEN bn.item_id IS NOT NULL THEN 1 ELSE 0 END,
       COALESCE(bn.blocked_by, '')
FROM tree t
JOIN items i ON i.id = t.item_id
LEFT JOIN blocked_nearest bn ON bn.item_id = t.item_id AND bn.rn = 1
ORDER BY t.path`

	rows, err := s.db.QueryContext(ctx, query, rootID)
	if err != nil {
		return nil, fmt.Errorf("Tree: %w", err)
	}
	defer rows.Close()

	out, err := scanTreeNodes(rows)
	if err != nil {
		return nil, fmt.Errorf("Tree: %w", err)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("item %q: %w", rootID, ErrNotFound)
	}
	return out, nil
}

// scanTreeNodes scans rows produced by Tree's query: a leading depth column,
// itemColumns, then trailing blocked/blocked_by columns. It is a local loop
// rather than a call to scanItems -- Tree's row carries a leading depth and
// two trailing columns scanItems does not expect, the same reason Blocked's
// own scan loop does not call scanItems either.
func scanTreeNodes(rows *sql.Rows) ([]model.TreeNode, error) {
	var out []model.TreeNode
	for rows.Next() {
		var n model.TreeNode
		it := &n.Item
		if err := rows.Scan(
			&n.Depth,
			&it.ID, &it.ParentID, &it.Ordinal, &it.Stage, &it.Type, &it.Title,
			&it.Body, &it.Acceptance, &it.Status, &it.Value, &it.Confidence,
			&it.Effort, &it.Score, &it.Why, &it.Repo, &it.Paths,
			&it.ClaimedBySession, &it.ClaimedAt, &it.Assignee, &it.ExternalRef,
			&it.CreatedAt, &it.UpdatedAt, &it.ClosedAt,
			&n.Blocked, &n.BlockedBy,
		); err != nil {
			return nil, fmt.Errorf("scanTreeNodes: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanTreeNodes: %w", err)
	}
	return out, nil
}
