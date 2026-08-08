package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadMissingFileIsEmptyNotError proves Load(home) on a home whose
// .rounce/repos.json does not exist returns an empty envelope and a NIL
// error -- nothing registered yet is a normal state, not a fault.
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

// TestLoadMalformedJSONIsEmptyAndError proves Load on a file containing
// invalid JSON returns BOTH an empty envelope AND a non-nil error -- a
// caller that only checked the error would have nothing usable, and a
// caller that only checked the envelope would never warn.
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

// TestSaveLeavesNoTempFile proves that after Save, home/.rounce/ contains
// exactly one file (repos.json) -- no leftover temp file.
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

// TestSaveCreatesMissingRounceDir proves Save into a home whose .rounce
// directory does not yet exist creates it and succeeds.
func TestSaveCreatesMissingRounceDir(t *testing.T) {
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

// mustWriteRegistryFile writes raw bytes directly to home's registry path,
// creating the parent .rounce/ directory first -- used by tests that need
// to construct a malformed or wrong-version file, which Save (a
// well-formed writer) cannot produce.
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
