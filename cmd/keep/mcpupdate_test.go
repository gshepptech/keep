package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gshepptech/keep/internal/model"
)

// TestMCPUpdateLetsAnAgentCorrectItsOwnWork drives a real `keep mcp`
// subprocess. Without `update`, an agent could file an item but never fix
// one: a wrong title or score had to go back through `add`, which produces a
// second item with a second id and leaves the first one standing.
func TestMCPUpdateLetsAnAgentCorrectItsOwnWork(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=mcp-update-1")
	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatal("init failed")
	}
	cs := mcpSession(t, bin, dir, environ)

	var filed model.Item
	decodeInto(t, callTool(t, cs, "add", map[string]any{
		"title": "Expire refesh tokens", // the typo is the point
		"value": 9, "confidence": 8, "effort": 2,
	}), &filed)
	if filed.ID == "" {
		t.Fatal("add returned no id")
	}

	var fixed model.Item
	decodeInto(t, callTool(t, cs, "update", map[string]any{
		"id":    filed.ID,
		"title": "Expire refresh tokens",
	}), &fixed)

	// Same id. A duplicate would also produce a correctly titled item; only
	// keeping the id proves the original was corrected.
	if fixed.ID != filed.ID {
		t.Errorf("update returned id %q, want the original %q", fixed.ID, filed.ID)
	}
	if fixed.Title != "Expire refresh tokens" {
		t.Errorf("title = %q, want the corrected one", fixed.Title)
	}

	// Untouched fields stay untouched -- update is a patch, not a replace.
	if fixed.Value == nil || *fixed.Value != 9 {
		t.Errorf("value = %v, want 9 left alone", fixed.Value)
	}

	// And there is exactly one item, not two.
	var listed []model.Item
	decodeInto(t, callTool(t, cs, "list", map[string]any{}), &listed)
	if len(listed) != 1 {
		t.Errorf("store holds %d items, want 1 -- a correction must not fork the record", len(listed))
	}
}

// TestMCPUpdateRefusalsComeBackAsToolErrors pins that a refused update is
// something the agent can read and retry from, not a protocol failure that
// looks like the server broke.
func TestMCPUpdateRefusalsComeBackAsToolErrors(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=mcp-update-2")
	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatal("init failed")
	}
	cs := mcpSession(t, bin, dir, environ)

	var filed model.Item
	decodeInto(t, callTool(t, cs, "add", map[string]any{"title": "A real item"}), &filed)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			// ErrNoFieldsToUpdate. The classification table in
			// internal/mcp/errors.go called this unreachable and classified
			// it anyway "so a future `update` tool inherits the decision".
			// This is that tool.
			name: "no fields supplied",
			args: map[string]any{"id": filed.ID},
			want: "no fields supplied",
		},
		{
			// ErrEmptyTitle must be classified, or it escapes as a raw
			// protocol error from `update`, `add` and `capture`.
			name: "a blank title",
			args: map[string]any{"id": filed.ID, "title": "   "},
			want: "needs a title",
		},
		{
			name: "an id that does not exist",
			args: map[string]any{"id": "rnc-000000", "title": "x"},
			want: "not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "update", Arguments: tc.args})
			if err != nil {
				t.Fatalf("came back as a protocol error, which an agent cannot act on: %v", err)
			}
			if !res.IsError {
				t.Fatalf("IsError = false; the call was refused and must say so")
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("Content[0] is %T, want *mcp.TextContent", res.Content[0])
			}
			if !strings.Contains(text.Text, tc.want) {
				t.Errorf("message %q does not contain %q, so it does not say what to fix", text.Text, tc.want)
			}
		})
	}
}

// TestMCPUpdateCannotChangeStatus pins the one field `update` must never
// carry. open -> in_progress -> closed/dropped moves only through
// start/close/drop so the log always records WHY a status changed; a status
// key here would let an agent close work while bypassing close's --reason.
func TestMCPUpdateCannotChangeStatus(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=mcp-update-3")
	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatal("init failed")
	}
	cs := mcpSession(t, bin, dir, environ)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tl := range res.Tools {
		if tl.Name != "update" {
			continue
		}
		m, ok := tl.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("update InputSchema is %T, want map[string]any", tl.InputSchema)
		}
		props, ok := m["properties"].(map[string]any)
		if !ok {
			t.Fatalf("update schema has no properties object: %v", m)
		}
		if _, ok := props["status"]; ok {
			t.Error("the update schema offers a status property -- status changes must go through start/close/drop")
		}
		if _, ok := props["title"]; !ok {
			t.Error("the update schema has no title property; the schema is not the one this test thinks it is")
		}
		return
	}
	t.Fatal("no update tool in tools/list")
}

// TestMCPRefusesAnOutOfRangeScore covers the path the CLI's flag parser never
// guarded. Until the store enforced the range, an agent could file effort=100
// through `add` for a score of -89 -- below an unscored item's 0 -- and bury
// the item beneath work nobody had even estimated.
//
// The schema now advertises minimum and maximum, but the SDK does not
// validate arguments against InputSchema (internal/mcp/input.go records that
// a published enum was ignored on a real call), so the schema steers and the
// store refuses. This test drives the real subprocess to prove which one is
// actually load-bearing.
func TestMCPRefusesAnOutOfRangeScore(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=mcp-score-1")
	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatal("init failed")
	}
	cs := mcpSession(t, bin, dir, environ)

	// The bounds reach the agent, so a model has something to steer by.
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tl := range tools.Tools {
		if tl.Name != "add" {
			continue
		}
		m, ok := tl.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("add InputSchema is %T", tl.InputSchema)
		}
		props, _ := m["properties"].(map[string]any)
		effort, _ := props["effort"].(map[string]any)
		if effort["minimum"] == nil || effort["maximum"] == nil {
			t.Errorf("add.effort advertises no bounds, so a model has nothing to steer by: %v", effort)
		}
	}

	for _, tc := range []struct {
		name, tool string
		args       map[string]any
	}{
		{"add with an effort past the range", "add", map[string]any{"title": "wild", "effort": 100}},
		{"add with a value past the range", "add", map[string]any{"title": "wild", "value": 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("came back as a protocol error, which an agent cannot act on: %v", err)
			}
			if !res.IsError {
				t.Fatal("accepted; the queue can now be inverted from an agent")
			}
			text, _ := res.Content[0].(*mcp.TextContent)
			if !strings.Contains(text.Text, "0-10") {
				t.Errorf("message %q does not say what the legal range is", text.Text)
			}
		})
	}

	// And the queue is intact: nothing got in.
	var listed []model.Item
	decodeInto(t, callTool(t, cs, "list", map[string]any{}), &listed)
	if len(listed) != 0 {
		t.Errorf("store holds %d items, want 0 -- a refused call must not write", len(listed))
	}
}
