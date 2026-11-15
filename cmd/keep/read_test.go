package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// mustAdd runs `keep add [flags...] <title>` and fails the test immediately
// if it does not succeed, returning the trimmed id. Flags must precede the
// title (runAdd's documented invocation order), so callers pass flags first
// in flagsAndTitle, title last -- exactly the shape every add_test.go/
// dep_test.go call already uses.
func mustAdd(t *testing.T, bin, repo string, e []string, flagsAndTitle ...string) string {
	t.Helper()
	args := append([]string{"add"}, flagsAndTitle...)
	stdout, stderr, code := run(t, bin, repo, e, args...)
	if code != 0 {
		t.Fatalf("add %v: exit %d, stderr=%q", flagsAndTitle, code, stderr)
	}
	return strings.TrimSpace(stdout)
}

// mustDep runs `keep dep <a> <type> <b>` and fails the test immediately if
// it does not succeed.
func mustDep(t *testing.T, bin, repo string, e []string, a, depType, b string) {
	t.Helper()
	_, stderr, code := run(t, bin, repo, e, "dep", a, depType, b)
	if code != 0 {
		t.Fatalf("dep %s %s %s: exit %d, stderr=%q", a, depType, b, code, stderr)
	}
}

// idsFromLines extracts the first whitespace-separated field (the id column)
// from each non-empty line of renderItems' plain-text output, in order.
func idsFromLines(stdout string) []string {
	stdout = strings.TrimRight(stdout, "\n")
	if stdout == "" {
		return nil
	}
	lines := strings.Split(stdout, "\n")
	ids := make([]string, len(lines))
	for i, l := range lines {
		fields := strings.Fields(l)
		if len(fields) > 0 {
			ids[i] = fields[0]
		}
	}
	return ids
}

// TestReadyOrderAndLimit proves runReady renders store.Ready's own
// score-descending order untouched -- items are added in an order that
// deliberately does NOT match score order (so a created_at-based sort would
// produce a different, wrong result), and
// --limit trims the result without altering the order.
func TestReadyOrderAndLimit(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	// Insertion order (mid, low, high) deliberately differs from score
	// order (high, mid, low) added_at ordering: value+confidence-effort+11.
	mid := mustAdd(t, bin, repo, e, "--score", "5/5/5", "mid item")     // score 16
	low := mustAdd(t, bin, repo, e, "--score", "1/1/10", "low item")    // score 3
	high := mustAdd(t, bin, repo, e, "--score", "10/10/1", "high item") // score 30

	stdout, stderr, code := run(t, bin, repo, e, "ready")
	if code != 0 {
		t.Fatalf("ready: exit %d, stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("ready: stderr = %q, want empty", stderr)
	}
	gotIDs := idsFromLines(stdout)
	wantIDs := []string{high, mid, low}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("ready ids = %v, want %v", gotIDs, wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("ready ids = %v, want %v (score-descending)", gotIDs, wantIDs)
		}
	}

	limStdout, limStderr, limCode := run(t, bin, repo, e, "ready", "--limit", "2")
	if limCode != 0 {
		t.Fatalf("ready --limit 2: exit %d, stderr=%q", limCode, limStderr)
	}
	limIDs := idsFromLines(limStdout)
	wantLim := []string{high, mid}
	if len(limIDs) != len(wantLim) {
		t.Fatalf("ready --limit 2 ids = %v, want %v", limIDs, wantLim)
	}
	for i := range wantLim {
		if limIDs[i] != wantLim[i] {
			t.Fatalf("ready --limit 2 ids = %v, want %v", limIDs, wantLim)
		}
	}
}

// TestReadyEmptyIsExitZero proves an empty workable queue is a normal
// answer, not an error: exit 0, empty stdout, empty stderr,
// and --json prints "[]", never "null".
func TestReadyEmptyIsExitZero(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "ready")
	if code != 0 {
		t.Fatalf("ready: exit %d, want 0 (stderr=%q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("ready: stdout = %q, want empty", stdout)
	}
	if stderr != "" {
		t.Fatalf("ready: stderr = %q, want empty", stderr)
	}

	jsonStdout, jsonStderr, jsonCode := run(t, bin, repo, e, "ready", "--json")
	if jsonCode != 0 {
		t.Fatalf("ready --json: exit %d, want 0 (stderr=%q)", jsonCode, jsonStderr)
	}
	if jsonStderr != "" {
		t.Fatalf("ready --json: stderr = %q, want empty", jsonStderr)
	}
	if strings.TrimSpace(jsonStdout) != "[]" {
		t.Fatalf("ready --json: stdout = %q, want \"[]\"", jsonStdout)
	}
}

// TestReadyExcludesRawFromCLI proves an item added with --stage raw never
// appears in ready output through the CLI -- raw is never workable, enforced end to end,
// not just at the store layer.
func TestReadyExcludesRawFromCLI(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	rawID := mustAdd(t, bin, repo, e, "--stage", "raw", "raw idea")
	ticketID := mustAdd(t, bin, repo, e, "real ticket")

	stdout, stderr, code := run(t, bin, repo, e, "ready", "--json")
	if code != 0 {
		t.Fatalf("ready --json: exit %d, stderr=%q", code, stderr)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout, err)
	}
	found := map[string]bool{}
	for _, it := range items {
		found[it["id"].(string)] = true
	}
	if found[rawID] {
		t.Fatalf("ready --json included raw item %s: %v", rawID, items)
	}
	if !found[ticketID] {
		t.Fatalf("ready --json missing ticket item %s: %v", ticketID, items)
	}
}

