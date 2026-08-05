package store

import (
	"context"
	"database/sql"
	"fmt"

	// Blank import registers the database/sql driver name "sqlite". Every
	// connection in this package (and every test's) goes through dsn() below,
	// which is the single source of DSN truth for the whole codebase.
	_ "modernc.org/sqlite"
)

// readWriteDSNFmt is the one DSN format string in the codebase. Copied
// character-for-character from 01-RESEARCH.md "Verified DSN and driver
// registration". It is built only from this code-level constant and a
// filesystem path — never from user or agent input — because the driver
// does not validate _pragma values; an attacker- or agent-controlled path
// segment could not inject a pragma, but a hand-built DSN elsewhere in the
// codebase could still drift from this one, which is the anti-pattern
// 01-RESEARCH.md calls out explicitly.
const readWriteDSNFmt = "file:%s?_pragma=busy_timeout(3000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"

// OpenOptions controls how Open connects to a store. Phase 1 only ever
// passes the zero value. GSD Phase 6 (REPO-04) is the first caller that will
// set ReadOnly. The DSN's immediate transaction locking (see
// readWriteDSNFmt below) is deliberately retained for read-only opens:
// 01-RESEARCH.md verified that a bare BEGIN under mode=ro does not error —
// it is the write statement inside the transaction that correctly fails with
// SQLITE_READONLY. That means dsn() needs no read/write special-casing
// beyond appending mode=ro.
type OpenOptions struct {
	ReadOnly bool
}

// dsn builds the single DSN string used by every connection this package
// opens, production or test. A second, hand-built DSN anywhere else in the
// codebase is the anti-pattern 01-RESEARCH.md warns against: it would be a
// second source of truth for the pragma set that could silently drift from
// this one.
func dsn(path string, opts OpenOptions) string {
	d := fmt.Sprintf(readWriteDSNFmt, path)
	if opts.ReadOnly {
		d += "&mode=ro"
	}
	return d
}

// Store is the SQLite persistence layer. It exposes no *sql.DB, no
// *sql.Rows, and no query string above the package boundary (STORE-10); db
// stays unexported with no accessor.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path and returns a *Store
// wrapping it. It does not read schema_version here — the meta table does
// not exist until plan 02 creates it; plan 02 adds that guard to this
// function.
func Open(path string, opts OpenOptions) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path, opts))
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	// STORE-09: one rounce process, one connection. Go's database/sql pool
	// defaults assume a network database with many concurrent backends;
	// SQLite is embedded and single-writer, so the pool must never be
	// allowed to open a second physical connection in production.
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}

// Close closes the underlying connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// withTx runs fn inside a transaction, committing if fn returns nil and
// rolling back otherwise. The DSN's transaction-locking mode is a
// connection-level setting (see dsn/readWriteDSNFmt above), so every
// db.BeginTx call on this *sql.DB already begins IMMEDIATE — this helper
// does not pass driver-specific TxOptions to "ask for" that, since doing so
// would be redundant and a place a future contributor could get wrong.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
