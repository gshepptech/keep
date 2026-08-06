package store

import (
	"context"
	"testing"

	"github.com/gshepptech/rounce/internal/model"
)

// intPtr is a small test-only helper for AddItemInput's *int fields.
func intPtr(i int) *int { return &i }

// idsOf extracts ids from a []model.Item in order, for assertions against
// Ready's returned order.
func idsOf(items []model.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// equalIDs compares two ordered id slices exactly -- Ready's ordering
// guarantee (score DESC, created_at ASC) is a total order the tests assert
// against precisely, not as a set.
func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestReadyOrdering proves Ready orders score DESC, then created_at ASC for
// ties. Two items tie at score 23 (value 6, confidence 6, effort 0 ->
// 6+6-0+11=23); their created_at is set explicitly via execSQL rather than
// relying on wall-clock separation between two AddItem calls in the same
// test process -- model.TimeFormat is fixed-width specifically so this is
// deterministic on a TEXT column, but two AddItem calls landing in the same
// nanosecond would still make an ordering assertion ambiguous if left to
// wall-clock timing. Setting created_at explicitly for this one fixture
// removes that flake risk entirely.
func TestReadyOrdering(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	early := mustAdd(t, s, AddItemInput{Title: "early-23", Actor: "tester", Value: intPtr(6), Confidence: intPtr(6), Effort: intPtr(0)})
	execSQL(t, s, `UPDATE items SET created_at = ? WHERE id = ?`, "2020-01-01T00:00:00.000000000Z", early.ID)

	late := mustAdd(t, s, AddItemInput{Title: "late-23", Actor: "tester", Value: intPtr(6), Confidence: intPtr(6), Effort: intPtr(0)})
	execSQL(t, s, `UPDATE items SET created_at = ? WHERE id = ?`, "2020-01-02T00:00:00.000000000Z", late.ID)

	seven := mustAdd(t, s, AddItemInput{Title: "seven", Actor: "tester", Value: intPtr(0), Confidence: intPtr(0), Effort: intPtr(4)}) // 0+0-4+11=7
	unscored := mustAdd(t, s, AddItemInput{Title: "unscored", Actor: "tester"})

	if early.Score != 23 || late.Score != 23 || seven.Score != 7 || unscored.Score != 0 {
		t.Fatalf("sanity: fixture scores = early %d, late %d, seven %d, unscored %d, want 23 23 7 0",
			early.Score, late.Score, seven.Score, unscored.Score)
	}

	items, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}

	want := []string{early.ID, late.ID, seven.ID, unscored.ID}
	if got := idsOf(items); !equalIDs(got, want) {
		t.Fatalf("Ready order = %v, want %v (score DESC, then created_at ASC for the tie)", got, want)
	}
}

// TestReadyUnscoredSortsLastNotExcluded asserts BOTH halves of RANK-02:
// the unscored ticket is present (never excluded) AND its score is 0 AND it
// sorts last. Asserting only "score is 0" would miss RANK-02's actual point
// entirely -- a store that silently dropped unscored tickets from ready
// would still "pass" a presence-blind assertion.
func TestReadyUnscoredSortsLastNotExcluded(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	scored := mustAdd(t, s, AddItemInput{Title: "scored", Actor: "tester", Value: intPtr(0), Confidence: intPtr(0), Effort: intPtr(10)}) // 0+0-10+11=1
	unscored := mustAdd(t, s, AddItemInput{Title: "unscored", Actor: "tester"})

	if scored.Score < 1 {
		t.Fatalf("sanity: scored fixture score = %d, want >= 1", scored.Score)
	}

	items, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}

	ids := idsOf(items)
	if !containsID(ids, unscored.ID) {
		t.Fatalf("Ready: unscored item %s is not present at all -- RANK-02 requires it be included, never excluded (got %v)", unscored.ID, ids)
	}

	var got *model.Item
	for i := range items {
		if items[i].ID == unscored.ID {
			got = &items[i]
		}
	}
	if got.Score != 0 {
		t.Fatalf("Ready: unscored item's Score field = %d, want 0", got.Score)
	}
	if ids[len(ids)-1] != unscored.ID {
		t.Fatalf("Ready: unscored item is not last (order = %v), want it sorted after every scored item", ids)
	}
}

