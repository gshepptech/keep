package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The Interlock join is the one feature of keep that no test could break.
//
// Both READMEs lead with it, and until this test both suites were blind to
// it: keep's internal/interlock tests drive a /bin/sh stub that answers
// whatever they wrote into it, and Interlock's own suite has never heard of
// keep. The only end-to-end proof was scripts/join-keep.sh in the Interlock
// repository, which nothing runs. A contract change on either side -- a
// renamed JSON field, a bumped version, a different exit code -- would have
// shipped green on both sides and been found by a person looking at a board.
//
// So this builds a real interlock from source, takes a real lease through a
// real hook, and asserts that a real `keep ready` drops the item. It is the
// slow kind of test on purpose: a stub cannot fail the way the real contract
// can, which is the entire point.
//
// If Interlock is not on this machine the test skips with a stated reason,
// because keep must build and pass alone -- a Go import between the two would
// have made the independence a slogan instead of a fact. Set
// KEEP_REQUIRE_INTERLOCK=1 to turn that skip into a failure, which is what CI
// should do once Interlock is available to it: a skip nobody notices is the
// same blind spot in a new place.
func TestInterlockJoinAgainstARealBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the join is exercised through a POSIX shell hook payload; Windows support is a separate, deliberately deferred question")
	}

	src := interlockSource(t)
	interlockBin := buildInterlock(t, src)
	keepBin := buildKeep(t, "")

	repo := tempDir(t)
	home := tempDir(t)

	// PATH is how keep finds Interlock (internal/interlock/binary uses
	// exec.LookPath), so the built binary goes in a directory of its own and
	// that directory goes on PATH. Nothing else about the environment is
	// inherited -- run() takes a complete environment on purpose.
	binDir := filepath.Dir(interlockBin)
	environ := append(env(home, "", "KEEP_SESSION=join-test"), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "t@e.com")
	mustGit(t, repo, "config", "user.name", "t")
	if err := os.MkdirAll(filepath.Join(repo, "internal"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"shared.go", "other.go"} {
		if err := os.WriteFile(filepath.Join(repo, "internal", name), []byte("package internal\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-qm", "init")

	if out, errOut, code := run(t, interlockBin, repo, environ, "init"); code != 0 {
		t.Fatalf("interlock init exited %d: %s %s", code, out, errOut)
	}
	if _, _, code := run(t, keepBin, repo, environ, "init"); code != 0 {
		t.Fatal("keep init failed")
	}

	add := func(paths, score, title string) string {
		t.Helper()
		args := []string{"add", "--score", score}
		if paths != "" {
			args = append(args, "--paths", paths)
		}
		args = append(args, title)
		out, errOut, code := run(t, keepBin, repo, environ, args...)
		if code != 0 {
			t.Fatalf("add %q exited %d: %s", title, code, errOut)
		}
		return strings.TrimSpace(out)
	}
	held := add("internal/shared.go", "9/9/2", "Refactor the shared helper")
	free := add("internal/other.go", "8/8/3", "Touch something else")
	bare := add("", "7/7/4", "No paths declared at all")

	before, _, _ := run(t, keepBin, repo, environ, "ready")
	for _, id := range []string{held, free, bare} {
		if !strings.Contains(before, id) {
			t.Fatalf("%s is missing from the queue before any lease; the rest of this test proves nothing:\n%s", id, before)
		}
	}

	// A real lease, taken the way an agent takes one: a PreToolUse hook
	// payload on stdin. Nothing here reaches into Interlock's storage.
	payload := fmt.Sprintf(
		`{"session_id":"agent-a","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":%q}}`,
		repo, filepath.Join(repo, "internal", "shared.go"))
	if _, errOut, code := runStdin(t, interlockBin, repo, environ, payload, "hook", "PreToolUse", "claude"); code != 0 {
		t.Fatalf("interlock hook exited %d: %s", code, errOut)
	}

	// The contract itself, before asking keep to interpret it. If this fails,
	// the break is on Interlock's side and keep is innocent -- worth
	// distinguishing, because the two are released separately.
	status, _, code := run(t, interlockBin, repo, environ, "status", "--json")
	if code != 0 {
		t.Fatalf("interlock status --json exited %d", code)
	}
	if !strings.Contains(status, `"version": 1`) {
		t.Errorf("the contract is not speaking version 1, so keep is reading a shape it was not built for:\n%s", status)
	}
	if !strings.Contains(status, "internal/shared.go") {
		t.Fatalf("interlock does not report the lease it just took:\n%s", status)
	}

	// Queue on stdout, explanation on stderr. A script piping `keep ready`
	// must get workable items and nothing else; a person still gets told why
	// the top item vanished.
	queue, why, _ := run(t, keepBin, repo, environ, "ready")

	if strings.Contains(queue, held) {
		t.Errorf("the held item is still offered as workable:\n%s", queue)
	}
	for _, want := range []struct{ what, s string }{
		{"the file", "internal/shared.go"},
		{"the holder", "claude"},
		{"the holding session", "agent-a"},
	} {
		if !strings.Contains(why, want.s) {
			t.Errorf("the explanation does not name %s (%q):\n%s", want.what, want.s, why)
		}
	}

	// An item that declares no paths is never hidden: keep not knowing what
	// the work touches is not evidence that it collides.
	if !strings.Contains(queue, bare) {
		t.Errorf("an item with no declared paths was hidden:\n%s", queue)
	}
	if !strings.Contains(queue, free) {
		t.Errorf("an item touching a free file was hidden:\n%s", queue)
	}

	// The queue omitting a held item is only half the join. An agent that
	// already knows the id it wants never sees the omission, so `start` has
	// to refuse too -- and `next`, which claims for you, has to choose past
	// it rather than hand you the collision the queue existed to prevent.
	t.Run("start refuses an item whose file is held", func(t *testing.T) {
		_, errOut, code := run(t, keepBin, repo, environ, "start", held)
		if code == 0 {
			t.Fatalf("start on a held item exited 0; the queue hides it and the claim allowed it, which is worse than either alone")
		}
		for _, want := range []string{"internal/shared.go", "claude", "agent-a", "--force"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("the refusal does not name %q:\n%s", want, errOut)
			}
		}

		// It really did not claim it.
		out, _, _ := run(t, keepBin, repo, environ, "show", held, "--json")
		if !strings.Contains(out, `"status":"open"`) && !strings.Contains(out, `"status": "open"`) {
			t.Errorf("the refused start changed the item's status:\n%s", out)
		}
	})

	t.Run("next chooses past a held item rather than claiming it", func(t *testing.T) {
		// held scores highest (9/9/2), so without the join this is exactly
		// the item `next` would take.
		out, errOut, code := run(t, keepBin, repo, environ, "next")
		if code != 0 {
			t.Fatalf("next exited %d: %s", code, errOut)
		}
		if strings.Contains(out, held) {
			t.Fatalf("next claimed the held item:\n%s", out)
		}
		if !strings.Contains(out, free) {
			t.Fatalf("next claimed %q, want the highest-ranked workable item %s:\n%s", out, free, out)
		}
		if !strings.Contains(errOut, "internal/shared.go") {
			t.Errorf("next did not say why it passed over the top of the queue:\n%s", errOut)
		}
	})

	t.Run("--force claims anyway, for the person who can see the holder is gone", func(t *testing.T) {
		if _, errOut, code := run(t, keepBin, repo, environ, "start", held, "--force"); code != 0 {
			t.Fatalf("start --force exited %d: %s", code, errOut)
		}
		// Put it back, so the release assertion below still measures the
		// lease rather than this claim.
		if _, errOut, code := run(t, keepBin, repo, environ, "unclaim", held, "--reason", "restoring the fixture"); code != 0 {
			t.Fatalf("unclaim exited %d: %s", code, errOut)
		}
	})

	if _, errOut, code := run(t, interlockBin, repo, environ, "lease", "release", "agent-a", "--confirm", "agent-a"); code != 0 {
		t.Fatalf("releasing the lease exited %d: %s", code, errOut)
	}
	after, _, _ := run(t, keepBin, repo, environ, "ready")
	if !strings.Contains(after, held) {
		t.Errorf("the item did not come back after the lease was released:\n%s", after)
	}

	// The negative case, and the one most repositories are in. With Interlock
	// off PATH the queue must be exactly what it was before any of this: a
	// join that degrades loudly where the peer is absent would be worse than
	// no join at all.
	t.Run("without interlock on PATH the queue is untouched", func(t *testing.T) {
		payload := fmt.Sprintf(
			`{"session_id":"agent-b","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":%q}}`,
			repo, filepath.Join(repo, "internal", "shared.go"))
		if _, _, code := runStdin(t, interlockBin, repo, environ, payload, "hook", "PreToolUse", "claude"); code != 0 {
			t.Fatalf("taking the second lease failed")
		}
		t.Cleanup(func() {
			run(t, interlockBin, repo, environ, "lease", "release", "agent-b", "--confirm", "agent-b")
		})

		bare := env(home, "", "KEEP_SESSION=join-test", "PATH=/nonexistent")
		queue, why, code := run(t, keepBin, repo, bare, "ready")
		if code != 0 {
			t.Fatalf("keep ready exited %d without interlock; it must work alone: %s", code, why)
		}
		if !strings.Contains(queue, held) {
			t.Errorf("the item is hidden even though keep cannot see interlock:\n%s", queue)
		}
		if strings.TrimSpace(why) != "" {
			t.Errorf("keep complained about a peer it is not required to have:\n%s", why)
		}
	})
}

// interlockSource finds the Interlock checkout, or skips.
//
// Three outcomes, deliberately distinct. An explicitly set INTERLOCK_SRC that
// does not hold a Go module is a misconfiguration and fails loudly -- falling
// back from it would turn "you pointed this at the wrong place" into "the
// join proof quietly did not run", which is the exact failure this test
// exists to end. An absent Interlock is a skip, because keep must build and
// pass alone. And KEEP_REQUIRE_INTERLOCK turns that skip into a failure, so CI
// can demand the real thing once it has it.
func interlockSource(t *testing.T) string {
	t.Helper()

	isModule := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		return err == nil
	}

	if dir := os.Getenv("INTERLOCK_SRC"); dir != "" {
		if !isModule(dir) {
			t.Fatalf("INTERLOCK_SRC=%q holds no go.mod. Point it at the Interlock checkout, or unset it to fall back to ~/Projects/interlock.", dir)
		}
		return dir
	}

	if home, err := os.UserHomeDir(); err == nil {
		if dir := filepath.Join(home, "Projects", "interlock"); isModule(dir) {
			return dir
		}
	}

	msg := "Interlock source not found at ~/Projects/interlock. Set INTERLOCK_SRC to its checkout. " +
		"keep builds and passes without it on purpose -- neither program imports the other."
	if os.Getenv("KEEP_REQUIRE_INTERLOCK") != "" {
		t.Fatal("KEEP_REQUIRE_INTERLOCK is set and " + msg)
	}
	t.Skip(msg)
	return ""
}

func buildInterlock(t *testing.T, src string) string {
	t.Helper()

	dst := filepath.Join(t.TempDir(), "bin", "interlock")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/interlock")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building interlock from %s: %v\n%s", src, err, out)
	}
	return dst
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
