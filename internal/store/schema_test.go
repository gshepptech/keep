package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
)

// TestInitCreatesSchema proves STORE-01: a fresh Init produces items, deps,
// events, meta, the ready view, the full-text index and its triggers, and
// meta.schema_version is this binary's. Object
// presence is asserted directly against sqlite_master (name + type), not
// inferred by trying an operation -- and the ready view is separately
// queried, because a view with a broken body succeeds at CREATE time and
// only fails when selected from, so sqlite_master presence alone would not
// be proof.
func TestInitCreatesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")

	s, err := Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	wantObjects := map[string]string{
		"items":  "table",
		"deps":   "table",
		"events": "table",
		"meta":   "table",
		"ready":  "view",
		// The full-text index and the three triggers that keep it in step
		// with items. A store missing any of them still opens and still
		// answers every query except `search`, which quietly returns
		// nothing for items it should find -- so their presence is asserted
		// here rather than left to the search tests, which pass on a
		// LIKE-only fallback too.
		"items_fts":              "table",
		"items_fts_after_insert": "trigger",
		"items_fts_after_delete": "trigger",
		"items_fts_after_update": "trigger",
	}
	for name, wantType := range wantObjects {
		var gotType string
		err := s.db.QueryRowContext(ctx,
			"SELECT type FROM sqlite_master WHERE name = ?", name).Scan(&gotType)
		if err != nil {
			t.Fatalf("sqlite_master missing %q: %v", name, err)
		}
		if gotType != wantType {
			t.Fatalf("sqlite_master %q: type = %q, want %q", name, gotType, wantType)
		}
	}

	rows, err := s.db.QueryContext(ctx, "SELECT * FROM ready")
	if err != nil {
		t.Fatalf("SELECT * FROM ready: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating ready: %v", err)
	}
	rows.Close()

	var version string
	if err := s.db.QueryRowContext(ctx,
		"SELECT value FROM meta WHERE key = 'schema_version'").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if want := strconv.Itoa(schemaVersion); version != want {
		t.Fatalf("schema_version = %q, want %q", version, want)
	}
}

// TestInitIsIdempotentAndPreservesData proves STORE-02, reproducing the
// exact exit-gate scenario from 01-RESEARCH.md "Idempotent init": init,
// write a canary items row directly (raw SQL is in-bounds in the store
// package's own tests), init again, and confirm the row survives, still
// appears in ready, and meta still holds exactly one schema_version row.
// Existence checks alone do not satisfy STORE-02 -- the row-survival
// assertion is the requirement.
func TestInitIsIdempotentAndPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")

	s1, err := Init(path)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	ctx := context.Background()
	const now = "2026-08-05T00:00:00Z"
	_, err = s1.db.ExecContext(ctx, `
		INSERT INTO items (id, parent_id, ordinal, stage, type, title, status, created_at, updated_at)
		VALUES ('rnc-canary', NULL, NULL, 'ticket', 'task', 'canary', 'open', ?, ?)`,
		now, now)
	if err != nil {
		t.Fatalf("insert canary row: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close after first Init: %v", err)
	}

	s2, err := Init(path)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	defer s2.Close()

	var title string
	if err := s2.db.QueryRowContext(ctx,
		"SELECT title FROM items WHERE id = 'rnc-canary'").Scan(&title); err != nil {
		t.Fatalf("canary row missing after second Init: %v", err)
	}
	if title != "canary" {
		t.Fatalf("canary title = %q, want %q", title, "canary")
	}

	var readyID string
	if err := s2.db.QueryRowContext(ctx,
		"SELECT id FROM ready WHERE id = 'rnc-canary'").Scan(&readyID); err != nil {
		t.Fatalf("canary not present in ready after second Init: %v", err)
	}

	var versionCount int
	if err := s2.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM meta WHERE key = 'schema_version'").Scan(&versionCount); err != nil {
		t.Fatalf("count schema_version rows: %v", err)
	}
	if versionCount != 1 {
		t.Fatalf("schema_version row count = %d, want 1", versionCount)
	}

	var version string
	if err := s2.db.QueryRowContext(ctx,
		"SELECT value FROM meta WHERE key = 'schema_version'").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if want := strconv.Itoa(schemaVersion); version != want {
		t.Fatalf("schema_version = %q, want %q", version, want)
	}
}

// TestOpenRejectsSchemaDrift proves Open refuses a store whose stored
// schema_version has drifted from what this binary expects (01-RESEARCH.md
// Pitfall 4).
func TestOpenRejectsSchemaDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")

	s, err := Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		"UPDATE meta SET value = ? WHERE key = 'schema_version'", schemaVersion+1); err != nil {
		t.Fatalf("drift meta row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = Open(path, OpenOptions{})
	if err == nil {
		t.Fatal("Open succeeded against a drifted store, want error")
	}
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("Open error = %v, want errors.Is(err, ErrSchemaVersion)", err)
	}
}

// TestOpenRejectsUninitializedPath proves Open on a path that was never
// initialized fails rather than presenting an empty store as valid -- a
// default read-write sql.Open fabricates an empty database file, and this
// is what stops that from looking like success.
func TestOpenRejectsUninitializedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-initialized.sqlite")

	_, err := Open(path, OpenOptions{})
	if err == nil {
		t.Fatal("Open succeeded on a never-initialized path, want error")
	}
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("Open error = %v, want errors.Is(err, ErrSchemaVersion)", err)
	}
}

// TestInitDDLIsAtomic drives withTx's rollback against real DDL, proving the
// "applied in one transaction" claim rather than asserting it in a comment.
// The second statement is syntactically valid and fails only at execution
// time -- a syntax error could be rejected before the first statement ever
// ran, which would make this pass vacuously.
func TestInitDDLIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")

	const badDDL = "CREATE TABLE ddl_canary (x);\n" +
		"CREATE INDEX idx_ddl_canary ON no_such_table(x);"

	if _, err := initWithDDL(path, badDDL, schemaVersion); err == nil {
		t.Fatal("initWithDDL with a DDL that fails partway through returned no error")
	}

	db, err := sql.Open("sqlite", dsn(path, OpenOptions{}))
	if err != nil {
		t.Fatalf("open resulting file: %v", err)
	}
	defer db.Close()

	var name string
	err = db.QueryRowContext(context.Background(),
		"SELECT name FROM sqlite_master WHERE name = 'ddl_canary'").Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ddl_canary survived the failing DDL: err = %v, name = %q", err, name)
	}
}
