package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// TestReopenIsTheWayOutOfTerminal checks that reopen moves a closed or
// dropped item back into the queue.
//
// Without reopen, close and drop would be statuses with no exit. A
// mis-typed id could not be undone: the work would have to be filed again
// under a new id, losing the event history and every dependency edge
// pointing at the original, and leaving the first record still claiming
// the work is done.
//
// The events table is the complete record of every mutation, so the way
// out is not to edit the close (nothing edits history here) but to
// append a `reopen` that says the close was wrong. These tests pin what that
// append has to record for the log to stay honest.
func TestReopenIsTheWayOutOfTerminal(t *testing.T) {
	ctx := context.Background()

	finished := func(t *testing.T, how string) (*Store, model.Item) {
		t.Helper()
		s := openTestStore(t)
		item, err := s.AddItem(ctx, AddItemInput{Actor: "filer", Title: "Expire refresh tokens"})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}
		switch how {
		case model.StatusClosed:
			_, err = s.CloseItem(ctx, CloseItemInput{ID: item.ID, Reason: "merged in #42", Actor: "agent"})
		case model.StatusDropped:
			_, err = s.DropItem(ctx, DropItemInput{ID: item.ID, Reason: "not worth doing", Actor: "agent"})
		default:
			t.Fatalf("finished: unknown status %q", how)
		}
		if err != nil {
			t.Fatalf("finishing the item as %s: %v", how, err)
		}
		return s, item
	}

	t.Run("a closed item comes back to the queue", func(t *testing.T) {
		s, item := finished(t, model.StatusClosed)

		got, err := s.ReopenItem(ctx, ReopenItemInput{
			ID: item.ID, Reason: "wrong id, that was not the one", Actor: "greg",
		})
		if err != nil {
			t.Fatalf("ReopenItem: %v", err)
		}
		if got.Status != model.StatusOpen {
			t.Errorf("status = %q, want %q", got.Status, model.StatusOpen)
		}

		ready := readyIDs(t, s)
		if !containsID(ready, item.ID) {
			t.Errorf("a reopened item is not workable; ready held %v", ready)
		}
	})

	t.Run("a dropped item comes back too", func(t *testing.T) {
		s, item := finished(t, model.StatusDropped)

		got, err := s.ReopenItem(ctx, ReopenItemInput{
			ID: item.ID, Reason: "it is worth doing after all", Actor: "greg",
		})
		if err != nil {
			t.Fatalf("ReopenItem: %v", err)
		}
		if got.Status != model.StatusOpen {
			t.Errorf("status = %q, want %q", got.Status, model.StatusOpen)
		}
	})

	// A reopened item has no close date. Leaving the old one would make every
	// date-ordered read report a finish that was taken back.
	t.Run("closed_at is cleared, not left stale", func(t *testing.T) {
		s, item := finished(t, model.StatusClosed)

		before, err := s.GetItem(ctx, item.ID)
		if err != nil {
			t.Fatalf("GetItem before: %v", err)
		}
		if before.ClosedAt == nil {
			t.Fatal("the fixture is wrong: a closed item should carry closed_at")
		}

		got, err := s.ReopenItem(ctx, ReopenItemInput{ID: item.ID, Reason: "undo", Actor: "greg"})
		if err != nil {
			t.Fatalf("ReopenItem: %v", err)
		}
		if got.ClosedAt != nil {
			t.Errorf("returned closed_at = %v, want nil", *got.ClosedAt)
		}

		after, err := s.GetItem(ctx, item.ID)
		if err != nil {
			t.Fatalf("GetItem after: %v", err)
		}
		if after.ClosedAt != nil {
			t.Errorf("stored closed_at = %v, want nil", *after.ClosedAt)
		}
	})

	// Reopening cannot resurrect a claim. The session that held it is gone,
	// and in_progress asserts that someone is working on it right now.
	t.Run("it returns to open, never to in progress", func(t *testing.T) {
		ctx := context.Background()
		s := openTestStore(t)
		item, err := s.AddItem(ctx, AddItemInput{Actor: "filer", Title: "Expire refresh tokens"})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}
		if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent", SessionID: "sess-gone"}); err != nil {
			t.Fatalf("StartItem: %v", err)
		}
		if _, err := s.CloseItem(ctx, CloseItemInput{ID: item.ID, Reason: "done", Actor: "agent"}); err != nil {
			t.Fatalf("CloseItem: %v", err)
		}

		got, err := s.ReopenItem(ctx, ReopenItemInput{ID: item.ID, Reason: "not actually done", Actor: "greg"})
		if err != nil {
			t.Fatalf("ReopenItem: %v", err)
		}
		if got.Status != model.StatusOpen {
			t.Fatalf("status = %q, want %q", got.Status, model.StatusOpen)
		}
		if got.ClaimedBySession != nil {
			t.Errorf("claimed_by_session = %q, want nil -- the holding session is gone", *got.ClaimedBySession)
		}
		if got.ClaimedAt != nil {
			t.Errorf("claimed_at = %v, want nil", *got.ClaimedAt)
		}
	})

	// close and drop are kept distinguishable. If the reopen event does
	// not say which one it undid, the log cannot tell "we said this was done
	// and it was not" from "we said this was not worth doing and it was".
	t.Run("the event names the status it came back from", func(t *testing.T) {
		for _, from := range []string{model.StatusClosed, model.StatusDropped} {
			t.Run(from, func(t *testing.T) {
				s, item := finished(t, from)
				if _, err := s.ReopenItem(ctx, ReopenItemInput{
					ID: item.ID, Reason: "the call was wrong", Actor: "greg", SessionID: "sess-rescue",
				}); err != nil {
					t.Fatalf("ReopenItem: %v", err)
				}

				detail, err := s.GetItem(ctx, item.ID)
				if err != nil {
					t.Fatalf("GetItem: %v", err)
				}
				var found bool
				for _, e := range detail.Events {
					if e.Kind != "reopen" {
						continue
					}
					found = true
					if e.Actor != "greg" {
						t.Errorf("actor = %q, want %q", e.Actor, "greg")
					}
					if e.Payload == nil {
						t.Fatalf("reopen event carries no payload")
					}
					if !strings.Contains(*e.Payload, `"from":"`+from+`"`) {
						t.Errorf("payload %s does not name the status it came from (%q)", *e.Payload, from)
					}
					if !strings.Contains(*e.Payload, "the call was wrong") {
						t.Errorf("payload %s does not carry the reason", *e.Payload)
					}
				}
				if !found {
					t.Errorf("no reopen event for %s", item.ID)
				}
			})
		}
	})

	t.Run("only a finished item can be reopened", func(t *testing.T) {
		ctx := context.Background()
		s := openTestStore(t)
		item, err := s.AddItem(ctx, AddItemInput{Actor: "filer", Title: "Still open"})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}

		_, err = s.ReopenItem(ctx, ReopenItemInput{ID: item.ID, Reason: "nothing to undo", Actor: "greg"})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("reopening an open item: err = %v, want ErrInvalidTransition", err)
		}

		if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent", SessionID: "s1"}); err != nil {
			t.Fatalf("StartItem: %v", err)
		}
		_, err = s.ReopenItem(ctx, ReopenItemInput{ID: item.ID, Reason: "nothing to undo", Actor: "greg"})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("reopening an in-progress item: err = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("an actor is required", func(t *testing.T) {
		s, item := finished(t, model.StatusClosed)
		if _, err := s.ReopenItem(ctx, ReopenItemInput{ID: item.ID, Reason: "undo"}); err == nil {
			t.Fatal("reopening with no actor succeeded; the log would not say who did it")
		}
	})
}
