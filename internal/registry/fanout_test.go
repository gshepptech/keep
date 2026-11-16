package registry

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// addItem is this package's own minimal fixture helper, because
// internal/store's testhelp_test.go helpers are unexported to that package.
// It always fails the test on error, matching that package's mustAdd shape.
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

// TestClassify covers all six behaviours of classify: the three
// unreadable-store failure modes classify distinctly (and pairwise
// differently), and a clean stat leaves the deeper failure to store.Open.
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
		// Windows does not implement Unix permission bits, so os.Chmod
		// cannot make a file unreadable there and the case this asserts
		// does not exist. Same shape as the root skip above: the mechanism
		// is absent, not the behaviour wrong.
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no Unix permission bits for chmod 000 to clear")
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
		// Windows does not implement Unix permission bits, so os.Chmod
		// cannot make a file unreadable there and the case this asserts
		// does not exist. Same shape as the root skip above: the mechanism
		// is absent, not the behaviour wrong.
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no Unix permission bits for chmod 000 to clear")
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
		// "store at %s: %w" template for every failure mode. What must
		// actually be tested is that the WORDING is categorised: the two
		// fs.ErrNotExist cases (missing file, missing parent directory --
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

// TestWithStoreMissingStoreNeverOpens checks that withStore on a missing
// store returns classify's error and never reaches store.Open. That is
// observed through the empty-store trap: no file appears at path.
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

// ---- FetchReady ----

// TestFetchReadyOrderAndContent proves FetchReady over three registered
// repos returns exactly three results, in input order, each carrying its
// Repo name and Path, with the real Ready() rows attached.
func TestFetchReadyOrderAndContent(t *testing.T) {
	repos := make([]Repo, 3)
	for i, name := range []string{"alpha", "beta", "gamma"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "db.sqlite")
		st, err := store.Init(path)
		if err != nil {
			t.Fatalf("store.Init: %v", err)
		}
		addItem(t, st, store.AddItemInput{Title: name + "-item"})
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		repos[i] = Repo{Path: path, Name: name}
	}

	results := FetchReady(context.Background(), repos)
	if len(results) != 3 {
		t.Fatalf("FetchReady: len = %d, want 3", len(results))
	}
	for i, want := range repos {
		got := results[i]
		if got.Repo != want.Name || got.Path != want.Path {
			t.Fatalf("FetchReady[%d]: Repo/Path = %q/%q, want %q/%q", i, got.Repo, got.Path, want.Name, want.Path)
		}
		if got.Err != nil {
			t.Fatalf("FetchReady[%d]: Err = %v, want nil", i, got.Err)
		}
		if len(got.Items) != 1 || got.Items[0].Title != want.Name+"-item" {
			t.Fatalf("FetchReady[%d]: Items = %+v, want one item titled %q", i, got.Items, want.Name+"-item")
		}
	}
}

// TestFetchReadyIsolatesFailures proves that with the middle repo's store
// file deleted, the first and third results still carry their items with a
// nil Err, the middle result carries a non-nil Err and no items, and the
// length and order are unchanged. A fan-out that returned early or skipped
// the bad row would return a silent partial result.
func TestFetchReadyIsolatesFailures(t *testing.T) {
	repos := make([]Repo, 3)
	for i, name := range []string{"alpha", "beta", "gamma"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "db.sqlite")
		st, err := store.Init(path)
		if err != nil {
			t.Fatalf("store.Init: %v", err)
		}
		addItem(t, st, store.AddItemInput{Title: name + "-item"})
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		repos[i] = Repo{Path: path, Name: name}
	}

	if err := os.Remove(repos[1].Path); err != nil {
		t.Fatalf("os.Remove(middle store): %v", err)
	}

	results := FetchReady(context.Background(), repos)
	if len(results) != 3 {
		t.Fatalf("FetchReady: len = %d, want 3 (isolation failed)", len(results))
	}

	if results[0].Err != nil || len(results[0].Items) != 1 {
		t.Fatalf("FetchReady[0] (alpha): Err=%v Items=%+v, want nil err and one item", results[0].Err, results[0].Items)
	}
	if results[0].Repo != "alpha" {
		t.Fatalf("FetchReady[0]: Repo = %q, want alpha (order changed)", results[0].Repo)
	}

	if results[1].Err == nil {
		t.Fatalf("FetchReady[1] (beta, deleted): Err = nil, want non-nil")
	}
	if len(results[1].Items) != 0 {
		t.Fatalf("FetchReady[1] (beta, deleted): Items = %+v, want none", results[1].Items)
	}
	if results[1].Repo != "beta" {
		t.Fatalf("FetchReady[1]: Repo = %q, want beta (order changed)", results[1].Repo)
	}

	if results[2].Err != nil || len(results[2].Items) != 1 {
		t.Fatalf("FetchReady[2] (gamma): Err=%v Items=%+v, want nil err and one item", results[2].Err, results[2].Items)
	}
	if results[2].Repo != "gamma" {
		t.Fatalf("FetchReady[2]: Repo = %q, want gamma (order changed)", results[2].Repo)
	}
}

