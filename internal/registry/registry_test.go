package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadMissingFileIsEmptyNotError checks that Load(home) on a home whose
// .rounce/repos.json does not exist returns an empty envelope and a nil
// error, because nothing registered yet is a normal state, not a fault.
func TestLoadMissingFileIsEmptyNotError(t *testing.T) {
	home := t.TempDir()

	env, err := Load(home)
	if err != nil {
		t.Fatalf("Load(missing): unexpected error: %v", err)
	}
	if env.Version != Version {
		t.Fatalf("Load(missing): Version = %d, want %d", env.Version, Version)
	}
	if len(env.Repos) != 0 {
		t.Fatalf("Load(missing): Repos = %v, want empty", env.Repos)
	}
}

// TestLoadMalformedJSONIsEmptyAndError checks that Load on a file
// containing invalid JSON returns both an empty envelope and a non-nil
// error. A caller that only checked the error would have nothing usable,
// and a caller that only checked the envelope would never warn.
func TestLoadMalformedJSONIsEmptyAndError(t *testing.T) {
	home := t.TempDir()
	mustWriteRegistryFile(t, home, []byte("not json at all"))

	env, err := Load(home)
	if err == nil {
		t.Fatalf("Load(malformed): err = nil, want non-nil")
	}
	if env.Version != Version || len(env.Repos) != 0 {
		t.Fatalf("Load(malformed): env = %+v, want empty envelope (Version=%d, no Repos)", env, Version)
	}
}

// TestLoadWrongVersionIsEmptyAndError proves Load on a well-formed file
// whose version is 2 returns an empty envelope AND a non-nil error naming
// both versions.
func TestLoadWrongVersionIsEmptyAndError(t *testing.T) {
	home := t.TempDir()
	mustWriteRegistryFile(t, home, []byte(`{"version":2,"repos":[]}`))

	env, err := Load(home)
	if err == nil {
		t.Fatalf("Load(wrong version): err = nil, want non-nil")
	}
	if env.Version != Version || len(env.Repos) != 0 {
		t.Fatalf("Load(wrong version): env = %+v, want empty envelope", env)
	}
	if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "1") {
		t.Fatalf("Load(wrong version): error %q does not name both the found version (2) and the wanted version (1)", err.Error())
	}
}

