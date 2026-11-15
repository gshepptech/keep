package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// countDeps returns the total number of rows in deps, for tests that assert
// "no row was written" by diffing a before/after count -- mirrors
// testhelp_test.go's countEvents.
func countDeps(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM deps").Scan(&n); err != nil {
		t.Fatalf("countDeps: %v", err)
	}
	return n
}

// countDepsOfType returns how many deps rows exactly match
// (item_id, depends_on_id, type) -- the table's own PRIMARY KEY, so this is
// always 0 or 1, but tests read better asserting "exactly 1" than "exists".
func countDepsOfType(t *testing.T, s *Store, itemID, dependsOnID, typ string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM deps WHERE item_id = ? AND depends_on_id = ? AND type = ?",
		itemID, dependsOnID, typ).Scan(&n); err != nil {
		t.Fatalf("countDepsOfType: %v", err)
	}
	return n
}

// containsID reports whether id is present in ids -- used against
// readyIDs's output.
func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// TestCreateDepVocabulary proves blocks/related/derived-from each
// succeed and produce exactly one deps row of that type; anything outside
// the closed vocabulary fails before any row is written.
func TestCreateDepVocabulary(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := mustAdd(t, s, AddItemInput{Title: "a", Actor: "tester"})
	b := mustAdd(t, s, AddItemInput{Title: "b", Actor: "tester"})

	for _, typ := range []string{model.DepBlocks, model.DepRelated, model.DepDerivedFrom} {
		t.Run(typ, func(t *testing.T) {
			if err := s.CreateDep(ctx, DepInput{ItemID: a.ID, DependsOnID: b.ID, Type: typ, Actor: "tester"}); err != nil {
				t.Fatalf("CreateDep(%s): %v", typ, err)
			}
			if n := countDepsOfType(t, s, a.ID, b.ID, typ); n != 1 {
				t.Fatalf("CreateDep(%s) produced %d deps rows, want exactly 1", typ, n)
			}
		})
	}

	for _, typ := range []string{"depends-on", "blocked-by", ""} {
		t.Run("rejects "+typ, func(t *testing.T) {
			before := countDeps(t, s)
			err := s.CreateDep(ctx, DepInput{ItemID: a.ID, DependsOnID: b.ID, Type: typ, Actor: "tester"})
			if !errors.Is(err, ErrInvalidDepType) {
				t.Fatalf("CreateDep(%q) = %v, want ErrInvalidDepType", typ, err)
			}
			if after := countDeps(t, s); after != before {
				t.Fatalf("CreateDep(%q) wrote %d deps rows, want 0", typ, after-before)
			}
		})
	}
}

// TestCreateDepRejectsSelfEdge proves every one of
// the four types rejects a -> a. parent-child is special-cased to accept
// either ErrSelfEdge or ErrParentChildViaDep, since CreateDep's validation
// order checks the parent-child refusal before the self-edge check and both
// are correct rejections -- what matters is that no row was written.
func TestCreateDepRejectsSelfEdge(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := mustAdd(t, s, AddItemInput{Title: "a", Actor: "tester"})

	for _, typ := range []string{model.DepBlocks, model.DepParentChild, model.DepRelated, model.DepDerivedFrom} {
		t.Run(typ, func(t *testing.T) {
			before := countDeps(t, s)
			err := s.CreateDep(ctx, DepInput{ItemID: a.ID, DependsOnID: a.ID, Type: typ, Actor: "tester"})
			if typ == model.DepParentChild {
				if !errors.Is(err, ErrSelfEdge) && !errors.Is(err, ErrParentChildViaDep) {
					t.Fatalf("CreateDep(parent-child, a, a) = %v, want ErrSelfEdge or ErrParentChildViaDep", err)
				}
			} else if !errors.Is(err, ErrSelfEdge) {
				t.Fatalf("CreateDep(%s, a, a) = %v, want ErrSelfEdge", typ, err)
			}
			if after := countDeps(t, s); after != before {
				t.Fatalf("CreateDep(%s, a, a) wrote %d deps rows, want 0", typ, after-before)
			}
		})
	}
}

// TestCreateDepRefusesParentChild proves dep <a> parent-child <b> returns
// ErrParentChildViaDep, writes no row and no event, and the error message
// names `add --parent` -- the `dep` CLI tests assert against this wording.
func TestCreateDepRefusesParentChild(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := mustAdd(t, s, AddItemInput{Title: "a", Actor: "tester"})
	b := mustAdd(t, s, AddItemInput{Title: "b", Actor: "tester"})

	beforeDeps := countDeps(t, s)
	beforeEvents := countEvents(t, s)

	err := s.CreateDep(ctx, DepInput{ItemID: a.ID, DependsOnID: b.ID, Type: model.DepParentChild, Actor: "tester"})
	if !errors.Is(err, ErrParentChildViaDep) {
		t.Fatalf("CreateDep(parent-child) = %v, want ErrParentChildViaDep", err)
	}
	if !strings.Contains(err.Error(), "add --parent") {
		t.Fatalf("CreateDep(parent-child) error %q does not name `add --parent`", err.Error())
	}
	if got := countDeps(t, s); got != beforeDeps {
		t.Fatalf("CreateDep(parent-child) wrote %d deps rows, want 0", got-beforeDeps)
	}
	if got := countEvents(t, s); got != beforeEvents {
		t.Fatalf("CreateDep(parent-child) wrote %d events, want 0", got-beforeEvents)
	}
}

