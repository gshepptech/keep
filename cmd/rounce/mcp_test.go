package main

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSession starts a real `rounce mcp` subprocess and connects the SDK's
// own client to it over its real stdin/stdout pipes.
//
// mcp.CommandTransport wraps an *exec.Cmd and calls Start() on it
// (mcp/cmd.go), so this is a genuine subprocess of the compiled binary --
// not the SDK's in-memory-transports constructor, which is an in-process
// net.Pipe and would prove nothing about `rounce mcp`.
//
// This is the harness for CORRECTNESS assertions (MCP-01, MCP-02, MCP-03,
// MCP-04). It is deliberately NOT used for MCP-06 or MCP-07, which need
// literal control over write timing and over the raw stdout bytes before
// the SDK's own decoder touches them -- plan 06 adds a second, raw harness
// for those.
func mcpSession(t *testing.T, bin, dir string, environ []string) *mcp.ClientSession {
	t.Helper()

	cmd := exec.Command(bin, "mcp")
	cmd.Dir = dir
	cmd.Env = environ

	client := mcp.NewClient(&mcp.Implementation{Name: "rounce-test-client", Version: "0"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to rounce mcp: %v", err)
	}
	t.Cleanup(func() {
		cs.Close()
	})
	return cs
}

// TestMCPToolsListReturnsCommandSurface proves MCP-01: a real `rounce mcp`
// subprocess answers tools/list with exactly the ten locked tools, each with
// a description and an object input schema.
func TestMCPToolsListReturnsCommandSurface(t *testing.T) {
	bin := buildRounce(t, "")
	dir := t.TempDir()
	environ := env(dir, "", "ROUNCE_SESSION=mcp-session-1")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	cs := mcpSession(t, bin, dir, environ)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	// This is 04-CONTEXT.md decision 2's locked ten. It grows only when the
	// CLI grows -- an eleventh tool arriving here is a design decision, not
	// a test to update.
	//
	// NOTE: this is NOT registration order (04-04-PLAN.md's own behaviour
	// spec assumed tools/list would return registration order -- capture,
	// add, ready, backlog, show, list, dep, start, close, drop). Verified
	// live against the real SDK (go-sdk@v1.7.0 mcp/features.go: tools are
	// held in a featureSet keyed by name, and featureSet.all() always
	// iterates s.sortedKeys, a lexically-sorted key slice recomputed after
	// every add/remove) that tools/list is unconditionally alphabetical by
	// tool name, with no way to opt out -- registration order is not
	// observable on the wire at all. This is deterministic SDK behavior,
	// not a race: rerun 5 times, byte-identical order every time. The set
	// is what 04-CONTEXT.md's decision 2 actually locks; asserting the
	// SDK's own deterministic sort order in place of the plan's incorrect
	// registration-order assumption.
	want := []string{"add", "backlog", "capture", "close", "dep", "drop", "list", "ready", "show", "start"}

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
			t.Errorf("tool %q has an empty Description -- a model choosing between ten tools has only this to go on", tl.Name)
		}
		m, ok := tl.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("tool %q InputSchema is %T, want map[string]any", tl.Name, tl.InputSchema)
		}
		if m["type"] != "object" {
			t.Errorf("tool %q InputSchema[\"type\"] = %v, want \"object\"", tl.Name, m["type"])
		}
	}

	// 04-RESEARCH.md's live probe used a hand-rolled JSON-RPC client that
	// explicitly sent "protocolVersion":"2025-11-25" in its initialize
	// request, and observed the classic initialize handshake cap the
	// negotiated version at that value. mcp.NewClient's own default
	// (opts == nil, as used by mcpSession above) does not behave that way:
	// verified live, reproducibly, that Client.Connect tries the newer
	// SEP-2575 server/discover RPC first at latestProtocolVersion
	// (2026-07-28, mcp/shared.go), the negotiate-when-supported branch in
	// discover() takes it (rounce's server supports every version, since
	// mcp.NewServer's ServerOptions never restricts one), and
	// negotiatedVersion's 2025-11-25 cap is only reachable through the
	// legacy `initialize` fallback path this client never takes.
	// ClientSessionOptions' protocolVersion field is unexported ("reserved
	// for future use" / "for testing"), so this package cannot force the
	// older handshake to observe the capped value directly. Asserting the
	// actual, verified value here rather than the plan's predicted one.
	if got := cs.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Errorf("negotiated protocol version = %q, want \"2026-07-28\" (mcp.NewClient's default discover-first negotiation, verified live)", got)
	}
}
