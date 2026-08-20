package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// containsID (deps_test.go) reports whether ids contains id -- reused here
// for TestCloseAndDropAreDistinguishable's ready-membership assertions.

// TestStartItemStateTable has one subtest per row of the locked six-row
// state table (02-CONTEXT.md decision 3), each with its own fixture -- a
// shared fixture mutated across subtests would make a failure in one row
// cascade into later rows and hide which one actually broke. Every subtest
// asserts the returned error (or nil), the resulting status, and the
// change in event count.
func TestStartItemStateTable(t *testing.T) {
	ctx := context.Background()

	t.Run("open unblocked ticket succeeds and writes one event", func(t *testing.T) {
		s := openTestStore(t)
		item := mustAdd(t, s, AddItemInput{Title: "open-ticket", Actor: "tester"})

		before := countEvents(t, s)
		got, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-1"})
		if err != nil {
			t.Fatalf("StartItem: %v, want success", err)
		}
		after := countEvents(t, s)

		if got.Status != model.StatusInProgress {
			t.Errorf("Status = %q, want %q", got.Status, model.StatusInProgress)
		}
		if got.ClaimedBySession == nil || *got.ClaimedBySession != "sess-1" {
			t.Errorf("ClaimedBySession = %v, want %q", got.ClaimedBySession, "sess-1")
		}
		if got.ClaimedAt == nil || *got.ClaimedAt == "" {
			t.Errorf("ClaimedAt = %v, want non-empty", got.ClaimedAt)
		}
		if after-before != 1 {
			t.Errorf("event count delta = %d, want 1", after-before)
		}
	})

	t.Run("already in_progress same session is an idempotent no-op with no second event", func(t *testing.T) {
		s := openTestStore(t)
		item := mustAdd(t, s, AddItemInput{Title: "same-session-retry", Actor: "tester"})
		if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-1"}); err != nil {
			t.Fatalf("initial StartItem: %v", err)
		}

		before := countEvents(t, s)
		got, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-1"})
		if err != nil {
			t.Fatalf("StartItem (same-session retry): %v, want a nil-error no-op", err)
		}
		after := countEvents(t, s)

		if got.Status != model.StatusInProgress {
			t.Errorf("Status = %q, want %q", got.Status, model.StatusInProgress)
		}
		if after != before {
			t.Errorf("same-session retry wrote %d events, want exactly 0", after-before)
		}
	})

	t.Run("already in_progress different session returns ErrConflict naming holder and claimed_at", func(t *testing.T) {
		s := openTestStore(t)
		item := mustAdd(t, s, AddItemInput{Title: "conflicting-claim", Actor: "tester"})
		first, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-holder"})
		if err != nil {
			t.Fatalf("initial StartItem: %v", err)
		}

		before := countEvents(t, s)
		_, err = s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-other"})
		after := countEvents(t, s)

		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want errors.Is(err, ErrConflict)", err)
		}
		if !strings.Contains(err.Error(), "sess-holder") {
			t.Errorf("error %q does not name the holding session %q", err.Error(), "sess-holder")
		}
		if first.ClaimedAt == nil || !strings.Contains(err.Error(), *first.ClaimedAt) {
			t.Errorf("error %q does not name claimed_at %v", err.Error(), first.ClaimedAt)
		}
		if after != before {
			t.Errorf("conflicting StartItem wrote %d events, want 0", after-before)
		}
	})

	t.Run("stage raw returns ErrInvalidTransition", func(t *testing.T) {
		s := openTestStore(t)
		item := mustAdd(t, s, AddItemInput{Title: "raw-item", Actor: "tester", Stage: model.StageRaw})

		before := countEvents(t, s)
		_, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-1"})
		after := countEvents(t, s)

		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("err = %v, want errors.Is(err, ErrInvalidTransition)", err)
		}
		if after != before {
			t.Errorf("StartItem on a raw item wrote %d events, want 0", after-before)
		}
	})

	t.Run("blocked directly or through parent tree returns ErrInvalidTransition naming the blocker", func(t *testing.T) {
		s := openTestStore(t)
		blocker := mustAdd(t, s, AddItemInput{Title: "the-blocker", Actor: "tester"})
		target := mustAdd(t, s, AddItemInput{Title: "blocked-target", Actor: "tester"})
		if err := s.CreateDep(ctx, DepInput{ItemID: target.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
			t.Fatalf("CreateDep: %v", err)
		}

		before := countEvents(t, s)
		_, err := s.StartItem(ctx, StartItemInput{ID: target.ID, Actor: "tester", SessionID: "sess-1"})
		after := countEvents(t, s)

		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("err = %v, want errors.Is(err, ErrInvalidTransition)", err)
		}
		if !strings.Contains(err.Error(), blocker.ID) {
			t.Errorf("error %q does not name the blocking item %q", err.Error(), blocker.ID)
		}
		if after != before {
			t.Errorf("StartItem on a blocked item wrote %d events, want 0", after-before)
		}
	})

	t.Run("closed or dropped returns ErrInvalidTransition", func(t *testing.T) {
		s := openTestStore(t)
		closedItem := mustAdd(t, s, AddItemInput{Title: "already-closed", Actor: "tester"})
		if _, err := s.CloseItem(ctx, CloseItemInput{ID: closedItem.ID, Reason: "done", Actor: "tester"}); err != nil {
			t.Fatalf("CloseItem: %v", err)
		}
		droppedItem := mustAdd(t, s, AddItemInput{Title: "already-dropped", Actor: "tester"})
		if _, err := s.DropItem(ctx, DropItemInput{ID: droppedItem.ID, Reason: "abandoned", Actor: "tester"}); err != nil {
			t.Fatalf("DropItem: %v", err)
		}

		before := countEvents(t, s)
		if _, err := s.StartItem(ctx, StartItemInput{ID: closedItem.ID, Actor: "tester", SessionID: "sess-1"}); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("StartItem on closed item: err = %v, want errors.Is(err, ErrInvalidTransition)", err)
		}
		if _, err := s.StartItem(ctx, StartItemInput{ID: droppedItem.ID, Actor: "tester", SessionID: "sess-1"}); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("StartItem on dropped item: err = %v, want errors.Is(err, ErrInvalidTransition)", err)
		}
		after := countEvents(t, s)
		if after != before {
			t.Errorf("StartItem on closed/dropped items wrote %d events, want 0", after-before)
		}
	})
}