// TestCreateDepRejectsCycle builds a real a -> b -> c parent-child tree
// through AddItem --parent (mustAdd's ParentID), so the deps rows exercised
// are the ones AddItem actually writes, not a hand-built fixture. Calling
// the unexported insertParentChildEdge directly is the correct way to reach
// this guard: dep deliberately cannot (TestCreateDepRefusesParentChild).
func TestCreateDepRejectsCycle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	a := mustAdd(t, s, AddItemInput{Title: "a", Actor: "tester"})
	b := mustAdd(t, s, AddItemInput{Title: "b", Actor: "tester", ParentID: a.ID})
	c := mustAdd(t, s, AddItemInput{Title: "c", Actor: "tester", ParentID: b.ID})
	d := mustAdd(t, s, AddItemInput{Title: "d", Actor: "tester"})

	t.Run("closing the loop is rejected", func(t *testing.T) {
		// a is already an ancestor of c (a -> b -> c). Making a a child of c
		// would close the loop.
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			return insertParentChildEdge(ctx, tx, a.ID, c.ID, "tester", model.Now())
		})
		if !errors.Is(err, ErrCycle) {
			t.Fatalf("insertParentChildEdge(a as child of c) = %v, want ErrCycle", err)
		}
	})

	t.Run("an unrelated edge is allowed -- a guard that rejects everything is not a guard", func(t *testing.T) {
		// d has no relationship to the a/b/c tree at all.
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			return insertParentChildEdge(ctx, tx, d.ID, c.ID, "tester", model.Now())
		})
		if err != nil {
			t.Fatalf("insertParentChildEdge(d as child of c) = %v, want nil (d is unrelated to the tree)", err)
		}
	})
}

// TestCreateDepWritesOneEvent proves the one-event-per-mutation rule for
// CreateDep specifically:
// exactly one event per successful call, zero per rejected call.
func TestCreateDepWritesOneEvent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := mustAdd(t, s, AddItemInput{Title: "a", Actor: "tester"})
	b := mustAdd(t, s, AddItemInput{Title: "b", Actor: "tester"})

	t.Run("successful call writes exactly one event", func(t *testing.T) {
		before := countEvents(t, s)
		if err := s.CreateDep(ctx, DepInput{ItemID: a.ID, DependsOnID: b.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
			t.Fatalf("CreateDep: %v", err)
		}
		if after := countEvents(t, s); after-before != 1 {
			t.Fatalf("CreateDep wrote %d events, want exactly 1", after-before)
		}
	})

	t.Run("rejected call writes zero events", func(t *testing.T) {
		before := countEvents(t, s)
		err := s.CreateDep(ctx, DepInput{ItemID: a.ID, DependsOnID: a.ID, Type: model.DepRelated, Actor: "tester"})
		if !errors.Is(err, ErrSelfEdge) {
			t.Fatalf("CreateDep(self-edge) = %v, want ErrSelfEdge", err)
		}
		if after := countEvents(t, s); after != before {
			t.Fatalf("rejected CreateDep wrote %d events, want 0", after-before)
		}
	})
}

// TestParentChildDepthBoundRefusesChainPastMaxDepth asserts the bound is
// enforced where the depth is CREATED, not where it is read.
//
// Every recursive walk in this package stops at MaxDepth. A chain deeper
// than that would be a blind spot: parentChildCycleCheckSQL's ancestor walk
// would never visit the hops past the bound, and the ready view, which uses
// the same bound, would not error -- it would quietly stop propagating a
// block partway down a tree.
//
// checkParentChildDepth (depth.go) prevents it by making the input unbuildable.
// A chain of exactly MaxDepth hops is allowed; the hop that would create an
// item at MaxDepth+1 is refused with ErrTooDeep and writes nothing. Every
// recursive query in this package can therefore reach every item in any store
// this binary wrote.
func TestParentChildDepthBoundRefusesChainPastMaxDepth(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ids := make([]string, MaxDepth+1)
	ids[0] = mustAdd(t, s, AddItemInput{Title: "p0", Actor: "tester"}).ID
	for i := 1; i <= MaxDepth; i++ {
		ids[i] = mustAdd(t, s, AddItemInput{Title: fmt.Sprintf("p%d", i), Actor: "tester", ParentID: ids[i-1]}).ID
	}

	if got, err := s.DeepestChain(ctx); err != nil || got != MaxDepth {
		t.Fatalf("DeepestChain after building a %d-hop chain = %d, %v; want %d, nil", MaxDepth, got, err, MaxDepth)
	}

	// One hop further is refused, and refused before anything is written.
	before := countItems(t, s)
	_, err := s.AddItem(ctx, AddItemInput{Title: "one too deep", Actor: "tester", ParentID: ids[MaxDepth]})
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("AddItem at depth %d = %v, want ErrTooDeep", MaxDepth+1, err)
	}
	if after := countItems(t, s); after != before {
		t.Fatalf("refused AddItem wrote %d items, want 0", after-before)
	}

	// The cycle guard's old blind spot: closing the loop by making the root a
	// child of its own deepest descendant. It is now unreachable for the
	// depth reason before the cycle reason is ever consulted, which is the
	// point -- the store never holds a chain the ancestor walk cannot cross.
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		return insertParentChildEdge(ctx, tx, ids[0], ids[MaxDepth], "tester", model.Now())
	})
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("insertParentChildEdge(root under its own deepest descendant) = %v, want ErrTooDeep", err)
	}
}

