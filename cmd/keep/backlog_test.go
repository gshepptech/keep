package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// mustBacklogCapture runs `keep capture [flags...] <title>` and fails the test
// immediately if it does not succeed, returning the trimmed id. Mirrors
// mustAdd's (read_test.go) shape: flags first, title last.
func mustBacklogCapture(t *testing.T, bin, repo string, e []string, flagsAndTitle ...string) string {
	t.Helper()
	args := append([]string{"capture"}, flagsAndTitle...)
	stdout, stderr, code := run(t, bin, repo, e, args...)
	if code != 0 {
		t.Fatalf("capture %v: exit %d, stderr=%q", flagsAndTitle, code, stderr)
	}
	return strings.TrimSpace(stdout)
}

// backlogLines splits backlog's rendered stdout into its non-trailing-blank
// lines, preserving line index. Group headers ("roadmap", "note", ...) and
// item rows live in this same indexed list, which lets a test assert
// relative order (a header's line index vs. another header's, or one item
// row's line index vs. another's) rather than mere presence.
func backlogLines(stdout string) []string {
	stdout = strings.TrimRight(stdout, "\n")
	if stdout == "" {
		return nil
	}
	return strings.Split(stdout, "\n")
}

// lineIndexOfHeader returns the index of the line that is EXACTLY header
// (a bare group header line has no other content), or -1 if absent. An
// exact match, not strings.Contains, so a header line can never be
// confused with an item row whose title happens to contain the same word.
func lineIndexOfHeader(lines []string, header string) int {
	for i, l := range lines {
		if l == header {
			return i
		}
	}
	return -1
}

// lineIndexOfID returns the index of the line whose exact first
// whitespace-separated field is id, never a substring match. An id is a
// literal prefix of its own children's ids (rnc-a3f8e9 vs rnc-a3f8e9.1), so
// a substring/prefix match on the whole line would silently pass for the
// wrong row. idsFromLines (read_test.go) matches the same way.
func lineIndexOfID(lines []string, id string) int {
	for i, l := range lines {
		fields := strings.Fields(l)
		if len(fields) > 0 && fields[0] == id {
			return i
		}
	}
	return -1
}

// backlogFixture captures/adds the shared set of items every test in this
// file runs against: two notes and one roadmap captured in an interleaved
// order (note, roadmap, note, not two notes back to back, so a sort bug
// grouping by insertion order rather than by type cannot pass by accident),
// one raw task later dropped, one raw bug later closed, and one ordinary
// ticket (never raw, so it can never appear in backlog regardless of
// status).
type backlogFixture struct {
	firstNote   string // captured first, so must sort after secondNote
	roadmap     string
	secondNote  string // captured last, so must sort before firstNote
	droppedTask string
	closedBug   string
	ticket      string
}

func buildBacklogFixture(t *testing.T, bin, repo string, e []string) backlogFixture {
	t.Helper()
	var f backlogFixture

	f.firstNote = mustBacklogCapture(t, bin, repo, e, "--type", "note", "first note, captured earliest")
	f.roadmap = mustBacklogCapture(t, bin, repo, e, "--type", "roadmap", "Q3 roadmap")
	f.secondNote = mustBacklogCapture(t, bin, repo, e, "--type", "note", "second note, captured latest")

	f.droppedTask = mustBacklogCapture(t, bin, repo, e, "--type", "task", "a raw task, later dropped")
	if _, stderr, code := run(t, bin, repo, e, "drop", "--reason", "not needed", f.droppedTask); code != 0 {
		t.Fatalf("drop %s: exit %d, stderr=%q", f.droppedTask, code, stderr)
	}

	f.closedBug = mustBacklogCapture(t, bin, repo, e, "--type", "bug", "a raw bug, later closed")
	if _, stderr, code := run(t, bin, repo, e, "close", "--reason", "fixed elsewhere", f.closedBug); code != 0 {
		t.Fatalf("close %s: exit %d, stderr=%q", f.closedBug, code, stderr)
	}

	f.ticket = mustAdd(t, bin, repo, e, "an ordinary ticket")

	return f
}