// TestReadyExcludesRaw proves READY-03 from the caller's side: an item added
// with Stage: "raw" never appears in Ready. This exercises the view's own
// WHERE i.stage = 'ticket' predicate -- Ready itself adds no stage filter
// (see ready.go's doc comment).
func TestReadyExcludesRaw(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	raw := mustAdd(t, s, AddItemInput{Title: "raw item", Actor: "tester", Stage: model.StageRaw})
	ticket := mustAdd(t, s, AddItemInput{Title: "ticket item", Actor: "tester"})

	items, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	ids := idsOf(items)
	if containsID(ids, raw.ID) {
		t.Fatalf("Ready included raw-stage item %s: %v", raw.ID, ids)
	}
	if !containsID(ids, ticket.ID) {
		t.Fatalf("Ready did not include ticket-stage item %s: %v", ticket.ID, ids)
	}
}

// TestReadyLimit proves Ready(ctx, 2) returns the first two of the view's
// own order and Ready(ctx, 0) returns all of them -- limit <= 0 means no
// limit.
func TestReadyLimit(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	a := mustAdd(t, s, AddItemInput{Title: "a", Actor: "tester", Value: intPtr(10), Confidence: intPtr(10), Effort: intPtr(0)})
	b := mustAdd(t, s, AddItemInput{Title: "b", Actor: "tester", Value: intPtr(5), Confidence: intPtr(5), Effort: intPtr(0)})
	c := mustAdd(t, s, AddItemInput{Title: "c", Actor: "tester", Value: intPtr(0), Confidence: intPtr(0), Effort: intPtr(0)})

	all, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready(0): %v", err)
	}
	wantAll := []string{a.ID, b.ID, c.ID}
	if got := idsOf(all); !equalIDs(got, wantAll) {
		t.Fatalf("Ready(0) = %v, want %v", got, wantAll)
	}

	limited, err := s.Ready(ctx, 2)
	if err != nil {
		t.Fatalf("Ready(2): %v", err)
	}
	wantLimited := []string{a.ID, b.ID}
	if got := idsOf(limited); !equalIDs(got, wantLimited) {
		t.Fatalf("Ready(2) = %v, want %v (the first two of the full order)", got, wantLimited)
	}
}

// openTicketIDs queries the raw universe of open tickets directly -- not
// through Ready or Blocked -- so TestReadyAndBlockedPartitionOpenTickets has
// an independently-derived reference set to check the partition claim
// against.
func openTicketIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `SELECT id FROM items WHERE stage = 'ticket' AND status = 'open'`)
	if err != nil {
		t.Fatalf("openTicketIDs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("openTicketIDs: scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("openTicketIDs: rows: %v", err)
	}
	return out
}

