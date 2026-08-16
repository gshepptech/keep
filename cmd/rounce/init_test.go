package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gshepptech/rounce/internal/store"
)

func TestVersionUsesLdflags(t *testing.T) {
	bin := buildRounce(t, "t-9.9.9")
	home := tempDir(t)

	stdout, stderr, code := run(t, bin, home, env(home, ""), "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if stdout != "t-9.9.9\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "t-9.9.9\n")
	}
}

func TestVersionDashVRemoved(t *testing.T) {
	bin := buildRounce(t, "t-9.9.9")
	home := tempDir(t)

	_, stderr, code := run(t, bin, home, env(home, ""), "-v")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Fatalf("stderr = %q, want it to mention an unknown command", stderr)
	}
}

func TestInitCreatesRepoStore(t *testing.T) {
	bin := buildRounce(t, "test")
	repo := tempDir(t)
	home := tempDir(t)

	stdout, stderr, code := run(t, bin, repo, env(home, ""), "init")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}

	want := filepath.Join(repo, ".rounce", "db.sqlite")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one line", stdout)
	}
	if lines[0] != want {
		t.Fatalf("stdout path = %q, want %q", lines[0], want)
	}
	if !filepath.IsAbs(lines[0]) {
		t.Fatalf("stdout path %q is not absolute", lines[0])
	}

	s, err := store.Open(want, store.OpenOptions{})
	if err != nil {
		t.Fatalf("store.Open(%s) = %v, want success", want, err)
	}
	s.Close()
}

func TestInitGlobal(t *testing.T) {
	bin := buildRounce(t, "test")
	repo := tempDir(t)
	home := tempDir(t)

	stdout, stderr, code := run(t, bin, repo, env(home, ""), "init", "--global")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}

	want := filepath.Join(home, ".rounce", "db.sqlite")
	if got := strings.TrimSpace(stdout); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	if _, err := os.Stat(filepath.Join(repo, ".rounce")); !os.IsNotExist(err) {
		t.Fatalf("./.rounce was created in repo dir, want it absent (stat err=%v)", err)
	}

	s, err := store.Open(want, store.OpenOptions{})
	if err != nil {
		t.Fatalf("store.Open(%s) = %v, want success", want, err)
	}
	s.Close()
}

func TestInitDBFlag(t *testing.T) {
	bin := buildRounce(t, "test")
	repo := tempDir(t)
	home := tempDir(t)
	custom := filepath.Join(repo, "custom", "x.sqlite")

	stdout, stderr, code := run(t, bin, repo, env(home, ""), "init", "--db", custom)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if got := strings.TrimSpace(stdout); got != custom {
		t.Fatalf("stdout = %q, want %q", got, custom)
	}

	s, err := store.Open(custom, store.OpenOptions{})
	if err != nil {
		t.Fatalf("store.Open(%s) = %v, want success", custom, err)
	}
	s.Close()
}

func TestInitDBFlagWithGlobalIsUsageError(t *testing.T) {
	bin := buildRounce(t, "test")
	repo := tempDir(t)
	home := tempDir(t)
	custom := filepath.Join(repo, "custom", "x.sqlite")

	_, stderr, code := run(t, bin, repo, env(home, ""), "init", "--db", custom, "--global")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stderr == "" {
		t.Fatalf("stderr is empty, want a usage message")
	}
	if _, err := os.Stat(custom); !os.IsNotExist(err) {
		t.Fatalf("%s was created, want no file created (stat err=%v)", custom, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".rounce")); !os.IsNotExist(err) {
		t.Fatalf("home store was created, want no file created (stat err=%v)", err)
	}
}

func TestInitTwiceIsIdempotent(t *testing.T) {
	bin := buildRounce(t, "test")
	repo := tempDir(t)
	home := tempDir(t)

	stdout1, stderr1, code1 := run(t, bin, repo, env(home, ""), "init")
	if code1 != 0 {
		t.Fatalf("first init: exit code = %d, want 0 (stderr=%q)", code1, stderr1)
	}
	want := filepath.Join(repo, ".rounce", "db.sqlite")
	if got := strings.TrimSpace(stdout1); got != want {
		t.Fatalf("first init stdout = %q, want %q", got, want)
	}

	stdout2, stderr2, code2 := run(t, bin, repo, env(home, ""), "init")
	if code2 != 0 {
		t.Fatalf("second init: exit code = %d, want 0 (stderr=%q)", code2, stderr2)
	}
	wantLine2 := "unchanged: " + want
	if got := strings.TrimSpace(stdout2); got != wantLine2 {
		t.Fatalf("second init stdout = %q, want %q", got, wantLine2)
	}

	s, err := store.Open(want, store.OpenOptions{})
	if err != nil {
		t.Fatalf("store.Open(%s) after second init = %v, want success", want, err)
	}
	s.Close()
}

func TestInitNestedWarnsOnStderr(t *testing.T) {
	bin := buildRounce(t, "test")
	outer := tempDir(t)
	home := tempDir(t)

	stdoutOuter, stderrOuter, codeOuter := run(t, bin, outer, env(home, ""), "init")
	if codeOuter != 0 {
		t.Fatalf("outer init: exit code = %d, want 0 (stderr=%q)", codeOuter, stderrOuter)
	}
	outerStore := strings.TrimSpace(stdoutOuter)

	sub := filepath.Join(outer, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	stdoutSub, stderrSub, codeSub := run(t, bin, sub, env(home, ""), "init")
	if codeSub != 0 {
		t.Fatalf("nested init: exit code = %d, want 0 (stderr=%q)", codeSub, stderrSub)
	}

	wantSubStore := filepath.Join(sub, ".rounce", "db.sqlite")
	lines := strings.Split(strings.TrimRight(stdoutSub, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one line", stdoutSub)
	}
	if lines[0] != wantSubStore {
		t.Fatalf("stdout path = %q, want %q", lines[0], wantSubStore)
	}
	if !filepath.IsAbs(lines[0]) {
		t.Fatalf("stdout path %q is not absolute", lines[0])
	}

	if !strings.Contains(stderrSub, outerStore) {
		t.Fatalf("nested init stderr = %q, want it to name the outer store %q", stderrSub, outerStore)
	}

	s, err := store.Open(wantSubStore, store.OpenOptions{})
	if err != nil {
		t.Fatalf("store.Open(%s) = %v, want success", wantSubStore, err)
	}
	s.Close()
}

func TestInitWarnsWhenRounceDBShadows(t *testing.T) {
	bin := buildRounce(t, "test")
	repo := tempDir(t)
	home := tempDir(t)
	elsewhere := filepath.Join(tempDir(t), "elsewhere.sqlite")

	stdout, stderr, code := run(t, bin, repo, env(home, elsewhere), "init")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	want := filepath.Join(repo, ".rounce", "db.sqlite")
	if got := strings.TrimSpace(stdout); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if !strings.Contains(stderr, elsewhere) {
		t.Fatalf("stderr = %q, want it to name ROUNCE_DB=%s", stderr, elsewhere)
	}
}
