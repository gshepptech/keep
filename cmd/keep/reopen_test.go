package main

import (
	"strings"
	"testing"
)

// TestReopenBringsBackAMisClose drives the real binary, because the point is
// the whole round trip: the item that vanished off the board comes back, it
// keeps its id, and the log still holds the close that was wrong.
func TestReopenBringsBackAMisClose(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_SESSION=sess-agent")
	mustInit(t, bin, repo, e)

	id, _, code := run(t, bin, repo, e, "add", "--score", "9/8/2", "Expire refresh tokens")
	if code != 0 {
		t.Fatalf("add exited %d", code)
	}
	id = strings.TrimSpace(id)

	if _, stderr, code := run(t, bin, repo, e, "close", id, "--reason", "wrong id, meant the other one"); code != 0 {
		t.Fatalf("close exited %d (stderr=%q)", code, stderr)
	}
	if out, _, _ := run(t, bin, repo, e, "ready"); strings.Contains(out, id) {
		t.Fatal("a closed item is still in ready; the rest of this test proves nothing")
	}

	out, stderr, code := run(t, bin, repo, e, "reopen", id, "--reason", "that close was a mistake")
	if code != 0 {
		t.Fatalf("reopen exited %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(out, id) {
		t.Errorf("reopen said %q, which does not name the item", out)
	}

	// Same id, back on the board. Filing a duplicate would also put work in
	// ready; only keeping the id proves the original came back.
	if out, _, _ := run(t, bin, repo, e, "ready"); !strings.Contains(out, id) {
		t.Errorf("the item is not back in ready after reopen:\n%s", out)
	}

	// The close is still on the record with the reason it was given.
	// Reopening says the close was wrong; it does not pretend it never
	// happened. `log` is the surface that renders reasons --
	// `show` lists the events but not their payloads.
	closeLog, _, _ := run(t, bin, repo, e, "log", "--kind", "close")
	if !strings.Contains(closeLog, "wrong id, meant the other one") {
		t.Errorf("the original close reason is gone from the log:\n%s", closeLog)
	}
	reopenLog, _, _ := run(t, bin, repo, e, "log", "--kind", "reopen")
	if !strings.Contains(reopenLog, id) {
		t.Errorf("no reopen event in the log:\n%s", reopenLog)
	}
	if !strings.Contains(reopenLog, "that close was a mistake") {
		t.Errorf("the reopen reason is not in the log:\n%s", reopenLog)
	}
}

// TestReopenRefusesWhatItCannotUndo pins the exit codes a script branches on.
func TestReopenRefusesWhatItCannotUndo(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_SESSION=sess-agent")
	mustInit(t, bin, repo, e)

	id, _, _ := run(t, bin, repo, e, "add", "Still open")
	id = strings.TrimSpace(id)

	// An open item has nothing to undo. Exit 1, not 2: the command was well
	// formed, the state was wrong.
	if _, stderr, code := run(t, bin, repo, e, "reopen", id, "--reason", "nothing to undo"); code != 1 {
		t.Errorf("reopen on an open item exited %d, want 1 (stderr=%q)", code, stderr)
	}

	// A missing or empty reason is a usage error, the same as close, drop and
	// unclaim. Reopening without one records that finished work came back and
	// nothing about why.
	if _, stderr, code := run(t, bin, repo, e, "reopen", id); code != 2 {
		t.Errorf("reopen with no --reason exited %d, want 2 (stderr=%q)", code, stderr)
	}
	if _, stderr, code := run(t, bin, repo, e, "reopen", id, "--reason", "   "); code != 2 {
		t.Errorf("reopen with a whitespace --reason exited %d, want 2 (stderr=%q)", code, stderr)
	}
	if _, _, code := run(t, bin, repo, e, "reopen", "--reason", "x"); code != 2 {
		t.Errorf("reopen with no id exited %d, want 2", code)
	}
}

// TestReopenClearsADeadClaim covers the contradiction the store tests found:
// close leaves claimed_by_session set, and carrying it back to `open` would
// show a holder for an item nobody holds.
func TestReopenClearsADeadClaim(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_SESSION=sess-gone")
	mustInit(t, bin, repo, e)

	id, _, _ := run(t, bin, repo, e, "add", "Expire refresh tokens")
	id = strings.TrimSpace(id)
	if _, _, code := run(t, bin, repo, e, "start", id); code != 0 {
		t.Fatalf("start exited %d", code)
	}
	if _, _, code := run(t, bin, repo, e, "close", id, "--reason", "done"); code != 0 {
		t.Fatalf("close exited %d", code)
	}
	if _, _, code := run(t, bin, repo, e, "reopen", id, "--reason", "not actually done"); code != 0 {
		t.Fatalf("reopen exited %d", code)
	}

	// A fresh session must be able to claim it. If the dead session's hold
	// survived, start would report someone else already has it.
	fresh := env(home, "", "KEEP_SESSION=sess-new")
	if _, stderr, code := run(t, bin, repo, fresh, "start", id); code != 0 {
		t.Errorf("a new session cannot claim the reopened item: exit %d (stderr=%q)", code, stderr)
	}
}
