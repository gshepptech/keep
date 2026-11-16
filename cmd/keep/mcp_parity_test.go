package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gshepptech/keep/internal/model"
)

// showItemDetailJSON runs `keep show <id> --json` (a third, ordinary,
// read-only CLI invocation, legitimate against an item written by either
// surface) and decodes its stdout into a model.ItemDetail.
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
// id and at are excluded from the comparison, and named here rather than
// silently skipped:
//
//   - id is INTEGER PRIMARY KEY AUTOINCREMENT (internal/store/schema.go) and
//     is unique across the two writes, since both events land in the same
//     events table, so it cannot match.
//   - at is a wall-clock timestamp (model.Now(), written by
//     internal/store/events.go's writeEvent at the moment of the write); the
//     CLI path and the MCP path run at different real times, so it cannot
//     match either.
//
// Every other column must match, and payload in particular is not skipped:
// closeDropEventPayload (internal/store/transitions.go) and addEventPayload
// (internal/store/items.go) are both fixed structs, so identical inputs
// marshal to byte-identical JSON, and the payload check is real.
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

	// session_id: checked non-nil on both sides before comparing values. An
	// empty KEEP_SESSION stores SQL NULL, not "" (internal/store/
	// transitions.go's nullableString), and comparing two nils would pass
	// while proving nothing about the session carrying through.
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

// TestCLIAndMCPWriteIdenticalEventRows proves the same logical
// operation (add, then close), driven through the CLI and through MCP with
// the same explicit actor and the same fixed session, writes `events` rows
// that differ only in the two columns that cannot match (id, at).
//
// This is asserted by comparing rows, not by reading output: two surfaces
// can print identical text while writing a different actor or a NULL
// session_id underneath, and a rendered-output comparison would not catch
// that.
func TestCLIAndMCPWriteIdenticalEventRows(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)

	// One store, one HOME, one KEEP_SESSION, shared by both paths, so
	// nothing about the environment itself can differ between them.
	environ := env(dir, "", "KEEP_SESSION=parity-session")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	// Path A: the CLI. Two real processes. --actor is explicit, not left to
	// $USER/$KEEP_ACTOR fallback, on both sides, because two fallbacks
	// agreeing would prove nothing about the mechanism.
	stdoutAdd, stderrAdd, code := run(t, bin, dir, environ, "add", "Parity A", "--actor", "parity-actor")
	if code != 0 {
		t.Fatalf("add (CLI path): exit %d, stderr=%s", code, stderrAdd)
	}
	idA := strings.TrimSpace(stdoutAdd)

	_, stderrClose, code := run(t, bin, dir, environ, "close", idA, "--reason", "finished", "--actor", "parity-actor")
	if code != 0 {
		t.Fatalf("close (CLI path): exit %d, stderr=%s", code, stderrClose)
	}

	// Path B: MCP. ONE keep mcp session, two tool calls, same explicit
	// actor supplied per-call rather than relying on the server's default.
	cs := mcpSession(t, bin, dir, environ)

	text := callTool(t, cs, "add", map[string]any{"title": "Parity B", "actor": "parity-actor"})
	var itemB model.Item
	decodeInto(t, text, &itemB)
	idB := itemB.ID

	callTool(t, cs, "close", map[string]any{"id": idB, "reason": "finished", "actor": "parity-actor"})

	// Read back through a third, ordinary CLI invocation, so neither
	// mutating path is trusted to report its own effect.
	detailA := showItemDetailJSON(t, bin, dir, environ, idA)
	detailB := showItemDetailJSON(t, bin, dir, environ, idB)

	if len(detailA.Events) < 2 {
		t.Fatalf("item A (CLI path) has %d events, want at least 2 (add, close)", len(detailA.Events))
	}
	if len(detailB.Events) < 2 {
		t.Fatalf("item B (MCP path) has %d events, want at least 2 (add, close)", len(detailB.Events))
	}

	// The first event of each (both "add") covers a creating mutation.
	assertEventsMatchExceptIDAndAt(t, "add", detailA.Events[0], detailB.Events[0])
	// The last event of each (both "close") covers a terminal mutation.
	assertEventsMatchExceptIDAndAt(t, "close", detailA.Events[len(detailA.Events)-1], detailB.Events[len(detailB.Events)-1])
}

