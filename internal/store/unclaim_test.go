package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// TestUnclaimIsTheThirdExitFromInProgress.
//
// Before this verb, `start` could move an item to in_progress and only
// `close` or `drop` could move it out. An agent that died mid-item left the
// row stuck out of `ready`, and both exits recorded something untrue: close
// says the work was done, drop says it was not worth doing.
//
// ITEM-08 keeps close and drop distinguishable so abandoned work is never
// counted as done. That guarantee only means something if there is a third
// exit for the case where neither is true — otherwise the rescue path is to
// pick one and lie, in the audit trail the whole design exists to keep
// honest.
func TestUnclaimIsTheThirdExitFromInProgress(t *testing.T) {
	ctx := context.Background()

	claimed := func(t *testing.T, session string) (*Store, model.Item) {
		t.Helper()
		s := openTestStore(t)
		item, err := s.AddItem(ctx, AddItemInput{Actor: "filer", Title: "Expire refresh tokens"})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}
		if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent", SessionID: session}); err != nil {
			t.Fatalf("StartItem: %v", err)
		}
		return s, item
	}

	t.Run("the item returns to the queue", func(t *testing.T) {
		s, item := claimed(t, "sess-dead")

		got, err := s.UnclaimItem(ctx, UnclaimItemInput{
			ID: item.ID, Actor: "dev", Reason: "the agent crashed",
		})
		if err != nil {
			t.Fatalf("UnclaimItem: %v", err)
		}
		if got.Status != model.StatusOpen {
			t.Errorf("status = %q, want %q", got.Status, model.StatusOpen)
		}
		if got.ClaimedBySession != nil {
			t.Errorf("claimed_by_session = %q, want cleared", *got.ClaimedBySession)
		}
		if got.ClaimedAt != nil {
			t.Errorf("claimed_at = %q, want cleared", *got.ClaimedAt)
		}

		// The point of the whole verb: it is workable again.
		ready, err := s.Ready(ctx, 0)
		if err != nil {
			t.Fatalf("Ready: %v", err)
		}
		var found bool
		for _, r := range ready {
			if r.ID == item.ID {
				found = true
			}
		}
		if !found {
			t.Error("the released item is not back in ready — the rescue did not rescue anything")
		}
	})

	// A rescue has to work when the holder is gone, which is the only case
	// it serves. StartItem refuses a foreign claim on purpose; this must not.
	t.Run("any session may release a claim it does not hold", func(t *testing.T) {
		s, item := claimed(t, "sess-dead")
		if _, err := s.UnclaimItem(ctx, UnclaimItemInput{
			ID: item.ID, Actor: "dev", SessionID: "sess-someone-else",
			Reason: "that session has been gone an hour",
		}); err != nil {
			t.Fatalf("UnclaimItem from a different session: %v", err)
		}
	})

	// A rescue is never anonymous, and the trail still has to answer whose
	// work it was after the claim itself is gone.
	t.Run("the event names the rescuer and the session that lost it", func(t *testing.T) {
		s, item := claimed(t, "sess-dead")
		if _, err := s.UnclaimItem(ctx, UnclaimItemInput{
			ID: item.ID, Actor: "dev", Reason: "the agent crashed",
		}); err != nil {
			t.Fatalf("UnclaimItem: %v", err)
		}

		detail, err := s.GetItem(ctx, item.ID)
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		last := detail.Events[len(detail.Events)-1]
		if last.Kind != "unclaim" {
			t.Fatalf("last event kind = %q, want %q — close and drop must stay distinguishable from this", last.Kind, "unclaim")
		}
		if last.Actor != "dev" {
			t.Errorf("actor = %q, want the rescuer", last.Actor)
		}
		if last.Payload == nil {
			t.Fatal("the unclaim event has no payload, so it records neither who lost the claim nor why")
		}
		for _, want := range []string{"sess-dead", "the agent crashed"} {
			if !strings.Contains(*last.Payload, want) {
				t.Errorf("payload %q does not carry %q", *last.Payload, want)
			}
		}
	})

	// Releasing something nobody holds is a mistake worth reporting, not a
	// silent no-op — the caller believes a claim exists.
	t.Run("only a claimed item can be released", func(t *testing.T) {
		for _, c := range []struct {
			name string
			prep func(t *testing.T, s *Store, id string)
		}{
			{"open", func(t *testing.T, s *Store, id string) {}},
			{"closed", func(t *testing.T, s *Store, id string) {
				if _, err := s.CloseItem(ctx, CloseItemInput{ID: id, Actor: "a", Reason: "done"}); err != nil {
					t.Fatal(err)
				}
			}},
			{"dropped", func(t *testing.T, s *Store, id string) {
				if _, err := s.DropItem(ctx, DropItemInput{ID: id, Actor: "a", Reason: "no"}); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(c.name, func(t *testing.T) {
				s := openTestStore(t)
				item, err := s.AddItem(ctx, AddItemInput{Actor: "filer", Title: "A thing"})
				if err != nil {
					t.Fatal(err)
				}
				c.prep(t, s, item.ID)

				_, err = s.UnclaimItem(ctx, UnclaimItemInput{ID: item.ID, Actor: "a", Reason: "why"})
				if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("UnclaimItem on a %s item = %v, want ErrInvalidTransition", c.name, err)
				}
			})
		}
	})

	// Claim answers "who has this now"; assignee answers "who should do
	// this". Only the first one ended.
	t.Run("assignee survives the release", func(t *testing.T) {
		s := openTestStore(t)
		item, err := s.AddItem(ctx, AddItemInput{Actor: "filer", Title: "A thing", Assignee: "priya"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent", SessionID: "sess-dead"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.UnclaimItem(ctx, UnclaimItemInput{ID: item.ID, Actor: "a", Reason: "crashed"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Assignee == nil || *got.Assignee != "priya" {
			t.Errorf("assignee = %v, want priya — releasing a claim is not unassigning the work", got.Assignee)
		}
	})
}
