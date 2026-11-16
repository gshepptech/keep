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

// schemaDDLBase is the schema this binary creates for a brand-new store,
// before the full-text index and version row are appended. It is the
// current shape, at the top of the migration ladder. A schema change edits
// it and appends a matching rung to `migrations` in the same commit, so
// stores that already exist reach the same shape.
// TestFreshInitMatchesMigratedV1 compares the two paths object by object and
// fails if they disagree, so "edit both" is enforced rather than a
// convention.
//
// It holds four tables, five indexes, one unique index, and the ready view,
// applied as a single statement string inside
// one transaction by initWithDDL. Every statement form uses its
// IF NOT EXISTS/OR IGNORE idiom so that a second Init on an
// already-initialized store is a true no-op rather than an error.
//
// items carries an ordinal INTEGER column. It is required for two things:
// UNIQUE(parent_id, ordinal) below cannot be expressed without it, and
// AddItem's MAX(ordinal)+1 child-ordinal allocation has no column to query
// without it. SQLite treats NULLs as distinct within a unique index, so
// multiple root items (parent_id AND ordinal both NULL) never collide on
// this constraint.
//
// There is no is_blocked column; blocking is computed by the view.
const schemaDDLBase = `

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
    WHERE d.type = 'parent-child' AND bt.depth < $MAXDEPTH$
  )
SELECT i.* FROM items i
LEFT JOIN blocked_tree bt ON bt.item_id = i.id
WHERE i.stage  = 'ticket'
  AND i.status = 'open'
  AND bt.item_id IS NULL
ORDER BY i.score DESC, i.created_at ASC;

`

// metaDDL writes the version row a fresh store opens at. It is the top of
// the ladder, substituted from schemaVersion rather than typed, so a binary
// cannot create stores at a version its own migrations do not agree with.
const metaDDL = `
INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '$SCHEMAVERSION$');
`

// schemaDDL is the three pieces in the order they must run: the tables the
// index reads from, then the index and its triggers, then the version row.
var schemaDDL = expandSchemaSQL(schemaDDLBase + ftsDDL + metaDDL)

// ftsDDL is the full-text index and its three synchronisation triggers,
// held separately because it has two callers that must produce the same
// result: schemaDDL, which gives a brand-new store the index from the start,
// and migrations rung 2, which adds it to a store that already has items in
// it. A second copy of this text could leave a migrated store with a subtly
// different index from a fresh one. TestFreshInitMatchesMigratedV1 compares
// sqlite_master between the two paths and would catch that.
const ftsDDL = `-- The full-text index over the five columns Search matches on. It is an
-- EXTERNAL-CONTENT table (content='items'): FTS5 stores only the index and
-- reads the text back out of items, so a captured roadmap's body is not
-- held twice and cannot drift between two copies.
--
-- tokenize='trigram' rather than the default unicode61, because Search is a
-- substring search. The default tokenizer matches WORDS, so a search for
-- "oken" would not find "token". Trigram indexes every three-character run,
-- which makes MATCH an arbitrary case-insensitive SUBSTRING search: the same
-- thing LIKE '%x%' does, and the same thing the doc comment on
-- SearchFilter.Query promises ("A person searching for 'token expiry' means
-- those characters, and a search syntax is a thing to learn before you can
-- find your own note"). With the pinned driver, infix 'oken' matches, and
-- 'REFRESH TOK' matches lower-case text.
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
    title, why, acceptance, paths, body,
    content='items', content_rowid='rowid', tokenize='trigram'
);

-- External-content FTS5 does not update itself. These three triggers are
-- the whole synchronisation mechanism, and the 'delete' command rows are
-- not optional: without them an updated or deleted item leaves its old text
-- in the index forever, so search keeps returning a title nobody can find
-- in the store any more.
CREATE TRIGGER IF NOT EXISTS items_fts_after_insert AFTER INSERT ON items BEGIN
    INSERT INTO items_fts(rowid, title, why, acceptance, paths, body)
    VALUES (new.rowid, new.title, new.why, new.acceptance, new.paths, new.body);
END;

CREATE TRIGGER IF NOT EXISTS items_fts_after_delete AFTER DELETE ON items BEGIN
    INSERT INTO items_fts(items_fts, rowid, title, why, acceptance, paths, body)
    VALUES ('delete', old.rowid, old.title, old.why, old.acceptance, old.paths, old.body);
END;

CREATE TRIGGER IF NOT EXISTS items_fts_after_update AFTER UPDATE ON items BEGIN
    INSERT INTO items_fts(items_fts, rowid, title, why, acceptance, paths, body)
    VALUES ('delete', old.rowid, old.title, old.why, old.acceptance, old.paths, old.body);
    INSERT INTO items_fts(rowid, title, why, acceptance, paths, body)
    VALUES (new.rowid, new.title, new.why, new.acceptance, new.paths, new.body);
END;
`