// TestMCPMalformedArgumentsAreProtocolErrorsAndChangeNothing checks that a
// tool call with malformed arguments fails as a protocol error and leaves the
// store unchanged. It checks three separate things:
//
//  1. a protocol error, specifically, not an isError tool result. This is
//     why internal/mcp uses the SDK's low-level Server.AddTool path
//     (internal/mcp/server.go): the generic mcp.AddTool[In,Out] routes a
//     JSON type mismatch into isError:true, through a code path that never
//     calls the tool handler.
//  2. not a panic, shown by the server still answering a normal call on
//     the same session afterwards. A panicking handler would take the
//     process down and that call would fail.
//  3. the store unchanged, shown by a byte-identical before/after snapshot
//     of `show --json` (item row, both dependency directions, and the
//     complete event history) and `list --json` (the item set). A handler
//     that writes and then fails to marshal would still look like a clean
//     failure from the client's side; only the store snapshot catches that.
//
// Not asserted here: that the SDK rejects a schema-invalid argument. It does
// not. mcp.Server.AddTool performs no argument validation, and a published
// `enum` and `additionalProperties:false` are both ignored on a call.
// Enforcement for the one closed vocabulary that matters here (dep's type)
// comes from store.CreateDep's ErrInvalidDepType, which is why case 2 below
// exists and why internal/mcp/errors.go's resultFor routes that sentinel to a
// protocol error rather than to isError.
func TestMCPMalformedArgumentsAreProtocolErrorsAndChangeNothing(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=malformed-session")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	// A store holding one known item, written before the session under
	// test even starts.
	stdoutAdd, stderrAdd, code := run(t, bin, dir, environ, "add", "Known item", "--actor", "malformed-actor")
	if code != 0 {
		t.Fatalf("add (seed item): exit %d, stderr=%s", code, stderrAdd)
	}
	id := strings.TrimSpace(stdoutAdd)

	// Before-snapshot: raw strings, not decoded structs. A struct comparison
	// only catches fields someone thought to assert on; a raw whole-buffer
	// string comparison catches a change to ANY field of ANY event,
	// including one nobody thought to assert.
	beforeShow, stderrShow, code := run(t, bin, dir, environ, "show", id, "--json")
	if code != 0 {
		t.Fatalf("show --json (before): exit %d, stderr=%s", code, stderrShow)
	}
	beforeList, stderrList, code := run(t, bin, dir, environ, "list", "--json")
	if code != 0 {
		t.Fatalf("list --json (before): exit %d, stderr=%s", code, stderrList)
	}

	cs := mcpSession(t, bin, dir, environ)

	// Case 1: a JSON type mismatch, with title sent as a number, not a
	// string. This is a real json.Unmarshal failure, the only malformed
	// input the low-level path rejects (a bogus enum value or an unknown
	// field reaches the handler and unmarshals fine, since Server.AddTool
	// performs no schema validation). It calls CallTool directly, not the
	// callTool helper, which fails the test on error.
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "add",
		Arguments: map[string]any{"title": 123},
	}); err == nil {
		t.Errorf(`add {"title":123}: CallTool returned a nil error, want a non-nil protocol error`)
	}

	// Case 2: a value outside dep's closed vocabulary. Correctly-typed
	// JSON, but store.CreateDep's ErrInvalidDepType classifies it as a
	// protocol error (internal/mcp/errors.go's resultFor). The store
	// handles it, because the SDK does not enforce the schema `enum`.
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "dep",
		Arguments: map[string]any{"item_id": "a", "depends_on_id": "b", "type": "nonsense"},
	}); err == nil {
		t.Errorf(`dep {"type":"nonsense"}: CallTool returned a nil error, want a non-nil protocol error`)
	}

	// Case 3: an unknown tool name, a protocol error from the SDK's own
	// dispatch, free of any of this package's code.
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "nope",
		Arguments: map[string]any{},
	}); err == nil {
		t.Errorf(`tool "nope": CallTool returned a nil error, want a non-nil protocol error (unknown tool)`)
	}

	// Contrast case, in the SAME test: a store sentinel from a WELL-FORMED
	// call is a NIL Go error carrying IsError == true, not a protocol
	// error. Without this the test cannot distinguish "everything fails as
	// a protocol error" from the intended split.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "show",
		Arguments: map[string]any{"id": "rnc-doesnotexist"},
	})
	if err != nil {
		t.Fatalf(`show {"id":"rnc-doesnotexist"}: CallTool returned protocol error %v, want a nil error with IsError=true`, err)
	}
	if !res.IsError {
		t.Errorf(`show {"id":"rnc-doesnotexist"}: IsError = false, want true`)
	}
	if len(res.Content) != 1 {
		t.Fatalf(`show {"id":"rnc-doesnotexist"}: len(Content) = %d, want 1`, len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf(`show {"id":"rnc-doesnotexist"}: Content[0] is %T, want *mcp.TextContent`, res.Content[0])
	}
	if !strings.Contains(tc.Text, "rnc-doesnotexist") {
		t.Errorf(`show {"id":"rnc-doesnotexist"}: error text %q does not name the missing id`, tc.Text)
	}

	// No panic, and the server survives: a normal call on the SAME session
	// still succeeds after all four failing calls above.
	_ = callTool(t, cs, "ready", map[string]any{})

	// Store unchanged: byte-identical before/after snapshots, taken via
	// ordinary CLI invocations (the same read-only surface used for the
	// before-snapshot).
	afterShow, stderrShow2, code := run(t, bin, dir, environ, "show", id, "--json")
	if code != 0 {
		t.Fatalf("show --json (after): exit %d, stderr=%s", code, stderrShow2)
	}
	afterList, stderrList2, code := run(t, bin, dir, environ, "list", "--json")
	if code != 0 {
		t.Fatalf("list --json (after): exit %d, stderr=%s", code, stderrList2)
	}

	if beforeShow != afterShow {
		t.Errorf("show --json changed after the four failing calls:\nbefore: %s\nafter:  %s", beforeShow, afterShow)
	}
	if beforeList != afterList {
		t.Errorf("list --json changed after the four failing calls:\nbefore: %s\nafter:  %s", beforeList, afterList)
	}
}
