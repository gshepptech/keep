package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/gshepptech/rounce/internal/model"
)

// fixtureItem inserts a minimal, valid items row directly (bypassing
// AddItem, which does not exist until this plan's task 2) so writeEvent's
// own tests can satisfy the events.item_id foreign key without depending on
// functionality this task does not own.
func fixtureItem(t *testing.T, s *Store, id string) {
	t.Helper()
	execSQL(t, s, `INSERT INTO items (id, stage, type, title, status, score, created_at, updated_at)
VALUES (?, 'ticket', 'task', 'fixture', 'open', 0, '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`, id)
}

func TestWriteEventInsertsRow(t *testing.T) {
	s := openTestStore(t)
	fixtureItem(t, s, "rnc-000001")

	err := s.withTx(context.Background(), func(tx *sql.Tx) error {
		return writeEvent(context.Background(), tx, "rnc-000001", "add", "tester", "session-a", map[string]string{"k": "v"})
	})
	if err != nil {
		t.Fatalf("writeEvent: %v", err)
	}

	events := eventsFor(t, s, "rnc-000001")
	if len(events) != 1 {
		t.Fatalf("eventsFor returned %d events, want 1", len(events))
	}
	e := events[0]
	if e.Kind != "add" {
		t.Errorf("Kind = %q, want %q", e.Kind, "add")
	}
	if e.Actor != "tester" {
		t.Errorf("Actor = %q, want %q", e.Actor, "tester")
	}
	if e.SessionID == nil || *e.SessionID != "session-a" {
		t.Errorf("SessionID = %v, want %q", e.SessionID, "session-a")
	}
	if e.Payload == nil {
		t.Error("Payload = nil, want non-nil JSON payload")
	}
}

// TestWriteEventEmptySessionIsNull proves an empty sessionID is stored as
// SQL NULL, not the empty string -- "no session" and "session named empty"
// must not be the same row (writeEvent's own doc comment states this rule).
func TestWriteEventEmptySessionIsNull(t *testing.T) {
	s := openTestStore(t)
	fixtureItem(t, s, "rnc-000002")

	err := s.withTx(context.Background(), func(tx *sql.Tx) error {
		return writeEvent(context.Background(), tx, "rnc-000002", "add", "tester", "", nil)
	})
	if err != nil {
		t.Fatalf("writeEvent: %v", err)
	}

	events := eventsFor(t, s, "rnc-000002")
	if len(events) != 1 {
		t.Fatalf("eventsFor returned %d events, want 1", len(events))
	}
	if events[0].SessionID != nil {
		t.Fatalf("SessionID = %q, want nil (NULL) for an empty sessionID input", *events[0].SessionID)
	}
	if events[0].Payload != nil {
		t.Fatalf("Payload = %q, want nil (NULL) for a nil payload input", *events[0].Payload)
	}
}

// wantMutatingMethods guards the table in
// TestEveryMutationWritesExactlyOneEvent: bump it deliberately, and add a
// row to the table below, whenever a new mutating store method is added.
// Without this guard, forgetting to add a new verb here would silently
// leave it outside EVENT-01's enumeration -- the same shape as plan 01's
// wantCases guard and plan 05's zero-file guard.
const wantMutatingMethods = 6

// TestEveryMutationWritesExactlyOneEvent is EVENT-01's enforcement: a
// table enumerating every mutating method in this package, each asserting
// a before/after event-count delta of exactly 1. A second subtest asserts
// the one deliberate exception (02-CONTEXT.md decision 3): a same-session
// StartItem retry writes ZERO events.
func TestEveryMutationWritesExactlyOneEvent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	blocker := mustAdd(t, s, AddItemInput{Title: "shared-blocker", Actor: "tester"})

	// Every fixture item each case's mutation acts on is created here, OUTSIDE
	// any case's own before/after countEvents window -- mustAdd itself writes
	// an `add` event, so calling it inside a case's timed closure would count
	// the fixture's own event alongside the mutation under test and produce a
	// false "wrote 2 events" failure that has nothing to do with the method
	// being tested.
	depSource := mustAdd(t, s, AddItemInput{Title: "dep-source", Actor: "tester"})
	startTarget := mustAdd(t, s, AddItemInput{Title: "start-target", Actor: "tester"})
	closeTarget := mustAdd(t, s, AddItemInput{Title: "close-target", Actor: "tester"})
	dropTarget := mustAdd(t, s, AddItemInput{Title: "drop-target", Actor: "tester"})
	updateTarget := mustAdd(t, s, AddItemInput{Title: "update-target", Actor: "tester"})
	updateTitle := "update-target-renamed"

	cases := []struct {
		name string
		fn   func() error
	}{
		{"AddItem", func() error {
			_, err := s.AddItem(ctx, AddItemInput{Title: "added-item", Actor: "tester"})
			return err
		}},
		{"CreateDep", func() error {
			return s.CreateDep(ctx, DepInput{ItemID: depSource.ID, DependsOnID: blocker.ID, Type: model.DepRelated, Actor: "tester"})
		}},
		{"StartItem", func() error {
			_, err := s.StartItem(ctx, StartItemInput{ID: startTarget.ID, Actor: "tester", SessionID: "sess-enum"})
			return err
		}},
		{"CloseItem", func() error {
			_, err := s.CloseItem(ctx, CloseItemInput{ID: closeTarget.ID, Reason: "done", Actor: "tester"})
			return err
		}},
		{"DropItem", func() error {
			_, err := s.DropItem(ctx, DropItemInput{ID: dropTarget.ID, Reason: "abandoned", Actor: "tester"})
			return err
		}},
		{"UpdateItem", func() error {
			_, err := s.UpdateItem(ctx, UpdateItemInput{ID: updateTarget.ID, Actor: "tester", Title: &updateTitle})
			return err
		}},
	}

	if len(cases) != wantMutatingMethods {
		t.Fatalf("enumerated %d mutating methods, want %d -- bump wantMutatingMethods deliberately (and add a row above) when a verb is added", len(cases), wantMutatingMethods)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countEvents(t, s)
			if err := tc.fn(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			after := countEvents(t, s)
			if after-before != 1 {
				t.Fatalf("%s wrote %d events, want exactly 1", tc.name, after-before)
			}
		})
	}

	t.Run("StartItem same-session retry writes ZERO events", func(t *testing.T) {
		item := mustAdd(t, s, AddItemInput{Title: "retry-target", Actor: "tester"})
		if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-retry"}); err != nil {
			t.Fatalf("initial StartItem: %v", err)
		}

		before := countEvents(t, s)
		if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "tester", SessionID: "sess-retry"}); err != nil {
			t.Fatalf("same-session retry StartItem: %v, want idempotent success", err)
		}
		after := countEvents(t, s)
		if after != before {
			t.Fatalf("same-session retry wrote %d events, want exactly 0", after-before)
		}
	})
}
