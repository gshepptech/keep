package store

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

var rootIDShape = regexp.MustCompile(`^rnc-[0-9a-f]{6}$`)

// TestAddItemIDShapes proves a root gets rnc-<6 hex>, three
// children of it get .1/.2/.3 in order, and a child of .1 gets .1.1 -- the
// whole parent chain is readable from the id alone.
func TestAddItemIDShapes(t *testing.T) {
	s := openTestStore(t)

	root := mustAdd(t, s, AddItemInput{Title: "root", Actor: "tester"})
	if !rootIDShape.MatchString(root.ID) {
		t.Fatalf("root id %q does not match ^rnc-[0-9a-f]{6}$", root.ID)
	}

	child1 := mustAdd(t, s, AddItemInput{Title: "child1", Actor: "tester", ParentID: root.ID})
	child2 := mustAdd(t, s, AddItemInput{Title: "child2", Actor: "tester", ParentID: root.ID})
	child3 := mustAdd(t, s, AddItemInput{Title: "child3", Actor: "tester", ParentID: root.ID})

	wantChild1 := root.ID + ".1"
	wantChild2 := root.ID + ".2"
	wantChild3 := root.ID + ".3"
	if child1.ID != wantChild1 {
		t.Errorf("child1.ID = %q, want %q", child1.ID, wantChild1)
	}
	if child2.ID != wantChild2 {
		t.Errorf("child2.ID = %q, want %q", child2.ID, wantChild2)
	}
	if child3.ID != wantChild3 {
		t.Errorf("child3.ID = %q, want %q", child3.ID, wantChild3)
	}

	grandchild := mustAdd(t, s, AddItemInput{Title: "grandchild", Actor: "tester", ParentID: child1.ID})
	wantGrandchild := child1.ID + ".1"
	if grandchild.ID != wantGrandchild {
		t.Errorf("grandchild.ID = %q, want %q", grandchild.ID, wantGrandchild)
	}
}

