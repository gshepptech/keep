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
// testhelp_test.go's countEvents, but deps has no plan-01-owned home yet, so
// it lives here rather than in the shared helper file.
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

// TestCreateDepVocabulary proves DEP-01: blocks/related/derived-from each
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

// TestCreateDepRejectsSelfEdge proves DEP-02's self-edge half: every one of
// the four types rejects a -> a. parent-child is special-cased to accept
// either ErrSelfEdge or ErrParentChildViaDep, since CreateDep's validation
// order checks the parent-child refusal before the self-edge check and both
// are correct rejections -- the plan requires only that no row was written.
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

// TestCreateDepRefusesParentChild proves the settled answer to
// 02-RESEARCH.md Open Question 1: dep <a> parent-child <b> returns
// ErrParentChildViaDep, writes no row and no event, and the error message
// names `add --parent` -- plan 07's `dep` CLI asserts against this wording.
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

// TestCreateDepWritesOneEvent proves EVENT-01 for CreateDep specifically:
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

// TestCycleGuardDepthBoundLimitsAncestorWalk genuinely exercises the
// depth < 50 bound in parentChildCycleCheckSQL (items.go), rather than
// leaving it as a documented-but-untested backstop. It builds a real
// 55-level parent-child chain through AddItem --parent (mustAdd's ParentID),
// then asks whether the chain's root is an ancestor of its own deepest
// descendant: it genuinely is, 55 hops up, but the guard's ancestor walk
// stops extending once a row's own depth reaches 50 (parentChildCycleCheckSQL:
// "WHERE anc.depth < 50" gates the recursive step), so the farthest ancestor
// it ever reaches is 50 hops away. The root at hop 55 is never visited, so
// the candidate edge is -- knowingly, not as an oversight -- allowed. This
// is the exact same bound the shipped ready view carries, for the same
// documented reason (schema.go): it is a backstop against pathological
// input, not a claim that every chain length is defended.
func TestCycleGuardDepthBoundLimitsAncestorWalk(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	const chainLen = 55 // exceeds the cycle guard's 50-hop ancestor walk
	ids := make([]string, chainLen+1)
	root := mustAdd(t, s, AddItemInput{Title: "p0", Actor: "tester"})
	ids[0] = root.ID
	for i := 1; i <= chainLen; i++ {
		child := mustAdd(t, s, AddItemInput{Title: fmt.Sprintf("p%d", i), Actor: "tester", ParentID: ids[i-1]})
		ids[i] = child.ID
	}

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		return insertParentChildEdge(ctx, tx, ids[0], ids[chainLen], "tester", model.Now())
	})
	if err != nil {
		t.Fatalf("insertParentChildEdge(root as child of its own 55th-generation descendant) = %v, want nil -- demonstrating the depth<50 bound's documented blind spot beyond 50 hops", err)
	}
}

// TestBlocksEdgeDirection is the direction assertion stated as an observable
// outcome: CreateDep(a blocks b) means "a is blocked until b closes"
// (02-RESEARCH.md, ready view's blocked_directly CTE), so it must remove a
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
