package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/gshepptech/keep/internal/model"
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

// TestEveryMutationWritesExactlyOneEvent's cases table is guarded by SET
// EQUALITY against mutatingStoreMethods(t) (mutating_scan_test.go), not by a
// hand-maintained count -- 02-VERIFICATION.md found a genuine seventh
// mutating method that skipped the event write, touched this file not at
// all, and the whole suite still passed under the count-based guard this
// replaced. A method the scan finds that this table does not enumerate now
// fails by name; a stale row naming a method the scan no longer finds
// (renamed or deleted) fails the other way, also by name. Add a row here --
// and nothing else -- whenever a new mutating store method is added; the
// scan is what notices if you forget.
//
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
	// Unclaim needs a claimed item, so this one is started before the timed
	// window opens -- the start writes its own event and would otherwise be
	// counted against the unclaim under test.
	unclaimTarget := mustAdd(t, s, AddItemInput{Title: "unclaim-target", Actor: "tester"})
	if _, err := s.StartItem(ctx, StartItemInput{ID: unclaimTarget.ID, Actor: "tester", SessionID: "sess-gone"}); err != nil {
		t.Fatalf("preparing the unclaim target: %v", err)
	}
	// Reopen needs a finished item, so this one is closed before the timed
	// window opens, for the same reason the unclaim target is started early.
	reopenTarget := mustAdd(t, s, AddItemInput{Title: "reopen-target", Actor: "tester"})
	if _, err := s.CloseItem(ctx, CloseItemInput{ID: reopenTarget.ID, Reason: "closed by mistake", Actor: "tester"}); err != nil {
		t.Fatalf("preparing the reopen target: %v", err)
	}
	// NextItem claims whatever `ready` puts first, so its target cannot be
	// named in advance -- it is whichever open ticket outranks the rest at
	// the moment the case runs. That is the point of the verb, and it is
	// why this fixture asserts the event count rather than the item id.
	nextTarget := mustAdd(t, s, AddItemInput{Title: "next-target", Actor: "tester", Value: intPtr(10), Confidence: intPtr(10), Effort: intPtr(0)})
	_ = nextTarget
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
		{"UnclaimItem", func() error {
			_, err := s.UnclaimItem(ctx, UnclaimItemInput{ID: unclaimTarget.ID, Reason: "the agent stopped", Actor: "tester"})
			return err
		}},
		{"ReopenItem", func() error {
			_, err := s.ReopenItem(ctx, ReopenItemInput{ID: reopenTarget.ID, Reason: "the close was wrong", Actor: "tester"})
			return err
		}},
		{"DropItem", func() error {
			_, err := s.DropItem(ctx, DropItemInput{ID: dropTarget.ID, Reason: "abandoned", Actor: "tester"})
			return err
		}},
		{"NextItem", func() error {
			_, err := s.NextItem(ctx, NextInput{Actor: "tester", SessionID: "sess-next"})
			return err
		}},
		{"UpdateItem", func() error {
			_, err := s.UpdateItem(ctx, UpdateItemInput{ID: updateTarget.ID, Actor: "tester", Title: &updateTitle})
			return err
		}},
	}

	// Set-equality against the scan, not a count: a wrong-number failure
	// ("enumerated 6, want 7") sends a reader to the wrong file, while a
	// named-method failure sends them straight to the fix.
	// exemptFromOneEvent names the mutating methods EVENT-01's "exactly one
	// event" rule does not apply to, with the reason, so an exemption is a
	// decision someone wrote down rather than a method quietly missing from
	// the table below.
	//
	// Import restores a store from an export. It writes the events table
	// directly, verbatim, in whatever number the source store held -- that
	// is the point of it. An `import` event of its own would be a row in the
	// audit trail claiming something happened to the work, when what
	// happened was that a file was copied back; and the count could never be
	// one, because the rows it writes ARE the history it is restoring.
	exemptFromOneEvent := map[string]string{
		"Import": "restores the events table verbatim; see internal/store/export.go",
	}

	scanned := mutatingStoreMethods(t)
	enumerated := make(map[string]bool, len(cases))
	for _, tc := range cases {
		enumerated[tc.name] = true
	}
	for name := range scanned {
		if _, exempt := exemptFromOneEvent[name]; exempt {
			continue
		}
		if !enumerated[name] {
			t.Fatalf("%s mutates the store but is not enumerated in EVENT-01's table -- add a row asserting it writes exactly one event", name)
		}
	}
	for name := range enumerated {
		if !scanned[name] {
			t.Fatalf("cases enumerates %q, but mutatingStoreMethods no longer finds it -- the method was renamed or deleted; remove or update this stale row", name)
		}
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