// TestSaveLoadRoundTrips proves Save(home, env) then Load(home) round-trips
// the repos exactly, and the file on disk contains "version": 1.
func TestSaveLoadRoundTrips(t *testing.T) {
	home := t.TempDir()

	want := Envelope{
		Version: Version,
		Repos: []Repo{
			{Path: "/a/b/repo1/.rounce/db.sqlite", Name: "repo1", LastSeen: "2026-08-07T12:00:00.000000000Z"},
			{Path: "/a/b/repo2/.rounce/db.sqlite", Name: "repo2", LastSeen: "2026-08-07T13:00:00.000000000Z"},
		},
	}
	if err := Save(home, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(home)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if got.Version != want.Version {
		t.Fatalf("Load after Save: Version = %d, want %d", got.Version, want.Version)
	}
	if len(got.Repos) != len(want.Repos) {
		t.Fatalf("Load after Save: %d repos, want %d", len(got.Repos), len(want.Repos))
	}
	byPath := map[string]Repo{}
	for _, r := range got.Repos {
		byPath[r.Path] = r
	}
	for _, wantRepo := range want.Repos {
		gotRepo, ok := byPath[wantRepo.Path]
		if !ok {
			t.Fatalf("Load after Save: missing repo %s", wantRepo.Path)
		}
		if gotRepo != wantRepo {
			t.Fatalf("Load after Save: repo %s = %+v, want %+v", wantRepo.Path, gotRepo, wantRepo)
		}
	}

	raw, err := os.ReadFile(PathIn(home))
	if err != nil {
		t.Fatalf("reading registry file directly: %v", err)
	}
	if !strings.Contains(string(raw), `"version": 1`) {
		t.Fatalf("registry file on disk does not contain %q; got:\n%s", `"version": 1`, raw)
	}
}

// TestSaveLeavesNoTempFile checks that after Save, home/.rounce/ contains
// exactly one file (repos.json) and no leftover temp file.
func TestSaveLeavesNoTempFile(t *testing.T) {
	home := t.TempDir()

	if err := Save(home, Envelope{Repos: []Repo{{Path: "/x/.rounce/db.sqlite", Name: "x", LastSeen: "2026-08-07T12:00:00.000000000Z"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dir := filepath.Dir(PathIn(home))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("%s contains %d entries %v, want exactly 1 (%s)", dir, len(entries), names, FileName)
	}
	if entries[0].Name() != FileName {
		t.Fatalf("%s's one entry is named %q, want %q", dir, entries[0].Name(), FileName)
	}
}

// TestSaveCreatesMissingKeepDir proves Save into a home whose .rounce
// directory does not yet exist creates it and succeeds.
func TestSaveCreatesMissingKeepDir(t *testing.T) {
	home := t.TempDir()

	if err := Save(home, Envelope{}); err != nil {
		t.Fatalf("Save into home with no .rounce/ yet: %v", err)
	}

	env, err := Load(home)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if env.Version != Version {
		t.Fatalf("Load after Save: Version = %d, want %d", env.Version, Version)
	}
}

// TestNameFor proves the two documented layouts derive the correct display
// name: the standard <repo>/.rounce/db.sqlite layout, and a scratch/custom
// path with no .rounce/ component at all.
func TestNameFor(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/a/b/myrepo/.rounce/db.sqlite", "myrepo"},
		{"/a/b/scratch/custom.sqlite", "scratch"},
	}
	for _, c := range cases {
		if got := NameFor(c.path); got != c.want {
			t.Errorf("NameFor(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestRegisterAddsOneRowOnEmptyRegistry proves Register on an empty registry
// adds one row carrying all three values.
func TestRegisterAddsOneRowOnEmptyRegistry(t *testing.T) {
	home := t.TempDir()

	if err := Register(home, "/a/repo/.rounce/db.sqlite", "repo", "2026-08-07T12:00:00.000000000Z"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	env, err := Load(home)
	if err != nil {
		t.Fatalf("Load after Register: %v", err)
	}
	if len(env.Repos) != 1 {
		t.Fatalf("Register on empty registry: %d rows, want 1", len(env.Repos))
	}
	got := env.Repos[0]
	want := Repo{Path: "/a/repo/.rounce/db.sqlite", Name: "repo", LastSeen: "2026-08-07T12:00:00.000000000Z"}
	if got != want {
		t.Fatalf("Register: row = %+v, want %+v", got, want)
	}
}

// TestRegisterSamePathTwiceUpdatesNotAppends proves registering the SAME
// path a second time with a later lastSeen leaves exactly ONE row, whose
// LastSeen is the newer value. The row count is asserted explicitly,
// because appending instead of updating leaves the newest value reachable
// and only the count wrong.
func TestRegisterSamePathTwiceUpdatesNotAppends(t *testing.T) {
	home := t.TempDir()
	path := "/a/repo/.rounce/db.sqlite"

	if err := Register(home, path, "repo", "2026-08-07T12:00:00.000000000Z"); err != nil {
		t.Fatalf("Register (first): %v", err)
	}
	if err := Register(home, path, "repo", "2026-08-07T13:00:00.000000000Z"); err != nil {
		t.Fatalf("Register (second): %v", err)
	}

	env, err := Load(home)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(env.Repos) != 1 {
		t.Fatalf("Register(path) twice: %d rows, want exactly 1", len(env.Repos))
	}
	if env.Repos[0].LastSeen != "2026-08-07T13:00:00.000000000Z" {
		t.Fatalf("Register(path) twice: LastSeen = %q, want the newer value", env.Repos[0].LastSeen)
	}
}

// TestRegisterSecondDifferentPathLeavesTwoRows proves registering a second,
// different path leaves two rows.
func TestRegisterSecondDifferentPathLeavesTwoRows(t *testing.T) {
	home := t.TempDir()

	if err := Register(home, "/a/repo1/.rounce/db.sqlite", "repo1", "2026-08-07T12:00:00.000000000Z"); err != nil {
		t.Fatalf("Register (repo1): %v", err)
	}
	if err := Register(home, "/a/repo2/.rounce/db.sqlite", "repo2", "2026-08-07T12:00:00.000000000Z"); err != nil {
		t.Fatalf("Register (repo2): %v", err)
	}

	env, err := Load(home)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(env.Repos) != 2 {
		t.Fatalf("Register two different paths: %d rows, want 2", len(env.Repos))
	}
}

// TestForgetRegisteredPathRemovesRowLeavesFileAlone proves Forget on a
// registered path returns (true, nil), removes the row, and leaves any file
// at that path untouched on disk.
func TestForgetRegisteredPathRemovesRowLeavesFileAlone(t *testing.T) {
	home := t.TempDir()
	storeDir := t.TempDir()
	storePath := filepath.Join(storeDir, "db.sqlite")
	if err := os.WriteFile(storePath, []byte("pretend sqlite bytes"), 0o644); err != nil {
		t.Fatalf("seeding store file: %v", err)
	}
	before, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("stat store file before Forget: %v", err)
	}

	if err := Register(home, storePath, "repo", "2026-08-07T12:00:00.000000000Z"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	found, err := Forget(home, storePath)
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if !found {
		t.Fatalf("Forget(registered path): found = false, want true")
	}

	env, err := Load(home)
	if err != nil {
		t.Fatalf("Load after Forget: %v", err)
	}
	if len(env.Repos) != 0 {
		t.Fatalf("Load after Forget: %d rows remain, want 0", len(env.Repos))
	}

	after, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("stat store file after Forget: %v", err)
	}
	if before.ModTime() != after.ModTime() || before.Size() != after.Size() {
		t.Fatalf("Forget modified the store file on disk: before=%+v after=%+v", before, after)
	}
}

// TestForgetUnregisteredPathReturnsFalseNotError checks that Forget on an
// unregistered path returns (false, nil) rather than an error, and leaves
// the registry unchanged.
func TestForgetUnregisteredPathReturnsFalseNotError(t *testing.T) {
	home := t.TempDir()
	if err := Register(home, "/a/repo/.rounce/db.sqlite", "repo", "2026-08-07T12:00:00.000000000Z"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	found, err := Forget(home, "/a/never-registered/.rounce/db.sqlite")
	if err != nil {
		t.Fatalf("Forget(unregistered): unexpected error: %v", err)
	}
	if found {
		t.Fatalf("Forget(unregistered): found = true, want false")
	}

	env, err := Load(home)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(env.Repos) != 1 {
		t.Fatalf("Forget(unregistered): registry now has %d rows, want unchanged 1", len(env.Repos))
	}
}

// mustWriteRegistryFile writes raw bytes directly to home's registry path,
// creating the parent .rounce/ directory first. Tests use it to construct a
// malformed or wrong-version file, which Save cannot produce.
func mustWriteRegistryFile(t *testing.T, home string, data []byte) {
	t.Helper()
	dir := filepath.Dir(PathIn(home))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mustWriteRegistryFile: MkdirAll(%s): %v", dir, err)
	}
	if err := os.WriteFile(PathIn(home), data, 0o644); err != nil {
		t.Fatalf("mustWriteRegistryFile: WriteFile: %v", err)
	}
}
