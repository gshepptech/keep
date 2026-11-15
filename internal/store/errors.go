package store

import "errors"

// Classification rule: SQLite-level failures are classified
// with errors.As(err, &sqliteErr) against *sqlite.Error, then comparing
// sqliteErr.Code() against the exported constants in modernc.org/sqlite/lib
// (e.g. sqlite3.SQLITE_BUSY, sqlite3.SQLITE_CONSTRAINT_UNIQUE), never by
// matching substrings of err.Error(). Driver message text is not a stable
// contract and can change across driver versions without notice; the error
// code is. New classifications must follow this rule rather than reaching
// for strings.Contains.
var (
	// ErrNotFound is returned when a requested item, dep, or other row does
	// not exist.
	ErrNotFound = errors.New("not found")

	// ErrCycle is returned when a dependency edge would introduce a cycle.
	ErrCycle = errors.New("dependency cycle")

	// ErrInvalidTransition is returned when a requested status/stage change
	// is not a legal transition.
	ErrInvalidTransition = errors.New("invalid state transition")

	// ErrConflict is returned for constraint violations and lock contention
	// that the caller should treat as a conflicting concurrent write.
	ErrConflict = errors.New("conflict")

	// ErrTooDeep is returned when a parent-child edge would put an item
	// deeper than MaxDepth. Every recursive query in this package stops
	// extending at that bound and returns a SHORTER answer rather than an
	// error when it is hit, so the bound is enforced at write time instead --
	// see depth.go.
	ErrTooDeep = errors.New("tree too deep")

	// ErrNoSession is returned when a CLAIM is attempted without a session
	// id. Every other verb may leave one empty -- the session on an `add` or
	// a `close` event is attribution, and a missing one costs nothing but
	// detail.
	//
	// A claim is different: it is an assertion that one specific worker
	// holds this item, and three separate rules read that id back to decide
	// what happens next. With an empty session all three collapse, because
	// "" equals "", and every anonymous worker becomes the same worker:
	//
	//   - StartItem's idempotent-retry arm treats a SECOND worker's claim on
	//     a held item as the first one retrying. Silent success, no event,
	//     no ErrConflict -- two workers each believing they own it, which is
	//     the exact collision `start` exists to prevent.
	//   - UnclaimItem's RequireOwnClaim guard, which is the whole reason
	//     `unclaim` is safe to expose over MCP, passes for anybody.
	//   - `stale` reports a claim it cannot attribute to anyone.
	//
	// Without this check, two StartItem calls with different actors and no
	// session both succeed, and the second actor can then release the
	// first's claim through the own-claim-only path.
	//
	// This is refused in the store rather than in the callers because all
	// three rules live here. cmd/keep and internal/dash each derive a
	// non-empty id appropriate to what they are -- see resolveClaimSession
	// and the board's own session -- so nothing legitimate is refused.
	ErrNoSession = errors.New("a claim needs a session id")

	// ErrEmptyReason is returned by close, drop, unclaim and reopen when no
	// reason is given. All four write a terminal or near-terminal fact about
	// a piece of work, and a record that says it happened without saying why
	// is the thing the `--reason` requirement exists to prevent.
	//
	// It is enforced HERE rather than in each caller, because a rule enforced in one surface (the CLI) and not the
	// others (MCP, the dashboard) ends up as three surfaces with three
	// answers to one rule. Surfaces are thin wrappers and carry no behaviour
	// of their own.
	ErrEmptyReason = errors.New("a reason is required")

	// ErrInvalidType and ErrInvalidStage are returned when a value outside
	// the closed vocabulary reaches the store.
	//
	// The schema's CHECK constraints already refused these, and that is not
	// the same thing: a CHECK produces a raw driver message ("constraint
	// failed: CHECK constraint failed: type IN ('roadmap', 'epic', 'task',
	// 'bug', 'note') (275)") which cmd/keep never sees because it
	// pre-validates, and which the MCP surface therefore returned verbatim
	// as a generic protocol error carrying SQLite's own text. An agent
	// reading that cannot tell a bad argument from a broken store. Named
	// here, both surfaces get an error that says which value was wrong and
	// what the legal ones are.
	ErrInvalidType  = errors.New("invalid item type")
	ErrInvalidStage = errors.New("invalid item stage")

	// ErrStoreNotEmpty is returned by Import when the target store already
	// holds items. Importing into a populated store would mean resolving
	// collisions between two histories, which is a sync product -- see
	// Import's doc comment.
	ErrStoreNotEmpty = errors.New("store is not empty")

	// ErrBadExport is returned by Import when the input is not a keep
	// export this binary can read: a missing header, an unreadable line, or
	// a record kind a newer keep wrote and this one would silently drop.
	ErrBadExport = errors.New("not a readable keep export")

	// ErrNeedsMigration is returned by Open when the store is at an OLDER
	// schema_version than this binary expects. It is deliberately distinct
	// from ErrSchemaVersion: this one has a fix (`keep migrate`), and a
	// caller that cannot tell the two apart can only report both as fatal.
	ErrNeedsMigration = errors.New("store needs migration")

	// ErrSchemaVersion is returned by Open when the store's on-disk
	// schema_version does not match the value this binary expects.
	ErrSchemaVersion = errors.New("schema version mismatch")
)
