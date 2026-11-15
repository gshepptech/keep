package store

import (
	"context"
	"database/sql"
	"fmt"
)

// LastActivity returns the timestamp of the most recent events row in this
// store, and whether there is one at all.
//
// It reads MAX(at) FROM events, NOT items' own last-write column. CreateDep (deps.go) writes
// exactly one events row and zero UPDATE statements against items --
// confirmed by reading the method, and locked by activity_test.go's
// dep-only fixture. A repository whose most recent work was `keep dep <a>
// blocks <b>` would therefore show a stale last-activity if this query read
// items' own last-write column instead. The events table is already the
// complete, append-only log of every mutation in the system;
// deriving "last activity" from anything else re-derives a strict subset of
// what events already guarantees.
//
// An empty events table is not an error: SQLite's MAX over zero rows
// returns one row containing NULL, which scans into a sql.NullString whose
// Valid is false. That is reported as ("", false, nil), so a freshly
// initialised store renders as "no activity yet" rather than as a failed
// row in `keep repos`.
func (s *Store) LastActivity(ctx context.Context) (string, bool, error) {
	var ns sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(at) FROM events`).Scan(&ns); err != nil {
		return "", false, fmt.Errorf("LastActivity: %w", err)
	}
	return ns.String, ns.Valid, nil
}
