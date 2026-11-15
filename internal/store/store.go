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

// readWriteDSNFmt is the one DSN format string in the codebase. It is
// built only from this code-level constant and a
// filesystem path, never from user or agent input, because the driver
// does not validate _pragma values; an attacker- or agent-controlled path
// segment could not inject a pragma, but a hand-built DSN elsewhere in the
// codebase could still drift from this one.
const readWriteDSNFmt = "file:%s?_pragma=busy_timeout(3000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"

// OpenOptions controls how Open connects to a store. ReadOnly is used by
// the dashboard and by cross-repository reads. The DSN's immediate
// transaction locking (see readWriteDSNFmt below) is deliberately retained
// for read-only opens: a bare BEGIN under mode=ro does not error;
// it is the write statement inside the transaction that correctly fails with
// SQLITE_READONLY. That means dsn() needs no read/write special-casing
// beyond appending mode=ro.
type OpenOptions struct {
	ReadOnly bool
}

// dsn builds the single DSN string used by every connection this package
// opens, production or test. A second, hand-built DSN anywhere else in the
// codebase would be a
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
// *sql.Rows, and no query string above the package boundary; db
// stays unexported with no accessor.
type Store struct {
	db *sql.DB
}

// Open opens an existing store at path and returns a *Store wrapping it,
// after confirming its on-disk schema_version matches this binary's
// schemaVersion constant (schema.go). This check exists alongside Init's
// idempotent DDL, not instead of it: CREATE TABLE IF NOT EXISTS is a name
// check, not a shape check -- it will silently
// accept a pre-existing table with entirely different columns. The DDL
// gives Init its idempotency; only this check gives drift detection.
// Someone will eventually try to delete one of the two as redundant, and
// this comment is what should stop them.
//
// Open shares its connection setup with Init via the unexported open()
// helper (schema.go) so the two entry points cannot drift apart. A caller
// that receives an error here is not handed a *Store it could still use,
// and the underlying *sql.DB is closed before returning -- no leaked
// connection on the failure path.
func Open(path string, opts OpenOptions) (*Store, error) {
	s, err := open(path, opts)
	if err != nil {
		return nil, err
	}
	if err := s.checkSchemaVersion(context.Background()); err != nil {
		s.db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// withTx runs fn inside a transaction, committing if fn returns nil and
// rolling back otherwise. The DSN's transaction-locking mode is a
// connection-level setting (see dsn/readWriteDSNFmt above), so every
// db.BeginTx call on this *sql.DB already begins IMMEDIATE, so this helper
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
