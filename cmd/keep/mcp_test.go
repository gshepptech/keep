package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpsrv "github.com/gshepptech/keep/internal/mcp"
	"github.com/gshepptech/keep/internal/model"
)

// sortedCopy returns s sorted, without disturbing s. tools/list is
// unconditionally alphabetical (see the note in the caller), so the
// registration-ordered ToolNames has to be sorted before comparison.
func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// mcpSession starts a real `keep mcp` subprocess and connects the SDK's
// own client to it over its real stdin/stdout pipes.
//
// mcp.CommandTransport wraps an *exec.Cmd and calls Start() on it
// (mcp/cmd.go), so this is a genuine subprocess of the compiled binary --
// not the SDK's in-memory-transports constructor, which is an in-process
// net.Pipe and would prove nothing about `keep mcp`.
//
// This is the harness for CORRECTNESS assertions. It is deliberately NOT
// used for pipelining or stdout-purity tests, which need literal control
// over write timing and over the raw stdout bytes before the SDK's own
// decoder touches them -- mcp_raw_test.go is the raw harness for those.
func mcpSession(t *testing.T, bin, dir string, environ []string) *mcp.ClientSession {
	t.Helper()

	cmd := exec.Command(bin, "mcp")
	cmd.Dir = dir
	cmd.Env = environ

	client := mcp.NewClient(&mcp.Implementation{Name: "keep-test-client", Version: "0"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to keep mcp: %v", err)
	}
	t.Cleanup(func() {
		cs.Close()
	})
	return cs
}

// callTool calls name and fails the test if the call errored at the
// protocol level or came back with IsError set. Use it for every step
// that is supposed to succeed, so a step that starts failing names itself
// instead of failing three steps later on a nil field.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: protocol error: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tools/call %s: IsError = true, content = %+v", name, res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("tools/call %s: len(Content) = %d, want 1", name, len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tools/call %s: Content[0] is %T, want *mcp.TextContent", name, res.Content[0])
	}
	return tc.Text
}

// decodeInto unmarshals a tool result's single TextContent into v.
func decodeInto(t *testing.T, text string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("decode %q into %T: %v", text, v, err)
	}
}

