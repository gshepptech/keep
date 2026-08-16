package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildRounce builds the real binary with the given version string baked in
// via -ldflags and returns its path. Every CLI test in this package goes
// through it: these tests exist to prove process-level behaviour, so nothing
// here may call runInit in-process.
// tempDir is t.TempDir() with symlinks resolved.
//
// On macOS /var is a symlink to /private/var, so t.TempDir() hands back
// /var/folders/... while rounce -- correctly, because the registry needs one
// canonical path per store -- reports /private/var/folders/.... A test
// comparing its own unresolved path against rounce's resolved one fails on
// the difference between two names for the same directory.
//
// It passed on the developer's Mac only because TMPDIR there already
// pointed at a resolved path. CI found it on the first run.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	return dir
}

func buildRounce(t *testing.T, version string) string {
	t.Helper()

	// .exe on Windows, or exec refuses to start what `go build` just wrote.
	// Found by CI's first run: every cross-process test failed there with
	// "executable file not found in %PATH%" while passing on the developer's
	// Mac, which is the whole reason the matrix exists.
	name := "rounce"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(t.TempDir(), name)
	args := []string{"build", "-o", dst}
	if version != "" {
		args = append(args, "-ldflags", "-X main.version="+version)
	}
	args = append(args, ".")

	cmd := exec.Command("go", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building rounce failed: %v\n%s", err, out)
	}
	return dst
}

// run executes bin with args, in dir, with env as its complete environment,
// and returns its stdout, stderr (captured separately -- the split is a
// requirement, not a formatting preference) and exit code.
func run(t *testing.T, bin, dir string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()

	if err == nil {
		return stdout, stderr, 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return stdout, stderr, exitErr.ExitCode()
	}
	t.Fatalf("running %s failed to even start: %v", bin, err)
	return "", "", -1
}

// runStdin is run's stdin-aware sibling, needed because no command before
// this phase reads stdin (03-RESEARCH.md Pitfall 3): args is variadic-last
// in run's signature, so a stdin parameter cannot be inserted into it
// without touching every existing call site, and an unset cmd.Stdin
// correctly defaults to os.DevNull for every other command's test -- so run
// itself needs no change and stays correct.
//
// A non-*os.File io.Reader (strings.NewReader here) is copied to the
// child's stdin via an internal os/exec goroutine, so Wait() (inside
// cmd.Run()) blocks until copying finishes -- no manual pipe/goroutine
// plumbing is needed here, confirmed against os/exec's own documentation
// and 03-RESEARCH.md's live proof that a 5MB payload round-trips
// byte-identically through a real os/exec-spawned process with no
// truncation.
func runStdin(t *testing.T, bin, dir string, env []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()

	if err == nil {
		return stdout, stderr, 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return stdout, stderr, exitErr.ExitCode()
	}
	t.Fatalf("running %s failed to even start: %v", bin, err)
	return "", "", -1
}

// env returns a minimal, explicit environment for a subprocess: PATH, HOME,
// USER, and ROUNCE_DB when non-empty, plus any extra entries appended
// verbatim (e.g. "ROUNCE_SESSION=s1"). Deliberately not os.Environ() plus
// overrides -- inheriting the developer's real HOME or a real ROUNCE_DB is
// exactly the leak that makes these tests pass on one machine and fail on
// another. USER is part of the base set (not left to extra) because plan
// 07 onward's mutating commands resolve an actor via resolveActor, which
// falls back to $USER -- every existing call site up through plan 06's
// CLI tests keeps compiling unchanged because extra is variadic.
func env(home, rounceDB string, extra ...string) []string {
	e := []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"HOME=" + home,
		"USER=rounce-test",
	}
	if rounceDB != "" {
		e = append(e, "ROUNCE_DB="+rounceDB)
	}
	e = append(e, extra...)
	return e
}
