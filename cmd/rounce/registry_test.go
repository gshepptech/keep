package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/registry"
)

// readRegistry decodes home's repos.json. Every test in this file passes a
// home inside tempDir(t) -- the developer's real ~/.rounce/repos.json must
// never be read or written by `go test`, which is why cli_test.go's env
// helper sets an explicit HOME rather than inheriting os.Environ().
func readRegistry(t *testing.T, home string) registry.Envelope {
	t.Helper()
	b, err := os.ReadFile(registry.PathIn(home))
	if err != nil {
		t.Fatalf("reading registry at %s: %v", registry.PathIn(home), err)
	}
	var env registry.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decoding registry at %s: %v", registry.PathIn(home), err)
	}
	return env
}

// TestInitRegistersStoreInRepoList proves REPO-01's core behaviour: a fresh
// `rounce init` in a directory named "myrepo" writes exactly one repo row
// naming the repository directory (never the store's own filename, which is
// always "db.sqlite"), whose path is the absolute store path `init` printed
// and whose last_seen parses with model.TimeFormat.
func TestInitRegistersStoreInRepoList(t *testing.T) {
	bin := buildRounce(t, "")
	base := tempDir(t)
	home := tempDir(t)
	repoDir := filepath.Join(base, "myrepo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", repoDir, err)
	}
	e := env(home, "")

	stdout, stderr, code := run(t, bin, repoDir, e, "init")
	if code != 0 {
		t.Fatalf("init: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	wantPath := strings.TrimSpace(stdout)
	if wantPath == "" {
		t.Fatalf("init: stdout was empty, want the store path")
	}

	envelope := readRegistry(t, home)
	if envelope.Version != registry.Version {
		t.Fatalf("registry version = %d, want %d", envelope.Version, registry.Version)
	}
	if len(envelope.Repos) != 1 {
		t.Fatalf("registry has %d repos, want exactly 1: %+v", len(envelope.Repos), envelope.Repos)
	}
	got := envelope.Repos[0]
	if got.Path != wantPath {
		t.Fatalf("registry repo path = %q, want %q (exact match, not substring)", got.Path, wantPath)
	}
	if got.Name != "myrepo" {
		t.Fatalf("registry repo name = %q, want %q", got.Name, "myrepo")
	}
	if _, err := time.Parse(model.TimeFormat, got.LastSeen); err != nil {
		t.Fatalf("registry repo last_seen = %q does not parse with model.TimeFormat: %v", got.LastSeen, err)
	}
}

// TestInitRegistrationIsIdempotentAndRefreshesLastSeen proves REPO-01's
// idempotency requirement: running `rounce init` a second time in the same
// repository leaves exactly ONE row, with a last_seen strictly greater than
// the first run's. The row count is asserted, not only the timestamp -- a
// bug that always appended would still refresh a *different* row's
// timestamp and could slip past a value-only check.
func TestInitRegistrationIsIdempotentAndRefreshesLastSeen(t *testing.T) {
	bin := buildRounce(t, "")
	repoDir := tempDir(t)
	home := tempDir(t)
	e := env(home, "")

	if _, stderr, code := run(t, bin, repoDir, e, "init"); code != 0 {
		t.Fatalf("first init: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	first := readRegistry(t, home)
	if len(first.Repos) != 1 {
		t.Fatalf("after first init: registry has %d repos, want exactly 1", len(first.Repos))
	}
	firstSeen := first.Repos[0].LastSeen

	// model.TimeFormat is nanosecond-resolution, but consecutive real process
	// runs are not guaranteed to land on different nanoseconds on every
	// platform -- a short sleep makes the strictly-greater assertion below
	// meaningful rather than flaky.
	time.Sleep(2 * time.Millisecond)

	if _, stderr, code := run(t, bin, repoDir, e, "init"); code != 0 {
		t.Fatalf("second init: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	second := readRegistry(t, home)
	if len(second.Repos) != 1 {
		t.Fatalf("after second init: registry has %d repos, want exactly 1 (idempotency failed): %+v", len(second.Repos), second.Repos)
	}
	secondSeen := second.Repos[0].LastSeen
	if secondSeen <= firstSeen {
		t.Fatalf("second init's last_seen %q is not strictly greater than first init's %q", secondSeen, firstSeen)
	}
}

// TestInitRegistrationSurvivesUnwritableHome proves REPO-01's best-effort
// contract (06-CONTEXT.md decision 3): with $HOME pointing at a REGULAR FILE
// (so ~/.rounce cannot be created under it), `init` still exits 0, stdout is
// still exactly the one store-path line, stderr carries a warning naming the
// store path, and -- critically -- the store `init` created is real and
// usable, proven by running `rounce ready --db <that path>` and requiring
// exit 0.
//
// $HOME being a regular file makes os.MkdirAll inside registry.Save fail
// with ENOTDIR for root and non-root alike, so this test needs no
// os.Geteuid skip. init without --db or --global targets the working
// directory and never otherwise consults HOME, so nothing but the
// registration is affected.
func TestInitRegistrationSurvivesUnwritableHome(t *testing.T) {
	bin := buildRounce(t, "")
	repoDir := tempDir(t)
	homeParent := tempDir(t)
	homeFile := filepath.Join(homeParent, "home-is-a-file")
	if err := os.WriteFile(homeFile, nil, 0o644); err != nil {
		t.Fatalf("creating regular file at %s: %v", homeFile, err)
	}
	e := env(homeFile, "")

	stdout, stderr, code := run(t, bin, repoDir, e, "init")
	if code != 0 {
		t.Fatalf("init with unwritable HOME: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("init with unwritable HOME: stdout = %q, want exactly one line", stdout)
	}
	storePath := lines[0]
	if storePath == "" {
		t.Fatalf("init with unwritable HOME: stdout line was empty, want the store path")
	}
	if !strings.Contains(stderr, storePath) {
		t.Fatalf("init with unwritable HOME: stderr = %q, want it to name the store path %q", stderr, storePath)
	}
	if !strings.Contains(strings.ToLower(stderr), "warning") {
		t.Fatalf("init with unwritable HOME: stderr = %q, want a warning", stderr)
	}

	// The store itself must be real and usable regardless of the
	// registration failure.
	readyOut, readyErr, readyCode := run(t, bin, repoDir, e, "ready", "--db", storePath)
	if readyCode != 0 {
		t.Fatalf("ready --db %s: exit code = %d, want 0 (stdout=%q stderr=%q)", storePath, readyCode, readyOut, readyErr)
	}
}

// TestGlobalInitRegistersStoreNotSpecialCased proves REPO-01's `--global`
// requirement: `rounce init --global` registers $HOME/.rounce/db.sqlite like
// any other store -- one row whose path is that file -- with no special
// case, and the registry file sits beside the store in the same directory.
func TestGlobalInitRegistersStoreNotSpecialCased(t *testing.T) {
	bin := buildRounce(t, "")
	repoDir := tempDir(t)
	home := tempDir(t)
	e := env(home, "")

	stdout, stderr, code := run(t, bin, repoDir, e, "init", "--global")
	if code != 0 {
		t.Fatalf("init --global: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	wantPath := strings.TrimSpace(stdout)

	envelope := readRegistry(t, home)
	if len(envelope.Repos) != 1 {
		t.Fatalf("registry has %d repos, want exactly 1: %+v", len(envelope.Repos), envelope.Repos)
	}
	got := envelope.Repos[0]
	if got.Path != wantPath {
		t.Fatalf("registry repo path = %q, want %q", got.Path, wantPath)
	}

	wantRegistryDir := filepath.Dir(wantPath)
	gotRegistryDir := filepath.Dir(registry.PathIn(home))
	if gotRegistryDir != wantRegistryDir {
		t.Fatalf("registry directory = %q, want it beside the store at %q", gotRegistryDir, wantRegistryDir)
	}
}
