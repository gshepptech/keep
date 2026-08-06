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
