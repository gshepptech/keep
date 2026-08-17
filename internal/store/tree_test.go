package store

import (
	"context"
	"errors"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// TestTree covers all seven behaviours 05-01-PLAN.md's Task 1 names. Every
// fixture is built through the real AddItem/CreateDep/CloseItem/DropItem via
// testhelp_test.go's helpers -- never raw SQL for the tree structure, since
// AddItem's dual parent-child write is precisely the mechanism under test.
// Returned ids are captured into local variables and asserted against those;
// no `rnc-` id is ever hard-coded (they are hash-derived from title plus
// wall-clock creation time and differ on every run).
func TestTree(t *testing.T) {
	const actor = "tester"

	t.Run("DescendantCountAndOrder", func(t *testing.T) {
		// A three-level chain: epic -> child -> grandchild. The direction
		// trap (joining the recursive step the same way items.go's ANCESTOR
		// walk does) does not error -- it silently returns exactly 1 row
		// (the root only), for any root that is not itself someone else's
		// child. Asserting descendant COUNT, not merely "no error", is what
		// catches that.
		s := openTestStore(t)
		ctx := context.Background()

		epic := mustAdd(t, s, AddItemInput{Title: "Epic root", Actor: actor})
		child := mustAdd(t, s, AddItemInput{Title: "Task A", Actor: actor, ParentID: epic.ID})
		grandchild := mustAdd(t, s, AddItemInput{Title: "Subtask under A", Actor: actor, ParentID: child.ID})

		got, err := s.Tree(ctx, epic.ID)
		if err != nil {
			t.Fatalf("Tree(%q): unexpected error: %v", epic.ID, err)
		}
		if len(got) != 3 {
			t.Fatalf("Tree(%q): got %d rows, want 3 (root + child + grandchild) -- ids: %v", epic.ID, len(got), ids(got))
		}
		wantOrder := []string{epic.ID, child.ID, grandchild.ID}
		gotOrder := ids(got)
		for i := range wantOrder {
			if gotOrder[i] != wantOrder[i] {
				t.Fatalf("Tree(%q): row %d id = %q, want %q (depth-first order): got order %v, want %v", epic.ID, i, gotOrder[i], wantOrder[i], gotOrder, wantOrder)
			}
		}
		wantDepths := []int{0, 1, 2}
		for i, n := range got {
			if n.Depth != wantDepths[i] {
				t.Errorf("row %d (%s): Depth = %d, want %d", i, n.ID, n.Depth, wantDepths[i])
			}
		}
	})

	t.Run("SiblingOrderAndDepthFirst", func(t *testing.T) {
		// Two children of one parent, plus a grandchild under the first
		// child. The test does no sorting of its own -- it asserts the
		// order Tree itself returns.
		s := openTestStore(t)
		ctx := context.Background()

		epic := mustAdd(t, s, AddItemInput{Title: "Epic root", Actor: actor})
		child1 := mustAdd(t, s, AddItemInput{Title: "Task A", Actor: actor, ParentID: epic.ID})
		child2 := mustAdd(t, s, AddItemInput{Title: "Task B", Actor: actor, ParentID: epic.ID})
		grandchild := mustAdd(t, s, AddItemInput{Title: "Subtask under A", Actor: actor, ParentID: child1.ID})

		got, err := s.Tree(ctx, epic.ID)
		if err != nil {
			t.Fatalf("Tree(%q): unexpected error: %v", epic.ID, err)
		}
		want := []string{epic.ID, child1.ID, grandchild.ID, child2.ID}
		gotIDs := ids(got)
		if len(gotIDs) != len(want) {
			t.Fatalf("Tree(%q): got %d rows %v, want %d %v", epic.ID, len(gotIDs), gotIDs, len(want), want)
		}
		for i := range want {
			if gotIDs[i] != want[i] {
				t.Fatalf("Tree(%q): order = %v, want %v (child1's ordinal precedes child2's; grandchild sits immediately after its own parent, not after both siblings)", epic.ID, gotIDs, want)
			}
		}
	})

	t.Run("RootBlockedPropagates", func(t *testing.T) {
		// The root blocked by a separate open item: every row is blocked;
		// the root's BlockedBy names the actual blocker, and every
		// descendant's BlockedBy names the nearest blocked ancestor (the
		// root itself here, since nothing closer blocks them).
		s := openTestStore(t)
		ctx := context.Background()

		blocker := mustAdd(t, s, AddItemInput{Title: "Separate blocking item", Actor: actor})
		epic := mustAdd(t, s, AddItemInput{Title: "Epic root", Actor: actor})
		child := mustAdd(t, s, AddItemInput{Title: "Task A", Actor: actor, ParentID: epic.ID})
		grandchild := mustAdd(t, s, AddItemInput{Title: "Subtask under A", Actor: actor, ParentID: child.ID})

		if err := s.CreateDep(ctx, DepInput{ItemID: epic.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: actor}); err != nil {
			t.Fatalf("CreateDep(blocks): %v", err)
		}

		got, err := s.Tree(ctx, epic.ID)
		if err != nil {
			t.Fatalf("Tree(%q): unexpected error: %v", epic.ID, err)
		}
		if len(got) != 3 {
			t.Fatalf("Tree(%q): got %d rows, want 3", epic.ID, len(got))
		}
		byID := indexByID(got)

		root := byID[epic.ID]
		if !root.Blocked {
			t.Errorf("root %s: Blocked = false, want true", epic.ID)
		}
		if root.BlockedBy != blocker.ID {
			t.Errorf("root %s: BlockedBy = %q, want %q (the actual blocker)", epic.ID, root.BlockedBy, blocker.ID)
		}

		for _, id := range []string{child.ID, grandchild.ID} {
			n := byID[id]
			if !n.Blocked {
				t.Errorf("descendant %s: Blocked = false, want true", id)
			}
			if n.BlockedBy != epic.ID {
				t.Errorf("descendant %s: BlockedBy = %q, want %q (nearest blocked ancestor)", id, n.BlockedBy, epic.ID)
			}
		}
	})

	t.Run("ClosedAndDroppedIncluded", func(t *testing.T) {
		// A closed child and a dropped child are both PRESENT in the
		// result -- the renderer dims them, the query does not decide they
		// do not exist (05-CONTEXT.md decision 1).
		s := openTestStore(t)
		ctx := context.Background()

		epic := mustAdd(t, s, AddItemInput{Title: "Epic root", Actor: actor})
		closedChild := mustAdd(t, s, AddItemInput{Title: "Closed child", Actor: actor, ParentID: epic.ID})
		droppedChild := mustAdd(t, s, AddItemInput{Title: "Dropped child", Actor: actor, ParentID: epic.ID})

		if _, err := s.CloseItem(ctx, CloseItemInput{ID: closedChild.ID, Reason: "done", Actor: actor}); err != nil {
			t.Fatalf("CloseItem: %v", err)
		}
		if _, err := s.DropItem(ctx, DropItemInput{ID: droppedChild.ID, Reason: "no longer needed", Actor: actor}); err != nil {
			t.Fatalf("DropItem: %v", err)
		}

		got, err := s.Tree(ctx, epic.ID)
		if err != nil {
			t.Fatalf("Tree(%q): unexpected error: %v", epic.ID, err)
		}
		byID := indexByID(got)

		closed, ok := byID[closedChild.ID]
		if !ok {
			t.Fatalf("Tree(%q): closed child %s missing from result: got ids %v", epic.ID, closedChild.ID, ids(got))
		}
		if closed.Status != model.StatusClosed {
			t.Errorf("closed child %s: Status = %q, want %q", closedChild.ID, closed.Status, model.StatusClosed)
		}

		dropped, ok := byID[droppedChild.ID]
		if !ok {
			t.Fatalf("Tree(%q): dropped child %s missing from result: got ids %v", epic.ID, droppedChild.ID, ids(got))
		}
		if dropped.Status != model.StatusDropped {
			t.Errorf("dropped child %s: Status = %q, want %q", droppedChild.ID, dropped.Status, model.StatusDropped)
		}
	})

	t.Run("DoubleBlockerNearestWins", func(t *testing.T) {
		// A grandchild whose parent is blocked by one item while the epic
		// is blocked by a DIFFERENT item yields exactly ONE row for that
		// grandchild (and for the parent), each naming the nearer of the
		// two ancestors -- proving blocked_nearest's ROW_NUMBER() dedup,
		// not a plain LEFT JOIN blocked_tree, which would multiply rows.
		s := openTestStore(t)
		ctx := context.Background()

		blockerY := mustAdd(t, s, AddItemInput{Title: "Blocks the epic", Actor: actor})
		blockerX := mustAdd(t, s, AddItemInput{Title: "Blocks the child directly", Actor: actor})
		epic := mustAdd(t, s, AddItemInput{Title: "Epic root", Actor: actor})
		child := mustAdd(t, s, AddItemInput{Title: "Task A", Actor: actor, ParentID: epic.ID})
		grandchild := mustAdd(t, s, AddItemInput{Title: "Subtask under A", Actor: actor, ParentID: child.ID})

		if err := s.CreateDep(ctx, DepInput{ItemID: epic.ID, DependsOnID: blockerY.ID, Type: model.DepBlocks, Actor: actor}); err != nil {
			t.Fatalf("CreateDep(epic blocks blockerY): %v", err)
		}
		if err := s.CreateDep(ctx, DepInput{ItemID: child.ID, DependsOnID: blockerX.ID, Type: model.DepBlocks, Actor: actor}); err != nil {
			t.Fatalf("CreateDep(child blocks blockerX): %v", err)
		}

		got, err := s.Tree(ctx, epic.ID)
		if err != nil {
			t.Fatalf("Tree(%q): unexpected error: %v", epic.ID, err)
		}
		if len(got) != 3 {
			t.Fatalf("Tree(%q): got %d rows, want exactly 3 (one per item, no duplication): ids %v", epic.ID, len(got), ids(got))
		}
		byID := indexByID(got)

		if n := byID[epic.ID]; n.BlockedBy != blockerY.ID {
			t.Errorf("epic %s: BlockedBy = %q, want %q", epic.ID, n.BlockedBy, blockerY.ID)
		}
		if n := byID[child.ID]; n.BlockedBy != blockerX.ID {
			t.Errorf("child %s: BlockedBy = %q, want %q (blocked directly, nearer than the epic's blocker)", child.ID, n.BlockedBy, blockerX.ID)
		}
		if n := byID[grandchild.ID]; n.BlockedBy != child.ID {
			t.Errorf("grandchild %s: BlockedBy = %q, want %q (child is the nearer of the two blocked ancestors, not the epic)", grandchild.ID, n.BlockedBy, child.ID)
		}
	})

	t.Run("UnknownRootReturnsErrNotFound", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()

		got, err := s.Tree(ctx, "rnc-nope")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Tree(%q): err = %v, want errors.Is(err, ErrNotFound)", "rnc-nope", err)
		}
		if got != nil {
			t.Fatalf("Tree(%q): got %v, want nil on error", "rnc-nope", got)
		}
	})

	t.Run("UnblockedTreeAllFalse", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()

		epic := mustAdd(t, s, AddItemInput{Title: "Epic root", Actor: actor})
		child := mustAdd(t, s, AddItemInput{Title: "Task A", Actor: actor, ParentID: epic.ID})

		got, err := s.Tree(ctx, epic.ID)
		if err != nil {
			t.Fatalf("Tree(%q): unexpected error: %v", epic.ID, err)
		}
		if len(got) != 2 {
			t.Fatalf("Tree(%q): got %d rows, want 2", epic.ID, len(got))
		}
		for _, n := range got {
			if n.Blocked {
				t.Errorf("%s: Blocked = true, want false", n.ID)
			}
			if n.BlockedBy != "" {
				t.Errorf("%s: BlockedBy = %q, want \"\"", n.ID, n.BlockedBy)
			}
		}
		_ = child
	})
}

// ids extracts each row's item id, in result order.
func ids(nodes []model.TreeNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

// indexByID maps each row's item id to itself, for assertions that check a
// specific node's fields regardless of its position in the result slice.
func indexByID(nodes []model.TreeNode) map[string]model.TreeNode {
	out := make(map[string]model.TreeNode, len(nodes))
	for _, n := range nodes {
		out[n.ID] = n
	}
	return out
}
