package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gshepptech/rounce/internal/model"
)

// showJSON runs `rounce show <id> --json` (a third, ordinary, read-only CLI
// invocation, legitimate against an item written by either surface) and
// decodes its stdout into a model.ItemDetail.
func showItemDetailJSON(t *testing.T, bin, dir string, environ []string, id string) model.ItemDetail {
	t.Helper()
	stdout, stderr, code := run(t, bin, dir, environ, "show", id, "--json")
	if code != 0 {
		t.Fatalf("show %s --json: exit %d, stderr=%s", id, code, stderr)
	}
	var detail model.ItemDetail
	if err := json.Unmarshal([]byte(stdout), &detail); err != nil {
		t.Fatalf("decode show --json output for %s: %v\nraw: %s", id, err, stdout)
	}
	return detail
}

// assertEventsMatchExceptIDAndAt compares two events written through the two
// different surfaces (CLI vs MCP) for the same logical operation. wantKind
// names the kind both are expected to be.
//
// id and at are deliberately excluded from the comparison, named here rather
// than silently skipped:
//
//   - id is INTEGER PRIMARY KEY AUTOINCREMENT (internal/store/schema.go) and
//     is globally unique across the two writes -- both events land in the
//     same events table, one write per surface -- so it cannot match, and
//     its differing is correct, not a defect.
//   - at is a wall-clock timestamp (model.Now(), written by
//     internal/store/events.go's writeEvent at the moment of the write); the
//     CLI path and the MCP path run at genuinely different real times, so it
//     cannot match either.
//
// Every OTHER column MUST match, and payload in particular is NOT skipped:
// closeDropEventPayload (internal/store/transitions.go) and addEventPayload
// (internal/store/items.go) are both fixed structs, so identical inputs
// marshal to byte-identical JSON deterministically -- the payload check is
// real, not vacuous.
func assertEventsMatchExceptIDAndAt(t *testing.T, wantKind string, cli, mcpEv model.Event) {
	t.Helper()

	if cli.Kind != wantKind {
		t.Errorf("CLI-path event kind = %q, want %q", cli.Kind, wantKind)
	}
	if mcpEv.Kind != wantKind {
		t.Errorf("MCP-path event kind = %q, want %q", mcpEv.Kind, wantKind)
	}
	if cli.Kind != mcpEv.Kind {
		t.Errorf("event kind mismatch: CLI=%q MCP=%q", cli.Kind, mcpEv.Kind)
	}

	if cli.Actor != mcpEv.Actor {
		t.Errorf("%s event actor mismatch: CLI=%q MCP=%q", wantKind, cli.Actor, mcpEv.Actor)
	}

	// session_id: proven non-nil on BOTH sides BEFORE comparing values. An
	// empty ROUNCE_SESSION stores SQL NULL, not "" (internal/store/
	// transitions.go's nullableString) -- comparing two nils would PASS
	// while proving nothing about the session actually carrying through.
	// This is exactly the vacuous-pass shape this test must not have.
	if cli.SessionID == nil {
		t.Fatalf("CLI-path %s event session_id is nil, want non-nil (\"parity-session\")", wantKind)
	}
	if mcpEv.SessionID == nil {
		t.Fatalf("MCP-path %s event session_id is nil, want non-nil (\"parity-session\")", wantKind)
	}
	if *cli.SessionID != *mcpEv.SessionID {
		t.Errorf("%s event session_id mismatch: CLI=%q MCP=%q", wantKind, *cli.SessionID, *mcpEv.SessionID)
	}

	// payload: byte-for-byte, both nil or both an identical string.
	switch {
	case cli.Payload == nil && mcpEv.Payload == nil:
		// Both nil is a legitimate match for an event kind with no payload.
	case cli.Payload == nil || mcpEv.Payload == nil:
		t.Errorf("%s event payload nil-ness mismatch: CLI=%v MCP=%v", wantKind, cli.Payload, mcpEv.Payload)
	case *cli.Payload != *mcpEv.Payload:
		t.Errorf("%s event payload mismatch: CLI=%q MCP=%q", wantKind, *cli.Payload, *mcpEv.Payload)
	}
}

// TestCLIAndMCPWriteIdenticalEventRows proves MCP-03: the same logical
// operation (add, then close), driven through the CLI and through MCP with
// the same explicit actor and the same fixed session, writes `events` rows
// that differ only in the two columns that cannot match (id, at).
//
// docs/design/phase-3.md is explicit that this must be "asserted by test,
// comparing rows, not by reading output" -- two surfaces can print identical
// text while writing a different actor or a NULL session_id underneath, and
// a rendered-output comparison would never catch that.
func TestCLIAndMCPWriteIdenticalEventRows(t *testing.T) {
	bin := buildRounce(t, "")
	dir := t.TempDir()

	// One store, one HOME, one ROUNCE_SESSION, shared by BOTH paths -- so
	// nothing about the environment itself can differ between them.
	environ := env(dir, "", "ROUNCE_SESSION=parity-session")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	// Path A: the CLI. Two real processes. --actor is explicit, not left to
	// $USER/$ROUNCE_ACTOR fallback -- 04-CONTEXT.md decision 3's point is
	// that making actor explicit on both sides is what makes MCP-03
	// "precisely testable": two fallbacks agreeing would prove nothing about
	// the mechanism, only that they happened to agree.
	stdoutAdd, stderrAdd, code := run(t, bin, dir, environ, "add", "Parity A", "--actor", "parity-actor")
	if code != 0 {
		t.Fatalf("add (CLI path): exit %d, stderr=%s", code, stderrAdd)
	}
	idA := strings.TrimSpace(stdoutAdd)

	_, stderrClose, code := run(t, bin, dir, environ, "close", idA, "--reason", "finished", "--actor", "parity-actor")
	if code != 0 {
		t.Fatalf("close (CLI path): exit %d, stderr=%s", code, stderrClose)
	}

	// Path B: MCP. ONE rounce mcp session, two tool calls, same explicit
	// actor supplied per-call rather than relying on the server's default.
	cs := mcpSession(t, bin, dir, environ)

	text := callTool(t, cs, "add", map[string]any{"title": "Parity B", "actor": "parity-actor"})
	var itemB model.Item
	decodeInto(t, text, &itemB)
	idB := itemB.ID

	callTool(t, cs, "close", map[string]any{"id": idB, "reason": "finished", "actor": "parity-actor"})

	// Read back through a third, ordinary CLI invocation -- neither
	// mutating path is trusted to have reported its own effect honestly.
	detailA := showItemDetailJSON(t, bin, dir, environ, idA)
	detailB := showItemDetailJSON(t, bin, dir, environ, idB)

	if len(detailA.Events) < 2 {
		t.Fatalf("item A (CLI path) has %d events, want at least 2 (add, close)", len(detailA.Events))
	}
	if len(detailB.Events) < 2 {
		t.Fatalf("item B (MCP path) has %d events, want at least 2 (add, close)", len(detailB.Events))
	}

	// The FIRST event of each (both "add") -- covers a creating mutation.
	assertEventsMatchExceptIDAndAt(t, "add", detailA.Events[0], detailB.Events[0])
	// The LAST event of each (both "close") -- covers a terminal mutation.
	assertEventsMatchExceptIDAndAt(t, "close", detailA.Events[len(detailA.Events)-1], detailB.Events[len(detailB.Events)-1])
}