// TestStartItemSameSessionKeepsOriginalClaimTime pins nowFunc to two
// distinct values across the initial claim and the same-session retry,
// proving the retry returns the ORIGINAL claim time rather than the retry's
// own timestamp -- a wall-clock-timing version of this test could pass by
// coincidence if both calls happened to land in the same instant.
func TestStartItemSameSessionKeepsOriginalClaimTime(t *testing.T) {
	s := openTestStore(t)
	item := mustAdd(t, s, AddItemInput{Title: "claim-time-fixture", Actor: "tester"})

	orig := nowFunc
	t.Cleanup(func() { nowFunc = orig })

	const firstClaim = "2026-08-06T12:00:00.000000000Z"
	nowFunc = func() string { return firstClaim }
	got1, err := s.StartItem(context.Background(), StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("initial StartItem: %v", err)
	}
	if got1.ClaimedAt == nil || *got1.ClaimedAt != firstClaim {
		t.Fatalf("initial ClaimedAt = %v, want %q", got1.ClaimedAt, firstClaim)
	}

	const retryClaim = "2026-08-06T13:00:00.000000000Z"
	nowFunc = func() string { return retryClaim }
	got2, err := s.StartItem(context.Background(), StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("same-session retry StartItem: %v, want idempotent success", err)
	}
	if got2.ClaimedAt == nil || *got2.ClaimedAt != firstClaim {
		t.Fatalf("retry ClaimedAt = %v, want unchanged original %q (not the retry time %q)", got2.ClaimedAt, firstClaim, retryClaim)
	}
}

