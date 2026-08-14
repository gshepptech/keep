package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockName is the registry lock, alongside repos.json in ~/.rounce.
const lockName = FileName + ".lock"

// These bound how long a caller waits and how long a lock is believed.
//
// lockTimeout matches the store's own busy_timeout(3000): the two are the
// same promise about the same situation -- another rounce process is
// mid-write, wait a moment rather than failing -- and picking a different
// number here would mean two different answers to "how long is contention
// normal for".
//
// lockStale is deliberately much longer. The window it protects is a read,
// a small JSON edit, and a rename; a process that has held the lock for a
// minute has not been slow, it has died. Reclaiming sooner would risk
// stealing the lock from a live writer on a loaded machine, which turns a
// lost update into a corrupted one.
const (
	lockTimeout = 3 * time.Second
	lockPoll    = 5 * time.Millisecond
	lockStale   = 60 * time.Second
)

// withLock runs fn holding an exclusive, cross-process lock on the
// registry.
//
// # Why a lock file rather than atomic writes
//
// Save already writes a temp file and renames it, which POSIX guarantees is
// atomic -- a reader sees the old file or the new one, never a torn one.
// That was enough for the guarantee Phase 6 made, and not enough for the
// one it did not: atomicity of the write says nothing about isolation of
// the read-modify-write sequence around it. Two processes registering two
// different repositories, each having read before the other's rename
// landed, produce one rename that discards the other's addition. Measured
// in 06-03-SUMMARY.md at 1-of-6 survivors, 10 runs out of 10.
//
// The fix has to cover Load and Save together, which no property of a
// single write can provide.
//
// # Why O_EXCL rather than flock
//
// syscall.Flock is the textbook answer and is not portable: it does not
// exist on Windows, so it would mean a build-tagged file per platform for
// a lock held across three filesystem operations. O_CREATE|O_EXCL is
// atomic on every filesystem rounce can plausibly run on, needs no build
// tags, and adds no dependency -- and the failure mode it does have (a
// process dying while holding the lock) is one flock does not have but
// which is handled below by reclaiming a lock that has gone stale.
//
// # Why failing to lock is not fatal
//
// The registry's whole contract is never-fatal: Load hands back a usable
// empty envelope on any error rather than refusing, and Register ignores
// Load's error deliberately so a corrupted registry cannot make every
// future `rounce init` fail. Refusing to register because a lock could not
// be taken would break exactly that promise, and would trade a rare lost
// update for a routine hard failure. So a lock that cannot be acquired
// within lockTimeout proceeds anyway, unlocked, which is precisely the
// behaviour that shipped in v1.0 -- no worse than before, and only for the
// runs that could not get in.
func withLock(home string, fn func() error) error {
	path := filepath.Join(filepath.Dir(PathIn(home)), lockName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("registry: mkdir %s: %w", filepath.Dir(path), err)
	}

	held := acquire(path)
	if held {
		defer os.Remove(path)
	}
	return fn()
}

// acquire tries to take the lock until lockTimeout elapses, reclaiming it
// once if the holder looks dead. It reports whether the lock is held --
// never an error, because not holding it is a degraded mode rather than a
// failure (see withLock).
func acquire(path string) bool {
	deadline := time.Now().Add(lockTimeout)
	reclaimed := false

	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			// The pid is for a human reading the directory after something
			// went wrong. Nothing reads it back: a pid from a dead process
			// can be reused by a live one, so trusting it would be worse
			// than the mtime check below.
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return true
		}
		if !errors.Is(err, os.ErrExist) {
			// A lock that cannot be created for any other reason (a
			// read-only home, a missing directory that MkdirAll could not
			// make) is not worth retrying for three seconds.
			return false
		}

		// Reclaim at most once per call. A second reclaim would mean two
		// processes are taking turns stealing from each other, which is
		// worse than one of them giving up and proceeding unlocked.
		if !reclaimed && staleSince(path, lockStale) {
			os.Remove(path)
			reclaimed = true
			continue
		}

		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(lockPoll)
	}
}

// staleSince reports whether path exists and has not been modified for at
// least d. A lock file that cannot be stat-ed is not treated as stale: it
// most likely just vanished because the holder released it, and the next
// O_EXCL attempt will say so definitively.
func staleSince(path string, d time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) >= d
}
