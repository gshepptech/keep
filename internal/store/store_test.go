package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestPragmasApplyToEverySecondConnection proves STORE-06: every pragma in
// the DSN is connection-scoped and applies to every physical connection the
// pool opens, not just the first. It reaches the production DSN builder
// (dsn) rather than hand-building a DSN string.
func TestPragmasApplyToEverySecondConnection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pragmas.sqlite")

	db, err := sql.Open("sqlite", dsn(path, OpenOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Test-only override: production Open() never raises its cap above 1
	// (STORE-09). Raising it here to 2 is the only way to force the pool to
	// open a genuinely distinct second physical connection, which is the
	// only way to exercise the property STORE-06 asks for. This does not
	// contradict STORE-09 — that property is asserted separately, on the
	// production Open() path, by TestOpenCapsMaxOpenConns below.
	db.SetMaxOpenConns(2)

	// Holding conn1 open while requesting conn2 is what forces the pool to
	// open a second, distinct physical connection instead of reusing one.
	conn1, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()

	conn2, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	if got := db.Stats().OpenConnections; got != 2 {
		t.Fatalf("OpenConnections = %d, want 2 (a future refactor that silently reuses one connection would make this test vacuous rather than passing)", got)
	}

	assertPragmas(t, ctx, conn1, "conn1")
	assertPragmas(t, ctx, conn2, "conn2")
}

func assertPragmas(t *testing.T, ctx context.Context, conn *sql.Conn, name string) {
	t.Helper()

	var foreignKeys int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("%s: PRAGMA foreign_keys: %v", name, err)
	}
	if foreignKeys != 1 {
		t.Errorf("%s: foreign_keys = %d, want 1", name, foreignKeys)
	}

	var journalMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("%s: PRAGMA journal_mode: %v", name, err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("%s: journal_mode = %q, want wal", name, journalMode)
	}

	var busyTimeout int
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("%s: PRAGMA busy_timeout: %v", name, err)
	}
	if busyTimeout != 3000 {
		t.Errorf("%s: busy_timeout = %d, want 3000", name, busyTimeout)
	}
}

// TestTxlockImmediateHonored proves STORE-07/STORE-08: a bare BEGIN on a
// rounce connection (connA, production DSN via dsn()) already holds the
// write lock before any statement runs, so a second connection (connB, DSN
// built by replacing busy_timeout(3000) with busy_timeout(0) so it retries
// zero times) fails immediately rather than after busy_timeout. Adapted from
// 01-RESEARCH.md "STORE-08: the actual, run, deterministic _txlock=immediate
// proof": dsnA now comes from the production dsn() function instead of a
// literal, and dsnB is derived from it by a single string replacement. No
// time.Sleep, no time.Since, no "should be fast" comment: the result is
// pass/fail on the first line, with no clock involved (Pitfall 2).
func TestTxlockImmediateHonored(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.sqlite")

	dsnA := dsn(path, OpenOptions{})
	dbA, err := sql.Open("sqlite", dsnA)
	if err != nil {
		t.Fatal(err)
	}
	defer dbA.Close()
	dbA.SetMaxOpenConns(1)
	if _, err := dbA.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}

	dsnB := strings.Replace(dsnA, "busy_timeout(3000)", "busy_timeout(0)", 1)
	if dsnB == dsnA {
		t.Fatal("replaceBusyTimeout: fragment busy_timeout(3000) not found in dsnA")
	}
	dbB, err := sql.Open("sqlite", dsnB)
	if err != nil {
		t.Fatal(err)
	}
	defer dbB.Close()
	dbB.SetMaxOpenConns(1)

	// Step 1: connA begins a transaction and issues NO statement at all. If
	// immediate transaction locking is honored, this bare BEGIN already
	// holds the write (RESERVED) lock.
	txA, err := dbA.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Step 2: connB, with busy_timeout=0, must fail *now* if the lock is
	// really held -- there is no window in which this could pass by luck.
	txB, err := dbB.BeginTx(ctx, nil)
	if err == nil {
		_, err = txB.Exec("INSERT INTO t (v) VALUES ('from-b')")
	}
	if err == nil {
		t.Fatal("immediate transaction locking NOT honored: connB wrote while connA held an open transaction")
	}

	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("expected *sqlite.Error, got %T: %v", err, err)
	}
	if sqliteErr.Code() != sqlite3.SQLITE_BUSY {
		t.Fatalf("expected SQLITE_BUSY, got code %d: %v", sqliteErr.Code(), err)
	}
	t.Logf("connB correctly failed with SQLITE_BUSY while connA held the lock: %v", err)

	// Step 3: release, and prove this isn't a permanent/false failure --
	// connB must now succeed immediately.
	if err := txA.Rollback(); err != nil {
		t.Fatal(err)
	}
	txB2, err := dbB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txB2.Exec("INSERT INTO t (v) VALUES ('after-release')"); err != nil {
		t.Fatal(err)
	}
	if err := txB2.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenCapsMaxOpenConns proves STORE-09 on the production path: a *Store
