package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// showDetail is the subset of model.ItemDetail's JSON shape these tests
// need: enough to count events by kind, read a payload's raw JSON, and read
// the item's own status/title/score. Kept local (not model.ItemDetail
// itself) so this file has no reliance on field ordering or unrelated
// columns -- just the ones these tests actually assert on.
type showDetail struct {
	Status string  `json:"status"`
	Title  string  `json:"title"`
	Score  int     `json:"score"`
	Events []event `json:"events"`
}

type event struct {
	Kind    string  `json:"kind"`
	Payload *string `json:"payload"`
}

// mustShow runs `keep show --json <id>` and unmarshals it into showDetail,
// failing the test immediately on any error.
func mustShow(t *testing.T, bin, repo string, e []string, id string) showDetail {
	t.Helper()
	stdout, stderr, code := run(t, bin, repo, e, "show", "--json", id)
	if code != 0 {
		t.Fatalf("show --json %s: exit %d, stderr=%q", id, code, stderr)
	}
	var d showDetail
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatalf("show --json %s: json.Unmarshal(%q): %v", id, stdout, err)
	}
	return d
}

// countEventsOfKind counts a show --json ItemDetail's events by kind.
// Counting through the real CLI's own JSON output -- not by reading the
// database directly -- is the whole point of TestStartClaimsAndIsIdempotent
// below: asserting "exit 0 twice" alone would also pass for an
// implementation that appends a second claim event, which is exactly the
// failure 02-CONTEXT.md decision 3 exists to prevent (02-06-SUMMARY.md
// proved this once at the store layer; this proves it again at the CLI
// boundary the plan is built on).
func countEventsOfKind(d showDetail, kind string) int {
	n := 0
	for _, ev := range d.Events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// TestStartClaimsAndIsIdempotent proves 02-CONTEXT.md decision 3's central
// claim: a same-session repeat of `start` succeeds again (exit 0) without
// writing a second `start` event.
func TestStartClaimsAndIsIdempotent(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_SESSION=s1")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "startable item")

	stdout, stderr, code := run(t, bin, repo, e, "start", id)
	if code != 0 {
		t.Fatalf("start: exit %d, stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("start: stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, id) {
		t.Fatalf("start stdout = %q, want it to name %q", stdout, id)
	}

	stdout2, stderr2, code2 := run(t, bin, repo, e, "start", id)
	if code2 != 0 {
		t.Fatalf("start (same-session retry): exit %d, want 0, stderr=%q", code2, stderr2)
	}
	if stderr2 != "" {
		t.Fatalf("start (same-session retry): stderr = %q, want empty", stderr2)
	}
	if !strings.Contains(stdout2, id) {
		t.Fatalf("start (same-session retry) stdout = %q, want it to name %q", stdout2, id)
	}

	d := mustShow(t, bin, repo, e, id)
	if got := countEventsOfKind(d, "start"); got != 1 {
		t.Fatalf("start events for %s = %d, want exactly 1 (idempotent retry must write NO second event)", id, got)
	}
}

// TestStartDifferentSessionConflicts proves a different session's start on
// an already-claimed item exits 1, prints nothing on stdout, and names the
// holding session on stderr.
func TestStartDifferentSessionConflicts(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e1 := env(home, "", "KEEP_SESSION=s1")
	e2 := env(home, "", "KEEP_SESSION=s2")
	mustInit(t, bin, repo, e1)

	id := mustAdd(t, bin, repo, e1, "contested item")

	if _, stderr, code := run(t, bin, repo, e1, "start", id); code != 0 {
		t.Fatalf("start (s1): exit %d, stderr=%q", code, stderr)
	}

	stdout, stderr, code := run(t, bin, repo, e2, "start", id)
	if code != 1 {
		t.Fatalf("start (s2, conflicting): exit %d, want 1 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("start (s2, conflicting): stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "s1") {
		t.Fatalf("start (s2, conflicting): stderr = %q, want it to name the holding session %q", stderr, "s1")
	}
}

// TestStartRefusesRawAndBlocked proves start on a raw item and start on a
// blocked item both exit 1 with a message identifying the cause.
func TestStartRefusesRawAndBlocked(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_SESSION=s1")
	mustInit(t, bin, repo, e)

	t.Run("raw", func(t *testing.T) {
		id := mustAdd(t, bin, repo, e, "--stage", "raw", "a raw idea")
		stdout, stderr, code := run(t, bin, repo, e, "start", id)
		if code != 1 {
			t.Fatalf("start raw item: exit %d, want 1 (stdout=%q stderr=%q)", code, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("start raw item: stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "raw") {
			t.Fatalf("start raw item: stderr = %q, want it to name the cause (raw)", stderr)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		blocker := mustAdd(t, bin, repo, e, "the blocker")
		id := mustAdd(t, bin, repo, e, "the blocked item")
		mustDep(t, bin, repo, e, id, "blocks", blocker)

		stdout, stderr, code := run(t, bin, repo, e, "start", id)
		if code != 1 {
			t.Fatalf("start blocked item: exit %d, want 1 (stdout=%q stderr=%q)", code, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("start blocked item: stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, blocker) {
			t.Fatalf("start blocked item: stderr = %q, want it to name the blocker %q", stderr, blocker)
		}
	})
}

// TestCloseAndDropAreDistinguishableOnCLI proves ITEM-08 end to end through
// the CLI: both close and drop remove an item from ready, but items.status
// and events.kind (and the --reason text carried in the event payload)
// stay distinguishable between the two verbs.
func TestCloseAndDropAreDistinguishableOnCLI(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	closedID := mustAdd(t, bin, repo, e, "closed item")
	droppedID := mustAdd(t, bin, repo, e, "dropped item")

	closeReason := "done for real"
	dropReason := "not worth doing"

	if _, stderr, code := run(t, bin, repo, e, "close", "--reason", closeReason, closedID); code != 0 {
		t.Fatalf("close: exit %d, stderr=%q", code, stderr)
	}
	if _, stderr, code := run(t, bin, repo, e, "drop", "--reason", dropReason, droppedID); code != 0 {
		t.Fatalf("drop: exit %d, stderr=%q", code, stderr)
	}

	readyStdout, readyStderr, readyCode := run(t, bin, repo, e, "ready", "--json")
	if readyCode != 0 {
		t.Fatalf("ready --json: exit %d, stderr=%q", readyCode, readyStderr)
	}
	var readyItems []map[string]any
	if err := json.Unmarshal([]byte(readyStdout), &readyItems); err != nil {
		t.Fatalf("ready --json: json.Unmarshal(%q): %v", readyStdout, err)
	}
	for _, it := range readyItems {
		if it["id"] == closedID || it["id"] == droppedID {
			t.Fatalf("ready --json still includes a closed/dropped item: %v", readyItems)
		}
	}

	closedDetail := mustShow(t, bin, repo, e, closedID)
	if closedDetail.Status != "closed" {
		t.Fatalf("closed item's status = %q, want %q", closedDetail.Status, "closed")
	}
	if countEventsOfKind(closedDetail, "close") != 1 {
		t.Fatalf("closed item's events = %v, want exactly one %q event", closedDetail.Events, "close")
	}
	if countEventsOfKind(closedDetail, "drop") != 0 {
		t.Fatalf("closed item wrote a %q event, want none", "drop")
	}
	foundReason := false
	for _, ev := range closedDetail.Events {
		if ev.Kind == "close" && ev.Payload != nil && strings.Contains(*ev.Payload, closeReason) {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("closed item's close event payload does not contain the --reason text %q: %v", closeReason, closedDetail.Events)
	}

	droppedDetail := mustShow(t, bin, repo, e, droppedID)
	if droppedDetail.Status != "dropped" {
		t.Fatalf("dropped item's status = %q, want %q", droppedDetail.Status, "dropped")
	}
	if countEventsOfKind(droppedDetail, "drop") != 1 {
		t.Fatalf("dropped item's events = %v, want exactly one %q event", droppedDetail.Events, "drop")
	}
	if countEventsOfKind(droppedDetail, "close") != 0 {
		t.Fatalf("dropped item wrote a %q event, want none", "close")
	}
	foundReason = false
	for _, ev := range droppedDetail.Events {
		if ev.Kind == "drop" && ev.Payload != nil && strings.Contains(*ev.Payload, dropReason) {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("dropped item's drop event payload does not contain the --reason text %q: %v", dropReason, droppedDetail.Events)
	}
}

// TestCloseWithoutReasonExitsTwo proves both an absent --reason and an
// explicit --reason "" exit 2 -- flag's *string default makes the two
// indistinguishable, so close.go must reject both explicitly.
func TestCloseWithoutReasonExitsTwo(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "no reason item")

	stdout, stderr, code := run(t, bin, repo, e, "close", id)
	if code != 2 {
		t.Fatalf("close without --reason: exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("close without --reason: stdout = %q, want empty", stdout)
	}

	stdout2, stderr2, code2 := run(t, bin, repo, e, "close", "--reason", "", id)
	if code2 != 2 {
		t.Fatalf("close --reason \"\": exit %d, want 2 (stdout=%q stderr=%q)", code2, stdout2, stderr2)
	}
	if stdout2 != "" {
		t.Fatalf("close --reason \"\": stdout = %q, want empty", stdout2)
	}
}

// TestUpdateChangesFieldsAndScore proves --title alone changes only the
// title (score untouched), and --score changes the score AND the resulting
// ordering consequence in ready -- asserting the stored score column alone
// would pass for a store that writes it and a ready view that ignores it.
func TestUpdateChangesFieldsAndScore(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	// score = value + confidence - effort + 11 (internal/rank).
	id := mustAdd(t, bin, repo, e, "--score", "5/5/5", "original title") // score 16

	stdout, stderr, code := run(t, bin, repo, e, "update", "--title", "new title", id)
	if code != 0 {
		t.Fatalf("update --title: exit %d, stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("update --title: stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, id) {
		t.Fatalf("update --title stdout = %q, want it to name %q", stdout, id)
	}

	afterTitle := mustShow(t, bin, repo, e, id)
	if afterTitle.Title != "new title" {
		t.Fatalf("after update --title: title = %q, want %q", afterTitle.Title, "new title")
	}
	if afterTitle.Score != 16 {
		t.Fatalf("after update --title: score = %d, want unchanged 16 (title-only update must not touch score)", afterTitle.Score)
	}

	// other's score is 30 (10/10/1); id is re-scored to 31 (10/10/0), so
	// the two are never tied and ready's ordering flip is unambiguous.
	other := mustAdd(t, bin, repo, e, "--score", "10/10/1", "other item") // score 30

	readyBefore, _, _ := run(t, bin, repo, e, "ready")
	beforeIDs := idsFromLines(readyBefore)
	if len(beforeIDs) == 0 || beforeIDs[0] != other {
		t.Fatalf("ready before re-score = %v, want %q first", beforeIDs, other)
	}

	_, stderr, code = run(t, bin, repo, e, "update", "--score", "10/10/0", id)
	if code != 0 {
		t.Fatalf("update --score: exit %d, stderr=%q", code, stderr)
	}

	afterScore := mustShow(t, bin, repo, e, id)
	if afterScore.Score != 31 {
		t.Fatalf("after update --score 10/10/0: score = %d, want 31", afterScore.Score)
	}

	readyAfter, _, _ := run(t, bin, repo, e, "ready")
	afterIDs := idsFromLines(readyAfter)
	if len(afterIDs) == 0 || afterIDs[0] != id {
		t.Fatalf("ready after re-score = %v, want %q first (score 31 > other's 30)", afterIDs, id)
	}
}

// TestUpdateNoFlagsExitsTwo proves `update <id>` with no field flags is a
// usage error, not a silent no-op (02-CONTEXT.md decision 4).
func TestUpdateNoFlagsExitsTwo(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "untouched item")

	stdout, stderr, code := run(t, bin, repo, e, "update", id)
	if code != 2 {
		t.Fatalf("update with no field flags: exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("update with no field flags: stdout = %q, want empty", stdout)
	}
}

// TestUpdateStatusFlagRejected proves --status is not a registered flag at
// all: flag's own "flag provided but not defined" error, exit 2.
func TestUpdateStatusFlagRejected(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "status-immune item")

	stdout, stderr, code := run(t, bin, repo, e, "update", "--status", "closed", id)
	if code != 2 {
		t.Fatalf("update --status: exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("update --status: stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "status") {
		t.Fatalf("update --status: stderr = %q, want it to name the unrecognized flag", stderr)
	}
}
