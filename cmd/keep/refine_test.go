package main

import (
	"slices"
	"strings"
	"testing"
)

// depsInSection parses the text block writeDepSection (show.go) writes under
// the given exact header line ("depends on" or "depended on by"), returning
// a map from dep type to every id listed under that type. The section runs
// from the header line to the next blank line: writeDepSection's shape is a
// blank line, a header, and one "<type>  <id>" line per dep, so a blank line
// ends the section being scanned.
func depsInSection(output, header string) map[string][]string {
	deps := map[string][]string{}
	inSection := false
	for _, l := range strings.Split(output, "\n") {
		if l == header {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.TrimSpace(l) == "" {
			break
		}
		fields := strings.Fields(l)
		if len(fields) == 2 {
			deps[fields[0]] = append(deps[fields[0]], fields[1])
		}
	}
	return deps
}

// blockedRowFor returns the full rendered line whose exact first field is
// id, from `blocked`'s plain-text output. It matches the exact first field,
// as idsFromLines/lineIndexOfID do, never a substring (an id is a literal
// prefix of its own children's ids).
func blockedRowFor(output, id string) (string, bool) {
	for _, l := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		fields := strings.Fields(l)
		if len(fields) > 0 && fields[0] == id {
			return l, true
		}
	}
	return "", false
}

// rowNamesBlocker reports whether row (a blocked row from blockedRowFor)
// ends with the renderItems-produced "← blocked by <blockerID>" suffix
// exactly.
func rowNamesBlocker(row, blockerID string) bool {
	return strings.HasSuffix(row, "← blocked by "+blockerID)
}

// TestRefineFromCapturedRoadmap proves refinement end to end against items
// that entered the store through `capture`, not against raw store-level
// fixtures: store.AddItem's dual parent-child/derived-from write and
// store.Blocked's tree propagation are proven at the store layer; this
// shows both reach a document captured through the `capture` command.
func TestRefineFromCapturedRoadmap(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	doc := "# Q3 roadmap\n\n" +
		"## Phase A\n\n" +
		"Ship the shared config loader.\n\n" +
		"## Phase B\n\n" +
		"Add integration coverage for it.\n"

	// The canonical invocation: title first, flags after, which permuteArgs
	// makes parse.
	docOut, stderr, code := runStdin(t, bin, repo, e, doc, "capture", "Q3 roadmap", "--type", "roadmap", "--body", "-")
	if code != 0 {
		t.Fatalf("capture: exit %d, stderr=%q", code, stderr)
	}
	docID := strings.TrimSpace(docOut)

	// Two tickets refined from the document, structure and provenance both
	// pointed at it in one `add` call each (the refinement recipe).
	t1 := mustAdd(t, bin, repo, e, "--parent", docID, "--derived-from", docID, "--score", "8/7/3", "Extract the shared config loader")
	t2 := mustAdd(t, bin, repo, e, "--parent", docID, "--derived-from", docID, "--score", "6/6/2", "Add integration tests for the loader")

	// Each ticket's show names both a parent-child and a derived-from edge
	// to the document: two separate edges, asserted by type rather than by
	// counting deps, since a count of 2 would also pass if both edges were
	// the same type.
	for _, tk := range []string{t1, t2} {
		out, stderr, code := run(t, bin, repo, e, "show", tk)
		if code != 0 {
			t.Fatalf("show %s: exit %d, stderr=%q", tk, code, stderr)
		}
		deps := depsInSection(out, "depends on")
		if got := deps["parent-child"]; !slices.Contains(got, docID) {
			t.Fatalf("show %s: parent-child targets = %v, want %q present\n%s", tk, got, docID, out)
		}
		if got := deps["derived-from"]; !slices.Contains(got, docID) {
			t.Fatalf("show %s: derived-from targets = %v, want %q present\n%s", tk, got, docID, out)
		}
	}

	// The document's own show names both tickets under "depended on by", so
	// provenance resolves in both directions.
	docShowOut, stderr, code := run(t, bin, repo, e, "show", docID)
	if code != 0 {
		t.Fatalf("show %s: exit %d, stderr=%q", docID, code, stderr)
	}
	dependedOnBy := depsInSection(docShowOut, "depended on by")
	var allTargets []string
	for _, ids := range dependedOnBy {
		allTargets = append(allTargets, ids...)
	}
	for _, tk := range []string{t1, t2} {
		if !slices.Contains(allTargets, tk) {
			t.Fatalf("show %s: depended-on-by targets = %v, want %q present\n%s", docID, allTargets, tk, docShowOut)
		}
	}

	// Both tickets appear in ready; the document never does.
	readyOut, stderr, code := run(t, bin, repo, e, "ready")
	if code != 0 {
		t.Fatalf("ready: exit %d, stderr=%q", code, stderr)
	}
	readyIDs := idsFromLines(readyOut)
	for _, tk := range []string{t1, t2} {
		if !slices.Contains(readyIDs, tk) {
			t.Fatalf("ready = %q, want %s present", readyOut, tk)
		}
	}
	if slices.Contains(readyIDs, docID) {
		t.Fatalf("ready = %q, want the captured roadmap %s absent", readyOut, docID)
	}

	// Blocking the document removes both refined tickets from
	// ready, and `blocked` names the document for each (not the underlying
	// blocker task). ticket1/ticket2 are not blocked directly; they are
	// blocked because their nearest ancestor (the document) is, which proves
	// the parent-child tree walk reaches items created through capture.
	// Absence from ready alone would also pass if blocking had deleted them,
	// so presence in `blocked` naming the document is checked too.
	blocker := mustAdd(t, bin, repo, e, "--type", "task", "--score", "3/3/1", "Blocker for the roadmap")
	mustDep(t, bin, repo, e, docID, "blocks", blocker)

	readyBlockedOut, stderr, code := run(t, bin, repo, e, "ready")
	if code != 0 {
		t.Fatalf("ready: exit %d, stderr=%q", code, stderr)
	}
	readyBlockedIDs := idsFromLines(readyBlockedOut)
	for _, tk := range []string{t1, t2} {
		if slices.Contains(readyBlockedIDs, tk) {
			t.Fatalf("ready (roadmap blocked) = %q, want %s absent", readyBlockedOut, tk)
		}
	}

	blockedOut, stderr, code := run(t, bin, repo, e, "blocked")
	if code != 0 {
		t.Fatalf("blocked: exit %d, stderr=%q", code, stderr)
	}
	for _, tk := range []string{t1, t2} {
		row, ok := blockedRowFor(blockedOut, tk)
		if !ok {
			t.Fatalf("blocked = %q, want a row for %s", blockedOut, tk)
		}
		if !rowNamesBlocker(row, docID) {
			t.Fatalf("blocked row for %s = %q, want it to name the document %s (nearest blocked ancestor)", tk, row, docID)
		}
	}

	// Closing the underlying blocker returns both tickets, which catches a
	// blockedTreeCTE that propagates blocking but never releases it.
	if _, stderr, code := run(t, bin, repo, e, "close", "--reason", "shipped", blocker); code != 0 {
		t.Fatalf("close %s: exit %d, stderr=%q", blocker, code, stderr)
	}

	readyAfterCloseOut, stderr, code := run(t, bin, repo, e, "ready")
	if code != 0 {
		t.Fatalf("ready: exit %d, stderr=%q", code, stderr)
	}
	readyAfterCloseIDs := idsFromLines(readyAfterCloseOut)
	for _, tk := range []string{t1, t2} {
		if !slices.Contains(readyAfterCloseIDs, tk) {
			t.Fatalf("ready (after closing the blocker) = %q, want %s present again", readyAfterCloseOut, tk)
		}
	}
	if slices.Contains(readyAfterCloseIDs, docID) {
		t.Fatalf("ready (after closing the blocker) = %q, want the captured roadmap %s still absent", readyAfterCloseOut, docID)
	}
}