// TestCloseAndDropAreDistinguishable proves close and drop both remove an
// item from ready and remain distinguishable by both items.status and
// events.kind (ITEM-08) -- a shared kind would defeat the reason close and
// drop are separate verbs.
func TestCloseAndDropAreDistinguishable(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	closedItem := mustAdd(t, s, AddItemInput{Title: "closed-me", Actor: "tester"})
	droppedItem := mustAdd(t, s, AddItemInput{Title: "dropped-me", Actor: "tester"})

	before := readyIDs(t, s)
	if !containsID(before, closedItem.ID) || !containsID(before, droppedItem.ID) {
		t.Fatalf("fixture items not in ready before close/drop: %v", before)
	}

	gotClosed, err := s.CloseItem(ctx, CloseItemInput{ID: closedItem.ID, Reason: "shipped", Actor: "tester"})
	if err != nil {
		t.Fatalf("CloseItem: %v", err)
	}
	gotDropped, err := s.DropItem(ctx, DropItemInput{ID: droppedItem.ID, Reason: "no longer needed", Actor: "tester"})
	if err != nil {
		t.Fatalf("DropItem: %v", err)
	}

	if gotClosed.Status != model.StatusClosed {
		t.Errorf("closed item Status = %q, want %q", gotClosed.Status, model.StatusClosed)
	}
	if gotDropped.Status != model.StatusDropped {
		t.Errorf("dropped item Status = %q, want %q", gotDropped.Status, model.StatusDropped)
	}
	if gotClosed.Status == gotDropped.Status {
		t.Fatalf("close and drop produced the same status %q", gotClosed.Status)
	}

	after := readyIDs(t, s)
	if containsID(after, closedItem.ID) {
		t.Errorf("closed item %q still in ready", closedItem.ID)
	}
	if containsID(after, droppedItem.ID) {
		t.Errorf("dropped item %q still in ready", droppedItem.ID)
	}

	closedEvents := eventsFor(t, s, closedItem.ID)
	droppedEvents := eventsFor(t, s, droppedItem.ID)
	lastClosed := closedEvents[len(closedEvents)-1]
	lastDropped := droppedEvents[len(droppedEvents)-1]
	if lastClosed.Kind != "close" {
		t.Errorf("closed item's last event kind = %q, want %q", lastClosed.Kind, "close")
	}
	if lastDropped.Kind != "drop" {
		t.Errorf("dropped item's last event kind = %q, want %q", lastDropped.Kind, "drop")
	}
	if lastClosed.Kind == lastDropped.Kind {
		t.Fatalf("close and drop wrote the same event kind %q", lastClosed.Kind)
	}
}

// TestCloseAlreadyClosedIsInvalidTransition proves closing (or dropping) an
// already-terminal item is refused rather than silently re-applied.
func TestCloseAlreadyClosedIsInvalidTransition(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	item := mustAdd(t, s, AddItemInput{Title: "close-twice", Actor: "tester"})
	if _, err := s.CloseItem(ctx, CloseItemInput{ID: item.ID, Reason: "done", Actor: "tester"}); err != nil {
		t.Fatalf("first CloseItem: %v", err)
	}

	before := countEvents(t, s)
	if _, err := s.CloseItem(ctx, CloseItemInput{ID: item.ID, Reason: "done again", Actor: "tester"}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("second CloseItem: err = %v, want errors.Is(err, ErrInvalidTransition)", err)
	}
	after := countEvents(t, s)
	if after != before {
		t.Errorf("rejected re-close wrote %d events, want 0", after-before)
	}
}

