package store

import "errors"

// Classification rule (01-RESEARCH.md "Extracting SQLITE_BUSY / constraint
// violations without string-matching"): SQLite-level failures are classified
// with errors.As(err, &sqliteErr) against *sqlite.Error, then comparing
// sqliteErr.Code() against the exported constants in modernc.org/sqlite/lib
// (e.g. sqlite3.SQLITE_BUSY, sqlite3.SQLITE_CONSTRAINT_UNIQUE) — never by
// matching substrings of err.Error(). Driver message text is not a stable
// contract and can change across driver versions without notice; the error
// code is. Phase 2 is the phase that will actually need ErrConflict and must
// follow this rule rather than reaching for strings.Contains.
var (
	// ErrNotFound is returned when a requested item, dep, or other row does
	// not exist. First produced by GSD Phase 2 (item/dep lookups).
	ErrNotFound = errors.New("not found")

	// ErrCycle is returned when a dependency edge would introduce a cycle.
	// First produced by GSD Phase 2 (dep creation).
	ErrCycle = errors.New("dependency cycle")

	// ErrInvalidTransition is returned when a requested status/stage change
	// is not a legal transition. First produced by GSD Phase 2 (item mutation).
	ErrInvalidTransition = errors.New("invalid state transition")

	// ErrConflict is returned for constraint violations and lock contention
	// that the caller should treat as a conflicting concurrent write. First
	// produced by GSD Phase 2 (concurrent mutation).
	ErrConflict = errors.New("conflict")

	// ErrSchemaVersion is returned by Open when the store's on-disk
	// schema_version does not match the value this binary expects. First
	// produced by this phase (plan 02's Open-time guard).
	ErrSchemaVersion = errors.New("schema version mismatch")
)
