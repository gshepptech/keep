package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeInterlock writes a stub `interlock` onto a PATH directory, answering
// `status --json` with the given envelope.
//
// A stub rather than the real binary because keep must build and test on a
// machine that has never heard of Interlock, which is why keep shells out
// instead of importing it. The join was also verified against
// the real binary by hand (a lease taken through `interlock hook PreToolUse
// claude`, the top-scoring item vanishing from `ready`, and returning after
// `interlock lease release`); what these tests lock is keep's half, which
// is the half that can regress here.
func fakeInterlock(t *testing.T, statusJSON string) string {
	t.Helper()
	dir := tempDir(t)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"status\" ]; then\n" +
		"  cat <<'INTERLOCK_EOF'\n" + statusJSON + "\nINTERLOCK_EOF\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 64\n"
	if err := os.WriteFile(filepath.Join(dir, "interlock"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return dir
}

// pathWith returns an environment whose PATH includes dir, so the child
// `keep` process finds the stub.
func pathWith(home, dir string) []string {
	e := env(home, "")
	for i, kv := range e {
		if strings.HasPrefix(kv, "PATH=") {
			e[i] = "PATH=" + dir + ":" + strings.TrimPrefix(kv, "PATH=")
		}
	}
	return e
}

const heldSharedGo = `{
  "version": 1,
  "repo": "/repo",
  "declared": false,
  "sessions": [],
  "leases": [
    {"path": "internal/shared.go", "agent": "claude", "team": "",
     "session_id": "agent-a", "implicit": true,
     "acquired": "2026-08-13T00:00:00Z", "expires_at": "2026-08-14T00:00:00Z"}
  ],
  "errors": []
}`

// readyWith runs a real `keep ready` subprocess with the given PATH and
// returns its stdout ids and stderr.
func readyWith(t *testing.T, bin, repo string, e []string) (string, string) {
	t.Helper()
	stdout, stderr, code := run(t, bin, repo, e, "ready")
	if code != 0 {
		t.Fatalf("ready exit %d, stderr=%q", code, stderr)
	}
	return stdout, stderr
}

// TestReadyExcludesItemsWhosePathsInterlockHolds checks that `keep ready`
// omits items whose paths a fake Interlock reports as held, across two real
// processes. keep shells out rather than importing Interlock, so an
// in-process test would prove nothing about the part that can break.
func TestReadyExcludesItemsWhosePathsInterlockHolds(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := pathWith(home, fakeInterlock(t, heldSharedGo))
	mustInit(t, bin, repo, e)

	heldID := mustAdd(t, bin, repo, e, "--score", "10/10/0", "--paths", "internal/shared.go", "held work")
	freeID := mustAdd(t, bin, repo, e, "--score", "8/8/2", "--paths", "cmd/other.go", "free work")
	bareID := mustAdd(t, bin, repo, e, "--score", "6/6/4", "undeclared work")

	stdout, stderr := readyWith(t, bin, repo, e)

	if strings.Contains(stdout, heldID) {
		t.Errorf("%s is in the queue although Interlock holds internal/shared.go:\n%s", heldID, stdout)
	}
	for _, id := range []string{freeID, bareID} {
		if !strings.Contains(stdout, id) {
			t.Errorf("%s was dropped from the queue but nothing of its is held:\n%s", id, stdout)
		}
	}

	// The holder must be named. An item that merely vanished would be
	// indistinguishable from one that was closed, which is the same reason
	// `keep blocked` names the blocking item rather than just omitting it.
	for _, want := range []string{heldID, "internal/shared.go", "claude", "agent-a"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}

	// stdout is the queue. A diagnostic there would break an agent piping
	// it.
	if strings.Contains(stdout, "held back") {
		t.Errorf("the diagnostic leaked onto stdout:\n%s", stdout)
	}
}

// TestReadyIsUnchangedWithoutInterlock checks that `keep ready` is unchanged
// when Interlock is switched off, which is the common case, since most
// repositories do not use Interlock.
//
// KEEP_INTERLOCK=off rather than merely leaving the stub off PATH: env()
// already pins PATH, but a developer with Interlock installed somewhere
// env() happens to include would silently turn this into a different test.
func TestReadyIsUnchangedWithoutInterlock(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_INTERLOCK=off")
	mustInit(t, bin, repo, e)

	heldID := mustAdd(t, bin, repo, e, "--score", "10/10/0", "--paths", "internal/shared.go", "held work")

	stdout, stderr := readyWith(t, bin, repo, e)
	if !strings.Contains(stdout, heldID) {
		t.Errorf("an item was filtered with no Interlock on PATH:\n%s", stdout)
	}
	if strings.Contains(stderr, "held back") {
		t.Errorf("stderr mentions holding with no Interlock present:\n%s", stderr)
	}
}

// TestReadyIgnoresAnUnsupportedStatusVersion checks that a status envelope
// with an unknown version is ignored. A field that changed meaning would
// produce a queue that hides workable items, which this integration must
// never do, so refusing to parse an envelope it does not recognise is the
// only safe reading.
func TestReadyIgnoresAnUnsupportedStatusVersion(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	future := strings.Replace(heldSharedGo, `"version": 1`, `"version": 99`, 1)
	e := pathWith(home, fakeInterlock(t, future))
	mustInit(t, bin, repo, e)

	heldID := mustAdd(t, bin, repo, e, "--score", "10/10/0", "--paths", "internal/shared.go", "held work")

	stdout, _ := readyWith(t, bin, repo, e)
	if !strings.Contains(stdout, heldID) {
		t.Errorf("an unrecognised status version was parsed anyway and hid an item:\n%s", stdout)
	}
}

// TestReadyLimitAppliesAfterTheInterlockFilter checks that --limit counts
// workable rows after held items are removed. Applying --limit in SQL and
// filtering afterwards returns fewer rows than asked for whenever anything
// is held. runBoard and mcpInstructions follow the same count-versus-rows
// rule, which is easy to miss here because nothing renders both numbers
// side by side.
func TestReadyLimitAppliesAfterTheInterlockFilter(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := pathWith(home, fakeInterlock(t, heldSharedGo))
	mustInit(t, bin, repo, e)

	// The held item outranks both survivors, so a filter applied after a
	// SQL limit of 2 would return one row instead of two.
	mustAdd(t, bin, repo, e, "--score", "10/10/0", "--paths", "internal/shared.go", "held work")
	a := mustAdd(t, bin, repo, e, "--score", "8/8/2", "free A")
	b := mustAdd(t, bin, repo, e, "--score", "7/7/2", "free B")

	stdout, stderr, code := run(t, bin, repo, e, "ready", "--limit", "2")
	if code != 0 {
		t.Fatalf("ready --limit 2 exit %d, stderr=%q", code, stderr)
	}
	for _, id := range []string{a, b} {
		if !strings.Contains(stdout, id) {
			t.Errorf("--limit 2 returned fewer than 2 workable rows -- the limit ran before the filter:\n%s", stdout)
		}
	}
}