// TestPromotedRawNoteLeavesBacklogEntersReady proves a raw note captured
// through `capture` (no --parent and no --derived-from, unlike the roadmap
// case above) leaves `backlog` and enters `ready` the moment it is promoted
// with `update --stage ticket`, and not before.
func TestPromotedRawNoteLeavesBacklogEntersReady(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	noteOut, stderr, code := run(t, bin, repo, e, "capture", "--type", "note", "A parked finding worth revisiting")
	if code != 0 {
		t.Fatalf("capture: exit %d, stderr=%q", code, stderr)
	}
	note := strings.TrimSpace(noteOut)

	backlogBefore, stderr, code := run(t, bin, repo, e, "backlog")
	if code != 0 {
		t.Fatalf("backlog: exit %d, stderr=%q", code, stderr)
	}
	if !slices.Contains(idsFromLines(backlogBefore), note) {
		t.Fatalf("backlog (before promotion) = %q, want %s present", backlogBefore, note)
	}

	readyBefore, stderr, code := run(t, bin, repo, e, "ready")
	if code != 0 {
		t.Fatalf("ready: exit %d, stderr=%q", code, stderr)
	}
	if slices.Contains(idsFromLines(readyBefore), note) {
		t.Fatalf("ready (before promotion) = %q, want %s absent", readyBefore, note)
	}

	if _, stderr, code := run(t, bin, repo, e, "update", "--stage", "ticket", "--score", "7/7/2", note); code != 0 {
		t.Fatalf("update --stage ticket: exit %d, stderr=%q", code, stderr)
	}

	backlogAfter, stderr, code := run(t, bin, repo, e, "backlog")
	if code != 0 {
		t.Fatalf("backlog: exit %d, stderr=%q", code, stderr)
	}
	if slices.Contains(idsFromLines(backlogAfter), note) {
		t.Fatalf("backlog (after promotion) = %q, want %s absent", backlogAfter, note)
	}

	readyAfter, stderr, code := run(t, bin, repo, e, "ready")
	if code != 0 {
		t.Fatalf("ready: exit %d, stderr=%q", code, stderr)
	}
	if !slices.Contains(idsFromLines(readyAfter), note) {
		t.Fatalf("ready (after promotion) = %q, want %s present", readyAfter, note)
	}
}
