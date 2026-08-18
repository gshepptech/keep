package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `init` registers a store automatically and, until this, nothing ever
// unregistered one. The registry only grew: a few days of scratch
// repositories under /tmp was enough to bury the real rows, and every dead
// one printed a full error line to stderr on each `ready --all` -- the
// command someone working across repositories runs most. The registry is
// most useful to exactly the person who accumulates dead entries fastest.
func TestReposForgetGoneDropsOnlyWhatIsGone(t *testing.T) {
	bin := buildKeep(t, "")
	home := tempDir(t)

	// Three stores, each registered by its own init.
	live := tempDir(t)
	deleted := tempDir(t)
	unreadable := tempDir(t)
	for _, dir := range []string{live, deleted, unreadable} {
		e := env(home, "", "KEEP_SESSION=prune")
		if _, errOut, code := run(t, bin, dir, e, "init"); code != 0 {
			t.Fatalf("init in %s exited %d: %s", dir, code, errOut)
		}
	}
	e := env(home, "", "KEEP_SESSION=prune")

	before, _, _ := run(t, bin, live, e, "repos")
	if strings.Count(strings.TrimSpace(before), "\n") != 2 {
		t.Fatalf("expected three registered stores before pruning, got:\n%s", before)
	}

	// One store is deleted outright. That registration is safe to drop.
	if err := os.RemoveAll(filepath.Join(deleted, ".rounce")); err != nil {
		t.Fatalf("removing the store: %v", err)
	}

	// The other is corrupted but present. It will not open, and it must be
	// KEPT: forgetting a registration because a file was briefly unreadable
	// -- a permission, a half-written copy, an unmounted disk about to come
	// back -- loses the only record that the repository was ever known.
	corrupt := filepath.Join(unreadable, ".rounce", "db.sqlite")
	if err := os.WriteFile(corrupt, []byte("this is not a database"), 0o644); err != nil {
		t.Fatalf("corrupting the store: %v", err)
	}

	out, errOut, code := run(t, bin, live, e, "repos", "forget", "--gone")
	if code != 0 {
		t.Fatalf("forget --gone exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, deleted) {
		t.Errorf("the deleted store was not forgotten:\n%s", out)
	}
	if strings.Contains(out, unreadable) {
		t.Errorf("a store that exists but will not open was forgotten:\n%s", out)
	}
	// Forgetting is not deleting, and the output has to say so.
	if !strings.Contains(out, "No store file was touched") {
		t.Errorf("the summary does not say the stores themselves survived:\n%s", out)
	}

	after, _, _ := run(t, bin, live, e, "repos")
	if strings.Contains(after, deleted) {
		t.Errorf("the deleted store is still registered:\n%s", after)
	}
	if !strings.Contains(after, live) {
		t.Errorf("the live store lost its registration:\n%s", after)
	}
	// The corrupt store is matched by the error it reports, not by its path:
	// a row that failed to open renders its name and its error, and only the
	// not-found error happens to quote the path.
	if !strings.Contains(after, "file is not a database") {
		t.Errorf("the unreadable store lost its registration -- it exists, so it must be kept:\n%s", after)
	}
	if lines := strings.Count(strings.TrimSpace(after), "\n") + 1; lines != 2 {
		t.Errorf("registry holds %d rows after pruning, want 2:\n%s", lines, after)
	}
}

func TestReposForgetGoneOnACleanRegistrySaysSo(t *testing.T) {
	bin := buildKeep(t, "")
	home := tempDir(t)
	dir := tempDir(t)
	e := env(home, "", "KEEP_SESSION=prune")
	if _, _, code := run(t, bin, dir, e, "init"); code != 0 {
		t.Fatal("init failed")
	}

	out, _, code := run(t, bin, dir, e, "repos", "forget", "--gone")
	if code != 0 {
		t.Fatalf("exited %d", code)
	}
	if !strings.Contains(out, "nothing to forget") {
		t.Errorf("output does not say there was nothing to do:\n%s", out)
	}
}

func TestReposForgetGoneTakesNoPath(t *testing.T) {
	bin := buildKeep(t, "")
	home := tempDir(t)
	dir := tempDir(t)
	e := env(home, "", "KEEP_SESSION=prune")
	run(t, bin, dir, e, "init")

	// --gone finds them itself. A path alongside it is ambiguous -- forget
	// that one, or forget all the gone ones? -- so it is a usage error
	// rather than a guess.
	if _, _, code := run(t, bin, dir, e, "repos", "forget", "--gone", "/some/path"); code != 2 {
		t.Errorf("forget --gone with a path exited %d, want 2", code)
	}
}

// A dozen dead registrations printing a dozen paragraphs above every queue is
// how a person learns to stop reading stderr. One is named in full because it
// is probably what they need to see; several are summarised.
func TestReadyAllSummarisesManyBrokenStores(t *testing.T) {
	bin := buildKeep(t, "")
	home := tempDir(t)

	live := tempDir(t)
	e := env(home, "", "KEEP_SESSION=prune")
	if _, _, code := run(t, bin, live, e, "init"); code != 0 {
		t.Fatal("init failed")
	}

	var dead []string
	for i := 0; i < 3; i++ {
		d := tempDir(t)
		if _, _, code := run(t, bin, d, e, "init"); code != 0 {
			t.Fatal("init failed")
		}
		dead = append(dead, d)
	}

	// One broken store is named in full.
	if err := os.RemoveAll(filepath.Join(dead[0], ".rounce")); err != nil {
		t.Fatalf("removing: %v", err)
	}
	_, errOut, _ := run(t, bin, live, e, "ready", "--all")
	if strings.Count(strings.TrimSpace(errOut), "\n") != 0 {
		t.Errorf("one broken store produced more than one line:\n%s", errOut)
	}
	if !strings.Contains(errOut, "not found") {
		t.Errorf("a lone failure does not carry its reason:\n%s", errOut)
	}

	// Three are summarised to one line that says how to fix it.
	for _, d := range dead[1:] {
		if err := os.RemoveAll(filepath.Join(d, ".rounce")); err != nil {
			t.Fatalf("removing: %v", err)
		}
	}
	_, errOut, code := run(t, bin, live, e, "ready", "--all")
	if code != 0 {
		t.Fatalf("ready --all exited %d; a broken peer store must not fail the queue", code)
	}
	if lines := strings.Count(strings.TrimSpace(errOut), "\n"); lines != 0 {
		t.Errorf("three broken stores produced %d extra lines, want one line total:\n%s", lines, errOut)
	}
	if !strings.Contains(errOut, "3 registered stores") {
		t.Errorf("the summary does not say how many:\n%s", errOut)
	}
	if !strings.Contains(errOut, "forget --gone") {
		t.Errorf("the summary does not say how to clear them:\n%s", errOut)
	}
}
