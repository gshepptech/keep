package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// schemaVersion is the compile-time value every Open compares the store's
// on-disk meta.schema_version against. Bumping this constant without a
// corresponding migration plan for stores created under the old value is
// exactly the failure mode checkSchemaVersion exists to make loud: it turns
// silent drift into a refused Open, rather than a store that limps along
// with columns the running binary does not expect.
const schemaVersion = 1

// schemaDDL is the entire schema -- four tables, five indexes, one unique
// index, and the ready view -- applied as a single statement string inside
// one transaction by initWithDDL. Every statement form uses its
// IF NOT EXISTS/OR IGNORE idiom (01-RESEARCH.md "Idempotent init: verified
// end-to-end") so that a second Init on an already-initialized store is a
// true no-op rather than an error.
//
// items carries an ordinal INTEGER column that root ROADMAP.md's own
// "Data model" table omits (01-RESEARCH.md Pitfall 3). It is required for
// two things: UNIQUE(parent_id, ordinal) below cannot be expressed without
// it, and GSD Phase 2's MAX(ordinal)+1 child-ordinal allocation has no
// column to query without it. SQLite treats NULLs as distinct within a
// unique index, so multiple root items (parent_id AND ordinal both NULL)
// never collide on this constraint -- that is what makes it safe to add the
// constraint now, before any code exists that allocates ordinals.
//
// No is_blocked column. It gets added when a benchmark says so, not in
// anticipation (README principle 6, phase-0.md).
const schemaDDL = `
CREATE TABLE IF NOT EXISTS items (
    id                  TEXT PRIMARY KEY,
    parent_id           TEXT REFERENCES items(id),
    ordinal             INTEGER,
    stage               TEXT NOT NULL CHECK (stage IN ('raw', 'ticket')),
    type                TEXT NOT NULL CHECK (type IN ('roadmap', 'epic', 'task', 'bug', 'note')),
    title               TEXT NOT NULL,
    body                TEXT NOT NULL DEFAULT '',
    acceptance          TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'in_progress', 'closed', 'dropped')),
    value               INTEGER,
    confidence          INTEGER,
    effort              INTEGER,
    score               INTEGER NOT NULL DEFAULT 0,
    why                 TEXT NOT NULL DEFAULT '',
    repo                TEXT,
    paths               TEXT,
    claimed_by_session  TEXT,
    claimed_at          TEXT,
    assignee            TEXT,
    external_ref        TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    closed_at           TEXT
);

CREATE INDEX IF NOT EXISTS idx_items_stage_status ON items(stage, status);
CREATE INDEX IF NOT EXISTS idx_items_parent_id ON items(parent_id);
CREATE INDEX IF NOT EXISTS idx_items_score ON items(score);
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_parent_ordinal ON items(parent_id, ordinal);

CREATE TABLE IF NOT EXISTS deps (
    item_id        TEXT NOT NULL REFERENCES items(id),
    depends_on_id  TEXT NOT NULL REFERENCES items(id),
    type           TEXT NOT NULL CHECK (type IN ('blocks', 'parent-child', 'related', 'derived-from')),
    created_at     TEXT NOT NULL,
    created_by     TEXT,
    PRIMARY KEY (item_id, depends_on_id, type)
);

CREATE INDEX IF NOT EXISTS idx_deps_depends_on_id_type ON deps(depends_on_id, type);

CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id     TEXT NOT NULL REFERENCES items(id),
    kind        TEXT NOT NULL,
    actor       TEXT NOT NULL,
    session_id  TEXT,
    at          TEXT NOT NULL,
    payload     TEXT
);

CREATE INDEX IF NOT EXISTS idx_events_item_id ON events(item_id);

CREATE TABLE IF NOT EXISTS meta (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL
);

CREATE VIEW IF NOT EXISTS ready AS
WITH RECURSIVE
  blocked_directly AS (
    SELECT DISTINCT d.item_id
    FROM deps d
    WHERE d.type = 'blocks'
      AND EXISTS (
        SELECT 1 FROM items b
        WHERE b.id = d.depends_on_id
          AND b.status NOT IN ('closed', 'dropped')
      )
  ),
  blocked_tree AS (
    SELECT item_id, 0 AS depth FROM blocked_directly
    UNION ALL
    SELECT d.item_id, bt.depth + 1
    FROM blocked_tree bt
    JOIN deps d ON d.depends_on_id = bt.item_id
    WHERE d.type = 'parent-child' AND bt.depth < 50
  )
SELECT i.* FROM items i
LEFT JOIN blocked_tree bt ON bt.item_id = i.id
WHERE i.stage  = 'ticket'
  AND i.status = 'open'
  AND bt.item_id IS NULL
ORDER BY i.score DESC, i.created_at ASC;

INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '1');
`

