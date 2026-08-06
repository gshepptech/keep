package store

import (
	"context"
	"database/sql"
	"testing"
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