// TestBlockedParentRemovesChildFromReady builds a three-level tree via real
// AddItem calls (epic -> child -> grandchild), inserts a `blocks` edge on
// the EPIC with raw fixture SQL (independent of CreateDep), and asserts the
// child and
// grandchild both disappear from `ready` -- proving AddItem wrote the
// parent-child deps rows, not just items.parent_id.
func TestBlockedParentRemovesChildFromReady(t *testing.T) {
	s := openTestStore(t)

	blocker := mustAdd(t, s, AddItemInput{Title: "blocker", Actor: "tester"})
	epic := mustAdd(t, s, AddItemInput{Title: "epic", Actor: "tester"})
	child := mustAdd(t, s, AddItemInput{Title: "child", Actor: "tester", ParentID: epic.ID})
	grandchild := mustAdd(t, s, AddItemInput{Title: "grandchild", Actor: "tester", ParentID: child.ID})

	// Positive assertion first: all four items are ready before the block.
	before := readyIDs(t, s)
	wantBefore := map[string]bool{blocker.ID: true, epic.ID: true, child.ID: true, grandchild.ID: true}
	if len(before) != len(wantBefore) {
		t.Fatalf("readyIDs before block = %v, want exactly %v", before, wantBefore)
	}
	for _, id := range before {
		if !wantBefore[id] {
			t.Fatalf("unexpected id %q in ready before block: %v", id, before)
		}
	}

	// Block the EPIC directly, via raw fixture SQL -- not a store method.
	execSQL(t, s, `INSERT INTO deps (item_id, depends_on_id, type, created_at, created_by) VALUES (?, ?, 'blocks', ?, ?)`,
		epic.ID, blocker.ID, nowFunc(), "tester")

	after := readyIDs(t, s)
	wantAfter := map[string]bool{blocker.ID: true}
	if len(after) != len(wantAfter) {
		t.Fatalf("readyIDs after block = %v, want exactly %v", after, wantAfter)
	}
	for _, id := range after {
		if !wantAfter[id] {
			t.Fatalf("unexpected id %q in ready after block: %v", id, after)
		}
	}
	for _, blocked := range []string{epic.ID, child.ID, grandchild.ID} {
		for _, id := range after {
			if id == blocked {
				t.Fatalf("%q still in ready after its epic was blocked: %v", blocked, after)
			}
		}
	}

	// Counter-assertion: pins the mechanism. A test that only checks `ready`
	// could in principle pass for the wrong reason; this confirms AddItem
	// itself wrote exactly the two parent-child deps rows (epic->child,
	// child->grandchild) responsible for the propagation above.
	var parentChildCount int
	if err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM deps WHERE type = 'parent-child'`).Scan(&parentChildCount); err != nil {
		t.Fatalf("count parent-child deps: %v", err)
	}
	if parentChildCount != 2 {
		t.Fatalf("parent-child deps count = %d, want 2 (epic->child, child->grandchild)", parentChildCount)
	}
}

// TestAddItemWritesOneEvent proves an add writes
// exactly one events row carrying the supplied actor and session id.
func TestAddItemWritesOneEvent(t *testing.T) {
	s := openTestStore(t)

	before := countEvents(t, s)
	item := mustAdd(t, s, AddItemInput{Title: "solo", Actor: "tester", SessionID: "sess-1"})
	after := countEvents(t, s)

	if after-before != 1 {
		t.Fatalf("AddItem wrote %d events, want exactly 1", after-before)
	}

	events := eventsFor(t, s, item.ID)
	if len(events) != 1 {
		t.Fatalf("eventsFor(%q) returned %d events, want 1", item.ID, len(events))
	}
	e := events[0]
	if e.Kind != "add" {
		t.Errorf("Kind = %q, want %q", e.Kind, "add")
	}
	if e.Actor != "tester" {
		t.Errorf("Actor = %q, want %q", e.Actor, "tester")
	}
	if e.SessionID == nil || *e.SessionID != "sess-1" {
		t.Errorf("SessionID = %v, want %q", e.SessionID, "sess-1")
	}
}

// TestAddItemRetriesRootIDCollision proves a pre-inserted colliding root id
// is retried rather than returned as an error. nowFunc is pinned for the
// duration of the test so the id AddItem's own attempt=0 would generate is
// fully deterministic and can be pre-computed and pre-inserted.
func TestAddItemRetriesRootIDCollision(t *testing.T) {
	s := openTestStore(t)

	const fixedNow = "2026-08-06T12:00:00.000000000Z"
	orig := nowFunc
	nowFunc = func() string { return fixedNow }
	t.Cleanup(func() { nowFunc = orig })

	const title = "collide-me"
	collidingID := generateRootID(title, fixedNow, 0)
	execSQL(t, s, `INSERT INTO items (id, stage, type, title, status, score, created_at, updated_at) VALUES (?, 'ticket', 'task', ?, 'open', 0, ?, ?)`,
		collidingID, title, fixedNow, fixedNow)

	item, err := s.AddItem(context.Background(), AddItemInput{Title: title, Actor: "tester"})
	if err != nil {
		t.Fatalf("AddItem: %v, want a successful retry past the collision", err)
	}
	if item.ID == collidingID {
		t.Fatalf("AddItem returned the colliding id %q, want a different id after retry", item.ID)
	}
	if !rootIDShape.MatchString(item.ID) {
		t.Fatalf("retried id %q does not match ^rnc-[0-9a-f]{6}$", item.ID)
	}
}

// TestAddItemUnknownReferentIsNotFound proves --parent, --blocks, and
// --derived-from each return ErrNotFound (via errors.Is) when they name an
// id that does not exist, rather than a raw foreign-key error.
func TestAddItemUnknownReferentIsNotFound(t *testing.T) {
	s := openTestStore(t)
	real := mustAdd(t, s, AddItemInput{Title: "real", Actor: "tester"})

	t.Run("parent", func(t *testing.T) {
		_, err := s.AddItem(context.Background(), AddItemInput{Title: "x", Actor: "tester", ParentID: "rnc-ffffff"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("AddItem with unknown parent: err = %v, want errors.Is(err, ErrNotFound)", err)
		}
	})
	t.Run("blocks", func(t *testing.T) {
		_, err := s.AddItem(context.Background(), AddItemInput{Title: "x", Actor: "tester", Blocks: []string{"rnc-ffffff"}})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("AddItem with unknown blocks target: err = %v, want errors.Is(err, ErrNotFound)", err)
		}
	})
	t.Run("derived-from", func(t *testing.T) {
		_, err := s.AddItem(context.Background(), AddItemInput{Title: "x", Actor: "tester", DerivedFrom: []string{"rnc-ffffff"}})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("AddItem with unknown derived-from target: err = %v, want errors.Is(err, ErrNotFound)", err)
		}
	})

	// Sanity: a real referent still succeeds, proving the check above isn't
	// vacuously rejecting everything.
	if _, err := s.AddItem(context.Background(), AddItemInput{Title: "x", Actor: "tester", Blocks: []string{real.ID}}); err != nil {
		t.Fatalf("AddItem with a real blocks target: %v, want success", err)
	}
}
