package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildRounce builds the real binary with the given version string baked in
// via -ldflags and returns its path. Every CLI test in this package goes
// through it: these tests exist to prove process-level behaviour, so nothing
// here may call runInit in-process.
func buildRounce(t *testing.T, version string) string {
	t.Helper()

	dst := filepath.Join(t.TempDir(), "rounce")
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

// env returns a minimal, explicit environment for a subprocess: PATH, HOME,
// and ROUNCE_DB when non-empty. Deliberately not os.Environ() plus
// overrides -- inheriting the developer's real HOME or a real ROUNCE_DB is
// exactly the leak that makes these tests pass on one machine and fail on
// another.
func env(home, rounceDB string) []string {
	e := []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"HOME=" + home,
	}
	if rounceDB != "" {
		e = append(e, "ROUNCE_DB="+rounceDB)
	}
	return e
}
