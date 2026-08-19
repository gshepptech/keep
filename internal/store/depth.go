package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// MaxDepth is the deepest parent-child chain this store will hold: a root at
// depth 0 through a descendant at depth MaxDepth, so MaxDepth+1 items.
//
// Every recursive CTE in this package stops extending at this bound --
// the shipped ready view (schema.go), blockedTreeCTE (ready.go), treeCTE
// (tree.go), and parentChildCycleCheckSQL (items.go). Those bounds exist
// because the cost of being wrong is an unbounded loop inside a browser
// request or a user's shell. But a bound in a read query does not error when
// it is hit; it silently returns a SHORTER answer. A tree deeper than the
// bound would make `ready` quietly stop propagating a block partway down --
// the one guarantee the whole product rests on, failing without a word.
//
// So the bound is enforced where the depth is created, not where it is read.
// insertParentChildEdge -- the only writer of a parent-child row anywhere in
// this package -- refuses an edge that would put any item past MaxDepth.
// With that guard in place the read-side bounds are unreachable by
// construction: they remain as backstops against a store some other program
// corrupted, not as the thing keeping the recursion finite.
//
// Changing this constant changes the SQL text of the ready view. Existing
// stores keep the view they were created with (CREATE VIEW IF NOT EXISTS
// does not replace), so a change here needs a migration that drops and
// recreates the view -- see migrations.go.
const MaxDepth = 50

// maxDepthSQL is MaxDepth as it appears inside the recursive CTEs, so the
// number lives in exactly one place instead of the four it used to.
var maxDepthSQL = strconv.Itoa(MaxDepth)

// ancestorDepthSQL counts how many parent-child hops separate the bound id
// from its farthest root. A root returns 0. It deliberately walks one hop
// PAST MaxDepth so that a chain already at the limit is reported as
// MaxDepth+1 rather than saturating at MaxDepth and reading as "still room".
const ancestorDepthSQL = `
WITH RECURSIVE ancestors(id, depth) AS (
  SELECT ?, 0
  UNION ALL
  SELECT d.depends_on_id, anc.depth + 1
  FROM ancestors anc
  JOIN deps d ON d.item_id = anc.id AND d.type = 'parent-child'
  WHERE anc.depth <= $MAXDEPTH$
)
SELECT MAX(depth) FROM ancestors`

// descendantHeightSQL counts how many parent-child hops separate the bound
// id from its deepest descendant. A leaf returns 0. Like ancestorDepthSQL it
// walks one hop past the limit so a subtree already at the limit is
// distinguishable from one exactly at it.
const descendantHeightSQL = `
WITH RECURSIVE descendants(id, depth) AS (
  SELECT ?, 0
  UNION ALL
  SELECT d.item_id, t.depth + 1
  FROM descendants t
  JOIN deps d ON d.depends_on_id = t.id AND d.type = 'parent-child'
  WHERE t.depth <= $MAXDEPTH$
)
SELECT MAX(depth) FROM descendants`

// rowQuerier is the one method checkParentChildDepth needs, satisfied by
// both *sql.Tx and *sql.DB. The guard runs inside insertParentChildEdge's
// caller-provided transaction; the DB form exists for the read-side audit.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// expandDepthSQL substitutes MaxDepth into a recursive CTE's bound. The
// placeholder is $MAXDEPTH$ rather than a `?` because SQLite does not bind
// parameters inside a recursive CTE's WHERE clause in every driver version,
// and a literal is what the four bounds this replaces already were.
func expandDepthSQL(q string) string {
	return strings.ReplaceAll(q, "$MAXDEPTH$", maxDepthSQL)
}

// expandSchemaSQL is expandDepthSQL plus the schema version, for the one
// string that has to carry both: schemaDDL writes the meta.schema_version
// row a fresh store opens with, and typing the number there by hand is how
// a binary ends up creating stores at a version its own ladder does not
// agree with. schemaDDLv1 deliberately does NOT use this -- it is frozen at
// 1 and must stay there however far the ladder climbs.
func expandSchemaSQL(q string) string {
	return strings.ReplaceAll(expandDepthSQL(q), "$SCHEMAVERSION$", strconv.Itoa(schemaVersion))
}

// checkParentChildDepth refuses an edge that would put any item deeper than
// MaxDepth. It measures both halves of the prospective chain -- how far
// parentID already sits below its own root, and how far childID's own
// subtree already reaches below it -- because insertParentChildEdge is
// reachable with an existing child, not only a freshly created leaf. The
// resulting deepest item sits at:
//
//	ancestorDepth(parent) + 1 + descendantHeight(child)
//
// which must be <= MaxDepth.
func checkParentChildDepth(ctx context.Context, q rowQuerier, childID, parentID string) error {
	var parentDepth sql.NullInt64
	if err := q.QueryRowContext(ctx, expandDepthSQL(ancestorDepthSQL), parentID).Scan(&parentDepth); err != nil {
		return fmt.Errorf("checkParentChildDepth: ancestor depth of %s: %w", parentID, err)
	}
	var childHeight sql.NullInt64
	if err := q.QueryRowContext(ctx, expandDepthSQL(descendantHeightSQL), childID).Scan(&childHeight); err != nil {
		return fmt.Errorf("checkParentChildDepth: descendant height of %s: %w", childID, err)
	}

	deepest := parentDepth.Int64 + 1 + childHeight.Int64
	if deepest > MaxDepth {
		return fmt.Errorf(
			"checkParentChildDepth: %s under %s would put an item at depth %d, past the %d this store holds: %w",
			childID, parentID, deepest, MaxDepth, ErrTooDeep)
	}
	return nil
}

// DeepestChain reports the depth of the deepest parent-child chain in the
// store. It exists so a store written by an older binary -- or by another
// program -- can be audited against MaxDepth rather than trusted, and so
// migrations.go can refuse to raise MaxDepth over data that already
// violates the new bound.
func (s *Store) DeepestChain(ctx context.Context) (int, error) {
	const q = `
WITH RECURSIVE chain(id, depth) AS (
  SELECT i.id, 0 FROM items i
  WHERE NOT EXISTS (SELECT 1 FROM deps d WHERE d.item_id = i.id AND d.type = 'parent-child')
  UNION ALL
  SELECT d.item_id, c.depth + 1
  FROM chain c
  JOIN deps d ON d.depends_on_id = c.id AND d.type = 'parent-child'
  WHERE c.depth <= $MAXDEPTH$
)
SELECT COALESCE(MAX(depth), 0) FROM chain`

	var depth int
	if err := s.db.QueryRowContext(ctx, expandDepthSQL(q)).Scan(&depth); err != nil {
		return 0, fmt.Errorf("DeepestChain: %w", err)
	}
	return depth, nil
}