// built by the real Open (not a *sql.DB the test built itself) reports
// MaxOpenConnections == 1. Kept as its own three-line test rather than an
// assertion bolted onto TestPragmasApplyToEverySecondConnection, since that
// test deliberately raises its own cap to 2 for its own purposes and so can
// never be the thing that proves production stays at 1.
func TestOpenCapsMaxOpenConns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caps.sqlite")
	// Open now enforces the schema_version guard (plan 02), so this must
	// go through Init first rather than Open on a never-initialized path.
	if s, err := Init(path); err != nil {
		t.Fatal(err)
	} else if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}
}

// TestWithTxCommitsAndRollsBack proves withTx commits fn's writes when fn
// returns nil and discards every one of them when fn returns an error. Both
// halves are required: an assertion that only checks the returned error
// would pass against a helper that surfaces fn's error but commits anyway,
// which is the exact bug this test exists to catch.
func TestWithTxCommitsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tx.sqlite")

	// Open now enforces the schema_version guard (plan 02), so this must
	// go through Init first rather than Open on a never-initialized path.
	s, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.db.ExecContext(ctx, "CREATE TABLE tx_canary (n INTEGER)"); err != nil {
		t.Fatal(err)
	}

	// Commit half.
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO tx_canary (n) VALUES (1)")
		return err
	})
	if err != nil {
		t.Fatalf("commit half: withTx returned unexpected error: %v", err)
	}
	assertCanaryCount(t, ctx, s, 1, 1)

	// Rollback half.
	sentinel := errors.New("rollback-half sentinel")
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, execErr := tx.ExecContext(ctx, "INSERT INTO tx_canary (n) VALUES (2)"); execErr != nil {
			return execErr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback half: withTx error = %v, want errors.Is(err, sentinel)", err)
	}
	assertCanaryCount(t, ctx, s, 2, 0)
}

// TestReadOnlyOpenServesReadsAndRefusesWrites proves the dashboard's whole
// mechanism (05-CONTEXT.md decision 4): OpenOptions{ReadOnly: true} coexists
// with this DSN's _txlock=immediate, serves every read method the dashboard
// needs, and refuses a write. This is the first test in the codebase that
// uses ReadOnly: true -- OpenOptions has carried the field unused since
// Phase 1.
//
// Confirmed live against this repository during 05 planning:
//
//	RO open err=<nil>
//	RO Ready err=<nil> n=1
//	RO Backlog err=<nil> n=0
//	RO GetItem err=<nil>
//	RO Blocked err=<nil> n=0
//	RO ListItems err=<nil> n=0
//	RO AddItem err=attempt to write a readonly database (8)
//
// The write's failure is asserted by err != nil plus an unchanged
// len(ListItems(...)) -- never strings.Contains(err.Error(), "readonly").
// Driver message text is not a contract (errors.go's standing
// classification rule). The extended code (8, SQLITE_READONLY) is recorded
// above for the SUMMARY's benefit only, not asserted on here.
func TestReadOnlyOpenServesReadsAndRefusesWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "readonly.sqlite")

	rw, err := Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	seed := mustAdd(t, rw, AddItemInput{Title: "Seed ticket", Actor: "tester"})
	if err := rw.Close(); err != nil {
		t.Fatalf("Close read-write handle: %v", err)
	}

	ro, err := Open(path, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("Open(ReadOnly: true): %v", err)
	}
	defer ro.Close()

	if _, err := ro.Ready(ctx, 0); err != nil {
		t.Errorf("read-only Ready: %v", err)
	}
	if _, err := ro.Backlog(ctx, ""); err != nil {
		t.Errorf("read-only Backlog: %v", err)
	}
	if _, err := ro.Blocked(ctx); err != nil {
		t.Errorf("read-only Blocked: %v", err)
	}
	before, err := ro.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Errorf("read-only ListItems: %v", err)
	}
	if _, err := ro.GetItem(ctx, seed.ID); err != nil {
		t.Errorf("read-only GetItem: %v", err)
	}
	if _, err := ro.Tree(ctx, seed.ID); err != nil {
		t.Errorf("read-only Tree: %v", err)
	}

	if _, err := ro.AddItem(ctx, AddItemInput{Title: "Should not be written", Actor: "tester"}); err == nil {
		t.Fatal("read-only AddItem: got nil error, want a write failure")
	} else {
		t.Logf("read-only AddItem correctly failed: %v", err)
	}

	after, err := ro.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("read-only ListItems (after): %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("item count changed after a refused write: before=%d after=%d", len(before), len(after))
	}
}

func assertCanaryCount(t *testing.T, ctx context.Context, s *Store, n, wantCount int) {
	t.Helper()
	var got int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tx_canary WHERE n = ?", n).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != wantCount {
		t.Fatalf("COUNT(*) WHERE n = %d = %d, want %d", n, got, wantCount)
	}
}