// TestMCPToolsListReturnsCommandSurface proves a real `keep mcp`
// subprocess answers tools/list with exactly the registered tools, each with
// a description and an object input schema.
func TestMCPToolsListReturnsCommandSurface(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=mcp-session-1")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	cs := mcpSession(t, bin, dir, environ)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	// It grows only when the CLI grows -- a new tool arriving here is a
	// design decision, not a test to update.
	//
	// NOTE: this is NOT registration order. The SDK (go-sdk mcp/features.go)
	// holds tools in a featureSet keyed by name, and featureSet.all() always
	// iterates a lexically-sorted key slice, so tools/list is unconditionally
	// alphabetical by tool name -- registration order is not observable on
	// the wire at all. This is deterministic SDK behavior, not a race.
	// Sorted copy of internal/mcp.ToolNames -- the registration order lives
	// there, and this asserts the same set arrives over a real stdio
	// session. Reading it back off the wire is what makes this independent
	// of the Go slice: a tool present in ToolNames but never registered
	// fails here.
	want := sortedCopy(mcpsrv.ToolNames)

	if len(res.Tools) != len(want) {
		names := make([]string, len(res.Tools))
		for i, tl := range res.Tools {
			names[i] = tl.Name
		}
		t.Fatalf("tools/list returned %d tools %v, want %d %v", len(res.Tools), names, len(want), want)
	}

	// Order check first (also proves the set is a permutation-free match,
	// not just a coincidental count).
	for i, tl := range res.Tools {
		if tl.Name != want[i] {
			t.Errorf("tools[%d].Name = %q, want %q (alphabetical by tool name, the SDK's own deterministic order)", i, tl.Name, want[i])
		}
	}

	// Set check in both directions: an extra tool fails by name, a missing
	// one fails by name.
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("tools/list is missing tool %q", w)
		}
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for name := range got {
		if !wantSet[name] {
			t.Errorf("tools/list returned unexpected tool %q", name)
		}
	}

	for _, tl := range res.Tools {
		if tl.Description == "" {
			t.Errorf("tool %q has an empty Description -- a model choosing between fourteen tools has only this to go on", tl.Name)
		}
		m, ok := tl.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("tool %q InputSchema is %T, want map[string]any", tl.Name, tl.InputSchema)
		}
		if m["type"] != "object" {
			t.Errorf("tool %q InputSchema[\"type\"] = %v, want \"object\"", tl.Name, m["type"])
		}
	}

	// A hand-rolled JSON-RPC client that explicitly sends
	// "protocolVersion":"2025-11-25" in its initialize request sees the
	// classic initialize handshake cap the negotiated version at that
	// value. mcp.NewClient's own default (opts == nil, as used by
	// mcpSession above) does not behave that way: Client.Connect tries the newer
	// SEP-2575 server/discover RPC first at latestProtocolVersion
	// (2026-07-28, mcp/shared.go), the negotiate-when-supported branch in
	// discover() takes it (keep's server supports every version, since
	// mcp.NewServer's ServerOptions never restricts one), and
	// negotiatedVersion's 2025-11-25 cap is only reachable through the
	// legacy `initialize` fallback path this client never takes.
	// ClientSessionOptions' protocolVersion field is unexported ("reserved
	// for future use" / "for testing"), so this package cannot force the
	// older handshake to observe the capped value directly. This asserts
	// the value the SDK actually negotiates.
	if got := cs.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Errorf("negotiated protocol version = %q, want \"2026-07-28\" (mcp.NewClient's default discover-first negotiation)", got)
	}
}

