package registry

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// addItem is this package's own minimal fixture helper -- internal/store's
// testhelp_test.go helpers are unexported to that package and cannot be
// reused here. It always fails the test on error, matching that package's
// mustAdd shape.
func addItem(t *testing.T, st *store.Store, in store.AddItemInput) model.Item {
	t.Helper()
	if in.Actor == "" {
		in.Actor = "tester"
	}
	item, err := st.AddItem(context.Background(), in)
	if err != nil {
		t.Fatalf("addItem(%+v): %v", in, err)
	}
	return item
}

// ---- classify ----

// TestClassify covers all six behaviours required by task 1: the three
// REPO-05 failure modes classify distinctly (and pairwise differently), and
// a clean stat leaves the deeper failure to store.Open.
func TestClassify(t *testing.T) {
	t.Run("missing file, parent directory exists", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db.sqlite")

		err := classify(path)
		if err == nil {
			t.Fatalf("classify(missing file): err = nil, want non-nil")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("classify(missing file): err = %v, want errors.Is(fs.ErrNotExist)", err)
		}
	})

	t.Run("missing parent directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "does-not-exist", "db.sqlite")

		err := classify(path)
		if err == nil {
			t.Fatalf("classify(missing parent dir): err = nil, want non-nil")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("classify(missing parent dir): err = %v, want errors.Is(fs.ErrNotExist)", err)
		}
	})

	t.Run("chmod 000 unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: chmod 000 does not restrict root's own reads, case is unreachable")
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "db.sqlite")
		st, err := store.Init(path)
		if err != nil {
			t.Fatalf("store.Init: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatalf("chmod 000: %v", err)
		}
		t.Cleanup(func() { os.Chmod(path, 0o600) }) // so t.TempDir() can remove it

		err = classify(path)
		if err == nil {
			t.Fatalf("classify(chmod 000): err = nil, want non-nil")
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("classify(chmod 000): err = %v, want errors.Is(fs.ErrPermission)", err)
		}
	})

	t.Run("three messages are pairwise different", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: chmod 000 does not restrict root's own reads, case is unreachable")
		}

		dir := t.TempDir()

		missingPath := filepath.Join(dir, "missing", "db.sqlite")
		missingParentPath := filepath.Join(dir, "gone", "also-gone", "db.sqlite")

		unreadablePath := filepath.Join(dir, "unreadable.sqlite")
		st, err := store.Init(unreadablePath)
		if err != nil {
			t.Fatalf("store.Init: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := os.Chmod(unreadablePath, 0o000); err != nil {
			t.Fatalf("chmod 000: %v", err)
		}
		t.Cleanup(func() { os.Chmod(unreadablePath, 0o600) })

		msg1 := classify(missingPath).Error()
		msg2 := classify(missingParentPath).Error()
		msg3 := classify(unreadablePath).Error()

		// Plain string inequality alone is NOT sufficient here: missingPath
		// and missingParentPath are necessarily different literal paths (one
		// has an extra non-existent directory component), so msg1 != msg2
		// would hold trivially even if classify used one single generic
		// "store at %s: %w" template for every failure mode -- verified
		// empirically against GATE-03 break 2 below, where collapsing
		// classify's three branches into that one generic template left a
		// bare pairwise-inequality check passing. What must actually be
		// tested is that the WORDING is categorised: the two fs.ErrNotExist
		// cases (missing file, missing parent directory -- REPO-05's
		// "deleted or moved", indistinguishable from each other by this
		// classifier by design) share the same category phrase, and the
		// fs.ErrPermission case ("not readable") uses a different one.
		if !strings.Contains(msg1, "deleted or moved") {
			t.Errorf("classify(missing file) = %q, want it to contain %q", msg1, "deleted or moved")
		}
		if !strings.Contains(msg2, "deleted or moved") {
			t.Errorf("classify(missing parent dir) = %q, want it to contain %q", msg2, "deleted or moved")
		}
		if !strings.Contains(msg3, "not readable") {
			t.Errorf("classify(unreadable) = %q, want it to contain %q", msg3, "not readable")
		}
		if strings.Contains(msg3, "deleted or moved") {
			t.Errorf("classify(unreadable) = %q, must NOT contain the not-found wording %q", msg3, "deleted or moved")
		}
		if strings.Contains(msg1, "not readable") || strings.Contains(msg2, "not readable") {
			t.Errorf("classify(not-found case) must NOT contain the unreadable wording %q: msg1=%q msg2=%q", "not readable", msg1, msg2)
		}

		// Also assert plain pairwise inequality, since it IS still a real
		// (if weaker) requirement even though it alone cannot catch the
		// collapse above.
		if msg1 == msg2 {
			t.Errorf("classify(missing file) and classify(missing parent dir) produced the SAME message: %q", msg1)
		}
		if msg1 == msg3 {
			t.Errorf("classify(missing file) and classify(unreadable) produced the SAME message: %q", msg1)
		}
		if msg2 == msg3 {
			t.Errorf("classify(missing parent dir) and classify(unreadable) produced the SAME message: %q", msg2)
		}
	})

	t.Run("clean stat returns nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db.sqlite")
		st, err := store.Init(path)
		if err != nil {
			t.Fatalf("store.Init: %v", err)
		}
		t.Cleanup(func() { st.Close() })

		if err := classify(path); err != nil {
			t.Fatalf("classify(real file): err = %v, want nil", err)
		}
	})
}

// ---- withStore ----

// TestWithStoreRealStoreRunsCallback proves withStore on a real, initialised
// store runs its callback and returns the callback's own error.
func TestWithStoreRealStoreRunsCallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")
	init, err := store.Init(path)
	if err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	if err := init.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ran := false
	err = withStore(path, func(st *store.Store) error {
		ran = true
		_, err := st.Ready(context.Background(), 0)
		return err
	})
	if !ran {
		t.Fatalf("withStore: callback never ran")
	}
	if err != nil {
		t.Fatalf("withStore: err = %v, want nil", err)
	}
}

// TestWithStoreMissingStoreNeverOpens proves withStore on a missing store
// returns classify's error and never reaches store.Open -- observed
// indirectly via the empty-store trap: no file appears at path.
func TestWithStoreMissingStoreNeverOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")

	ran := false
	err := withStore(path, func(st *store.Store) error {
		ran = true
		return nil
	})
	if ran {
		t.Fatalf("withStore: callback ran on a missing store, want it never reached")
	}
	if err == nil {
		t.Fatalf("withStore: err = nil, want non-nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("withStore: err = %v, want errors.Is(fs.ErrNotExist)", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("withStore: a file appeared at %s after a missing-store open attempt", path)
	}
}
