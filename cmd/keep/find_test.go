package main

import (
	"strings"
	"testing"
)

// TestSearchFindsAndSeparatesItsStreams.
//
// GATE-04: a cross-process run, because the thing being checked is what a
// person and a script each see, and that only exists once the binary has
// actually run.
func TestSearchFindsAndSeparatesItsStreams(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	if _, stderr, code := run(t, bin, repo, e, "add", "--score", "9/8/2", "Expire refresh tokens"); code != 0 {
		t.Fatalf("add: exit %d, stderr=%q", code, stderr)
	}
	if _, stderr, code := run(t, bin, repo, e, "add", "--score", "1/1/1", "Something unrelated"); code != 0 {
		t.Fatalf("add: exit %d, stderr=%q", code, stderr)
	}

	stdout, stderr, code := run(t, bin, repo, e, "search", "token")
	if code != 0 {
		t.Fatalf("search: exit %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Expire refresh tokens") {
		t.Errorf("search stdout = %q, want the matching item", stdout)
	}
	if strings.Contains(stdout, "Something unrelated") {
		t.Errorf("search stdout = %q, want only the match", stdout)
	}

	// Several words search as one phrase, so nobody has to quote.
	stdout, _, code = run(t, bin, repo, e, "search", "refresh", "tokens")
	if code != 0 || !strings.Contains(stdout, "Expire refresh tokens") {
		t.Errorf("multi-word search returned %q (exit %d), want the match", stdout, code)
	}

	// A miss is announced on stderr and leaves stdout clean, so a script
	// piping stdout gets an empty result rather than prose.
	stdout, stderr, code = run(t, bin, repo, e, "search", "nothinglikethis")
	if code != 0 {
		t.Errorf("a search that found nothing exited %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a search that found nothing wrote %q to stdout, want nothing", stdout)
	}
	if !strings.Contains(stderr, "nothing matches") {
		t.Errorf("stderr = %q, want it to say nothing matched", stderr)
	}
}

// TestLogReportsWhatHappened. The question the tool could not answer before.
func TestLogReportsWhatHappened(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, _, code := run(t, bin, repo, e, "add", "--score", "9/8/2", "Expire refresh tokens")
	if code != 0 {
		t.Fatalf("add: exit %d", code)
	}
	id := strings.TrimSpace(stdout)
	if _, stderr, code := run(t, bin, repo, e, "close", id, "--reason", "shipped it"); code != 0 {
		t.Fatalf("close: exit %d, stderr=%q", code, stderr)
	}

	stdout, stderr, code := run(t, bin, repo, e, "log")
	if code != 0 {
		t.Fatalf("log: exit %d, stderr=%q", code, stderr)
	}
	for _, want := range []string{"close", "add", id, "Expire refresh tokens", "shipped it"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("log stdout = %q, want it to contain %q", stdout, want)
		}
	}
	// Newest first: the close must appear above the add.
	if strings.Index(stdout, "close") > strings.Index(stdout, "add") {
		t.Error("log prints oldest first, want newest first")
	}
}

// TestLogRejectsATimeItCannotRead. Guessing at "--since lastweek" would
// silently answer a different question than the one asked.
func TestLogRejectsATimeItCannotRead(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	_, stderr, code := run(t, bin, repo, e, "log", "--since", "lastweek")
	if code != 2 {
		t.Errorf("log --since lastweek exited %d, want 2", code)
	}
	if !strings.Contains(stderr, "cannot read") {
		t.Errorf("stderr = %q, want it to say the time could not be read", stderr)
	}
}

// TestStaleReportsButNeverReleases.
//
// The design point worth locking: keep never unclaims anything on its
// own. An automatic release cannot tell a dead session from a slow one, and
// putting two agents on one item is the exact failure keep exists to
// prevent.
func TestStaleReportsButNeverReleases(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, _, _ := run(t, bin, repo, e, "add", "--score", "9/8/2", "Expire refresh tokens")
	id := strings.TrimSpace(stdout)
	if _, stderr, code := run(t, bin, repo, e, "start", id); code != 0 {
		t.Fatalf("start: exit %d, stderr=%q", code, stderr)
	}

	// A threshold that catches everything already claimed.
	stdout, stderr, code := run(t, bin, repo, e, "stale", "--after", "1ns")
	if code != 0 {
		t.Fatalf("stale: exit %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, id) {
		t.Errorf("stale stdout = %q, want the claimed item", stdout)
	}
	if !strings.Contains(stderr, "keep close") || !strings.Contains(stderr, "keep drop") {
		t.Errorf("stale stderr = %q, want it to name both ways out", stderr)
	}

	// The claim is untouched: reporting is not acting.
	after, _, _ := run(t, bin, repo, e, "show", id, "--json")
	if !strings.Contains(after, `"status": "in_progress"`) && !strings.Contains(after, `"status":"in_progress"`) {
		t.Errorf("after `stale`, item is %q -- stale must report, never release", after)
	}
}