// TestBacklogGroupsByTypeInSchemaOrder proves the roadmap header precedes
// the note header (schema-enum order, not alphabetical, where "note" would
// precede "roadmap"), and that only non-empty groups appear:
// no epic/task/bug header line, since this fixture's only task and bug are
// dropped/closed and excluded from backlog's scope entirely.
func TestBacklogGroupsByTypeInSchemaOrder(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	buildBacklogFixture(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "backlog")
	if code != 0 {
		t.Fatalf("backlog: exit %d, stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("backlog: stderr = %q, want empty", stderr)
	}

	lines := backlogLines(stdout)
	roadmapIdx := lineIndexOfHeader(lines, "roadmap")
	noteIdx := lineIndexOfHeader(lines, "note")
	if roadmapIdx == -1 {
		t.Fatalf("backlog stdout = %q, want a bare %q header", stdout, "roadmap")
	}
	if noteIdx == -1 {
		t.Fatalf("backlog stdout = %q, want a bare %q header", stdout, "note")
	}
	if roadmapIdx >= noteIdx {
		t.Fatalf("backlog: roadmap header at line %d, note header at line %d, want roadmap BEFORE note (schema-enum order)", roadmapIdx, noteIdx)
	}

	for _, absent := range []string{"epic", "task", "bug"} {
		if idx := lineIndexOfHeader(lines, absent); idx != -1 {
			t.Fatalf("backlog stdout = %q, want no %q header (empty groups print nothing), found at line %d", stdout, absent, idx)
		}
	}
}

// TestBacklogNewestFirstWithinGroup proves the later-captured note precedes
// the earlier one within the note group.
func TestBacklogNewestFirstWithinGroup(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	f := buildBacklogFixture(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "backlog")
	if code != 0 {
		t.Fatalf("backlog: exit %d, stderr=%q", code, stderr)
	}

	lines := backlogLines(stdout)
	secondIdx := lineIndexOfID(lines, f.secondNote)
	firstIdx := lineIndexOfID(lines, f.firstNote)
	if secondIdx == -1 {
		t.Fatalf("backlog stdout = %q, want a row for %s (captured last)", stdout, f.secondNote)
	}
	if firstIdx == -1 {
		t.Fatalf("backlog stdout = %q, want a row for %s (captured first)", stdout, f.firstNote)
	}
	if secondIdx >= firstIdx {
		t.Fatalf("backlog: second-captured note at line %d, first-captured note at line %d, want the later capture FIRST (newest first)", secondIdx, firstIdx)
	}
}

// TestBacklogExcludesClosedDroppedAndTickets proves neither the dropped raw
// task, the closed raw bug, nor the ordinary ticket appears anywhere in
// backlog's output.
func TestBacklogExcludesClosedDroppedAndTickets(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	f := buildBacklogFixture(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "backlog")
	if code != 0 {
		t.Fatalf("backlog: exit %d, stderr=%q", code, stderr)
	}

	lines := backlogLines(stdout)
	for name, id := range map[string]string{
		"dropped task": f.droppedTask,
		"closed bug":   f.closedBug,
		"ticket":       f.ticket,
	} {
		if idx := lineIndexOfID(lines, id); idx != -1 {
			t.Fatalf("backlog stdout = %q, want no row for the %s %s, found at line %d", stdout, name, id, idx)
		}
	}
}

// TestBacklogTypeFilter proves --type note shows both notes and no roadmap
// header.
func TestBacklogTypeFilter(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	f := buildBacklogFixture(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "backlog", "--type", "note")
	if code != 0 {
		t.Fatalf("backlog --type note: exit %d, stderr=%q", code, stderr)
	}

	lines := backlogLines(stdout)
	if idx := lineIndexOfHeader(lines, "roadmap"); idx != -1 {
		t.Fatalf("backlog --type note stdout = %q, want no roadmap header", stdout)
	}
	if idx := lineIndexOfID(lines, f.firstNote); idx == -1 {
		t.Fatalf("backlog --type note stdout = %q, want a row for %s", stdout, f.firstNote)
	}
	if idx := lineIndexOfID(lines, f.secondNote); idx == -1 {
		t.Fatalf("backlog --type note stdout = %q, want a row for %s", stdout, f.secondNote)
	}
	if idx := lineIndexOfID(lines, f.roadmap); idx != -1 {
		t.Fatalf("backlog --type note stdout = %q, want no row for the roadmap %s", stdout, f.roadmap)
	}
}

// TestBacklogTypeFilterRejectsUnknownType proves --type bogus exits 2 naming
// the legal types, before the store is opened: a nonexistent store path
// still produces the same usage error rather than a store-open failure.
func TestBacklogTypeFilterRejectsUnknownType(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	// Deliberately NOT mustInit-ed: --type validation must happen before
	// the store is opened, so an unknown type must still exit 2 here, not
	// fail with a "no store found" operational error.
	stdout, stderr, code := run(t, bin, repo, e, "backlog", "--type", "bogus")
	if code != 2 {
		t.Fatalf("backlog --type bogus: exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("backlog --type bogus: stdout = %q, want empty", stdout)
	}
	for _, want := range []string{"roadmap", "epic", "task", "bug", "note"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("backlog --type bogus: stderr = %q, want it to list legal type %q", stderr, want)
		}
	}
}

// TestBacklogJSONIsFlatArray proves --json emits a flat array (not a
// grouped object): unmarshalling the whole payload into []map[string]any
// must succeed, stderr must be empty, and the ids must appear in the same
// order the rendered output uses.
func TestBacklogJSONIsFlatArray(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	f := buildBacklogFixture(t, bin, repo, e)

	textStdout, textStderr, textCode := run(t, bin, repo, e, "backlog")
	if textCode != 0 {
		t.Fatalf("backlog: exit %d, stderr=%q", textCode, textStderr)
	}
	wantIDs := []string{}
	for _, l := range backlogLines(textStdout) {
		fields := strings.Fields(l)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "rnc-") {
			wantIDs = append(wantIDs, fields[0])
		}
	}
	if len(wantIDs) != 3 {
		t.Fatalf("backlog rendered output: want exactly 3 item rows (roadmap + 2 notes), got %v from %q", wantIDs, textStdout)
	}

	jsonStdout, jsonStderr, jsonCode := run(t, bin, repo, e, "backlog", "--json")
	if jsonCode != 0 {
		t.Fatalf("backlog --json: exit %d, stderr=%q", jsonCode, jsonStderr)
	}
	if jsonStderr != "" {
		t.Fatalf("backlog --json: stderr = %q, want empty", jsonStderr)
	}

	// The output must unmarshal into a flat []map[string]any. A grouped
	// {"note": [...], "roadmap": [...]} object would fail this unmarshal
	// (json.Unmarshal into a slice from a JSON object errors).
	var items []map[string]any
	if err := json.Unmarshal([]byte(jsonStdout), &items); err != nil {
		t.Fatalf("backlog --json: json.Unmarshal(%q) into []map[string]any: %v", jsonStdout, err)
	}
	if len(items) != len(wantIDs) {
		t.Fatalf("backlog --json items = %v, want %d entries matching the rendered output %v", items, len(wantIDs), wantIDs)
	}
	for i, want := range wantIDs {
		if items[i]["id"] != want {
			t.Fatalf("backlog --json items[%d].id = %v, want %q (same order as rendered output)", i, items[i]["id"], want)
		}
	}

	// None of the excluded items ever appear.
	for _, it := range items {
		id, _ := it["id"].(string)
		if id == f.droppedTask || id == f.closedBug || id == f.ticket {
			t.Fatalf("backlog --json included an excluded item: %v", items)
		}
	}
}

// TestBacklogEmptyStoreIsSilent proves an empty backlog prints nothing and
// exits 0, and --json prints "[]", never "null".
func TestBacklogEmptyStoreIsSilent(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "backlog")
	if code != 0 {
		t.Fatalf("backlog (empty store): exit %d, want 0 (stderr=%q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("backlog (empty store): stdout = %q, want empty", stdout)
	}
	if stderr != "" {
		t.Fatalf("backlog (empty store): stderr = %q, want empty", stderr)
	}

	jsonStdout, jsonStderr, jsonCode := run(t, bin, repo, e, "backlog", "--json")
	if jsonCode != 0 {
		t.Fatalf("backlog --json (empty store): exit %d, want 0 (stderr=%q)", jsonCode, jsonStderr)
	}
	if jsonStderr != "" {
		t.Fatalf("backlog --json (empty store): stderr = %q, want empty", jsonStderr)
	}
	if strings.TrimSpace(jsonStdout) != "[]" {
		t.Fatalf("backlog --json (empty store): stdout = %q, want \"[]\"", jsonStdout)
	}
}