// open performs the bare connection setup shared by Open and Init: sql.Open
// through dsn() plus SetMaxOpenConns(1). It is the one place either public
// entry point touches *sql.DB directly, so they cannot drift apart.
//
// STORE-09: one keep process, one connection. Go's database/sql pool
// defaults assume a network database with many concurrent backends; SQLite
// is embedded and single-writer, so the pool must never be allowed to open
// a second physical connection in production.
func open(path string, opts OpenOptions) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path, opts))
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}

// initDDLMaxAttempts bounds the SQLITE_BUSY retry described below. Each
// retry's own backoff is a few milliseconds, so even the worst case (every
// attempt but the last hits the race) adds well under a tenth of a second --
// this is not a substitute for busy_timeout, it covers a narrower window
// busy_timeout does not.
const initDDLMaxAttempts = 8

// initWithDDL is the seam that makes "the whole DDL block is applied in a
// single transaction" a falsifiable claim rather than an unverified comment.
// Init(path) is exactly initWithDDL(path, schemaDDL); the parameter stays
// unexported so no caller outside this package may choose its own schema --
// it exists only so this package's own tests can feed a DDL that fails
// partway through and assert that nothing survived.
func initWithDDL(path string, ddl string) (*Store, error) {
	s, err := open(path, OpenOptions{})
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	// The very first writer to a brand-new database file must briefly hold
	// an exclusive lock while SQLite bootstraps WAL mode for that file
	// (creating the -wal/-shm sidecars and running wal-index recovery --
	// see sqlite.org/wal.html, "Concurrency"). If two of this plan's four
	// concurrent processes race to be that first writer, the loser's BEGIN
	// IMMEDIATE can come back SQLITE_BUSY almost instantly, without waiting
	// out busy_timeout=3000 -- this was confirmed directly on this machine:
	// running TestConcurrentInitSameDirectory (cmd/keep) 30 times surfaced
	// exactly this failure roughly 1 run in 8, always at this BeginTx, always
	// well under 3000ms elapsed. It is a real, narrow SQLite WAL-bootstrap
	// race, not a flaw in this test.
	//
	// The whole DDL block is idempotent (IF NOT EXISTS / OR IGNORE
	// throughout), so retrying it from scratch is always safe -- a retry
	// here can never leave a partial or duplicated schema. A handful of
	// near-immediate retries (a few milliseconds of backoff each) covers
	// this bootstrap window without masking any other class of failure:
	// only errors classified as SQLITE_BUSY are retried; every other error
	// (including the "table already exists" failure this package's own
	// GATE-03 break produces) returns immediately on the first attempt.
	var txErr error
	for attempt := 0; attempt < initDDLMaxAttempts; attempt++ {
		txErr = s.withTx(ctx, func(tx *sql.Tx) error {
			_, execErr := tx.ExecContext(ctx, ddl)
			return execErr
		})
		if txErr == nil || !isSQLiteBusy(txErr) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Millisecond)
	}
	if txErr != nil {
		s.db.Close()
		return nil, fmt.Errorf("init schema %s: %w", path, txErr)
	}

	// Init runs the version guard itself, so an init against a store from a
	// different binary version fails the same way an Open would, rather than
	// silently succeeding just because the DDL happened to be a no-op.
	if err := s.checkSchemaVersion(ctx); err != nil {
		s.db.Close()
		return nil, err
	}

	return s, nil
}

// isSQLiteBusy classifies err the same non-string-matching way errors.go
// documents: errors.As against *sqlite.Error, then comparing .Code() against
// modernc.org/sqlite/lib's exported constant, never matching err.Error()
// text.
func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code() == sqlite3.SQLITE_BUSY
}

// Init creates (or idempotently re-verifies) the schema at path: items,
// deps, events, meta, and the ready view, all in one transaction, plus the
// meta.schema_version row. Init does not create path's parent directory --
// internal/store takes a path and does not invent filesystem layout; that is
// the CLI's job (plan 04).
func Init(path string) (*Store, error) {
	return initWithDDL(path, schemaDDL)
}

// checkSchemaVersion is the Open-time guard that idempotent DDL cannot
// provide (01-RESEARCH.md Pitfall 4): CREATE TABLE IF NOT EXISTS is a name
// check, not a shape check, and will silently accept a pre-existing table
// with entirely different columns. This is the mechanism that actually
// detects that kind of drift.
//
// A missing meta table or missing schema_version row is also treated as an
// ErrSchemaVersion failure, not a valid empty store -- a default read-write
// sql.Open fabricates an empty database file on a path that was never
// initialized, and without this, that would look identical to success.
func (s *Store) checkSchemaVersion(ctx context.Context) error {
	var got string
	err := s.db.QueryRowContext(ctx,
		"SELECT value FROM meta WHERE key = 'schema_version'").Scan(&got)
	if err != nil {
		return fmt.Errorf("read schema_version (path never initialized, or meta table missing): %w: %w", ErrSchemaVersion, err)
	}
	if want := fmt.Sprintf("%d", schemaVersion); got != want {
		return fmt.Errorf("store schema_version %s does not match binary's expected %s, refusing to open: %w", got, want, ErrSchemaVersion)
	}
	return nil
}
