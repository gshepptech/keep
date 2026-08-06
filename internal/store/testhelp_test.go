package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gshepptech/rounce/internal/model"
)

// openTestStore initializes a fresh store in t.TempDir() and registers
// t.Cleanup to close it, so every test in this package gets an isolated,
// self-cleaning database file rather than sharing state.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sqlite")
	s, err := Init(path)
	if err != nil {
		t.Fatalf("openTestStore: Init: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("openTestStore: Close: %v", err)
		}
	})
	return s
}

// countEvents returns the total number of rows in events, for tests that
// assert "exactly one event was written" by diffing a before/after count.
func countEvents(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM events").Scan(&n); err != nil {
		t.Fatalf("countEvents: %v", err)
	}
	return n
}

// eventsFor returns every events row for itemID, oldest first, with an
// explicit column list (never SELECT *, per 02-RESEARCH.md Pattern 6).
func eventsFor(t *testing.T, s *Store, id string) []model.Event {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT id, item_id, kind, actor, session_id, at, payload FROM events WHERE item_id = ? ORDER BY id ASC`, id)
	if err != nil {
		t.Fatalf("eventsFor: %v", err)
	}
	defer rows.Close()

	var out []model.Event
	for rows.Next() {
		var e model.Event
		if err := rows.Scan(&e.ID, &e.ItemID, &e.Kind, &e.Actor, &e.SessionID, &e.At, &e.Payload); err != nil {
			t.Fatalf("eventsFor: scan: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("eventsFor: rows: %v", err)
	}
	return out
}

// readyIDs selects only id from the ready view, in view order -- never
// SELECT * (02-RESEARCH.md Pattern 6), since the view's own i.* is not
// frozen at CREATE VIEW time.
func readyIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "SELECT id FROM ready")
	if err != nil {
		t.Fatalf("readyIDs: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("readyIDs: scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("readyIDs: rows: %v", err)
	}
	return out
}

// execSQL runs a raw fixture statement directly against the store's
// database, bypassing any store method. This lets a test build fixtures
// (e.g. a raw `blocks` deps row) that a later plan's not-yet-written store
// method would otherwise be needed for, keeping each test's assertion
// independent of unrelated, not-yet-built functionality.
func execSQL(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("execSQL: %v (query: %s)", err, query)
	}
}
