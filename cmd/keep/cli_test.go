package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// buildKeep builds the real binary with the given version string baked in
// via -ldflags and returns its path. Every CLI test in this package goes
// through it: these tests exist to prove process-level behaviour, so nothing
// here may call runInit in-process.
// tempDir is t.TempDir() with symlinks resolved.
//
// On macOS /var is a symlink to /private/var, so t.TempDir() hands back
// /var/folders/... while keep -- correctly, because the registry needs one
// canonical path per store -- reports /private/var/folders/.... A test
// comparing its own unresolved path against keep's resolved one fails on
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

// builtBinaries caches one compiled keep per -ldflags version, for the
// lifetime of the package's tests.
//
// buildKeep is called from most tests in this package. Running `go build`
// on every call takes minutes, and on Windows, where spawning a process is
// far more expensive, it can exceed `go test`'s 600-second timeout. The
// binaries are identical per version -- nothing here mutates one -- so
// building each exactly once is the same test with the waste removed.
var (
	builtMu       sync.Mutex
	builtBinaries = map[string]string{}
	builtDir      string
)

// TestMain owns the directory the cached binaries live in. It cannot be
// t.TempDir(): that belongs to one test, and these outlive every test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "keep-testbin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating the test binary dir: %v\n", err)
		os.Exit(1)
	}
	builtDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// buildKeep returns the path to a keep binary built with the given
// -ldflags version, compiling it the first time that version is asked for.
func buildKeep(t *testing.T, version string) string {
	t.Helper()

	builtMu.Lock()
	defer builtMu.Unlock()
	if path, ok := builtBinaries[version]; ok {
		return path
	}

	// .exe on Windows, or exec refuses to start what `go build` just wrote.
	// Found by CI's first run: every cross-process test failed there with
	// "executable file not found in %PATH%" while passing on the developer's
	// Mac, which is the whole reason the matrix exists.
	name := "keep"
	if version != "" {
		name += "-" + strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(version)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	dst := filepath.Join(builtDir, name)
	args := []string{"build", "-o", dst}
	if version != "" {
		args = append(args, "-ldflags", "-X main.version="+version)
	}
	// Most of this package's behaviour is tested in a subprocess, which
	// the in-process coverage instrument cannot see -- `go test -cover`
	// reports cmd/keep at a few percent and measures almost nothing. When
	// scripts/coverage.sh sets $KEEP_COVERDIR the binary is built
	// instrumented instead, so those runs report what they actually
	// exercised. Unset (every ordinary `go test`) this changes nothing.
	if coverDir() != "" {
		args = append(args, "-cover", "-coverpkg=./...")
	}
	args = append(args, ".")

	cmd := exec.Command("go", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building keep failed: %v\n%s", err, out)
	}
	builtBinaries[version] = dst
	return dst
}

// run executes bin with args, in dir, with env as its complete environment,
// and returns its stdout, stderr (captured separately -- the split is a
// requirement, not a formatting preference) and exit code.
func run(t *testing.T, bin, dir string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	// env is the process's COMPLETE environment on purpose, so a test can
	// prove what keep does with a variable absent. GOCOVERDIR is the one
	// addition, and only when the harness asked for an instrumented build:
	// an instrumented binary with nowhere to write drops its counters.
	cmd.Env = withCoverDir(env)

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

// runStdin is run's stdin-aware sibling: args is variadic-last
// in run's signature, so a stdin parameter cannot be inserted into it
// without touching every existing call site, and an unset cmd.Stdin
// correctly defaults to os.DevNull for every other command's test -- so run
// itself needs no change and stays correct.
//
// A non-*os.File io.Reader (strings.NewReader here) is copied to the
// child's stdin via an internal os/exec goroutine, so Wait() (inside
// cmd.Run()) blocks until copying finishes -- no manual pipe/goroutine
// plumbing is needed here, per os/exec's own documentation; a multi-megabyte
// payload round-trips byte-identically through a real os/exec-spawned
// process with no truncation.
func runStdin(t *testing.T, bin, dir string, env []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	// env is the process's COMPLETE environment on purpose, so a test can
	// prove what keep does with a variable absent. GOCOVERDIR is the one
	// addition, and only when the harness asked for an instrumented build:
	// an instrumented binary with nowhere to write drops its counters.
	cmd.Env = withCoverDir(env)
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
// USER, and KEEP_DB when non-empty, plus any extra entries appended
// verbatim (e.g. "KEEP_SESSION=s1"). Deliberately not os.Environ() plus
// overrides -- inheriting the developer's real HOME or a real KEEP_DB is
// exactly the leak that makes these tests pass on one machine and fail on
// another. USER is part of the base set (not left to extra) because
// mutating commands resolve an actor via resolveActor, which falls back to
// $USER.
func env(home, keepDB string, extra ...string) []string {
	e := []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"HOME=" + home,
		"USER=keep-test",
	}
	if keepDB != "" {
		e = append(e, "KEEP_DB="+keepDB)
	}
	e = append(e, extra...)
	return e
}

// coverDir reports where an instrumented binary should write its coverage
// counters, or "" when this run is not measuring. Read from the environment
// rather than a flag so it reaches every test in the package, including the
// ones that build a binary from a helper several calls deep.
func coverDir() string {
	return os.Getenv("KEEP_COVERDIR")
}

// withCoverDir appends GOCOVERDIR to a test's environment when, and only
// when, the binary under it was built with -cover. Each subprocess writes
// its own counter files into the shared directory; `go tool covdata` merges
// them, so no coordination is needed between concurrent tests.
func withCoverDir(env []string) []string {
	dir := coverDir()
	if dir == "" {
		return env
	}
	return append(append([]string(nil), env...), "GOCOVERDIR="+dir)
}