// schemaDDLv1 is the version-1 schema, frozen. It is never edited, not
// even to fix a typo in a comment or add an index, because it is the
// starting shape that `migrations` is proven to climb from. Without a frozen
// copy, the "does a migrated old store match a fresh new one" test would
// compare today's DDL against today's DDL and pass while asserting nothing.
var schemaDDLv1 = expandDepthSQL(`
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
    WHERE d.type = 'parent-child' AND bt.depth < $MAXDEPTH$
  )
SELECT i.* FROM items i
LEFT JOIN blocked_tree bt ON bt.item_id = i.id
WHERE i.stage  = 'ticket'
  AND i.status = 'open'
  AND bt.item_id IS NULL
ORDER BY i.score DESC, i.created_at ASC;

INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '1');
`)

// open performs the bare connection setup shared by Open and Init: sql.Open
// through dsn() plus SetMaxOpenConns(1). It is the one place either public
// entry point touches *sql.DB directly, so they cannot drift apart.
//
// One keep process, one connection. Go's database/sql pool
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
// attempt but the last hits the race) adds well under a tenth of a second;
// this is not a substitute for busy_timeout, it covers a narrower window
// busy_timeout does not.
const initDDLMaxAttempts = 8

// initWithDDL is the seam that makes "the whole DDL block is applied in a
// single transaction" a falsifiable claim rather than an unverified comment.
// Init(path) is exactly initWithDDL(path, schemaDDL, schemaVersion); the
// parameters stay unexported so no caller outside this package may choose
// its own schema. They exist only so this package's own tests can feed a
// DDL that fails partway through and assert that nothing survived, and so
// the migration tests can build a genuine version-1 store from the frozen
// schemaDDLv1 without the version guard rejecting it for being exactly what
// they asked for.
//
// wantVersion is what the post-DDL guard checks the resulting store against.
// It is a parameter rather than always schemaVersion for that second reason:
// a v1 store built on purpose is not drift.
func initWithDDL(path string, ddl string, wantVersion int) (*Store, error) {
	s, err := open(path, OpenOptions{})
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	// The very first writer to a brand-new database file must briefly hold
	// an exclusive lock while SQLite bootstraps WAL mode for that file
	// (creating the -wal/-shm sidecars and running wal-index recovery;
	// see sqlite.org/wal.html, "Concurrency"). If two concurrent `keep
	// init` processes race to be that first writer, the loser's BEGIN
	// IMMEDIATE can come back SQLITE_BUSY almost instantly, without
	// waiting out busy_timeout=3000. TestConcurrentInitSameDirectory
	// (cmd/keep) exercises this race, which always surfaces at this
	// BeginTx, well under 3000ms elapsed.
	//
	// The whole DDL block is idempotent (IF NOT EXISTS / OR IGNORE
	// throughout), so retrying it from scratch is always safe: a retry
	// here can never leave a partial or duplicated schema. A handful of
	// near-immediate retries (a few milliseconds of backoff each) covers
	// this bootstrap window without masking any other class of failure:
	// only errors classified as SQLITE_BUSY are retried; every other error
	// (including a "table already exists" failure from non-idempotent DDL)
	// returns immediately on the first attempt.
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
	if err := s.checkSchemaVersionAgainst(ctx, wantVersion); err != nil {
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
// meta.schema_version row. Init does not create path's parent directory:
// internal/store takes a path and does not invent filesystem layout; that is
// the CLI's job.
func Init(path string) (*Store, error) {
	return initWithDDL(path, schemaDDL, schemaVersion)
}

// checkSchemaVersion is the Open-time guard that idempotent DDL cannot
// provide: CREATE TABLE IF NOT EXISTS is a name
// check, not a shape check, and will silently accept a pre-existing table
// with entirely different columns. This is the mechanism that actually
// detects that kind of drift.
//
// A missing meta table or missing schema_version row is also treated as an
// ErrSchemaVersion failure, not a valid empty store: a default read-write
// sql.Open fabricates an empty database file on a path that was never
// initialized, and without this, that would look identical to success.
//
// A mismatch has two outcomes, because they need two different things from
// the person reading the error:
//
//   - OLDER than this binary: the store predates a schema change and there
//     is a ladder that fixes it. ErrNeedsMigration, naming `keep migrate`.
//   - NEWER than this binary: the store was written by a keep that knows
//     something this one does not. ErrSchemaVersion, and nothing this binary
//     can do about it, because migrating downwards would mean discarding
//     data it cannot even name.
func (s *Store) checkSchemaVersion(ctx context.Context) error {
	return s.checkSchemaVersionAgainst(ctx, schemaVersion)
}

// checkSchemaVersionAgainst is checkSchemaVersion with the expected version
// as a parameter, so initWithDDL can build a deliberate v1 store without the
// guard calling it drift. Everything else passes schemaVersion.
func (s *Store) checkSchemaVersionAgainst(ctx context.Context, want int) error {
	got, err := storedSchemaVersion(ctx, s.db)
	if err != nil {
		return err
	}
	switch {
	case got == want:
		return nil
	case got < want:
		return fmt.Errorf("store is at schema_version %d, this keep expects %d -- run `keep migrate` (it takes a backup first): %w",
			got, want, ErrNeedsMigration)
	default:
		return fmt.Errorf("store is at schema_version %d, newer than this keep's %d -- upgrade keep rather than downgrading the store: %w",
			got, want, ErrSchemaVersion)
	}
}