// TestBlockingPropagatesThreeLevels builds epic -> child -> grandchild via
// AddItem --parent (mustAdd's ParentID), then proves READY-02 at real depth
// from BOTH sides of the ready/blocked pair: blocking the epic removes all
// three from ready AND surfaces all three in Blocked, each naming epic as
// the nearest blocked ancestor (child and grandchild) or blocker directly
// (epic itself) -- matching 02-RESEARCH.md Pattern 5's naming rule. Closing
// the blocker (via a raw UPDATE -- CloseItem does not exist until plan 06)
// returns all three to ready and empties them out of Blocked. Asserting
// through Blocked() here, not just readyIDs, is deliberate: readyIDs queries
// the shipped `ready` view directly and is therefore blind to a bug confined
// to blockedTreeCTE (Go-only) -- this is what makes this test, not just
// TestBlockedNamesTheBlocker, sensitive to that constant.
func TestBlockingPropagatesThreeLevels(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	blocker := mustAdd(t, s, AddItemInput{Title: "blocker", Actor: "tester"})
	epic := mustAdd(t, s, AddItemInput{Title: "epic", Actor: "tester"})
	child := mustAdd(t, s, AddItemInput{Title: "child", Actor: "tester", ParentID: epic.ID})
	grandchild := mustAdd(t, s, AddItemInput{Title: "grandchild", Actor: "tester", ParentID: child.ID})

	before := readyIDs(t, s)
	for _, id := range []string{blocker.ID, epic.ID, child.ID, grandchild.ID} {
		if !containsID(before, id) {
			t.Fatalf("before blocking, ready = %v, want %s present", before, id)
		}
	}

	if err := s.CreateDep(ctx, DepInput{ItemID: epic.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(epic blocks blocker): %v", err)
	}

	afterBlock := readyIDs(t, s)
	for _, id := range []string{epic.ID, child.ID, grandchild.ID} {
		if containsID(afterBlock, id) {
			t.Fatalf("after blocking epic, ready = %v, want %s absent (blocking must propagate at any depth)", afterBlock, id)
		}
	}
	if !containsID(afterBlock, blocker.ID) {
		t.Fatalf("after blocking epic, ready = %v, want blocker %s still present", afterBlock, blocker.ID)
	}

	blockedAfterBlock, err := s.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	gotBlockedBy := map[string]string{}
	for _, b := range blockedAfterBlock {
		gotBlockedBy[b.ID] = b.BlockedBy
	}
	wantBlockedBy := map[string]string{
		epic.ID:       blocker.ID,
		child.ID:      epic.ID,
		grandchild.ID: epic.ID,
	}
	for id, want := range wantBlockedBy {
		if got, ok := gotBlockedBy[id]; !ok {
			t.Fatalf("Blocked() after blocking epic = %v, want %s present", gotBlockedBy, id)
		} else if got != want {
			t.Fatalf("Blocked()[%s].BlockedBy = %q, want %q", id, got, want)
		}
	}

	execSQL(t, s, `UPDATE items SET status = 'closed' WHERE id = ?`, blocker.ID)

	afterClose := readyIDs(t, s)
	for _, id := range []string{epic.ID, child.ID, grandchild.ID} {
		if !containsID(afterClose, id) {
			t.Fatalf("after closing blocker, ready = %v, want %s present again", afterClose, id)
		}
	}

	blockedAfterClose, err := s.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	for _, b := range blockedAfterClose {
		for _, id := range []string{epic.ID, child.ID, grandchild.ID} {
			if b.ID == id {
				t.Fatalf("Blocked() after closing blocker still contains %s, want it gone", id)
			}
		}
	}
}

// TestBlockedNamesTheBlocker reproduces 02-RESEARCH.md Pattern 5's fixture
// exactly (blocker; epic blocked by blocker; child1 under epic; grandchild
// under child1; lonebug also blocked by blocker but NOT in the tree) and
// asserts against the query's actual executed output verbatim, rather than
// recomputing what it "should" be. lonebug is what proves a directly-blocked
// item names its actual `blocks` target while a descendant names its
// nearest blocked ancestor -- a fixture without it cannot tell the two
// apart.
func TestBlockedNamesTheBlocker(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	blocker := mustAdd(t, s, AddItemInput{Title: "blocker", Actor: "tester"})
	epic := mustAdd(t, s, AddItemInput{Title: "epic", Actor: "tester"})
	child1 := mustAdd(t, s, AddItemInput{Title: "child1", Actor: "tester", ParentID: epic.ID})
	grandchild := mustAdd(t, s, AddItemInput{Title: "grandchild", Actor: "tester", ParentID: child1.ID})
	lonebug := mustAdd(t, s, AddItemInput{Title: "lonebug", Actor: "tester"})

	if err := s.CreateDep(ctx, DepInput{ItemID: epic.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(epic blocks blocker): %v", err)
	}
	if err := s.CreateDep(ctx, DepInput{ItemID: lonebug.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(lonebug blocks blocker): %v", err)
	}

	blocked, err := s.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}

	got := map[string]string{}
	for _, b := range blocked {
		got[b.ID] = b.BlockedBy
	}
	want := map[string]string{
		child1.ID:     epic.ID,
		epic.ID:       blocker.ID,
		grandchild.ID: epic.ID,
		lonebug.ID:    blocker.ID,
	}
	if len(got) != len(want) {
		t.Fatalf("Blocked returned %d items %v, want exactly %d %v", len(got), got, len(want), want)
	}
	for id, wantBlockedBy := range want {
		if got[id] != wantBlockedBy {
			t.Fatalf("Blocked[%s].BlockedBy = %q, want %q", id, got[id], wantBlockedBy)
		}
	}
}

// TestReadyAndBlockedPartitionOpenTickets is the structural claim: for a
// fixture containing scored, unscored, raw, directly-blocked,
// transitively-blocked, in-progress, and closed items, every
// stage='ticket' AND status='open' item appears in EXACTLY ONE of Ready and
// Blocked -- asserted as a set property over an independently-queried
// reference set (openTicketIDs), not as a hand-listed expectation, so it
// keeps working as later plans add fixtures.
func TestReadyAndBlockedPartitionOpenTickets(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	scored := mustAdd(t, s, AddItemInput{Title: "scored", Actor: "tester", Value: intPtr(5), Confidence: intPtr(5), Effort: intPtr(2)})
	unscored := mustAdd(t, s, AddItemInput{Title: "unscored", Actor: "tester"})
	mustAdd(t, s, AddItemInput{Title: "raw", Actor: "tester", Stage: model.StageRaw}) // not a ticket at all -- outside the universe
	blocker := mustAdd(t, s, AddItemInput{Title: "blocker", Actor: "tester"})
	blockedDirect := mustAdd(t, s, AddItemInput{Title: "blocked-direct", Actor: "tester"})
	blockedParent := mustAdd(t, s, AddItemInput{Title: "blocked-parent", Actor: "tester"})
	blockedChild := mustAdd(t, s, AddItemInput{Title: "blocked-child", Actor: "tester", ParentID: blockedParent.ID})
	inProgress := mustAdd(t, s, AddItemInput{Title: "in-progress", Actor: "tester"})
	closedItem := mustAdd(t, s, AddItemInput{Title: "closed", Actor: "tester"})

	if err := s.CreateDep(ctx, DepInput{ItemID: blockedDirect.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(blockedDirect blocks blocker): %v", err)
	}
	if err := s.CreateDep(ctx, DepInput{ItemID: blockedParent.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(blockedParent blocks blocker): %v", err)
	}

	execSQL(t, s, `UPDATE items SET status = 'in_progress' WHERE id = ?`, inProgress.ID)
	execSQL(t, s, `UPDATE items SET status = 'closed' WHERE id = ?`, closedItem.ID)

	openTickets := openTicketIDs(t, s)

	readyItems, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	blockedItems, err := s.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}

	readySet := map[string]bool{}
	for _, it := range readyItems {
		readySet[it.ID] = true
	}
	blockedSet := map[string]bool{}
	for _, it := range blockedItems {
		blockedSet[it.ID] = true
	}

	for _, id := range openTickets {
		inReady := readySet[id]
		inBlocked := blockedSet[id]
		if inReady == inBlocked {
			t.Fatalf("open ticket %s: inReady=%v inBlocked=%v, want exactly one true (union+disjoint)", id, inReady, inBlocked)
		}
	}

	openSet := map[string]bool{}
	for _, id := range openTickets {
		openSet[id] = true
	}
	for id := range readySet {
		if !openSet[id] {
			t.Fatalf("Ready contains %s, which is not in the independently-queried open-ticket set", id)
		}
	}
	for id := range blockedSet {
		if !openSet[id] {
			t.Fatalf("Blocked contains %s, which is not in the independently-queried open-ticket set", id)
		}
	}

	// Sanity: the fixture actually exercises both sides, or the partition
	// assertion above would be vacuous.
	if !readySet[scored.ID] || !readySet[unscored.ID] {
		t.Fatalf("sanity: scored/unscored should both be ready, readySet=%v", readySet)
	}
	if !blockedSet[blockedDirect.ID] || !blockedSet[blockedParent.ID] || !blockedSet[blockedChild.ID] {
		t.Fatalf("sanity: blockedDirect/blockedParent/blockedChild should all be blocked, blockedSet=%v", blockedSet)
	}
}

// TestIsBlockedNamesBlocker proves IsBlocked agrees with Blocked for every
// entry Blocked reports (both directly- and transitively-blocked), and
// returns blocked=false for a ready item.
func TestIsBlockedNamesBlocker(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	blocker := mustAdd(t, s, AddItemInput{Title: "blocker", Actor: "tester"})
	epic := mustAdd(t, s, AddItemInput{Title: "epic", Actor: "tester"})
	child1 := mustAdd(t, s, AddItemInput{Title: "child1", Actor: "tester", ParentID: epic.ID})
	readyItem := mustAdd(t, s, AddItemInput{Title: "ready-item", Actor: "tester"})

	if err := s.CreateDep(ctx, DepInput{ItemID: epic.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep: %v", err)
	}

	blocked, err := s.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}

	foundChild1 := false
	for _, b := range blocked {
		if b.ID == child1.ID {
			foundChild1 = true
		}
		gotBlockedBy, gotBlocked, err := s.IsBlocked(ctx, b.ID)
		if err != nil {
			t.Fatalf("IsBlocked(%s): %v", b.ID, err)
		}
		if !gotBlocked {
			t.Fatalf("IsBlocked(%s) = blocked=false, want true (Blocked() already reports it blocked by %s)", b.ID, b.BlockedBy)
		}
		if gotBlockedBy != b.BlockedBy {
			t.Fatalf("IsBlocked(%s) = %q, want %q (agree with Blocked())", b.ID, gotBlockedBy, b.BlockedBy)
		}
	}
	if !foundChild1 {
		t.Fatalf("sanity: child1 %s should appear in Blocked (transitively blocked via epic), got %v", child1.ID, blocked)
	}

	blockedBy, isBlocked, err := s.IsBlocked(ctx, readyItem.ID)
	if err != nil {
		t.Fatalf("IsBlocked(%s): %v", readyItem.ID, err)
	}
	if isBlocked {
		t.Fatalf("IsBlocked(%s) = blocked=true (by %q), want false", readyItem.ID, blockedBy)
	}
}