// TestBlockedNamesBlockerOnCLI proves end to end through the CLI:
// after `dep <epic> blocks <blocker>`, the epic's child (blocked
// transitively, through the parent-child tree) appears in `blocked` output
// naming the EPIC's id, not the root blocker's id -- the nearest blocked
// ancestor, matching store.Blocked's own documented behaviour.
func TestBlockedNamesBlockerOnCLI(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	blocker := mustAdd(t, bin, repo, e, "the blocker")
	epic := mustAdd(t, bin, repo, e, "the epic")
	child := mustAdd(t, bin, repo, e, "--parent", epic, "the child")
	mustDep(t, bin, repo, e, epic, "blocks", blocker)

	stdout, stderr, code := run(t, bin, repo, e, "blocked")
	if code != 0 {
		t.Fatalf("blocked: exit %d, stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("blocked: stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "← blocked by "+epic) {
		t.Fatalf("blocked stdout = %q, want a line naming %q as the blocker", stdout, epic)
	}
	var childLine string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, child+" ") || strings.HasPrefix(line, child+"\t") {
			childLine = line
		}
	}
	if childLine == "" {
		t.Fatalf("blocked stdout = %q, want a line for child %s", stdout, child)
	}
	if !strings.Contains(childLine, "← blocked by "+epic) {
		t.Fatalf("child's blocked line = %q, want it to name %q, not the root blocker", childLine, epic)
	}

	jsonStdout, jsonStderr, jsonCode := run(t, bin, repo, e, "blocked", "--json")
	if jsonCode != 0 {
		t.Fatalf("blocked --json: exit %d, stderr=%q", jsonCode, jsonStderr)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(jsonStdout), &items); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", jsonStdout, err)
	}
	found := false
	for _, it := range items {
		if it["id"] == child {
			found = true
			if it["blocked_by"] != epic {
				t.Fatalf("blocked --json: child's blocked_by = %v, want %q", it["blocked_by"], epic)
			}
		}
	}
	if !found {
		t.Fatalf("blocked --json missing child %s: %v", child, items)
	}
}

// TestShowBodyVerbatim proves the body prints byte-for-byte after the "body"
// header -- multi-line, a blank line, a non-ASCII character, and a trailing
// newline all survive the round trip through `show`.
func TestShowBodyVerbatim(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	body := "line one\n\nline three with a non-ASCII char: café\n"
	id := mustAdd(t, bin, repo, e, "--body", body, "documented item")

	stdout, stderr, code := run(t, bin, repo, e, "show", id)
	if code != 0 {
		t.Fatalf("show: exit %d, stderr=%q", code, stderr)
	}
	marker := "\nbody\n"
	idx := strings.Index(stdout, marker)
	if idx == -1 {
		t.Fatalf("show stdout = %q, want it to contain %q", stdout, marker)
	}
	got := stdout[idx+len(marker):]
	if got != body {
		t.Fatalf("show body = %q, want %q (byte-for-byte)", got, body)
	}
}

// TestShowUnknownID proves show on an id that does not exist exits 1, prints
// nothing on stdout, and names the id on stderr.
func TestShowUnknownID(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "show", "rnc-000000")
	if code != 1 {
		t.Fatalf("show unknown id: exit %d, want 1 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("show unknown id: stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "rnc-000000") {
		t.Fatalf("show unknown id: stderr = %q, want it to name the id", stderr)
	}
}

// TestListFiltersCompose proves list's filters compose as AND, asserted as
// an exact id set. Every item `add` produces is status=open, so --stage
// and --repo are the two axes exercised here -- both settable at `add`
// time, giving a genuine two-filter AND composition through the real CLI.
func TestListFiltersCompose(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	matchBoth := mustAdd(t, bin, repo, e, "--stage", "ticket", "--repo", "repoX", "matches both")
	mustAdd(t, bin, repo, e, "--stage", "raw", "--repo", "repoX", "stage mismatch")
	mustAdd(t, bin, repo, e, "--stage", "ticket", "--repo", "repoY", "repo mismatch")
	mustAdd(t, bin, repo, e, "--stage", "raw", "--repo", "repoY", "matches neither")

	stdout, stderr, code := run(t, bin, repo, e, "list", "--stage", "ticket", "--repo", "repoX", "--json")
	if code != 0 {
		t.Fatalf("list: exit %d, stderr=%q", code, stderr)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout, err)
	}
	if len(items) != 1 {
		t.Fatalf("list --stage ticket --repo repoX ids = %v, want exactly [%s]", items, matchBoth)
	}
	if items[0]["id"] != matchBoth {
		t.Fatalf("list --stage ticket --repo repoX = %v, want [%s]", items, matchBoth)
	}
}

// TestReadJSONStreamPurity is the --json test that matters most: for all
// four read commands, --json's ENTIRE stdout must parse as JSON with no
// leading or trailing non-JSON bytes, and stderr must be empty. Unmarshalling
// the whole buffer (not a line, not a prefix) is what catches a stray
// fmt.Println.
func TestReadJSONStreamPurity(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "a stream-purity item")

	cases := []struct {
		name string
		args []string
	}{
		{"ready", []string{"ready", "--json"}},
		{"blocked", []string{"blocked", "--json"}},
		{"show", []string{"show", "--json", id}},
		{"list", []string{"list", "--json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := run(t, bin, repo, e, tc.args...)
			if code != 0 {
				t.Fatalf("%v: exit %d, stderr=%q", tc.args, code, stderr)
			}
			if stderr != "" {
				t.Fatalf("%v: stderr = %q, want empty", tc.args, stderr)
			}
			var v any
			if err := json.Unmarshal([]byte(stdout), &v); err != nil {
				t.Fatalf("%v: json.Unmarshal(entire stdout %q): %v", tc.args, stdout, err)
			}
		})
	}
}
