package interlock

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recorder is an interlock that writes its argv, one line per invocation.
func recorder(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "argv")
	bin = filepath.Join(dir, "interlock")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> " + log + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write recorder: %v", err)
	}
	return bin, log
}

func argv(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// TestPromptCannotSmuggleFlags checks that a prompt beginning with a dash
// reaches Interlock as the prompt, after the "--" sentinel, and not as
// flags.
//
// A prompt is whatever a person typed into a web form. Passed as a bare
// argument it is parsed as flags, so --env and --model become injectable
// and the prompt itself is discarded. The run would then do something
// other than what was asked. Quoting cannot fix it from this side: argv
// carries no quoting.
func TestPromptCannotSmuggleFlags(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	bin, log := recorder(t)
	t.Setenv(EnvBinary, bin)

	hostile := "--env SECRET_TOKEN --model evil"
	if err := Prompt(context.Background(), "codex", hostile, t.TempDir()); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	got := argv(t, log)
	sentinel := -1
	for i, a := range got {
		if a == "--" {
			sentinel = i
		}
	}
	if sentinel < 0 {
		t.Fatalf("no -- sentinel in argv: %v", got)
	}
	// Every flag ahead of the sentinel, the prompt behind it, whole.
	if got[len(got)-1] != hostile {
		t.Errorf("the prompt was split or reordered: %v", got)
	}
	if sentinel != len(got)-2 {
		t.Errorf("something follows the prompt: %v", got)
	}
	for _, a := range got[:sentinel] {
		if a == "SECRET_TOKEN" || a == "evil" {
			t.Errorf("part of the prompt reached the flag side: %v", got)
		}
	}
}

// TestLaunchPassesNoPromptSentinel: an item launch has no untrusted text,
// so it carries no sentinel and nothing after the flags.
func TestLaunchPassesNoPromptSentinel(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	bin, log := recorder(t)
	t.Setenv(EnvBinary, bin)

	if err := Run(context.Background(), "claude", "rnc-abc123", t.TempDir()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, a := range argv(t, log) {
		if a == "--" {
			t.Errorf("an item launch carries a prompt sentinel: %v", argv(t, log))
		}
	}
}

// TestLaunchRunsInTheProjectDirectory checks that the launched process runs
// in the project directory. Interlock's own --cwd is relative to the
// repository it resolves, so an absolute path is rejected outright, which
// is why the directory is set on the process instead.
func TestLaunchRunsInTheProjectDirectory(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	bin, log := recorder(t)
	t.Setenv(EnvBinary, bin)

	dir := t.TempDir()
	if err := Run(context.Background(), "claude", "rnc-abc123", dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, a := range argv(t, log) {
		if a == "--cwd" {
			t.Errorf("--cwd is passed as a flag; Interlock wants it relative and rejects an absolute path: %v", argv(t, log))
		}
	}
}

// TestUnknownAgentIsRefused before anything is spawned.
func TestUnknownAgentIsRefused(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	bin, _ := recorder(t)
	t.Setenv(EnvBinary, bin)

	if err := Prompt(context.Background(), "gpt5", "hello", t.TempDir()); err == nil {
		t.Error("an unknown agent was accepted")
	}
}

// launcher writes an interlock whose `run` prints a run id and whose
// `runs show` reports the given state.
func launcher(t *testing.T, state string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "interlock")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"runs\" ]; then\n" +
		"  echo 'id: run-abc'\n" +
		"  echo 'state: " + state + "'\n" +
		"  echo 'outcome: failed with exit code 1'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo 'rnc-x is running'\n" +
		"echo 'run: run-abc'\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write launcher: %v", err)
	}
	return bin
}

// TestLaunchReportsARunThatDiedImmediately checks that a launched run which
// exits straight away is reported as a failure.
//
// --no-follow returns as soon as the supervisor is up, so a zero exit means
// "launched", not "working". A run that fails on configuration (not logged
// in, no such model, a broken hook) does so within a second or two.
// Reporting those as started is the same overclaim as announcing an outcome
// nobody checked.
func TestLaunchReportsARunThatDiedImmediately(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	t.Setenv(EnvBinary, launcher(t, "failed"))

	err := Run(context.Background(), "claude", "rnc-x", t.TempDir())
	if err == nil {
		t.Fatal("a run that failed immediately was reported as started")
	}
	if !strings.Contains(err.Error(), "exit code 1") {
		t.Errorf("the failure does not say what happened: %v", err)
	}
}

// TestLaunchAcceptsARunThatIsStillGoing checks that a run still alive after
// the startup wait is reported as started. The outcome of a live run
// belongs in Interlock, not in a redirect.
func TestLaunchAcceptsARunThatIsStillGoing(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. Without a Windows stub the test is skipped
	// there rather than passing on nothing, so keep's Interlock
	// integration is unverified on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	t.Setenv(EnvBinary, launcher(t, "running"))

	if err := Run(context.Background(), "claude", "rnc-x", t.TempDir()); err != nil {
		t.Errorf("a running agent was reported as a failure: %v", err)
	}
}