// TestFetchReadyMissingStoreCreatesNoFile checks both halves of the
// empty-store trap. A Repo points at a path inside an existing directory
// where no store file exists. After FetchReady, the result's Err is
// non-nil and os.Stat(path) still returns fs.ErrNotExist, so no stray
// database file was created. Dropping the read-only open makes this test
// fail.
func TestFetchReadyMissingStoreCreatesNoFile(t *testing.T) {
	dir := t.TempDir() // exists
	path := filepath.Join(dir, "db.sqlite")

	repos := []Repo{{Path: path, Name: "ghost"}}
	results := FetchReady(context.Background(), repos)

	if len(results) != 1 {
		t.Fatalf("FetchReady: len = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatalf("FetchReady(missing store): Err = nil, want non-nil")
	}

	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("FetchReady(missing store): a file now exists at %s (stat err = %v) -- the empty-store trap fired", path, err)
	}
}

// ---- FetchCounts ----

// TestFetchCounts proves FetchCounts on a real store returns ItemCount equal
// to the number of items added (including raw and closed ones), ReadyCount
// equal to the ready queue's length, and LastActivity equal to the store's
// own LastActivity.
func TestFetchCounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")
	st, err := store.Init(path)
	if err != nil {
		t.Fatalf("store.Init: %v", err)
	}

	// Two ready tickets.
	addItem(t, st, store.AddItemInput{Title: "ticket-1"})
	addItem(t, st, store.AddItemInput{Title: "ticket-2"})
	// One raw item, counted in ItemCount but not ReadyCount.
	addItem(t, st, store.AddItemInput{Title: "note-1", Stage: model.StageRaw})

	wantLastActivity, _, err := st.LastActivity(context.Background())
	if err != nil {
		t.Fatalf("LastActivity (direct): %v", err)
	}
	wantAll, err := st.ListItems(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatalf("ListItems (direct): %v", err)
	}
	wantReady, err := st.Ready(context.Background(), 0)
	if err != nil {
		t.Fatalf("Ready (direct): %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	results := FetchCounts(context.Background(), []Repo{{Path: path, Name: "r"}})
	if len(results) != 1 {
		t.Fatalf("FetchCounts: len = %d, want 1", len(results))
	}
	got := results[0]
	if got.Err != nil {
		t.Fatalf("FetchCounts: Err = %v, want nil", got.Err)
	}
	if got.ItemCount != len(wantAll) {
		t.Fatalf("FetchCounts: ItemCount = %d, want %d", got.ItemCount, len(wantAll))
	}
	if got.ReadyCount != len(wantReady) {
		t.Fatalf("FetchCounts: ReadyCount = %d, want %d", got.ReadyCount, len(wantReady))
	}
	if got.LastActivity != wantLastActivity {
		t.Fatalf("FetchCounts: LastActivity = %q, want %q", got.LastActivity, wantLastActivity)
	}
}

// TestFetchCountsEmptyStoreIsNormalRow checks that FetchCounts on a store
// with zero events returns LastActivity == "" and a nil Err, because an
// empty store is a normal row, not a failed one.
func TestFetchCountsEmptyStoreIsNormalRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")
	st, err := store.Init(path)
	if err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	results := FetchCounts(context.Background(), []Repo{{Path: path, Name: "empty"}})
	if len(results) != 1 {
		t.Fatalf("FetchCounts: len = %d, want 1", len(results))
	}
	got := results[0]
	if got.Err != nil {
		t.Fatalf("FetchCounts(empty store): Err = %v, want nil", got.Err)
	}
	if got.LastActivity != "" {
		t.Fatalf("FetchCounts(empty store): LastActivity = %q, want empty", got.LastActivity)
	}
	if got.ItemCount != 0 || got.ReadyCount != 0 {
		t.Fatalf("FetchCounts(empty store): ItemCount=%d ReadyCount=%d, want 0/0", got.ItemCount, got.ReadyCount)
	}
}