// TestMCPCallSequenceCaptureRefineReadyStartClose proves that over ONE
// `keep mcp` session, capture -> refine (add) -> show -> ready -> start ->
// close -> ready completes with the payload asserted at every step. This is
// the whole reason MCP exists: one agent session driving the entire loop
// without shelling out.
func TestMCPCallSequenceCaptureRefineReadyStartClose(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=mcp-session-1")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	cs := mcpSession(t, bin, dir, environ)

	// Step 1: capture a genuinely multi-section markdown document.
	body := `# Q3 roadmap

## Goals

- Ship the thing
- Measure the thing

## Detail

Some prose explaining the goals in more depth, spanning more than one
sentence so this is a realistic document body rather than a one-liner.

` + "```go\nfunc main() { println(\"hello\") }\n```" + `

## Non-goals

- Not doing the other thing
`

	text := callTool(t, cs, "capture", map[string]any{
		"title": "Q3 roadmap",
		"type":  "roadmap",
		"body":  body,
	})
	var doc model.Item
	decodeInto(t, text, &doc)
	if doc.Stage != model.StageRaw {
		t.Errorf("captured doc stage = %q, want %q", doc.Stage, model.StageRaw)
	}
	if doc.Type != "roadmap" {
		t.Errorf("captured doc type = %q, want roadmap", doc.Type)
	}
	if len(doc.ID) < 4 || doc.ID[:4] != "rnc-" {
		t.Errorf("captured doc id = %q, want rnc- prefix", doc.ID)
	}
	if doc.Body != body {
		gotLen, wantLen := len(doc.Body), len(body)
		diffAt := -1
		for i := 0; i < gotLen && i < wantLen; i++ {
			if doc.Body[i] != body[i] {
				diffAt = i
				break
			}
		}
		t.Fatalf("captured doc body mismatch: len(got)=%d len(want)=%d first differing byte offset=%d", gotLen, wantLen, diffAt)
	}

	// Step 2: add a ticket refined from the document, parented under it.
	text = callTool(t, cs, "add", map[string]any{
		"title":        "Ship the thing",
		"parent_id":    doc.ID,
		"derived_from": []string{doc.ID},
		"value":        8,
		"confidence":   7,
		"effort":       3,
	})
	var ticket model.Item
	decodeInto(t, text, &ticket)
	if ticket.Stage != model.StageTicket {
		t.Errorf("added ticket stage = %q, want %q", ticket.Stage, model.StageTicket)
	}
	if ticket.ParentID == nil || *ticket.ParentID != doc.ID {
		t.Errorf("added ticket parent_id = %v, want %q", ticket.ParentID, doc.ID)
	}
	wantScore := 8 + 7 - 3 + 11
	if ticket.Score != wantScore {
		t.Errorf("added ticket score = %d, want %d (value+confidence-effort+11)", ticket.Score, wantScore)
	}

	// Step 3: show the document -- depended_on_by must contain BOTH a
	// parent-child and a derived-from edge naming the ticket, asserted by
	// edge TYPE, never by counting to 2 (03-06 hit exactly this: a count of
	// 2 also passes if both edges are the same type).
	text = callTool(t, cs, "show", map[string]any{"id": doc.ID})
	var detail model.ItemDetail
	decodeInto(t, text, &detail)

	var hasParentChild, hasDerivedFrom bool
	for _, d := range detail.DependedOnBy {
		if d.ItemID != ticket.ID {
			continue
		}
		switch d.Type {
		case model.DepParentChild:
			hasParentChild = true
		case model.DepDerivedFrom:
			hasDerivedFrom = true
		}
	}
	if !hasParentChild {
		t.Errorf("show %s: depended_on_by has no parent-child edge naming %s", doc.ID, ticket.ID)
	}
	if !hasDerivedFrom {
		t.Errorf("show %s: depended_on_by has no derived-from edge naming %s", doc.ID, ticket.ID)
	}

	// Step 4: ready contains the ticket and does NOT contain the raw
	// document.
	text = callTool(t, cs, "ready", map[string]any{})
	var readyItems []model.Item
	decodeInto(t, text, &readyItems)
	var foundTicket, foundDoc bool
	for _, it := range readyItems {
		if it.ID == ticket.ID {
			foundTicket = true
		}
		if it.ID == doc.ID {
			foundDoc = true
		}
	}
	if !foundTicket {
		t.Errorf("ready does not contain ticket %s", ticket.ID)
	}
	if foundDoc {
		t.Errorf("ready contains raw document %s, which must never appear until refined", doc.ID)
	}

	// Step 5: start the ticket -- status becomes in_progress and
	// claimed_by_session is the server's fixed session, the value runMCP
	// read once from $KEEP_SESSION. This is the only assertion that
	// proves the fixed-session rule end to end.
	text = callTool(t, cs, "start", map[string]any{"id": ticket.ID})
	var started model.Item
	decodeInto(t, text, &started)
	if started.Status != "in_progress" {
		t.Errorf("started ticket status = %q, want in_progress", started.Status)
	}
	if started.ClaimedBySession == nil || *started.ClaimedBySession != "mcp-session-1" {
		t.Errorf("started ticket claimed_by_session = %v, want \"mcp-session-1\"", started.ClaimedBySession)
	}

	// Step 6: close the ticket.
	text = callTool(t, cs, "close", map[string]any{"id": ticket.ID, "reason": "done"})
	var closed model.Item
	decodeInto(t, text, &closed)
	if closed.Status != "closed" {
		t.Errorf("closed ticket status = %q, want closed", closed.Status)
	}
	if closed.ClosedAt == nil {
		t.Errorf("closed ticket closed_at is nil, want non-null")
	}

	// Step 7: ready again -- the ticket is gone.
	text = callTool(t, cs, "ready", map[string]any{})
	var readyAfterClose []model.Item
	decodeInto(t, text, &readyAfterClose)
	for _, it := range readyAfterClose {
		if it.ID == ticket.ID {
			t.Errorf("ready still contains closed ticket %s", ticket.ID)
		}
	}
}
