package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func lockPath(home string) string {
	return filepath.Join(filepath.Dir(PathIn(home)), lockName)
}

// TestWithLockRemovesTheLockFile: a lock left behind after a clean run
// would block every later writer for lockStale (a minute) before anyone
// reclaimed it -- turning a fixed race into a slow one.
func TestWithLockRemovesTheLockFile(t *testing.T) {
	home := t.TempDir()

	ran := false
	if err := withLock(home, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("withLock: %v", err)
	}
	if !ran {
		t.Fatal("withLock did not run fn")
	}
	if _, err := os.Stat(lockPath(home)); !os.IsNotExist(err) {
		t.Errorf("lock file survived a clean run (stat err = %v)", err)
	}
}

// TestWithLockRunsFnEvenWhenTheLockIsHeld is the never-fatal contract at
// this boundary. Register ignores Load's error so a corrupted registry
// cannot make every future `keep init` fail; refusing to register because
// a lock was busy would break the same promise from the other side, trading
// a rare lost update for a routine hard failure.
//
// The fresh lock is deliberately NOT stale, so this exercises the wait-then-
// give-up path rather than the reclaim path.
func TestWithLockRunsFnEvenWhenTheLockIsHeld(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(PathIn(home)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(lockPath(home), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	f.Close()

	start := time.Now()
	ran := false
	if err := withLock(home, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("withLock returned an error while the lock was held: %v", err)
	}
	waited := time.Since(start)

	if !ran {
		t.Error("fn did not run -- a busy lock must degrade to unlocked, never skip the work")
	}
	if waited < lockTimeout {
		t.Errorf("gave up after %s, want at least lockTimeout (%s) of waiting first", waited, lockTimeout)
	}

	// The held lock is someone else's. Proceeding unlocked must not delete
	// it, or the giving-up process would hand the lock away to a third.
	if _, err := os.Stat(lockPath(home)); err != nil {
		t.Errorf("withLock removed a lock it never held: %v", err)
	}
}

// TestAcquireReclaimsAStaleLock: O_EXCL's one weakness against flock is that
// a process dying mid-write leaves the lock forever. Reclaiming after
// lockStale is what keeps that from being permanent.
func TestAcquireReclaimsAStaleLock(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(PathIn(home)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := lockPath(home)
	if err := os.WriteFile(path, []byte("99999\n"), 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	// Backdate well past lockStale -- the holder is, by construction, dead.
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	start := time.Now()
	if !acquire(path) {
		t.Fatal("acquire did not reclaim a stale lock")
	}
	if waited := time.Since(start); waited >= lockTimeout {
		t.Errorf("reclaiming took %s -- it should not have waited out lockTimeout first", waited)
	}
	os.Remove(path)
}

// TestAcquireDoesNotReclaimAFreshLock is the other half of the same
// decision. Reclaiming eagerly would steal the lock from a live writer on a
// loaded machine, turning a lost update into a corrupted one -- strictly
// worse than the bug this all exists to fix.
func TestAcquireDoesNotReclaimAFreshLock(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(PathIn(home)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := lockPath(home)
	if err := os.WriteFile(path, []byte("1\n"), 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	if acquire(path) {
		t.Error("acquire took a lock that was only just written")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the fresh lock was removed anyway: %v", err)
	}
	os.Remove(path)
}

// TestRegisterAndForgetSerialiseThroughTheLock proves the lock is actually
// on the read-modify-write path rather than merely existing. Both mutating
// entry points must take it; Load and Save must not, since a reader that
// blocked on a writer would make `keep repos` fail while an init was in
// flight.
func TestRegisterAndForgetSerialiseThroughTheLock(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(PathIn(home)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Hold the lock, fresh, for the whole of each call below.
	f, err := os.OpenFile(lockPath(home), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	f.Close()
	defer os.Remove(lockPath(home))

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Register", func() error { return Register(home, "/x/db.sqlite", "x", "2026-01-01T00:00:00.000000000Z") }},
		{"Forget", func() error { _, err := Forget(home, "/x/db.sqlite"); return err }},
	} {
		start := time.Now()
		if err := tc.call(); err != nil {
			t.Fatalf("%s while the lock was held: %v", tc.name, err)
		}
		if waited := time.Since(start); waited < lockTimeout {
			t.Errorf("%s returned after %s without waiting on the lock -- it is not taking it", tc.name, waited)
		}
	}

	// Load must NOT block: a read that waited on a writer would make
	// `keep repos` hang whenever an init was in flight.
	start := time.Now()
	if _, err := Load(home); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if waited := time.Since(start); waited >= lockTimeout {
		t.Errorf("Load waited %s on the lock -- reads must not block on writers", waited)
	}
}
