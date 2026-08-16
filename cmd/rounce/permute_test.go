package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// showJSON runs `rounce show <id> --json` and decodes it into a map, then
// strips the fields that legitimately vary between two otherwise-identical
// items (id, timestamps) so two items created through different argument
// orders can be compared for result equality rather than exit-code-only.
func showJSON(t *testing.T, bin, repo string, e []string, id string) map[string]any {
	t.Helper()
	stdout, stderr, code := run(t, bin, repo, e, "show", id, "--json")
	if code != 0 {
		t.Fatalf("show %s --json: exit %d, stderr=%q", id, code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("show %s --json: json.Unmarshal(%q): %v", id, stdout, err)
	}
	// id/timestamps are unique per item by construction, and events carries
	// each event's own id/at/item_id -- none of that is expected to match
	// between two SEPARATE items even when they were created identically,
	// so all are stripped before a result-equality comparison.
	for _, volatile := range []string{"id", "created_at", "updated_at", "claimed_at", "closed_at", "claimed_by_session", "events"} {
		delete(got, volatile)
	}
	return got
}

func mapsEqual(t *testing.T, a, b map[string]any) bool {
	t.Helper()
	aj, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bj, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	return string(aj) == string(bj)
}

// TestFlagsAfterPositionalShow proves `show <id> --json` and
// `show --json <id>` produce identical results, not just identical exit
// codes -- an exit-0 that silently dropped a flag would pass a code-only
// check, the same failure shape the naive permuter's silent corruption
// would have produced.
func TestFlagsAfterPositionalShow(t *testing.T) {
	bin := buildRounce(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "an item to show")

	stdoutFlagsAfter, stderr, code := run(t, bin, repo, e, "show", id, "--json")
	if code != 0 {
		t.Fatalf("show <id> --json: exit %d, stderr=%q", code, stderr)
	}
	stdoutFlagsBefore, stderr, code := run(t, bin, repo, e, "show", "--json", id)
	if code != 0 {
		t.Fatalf("show --json <id>: exit %d, stderr=%q", code, stderr)
	}
	if stdoutFlagsAfter != stdoutFlagsBefore {
		t.Fatalf("show <id> --json produced different output than show --json <id>:\nafter:  %q\nbefore: %q", stdoutFlagsAfter, stdoutFlagsBefore)
	}
}

// TestFlagsAfterPositionalAdd proves `add "a title" --type bug` and
// `add --type bug "a title"` create the identical item (compared through
// show --json, ignoring id/timestamps), not just that both exit 0.
func TestFlagsAfterPositionalAdd(t *testing.T) {
	bin := buildRounce(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "add", "a title", "--type", "bug")
	if code != 0 {
		t.Fatalf(`add "a title" --type bug: exit %d, stderr=%q`, code, stderr)
	}
	idFlagsAfter := strings.TrimSpace(stdout)

	stdout, stderr, code = run(t, bin, repo, e, "add", "--type", "bug", "a title")
	if code != 0 {
		t.Fatalf(`add --type bug "a title": exit %d, stderr=%q`, code, stderr)
	}
	idFlagsBefore := strings.TrimSpace(stdout)

	got := showJSON(t, bin, repo, e, idFlagsAfter)
	want := showJSON(t, bin, repo, e, idFlagsBefore)
	if !mapsEqual(t, got, want) {
		t.Fatalf("add \"a title\" --type bug produced a different item than add --type bug \"a title\":\nflags-after:  %v\nflags-before: %v", got, want)
	}
	if got["type"] != "bug" {
		t.Fatalf("type = %v, want %q", got["type"], "bug")
	}
}

// TestFlagsAfterPositionalMutations proves start/close/drop/update each
// exit 0 with their flag placed after the positional id, asserting the
// resulting state through show --json rather than trusting exit codes
// alone.
func TestFlagsAfterPositionalMutations(t *testing.T) {
	bin := buildRounce(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "ROUNCE_SESSION=s1")
	mustInit(t, bin, repo, e)

	startID := mustAdd(t, bin, repo, e, "start target")
	if _, stderr, code := run(t, bin, repo, e, "start", startID, "--json"); code != 0 {
		t.Fatalf("start <id> --json: exit %d, stderr=%q", code, stderr)
	}
	got := showJSON(t, bin, repo, e, startID)
	if got["status"] != "in_progress" {
		t.Fatalf("after start <id> --json: status = %v, want %q", got["status"], "in_progress")
	}

	closeID := mustAdd(t, bin, repo, e, "close target")
	if _, stderr, code := run(t, bin, repo, e, "close", closeID, "--reason", "done"); code != 0 {
		t.Fatalf("close <id> --reason done: exit %d, stderr=%q", code, stderr)
	}
	got = showJSON(t, bin, repo, e, closeID)
	if got["status"] != "closed" {
		t.Fatalf("after close <id> --reason done: status = %v, want %q", got["status"], "closed")
	}

	dropID := mustAdd(t, bin, repo, e, "drop target")
	if _, stderr, code := run(t, bin, repo, e, "drop", dropID, "--reason", "no"); code != 0 {
		t.Fatalf("drop <id> --reason no: exit %d, stderr=%q", code, stderr)
	}
	got = showJSON(t, bin, repo, e, dropID)
	if got["status"] != "dropped" {
		t.Fatalf("after drop <id> --reason no: status = %v, want %q", got["status"], "dropped")
	}

	updateID := mustAdd(t, bin, repo, e, "update target")
	if _, stderr, code := run(t, bin, repo, e, "update", updateID, "--title", "renamed"); code != 0 {
		t.Fatalf("update <id> --title renamed: exit %d, stderr=%q", code, stderr)
	}
	got = showJSON(t, bin, repo, e, updateID)
	if got["title"] != "renamed" {
		t.Fatalf("after update <id> --title renamed: title = %v, want %q", got["title"], "renamed")
	}
}

// TestFlagsAfterPositionalNoPositionalCommandsUnaffected proves permuteArgs
// is a no-op for commands with no positional argument at all: list --stage
// raw and ready --limit 1 must behave exactly as before.
func TestFlagsAfterPositionalNoPositionalCommandsUnaffected(t *testing.T) {
	bin := buildRounce(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	mustAdd(t, bin, repo, e, "--stage", "raw", "a raw item")
	mustAdd(t, bin, repo, e, "a ticket item")

	stdout, stderr, code := run(t, bin, repo, e, "list", "--stage", "raw")
	if code != 0 {
		t.Fatalf("list --stage raw: exit %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "a raw item") {
		t.Fatalf("list --stage raw: stdout = %q, want it to contain the raw item", stdout)
	}
	if strings.Contains(stdout, "a ticket item") {
		t.Fatalf("list --stage raw: stdout = %q, want it NOT to contain the ticket item", stdout)
	}

	stdout, stderr, code = run(t, bin, repo, e, "ready", "--limit", "1")
	if code != 0 {
		t.Fatalf("ready --limit 1: exit %d, stderr=%q", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("ready --limit 1: stdout = %q, want exactly one item line", stdout)
	}
}

// TestFlagsAfterPositionalDepUnaffected proves a three-positional, no-flag
// invocation (dep <a> blocks <b>) is unaffected by permuteArgs.
func TestFlagsAfterPositionalDepUnaffected(t *testing.T) {
	bin := buildRounce(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	a := mustAdd(t, bin, repo, e, "item a")
	b := mustAdd(t, bin, repo, e, "item b")

	stdout, stderr, code := run(t, bin, repo, e, "dep", a, "blocks", b)
	if code != 0 {
		t.Fatalf("dep <a> blocks <b>: exit %d, stderr=%q", code, stderr)
	}
	want := a + " blocks " + b + "\n"
	if stdout != want {
		t.Fatalf("dep <a> blocks <b>: stdout = %q, want %q", stdout, want)
	}
}

// TestPermuteDashDashWeirdTitle proves `add -- --weird-title` still creates
// an item whose title is exactly "--weird-title".
func TestPermuteDashDashWeirdTitle(t *testing.T) {
	bin := buildRounce(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "add", "--", "--weird-title")
	if code != 0 {
		t.Fatalf("add -- --weird-title: exit %d, stderr=%q", code, stderr)
	}
	id := strings.TrimSpace(stdout)

	got := showJSON(t, bin, repo, e, id)
	if got["title"] != "--weird-title" {
		t.Fatalf("title = %v, want %q", got["title"], "--weird-title")
	}
}

// TestPermuteMissingValueDoesNotCorrupt proves `add "title" --body` (a
// value-taking flag with nothing after it, a genuine user error) exits 0
// and stores a body that is empty -- specifically NOT "--" -- with --body
// folded into the title, exactly matching this exact malformed
// invocation's behaviour before this plan. The fix's contract is "never
// silently wrong", not "now accepts everything" -- this test deliberately
// asserts UNCHANGED, pre-fix behaviour for a malformed invocation.
func TestPermuteMissingValueDoesNotCorrupt(t *testing.T) {
	bin := buildRounce(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "add", "title", "--body")
	if code != 0 {
		t.Fatalf(`add "title" --body: exit %d, stderr=%q`, code, stderr)
	}
	id := strings.TrimSpace(stdout)

	got := showJSON(t, bin, repo, e, id)
	if got["body"] != "" {
		t.Fatalf(`body = %v, want "" (specifically not "--")`, got["body"])
	}
	if got["title"] != "title --body" {
		t.Fatalf(`title = %v, want "title --body"`, got["title"])
	}
}