// TestUpdateRecomputesScore proves score is recomputed exactly when a
// scoring input changes (RANK-01) and left alone otherwise.
func TestUpdateRecomputesScore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	t.Run("changing effort alone changes score by exactly the expected delta", func(t *testing.T) {
		item := mustAdd(t, s, AddItemInput{Title: "effort-fixture", Actor: "tester", Value: intPtr(5), Confidence: intPtr(5), Effort: intPtr(5)})
		const wantOriginal = 16 // 5+5-5+11
		if item.Score != wantOriginal {
			t.Fatalf("fixture score = %d, want %d", item.Score, wantOriginal)
		}

		newEffort := 0
		newEffortPtr := &newEffort
		got, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "tester", Effort: &newEffortPtr})
		if err != nil {
			t.Fatalf("UpdateItem: %v", err)
		}
		const wantNew = 21 // 5+5-0+11 -- effort dropped by 5, score rises by exactly 5
		if got.Score != wantNew {
			t.Fatalf("Score = %d, want %d (delta of exactly %d)", got.Score, wantNew, wantNew-wantOriginal)
		}
	})

	t.Run("changing title alone leaves score untouched", func(t *testing.T) {
		item := mustAdd(t, s, AddItemInput{Title: "title-fixture", Actor: "tester", Value: intPtr(3), Confidence: intPtr(3), Effort: intPtr(3)})

		newTitle := "title-fixture-renamed"
		got, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "tester", Title: &newTitle})
		if err != nil {
			t.Fatalf("UpdateItem: %v", err)
		}
		if got.Score != item.Score {
			t.Fatalf("Score changed from %d to %d after a title-only update, want unchanged", item.Score, got.Score)
		}
		if got.Title != newTitle {
			t.Fatalf("Title = %q, want %q", got.Title, newTitle)
		}
	})
}

// TestUpdateRejectsEmptyChangeSet proves UpdateItem with no fields set is a
// caller error, not a silent no-op (02-CONTEXT.md decision 4) -- and that it
// writes nothing and leaves the row unchanged.
func TestUpdateRejectsEmptyChangeSet(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	item := mustAdd(t, s, AddItemInput{Title: "no-op-update", Actor: "tester"})

	before := countEvents(t, s)
	_, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "tester"})
	after := countEvents(t, s)

	if !errors.Is(err, ErrNoFieldsToUpdate) {
		t.Fatalf("err = %v, want errors.Is(err, ErrNoFieldsToUpdate)", err)
	}
	if after != before {
		t.Errorf("rejected empty update wrote %d events, want 0", after-before)
	}

	detail, gerr := s.GetItem(ctx, item.ID)
	if gerr != nil {
		t.Fatalf("GetItem: %v", gerr)
	}
	if detail.Title != item.Title || detail.UpdatedAt != item.UpdatedAt {
		t.Fatalf("row changed despite rejected update: got title=%q updated_at=%q, want title=%q updated_at=%q",
			detail.Title, detail.UpdatedAt, item.Title, item.UpdatedAt)
	}
}

// TestStartRecordsWhoClaimedIt.
//
// Found by looking at a real board: an item claimed from the page showed
// "nobody -- claimed from the board" in the column headed "Claimed by". The
// claim was recorded in the event trail but never on the item, so every
// view that asks who is working on something had to answer "nobody" about
// work a person had just picked up.
func TestStartRecordsWhoClaimedIt(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	item := mustAdd(t, st, AddItemInput{Title: "a thing to claim", Actor: "someone"})

	got, err := st.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "dev", SessionID: "sess-assignee"})
	if err != nil {
		t.Fatalf("StartItem: %v", err)
	}
	if got.Assignee == nil || *got.Assignee != "dev" {
		t.Errorf("Assignee = %v, want dev -- a claim must say who claimed it", got.Assignee)
	}

	// And it survives the round trip, rather than only being set on the
	// returned copy.
	reread, err := st.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if reread.Assignee == nil || *reread.Assignee != "dev" {
		t.Errorf("stored Assignee = %v, want dev", reread.Assignee)
	}
}

// TestStartLeavesAnExistingAssigneeAlone. Assigning work to someone and
// then claiming it yourself is a real sequence; the claim must not quietly
// reassign it. The event trail already names who did the claiming.
func TestStartLeavesAnExistingAssigneeAlone(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	assigned := "rae"
	item := mustAdd(t, st, AddItemInput{Title: "already assigned", Actor: "someone", Assignee: assigned})

	got, err := st.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "dev", SessionID: "sess-assignee"})
	if err != nil {
		t.Fatalf("StartItem: %v", err)
	}
	if got.Assignee == nil || *got.Assignee != "rae" {
		t.Errorf("Assignee = %v, want rae -- claiming must not reassign", got.Assignee)
	}
}