// TestParentChildDepthBoundCountsTheChildsOwnSubtree covers the half of
// checkParentChildDepth that a freshly-added leaf never exercises. AddItem
// --parent always attaches a childless item, so parentDepth+1 alone would
// pass every test in this file while silently allowing an existing subtree to
// be grafted somewhere that puts its deepest leaf past MaxDepth.
//
// insertParentChildEdge is reachable with an existing child (this test, and
// any future reparent command), so the guard measures the child's own height
// too. Here a 30-hop subtree is grafted under an item already at depth 30:
// the graft itself is legal arithmetic until you count what hangs off it.
func TestParentChildDepthBoundCountsTheChildsOwnSubtree(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	const trunk = 30
	trunkIDs := make([]string, trunk+1)
	trunkIDs[0] = mustAdd(t, s, AddItemInput{Title: "t0", Actor: "tester"}).ID
	for i := 1; i <= trunk; i++ {
		trunkIDs[i] = mustAdd(t, s, AddItemInput{Title: fmt.Sprintf("t%d", i), Actor: "tester", ParentID: trunkIDs[i-1]}).ID
	}

	const branch = 30
	branchRoot := mustAdd(t, s, AddItemInput{Title: "b0", Actor: "tester"}).ID
	prev := branchRoot
	for i := 1; i <= branch; i++ {
		prev = mustAdd(t, s, AddItemInput{Title: fmt.Sprintf("b%d", i), Actor: "tester", ParentID: prev}).ID
	}

	// trunk depth 30, + 1 for the new edge, + the branch's own 30 = 61.
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		return insertParentChildEdge(ctx, tx, branchRoot, trunkIDs[trunk], "tester", model.Now())
	})
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("grafting a %d-hop subtree under an item at depth %d = %v, want ErrTooDeep", branch, trunk, err)
	}

	// The same graft one level higher up the trunk lands its deepest leaf at
	// exactly MaxDepth and is allowed -- the guard rejects what is too deep,
	// not everything that is deep.
	if err := s.withTx(ctx, func(tx *sql.Tx) error {
		return insertParentChildEdge(ctx, tx, branchRoot, trunkIDs[MaxDepth-branch-1], "tester", model.Now())
	}); err != nil {
		t.Fatalf("grafting the same subtree so its deepest leaf lands at exactly %d = %v, want nil", MaxDepth, err)
	}
	if got, err := s.DeepestChain(ctx); err != nil || got != MaxDepth {
		t.Fatalf("DeepestChain after the legal graft = %d, %v; want %d, nil", got, err, MaxDepth)
	}
}

// TestBlocksEdgeDirection is the direction assertion stated as an observable
// outcome: CreateDep(a blocks b) means "a is blocked until b closes"
// (the ready view's blocked_directly CTE), so it must remove a
// from ready and leave b in it -- not the reverse.
func TestBlocksEdgeDirection(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := mustAdd(t, s, AddItemInput{Title: "a", Actor: "tester"})
	b := mustAdd(t, s, AddItemInput{Title: "b", Actor: "tester"})

	before := readyIDs(t, s)
	if !containsID(before, a.ID) || !containsID(before, b.ID) {
		t.Fatalf("ready before CreateDep = %v, want both %s and %s present", before, a.ID, b.ID)
	}

	if err := s.CreateDep(ctx, DepInput{ItemID: a.ID, DependsOnID: b.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep: %v", err)
	}

	after := readyIDs(t, s)
	if containsID(after, a.ID) {
		t.Fatalf("ready after CreateDep(a blocks b) still contains %s, want it removed", a.ID)
	}
	if !containsID(after, b.ID) {
		t.Fatalf("ready after CreateDep(a blocks b) does not contain %s, want it present", b.ID)
	}
}
