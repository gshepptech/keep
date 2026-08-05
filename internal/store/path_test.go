package store

import (
	"os"
	"path/filepath"
	"testing"
)

// existsFromDirs builds a synthetic ExistsFunc from the directories that are
// said to already contain a store. This is what makes the nested walk-up
// case testable without creating real directory trees or changing the
// process's working directory: a test case reads as "these stores exist,
// cwd is here, expect this path."
func existsFromDirs(dirs []string) ExistsFunc {
	set := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		set[StorePathIn(d)] = true
	}
	return func(path string) bool {
		return set[path]
	}
}

// TestResolvePath is a table-driven proof of all four resolution rules, in
// strict precedence order, including the nested walk-up and the
// per-repo-beats-home case. Each row's name identifies the *rule* being
// exercised so a failure names the requirement, not a line number. The
// precedence rows (rows that supply more than one candidate input at once)
// matter more than the single-input rows: STORE-04 is an ordering
// requirement, and a test that only ever supplies one input at a time
// cannot detect a swapped precedence.
func TestResolvePath(t *testing.T) {
	tests := []struct {
		name   string
		flagDB string
		envDB  string
		cwd    string
		home   string
		stores []string // directories that already have a store
		want   string
	}{
		{
			name:   "rule 1 flag only",
			flagDB: "/flag/db.sqlite",
			cwd:    "/work",
			home:   "/home/user",
			want:   "/flag/db.sqlite",
		},
		{
			name:   "rule 1 flag beats env",
			flagDB: "/flag/db.sqlite",
			envDB:  "/env/db.sqlite",
			cwd:    "/work",
			home:   "/home/user",
			want:   "/flag/db.sqlite",
		},
		{
			name:   "rule 1 flag beats env and nearby",
			flagDB: "/flag/db.sqlite",
			envDB:  "/env/db.sqlite",
			cwd:    "/repo",
			home:   "/home/user",
			stores: []string{"/repo"},
			want:   "/flag/db.sqlite",
		},
		{
			name:   "rule 2 env beats nearby",
			envDB:  "/env/db.sqlite",
			cwd:    "/repo",
			home:   "/home/user",
			stores: []string{"/repo"},
			want:   "/env/db.sqlite",
		},
		{
			name:   "rule 3 nearby store only",
			cwd:    "/repo",
			home:   "/home/user",
			stores: []string{"/repo"},
			want:   StorePathIn("/repo"),
		},
		{
			name:   "rule 3 walk-up from three levels down",
			cwd:    "/repo/a/b/c",
			home:   "/home/user",
			stores: []string{"/repo"},
			want:   StorePathIn("/repo"),
		},
		{
			name: "rule 4 no hit anywhere -- home",
			cwd:  "/tmp/x",
			home: "/home/user",
			want: StorePathIn("/home/user"),
		},
		{
			name:   "rule 3 beats rule 4 -- per-repo store wins over home",
			cwd:    "/repo",
			home:   "/home/user",
			stores: []string{"/repo", "/home/user"},
			want:   StorePathIn("/repo"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exists := existsFromDirs(tt.stores)
			got, err := ResolvePath(tt.flagDB, tt.envDB, tt.cwd, tt.home, exists)
			if err != nil {
				t.Fatalf("ResolvePath() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("ResolvePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFindUpwardTerminatesAtRoot asserts that FindUpward, on a tree with no
// store anywhere, terminates rather than spinning, and that it tested the
// root directory itself exactly once. An ExistsFunc that always returns
// false and counts its calls makes this a call-count assertion rather than
// a timing or "it eventually returned" assertion.
func TestFindUpwardTerminatesAtRoot(t *testing.T) {
	start := "/a/b/c/d"
	calls := 0
	exists := func(path string) bool {
		calls++
		return false
	}

	path, ok := FindUpward(start, exists)
	if ok {
		t.Fatalf("FindUpward(%q) = (%q, true); want (\"\", false)", start, path)
	}

	// /a/b/c/d ascends through /a/b/c, /a/b, /a, and finally / -- five
	// directories tested in total, including the root itself exactly once.
	const wantCalls = 5
	if calls != wantCalls {
		t.Fatalf("FindUpward tested %d directories, want %d (this is the proof the walk visited the root exactly once and did not spin)", calls, wantCalls)
	}
}

// TestOSExistsOnRealTempDir is the one non-table test in this file: it
// checks the real OSExists implementation against a real t.TempDir(), so
// the injected ExistsFunc used everywhere else is known to be trustworthy
// at least once, not just assumed correct because the synthetic fake agrees
// with itself.
func TestOSExistsOnRealTempDir(t *testing.T) {
	dir := t.TempDir()

	present := filepath.Join(dir, "present.txt")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", present, err)
	}
	absent := filepath.Join(dir, "absent.txt")

	if !OSExists(present) {
		t.Errorf("OSExists(%q) = false, want true", present)
	}
	if OSExists(absent) {
		t.Errorf("OSExists(%q) = true, want false", absent)
	}
}
