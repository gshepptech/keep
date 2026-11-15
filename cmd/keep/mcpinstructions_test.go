package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// initializeResult is the initialize response's Result payload, narrowed to
// the one field these tests are about. `instructions` is
// json:"instructions,omitempty" on the wire, so an empty string is not
// serialised at all -- decoding into *string rather than string is what
// distinguishes "absent" from "present and empty", which is the whole
// distinction TestMCPInstructionsAbsentOnEmptyStore asserts.
type initializeResult struct {
	Instructions *string `json:"instructions"`
}

// initializeInstructions runs one real `keep mcp` subprocess against dir,
// performs the initialize handshake, and returns the `instructions` field
// from the response -- nil when the field was absent.
//
// It spawns the binary rather than calling mcpInstructions directly on
// purpose: the claim under test is that the field reaches an MCP client
// over the wire, and a Go-level call would prove only that a string was
// built. Same reason mcp_raw_test.go drives a subprocess.
func initializeInstructions(t *testing.T, bin, dir string, environ []string) *string {
	t.Helper()

	c := startRawMCP(t, bin, dir, environ)
	c.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "keep-instructions-test", "version": "0"},
		},
	})
	resp := c.awaitID(t, 1)[1]
	if resp.Error != nil {
		t.Fatalf("initialize: protocol error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	var got initializeResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode initialize result: %v (raw=%s)", err, resp.Result)
	}

	c.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if code := c.closeAndWait(t); code != 0 {
		t.Fatalf("keep mcp exited %d, want 0", code)
	}
	return got.Instructions
}

// TestMCPInstructionsCarryTheQueue checks that the handshake instructions
// carry the ready queue: an MCP client learns what is workable from the handshake alone, with no hook, no
// plugin, and no per-agent instruction file -- which is what makes this
// channel reach Codex and every other MCP client on equal terms with Claude
// Code.
func TestMCPInstructionsCarryTheQueue(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	top := mustAdd(t, bin, repo, e, "--score", "10/10/0", "the top ticket")
	mustAdd(t, bin, repo, e, "--score", "1/1/9", "the bottom ticket")
	mustBacklogCapture(t, bin, repo, e, "--type", "note", "a raw note")

	got := initializeInstructions(t, bin, repo, e)
	if got == nil {
		t.Fatal("initialize carried no instructions field, want the queue")
	}
	ins := *got

	// The highest-scoring ticket's id must be present. Asserting the id
	// rather than the title is deliberate: an agent that acts on this text
	// must call `start <id>`, so the id is the part it needs.
	if !strings.Contains(ins, top) {
		t.Errorf("instructions do not name the top ticket %s:\n%s", top, ins)
	}

	// Counts must be exact. "ready 2" specifically -- the raw note must not
	// be counted as ready: raw is never workable, restated at this boundary.
	if !strings.Contains(ins, "ready 2,") {
		t.Errorf("instructions do not report exactly 2 ready (the raw note must not count):\n%s", ins)
	}
	if !strings.Contains(ins, "backlog 1") {
		t.Errorf("instructions do not report the raw note in backlog 1:\n%s", ins)
	}

	// The staleness sentence is not decoration. Without it an agent has no
	// reason to prefer a live `ready` call over these numbers an hour later.
	if !strings.Contains(ins, "do not update") {
		t.Errorf("instructions do not say the counts are a snapshot:\n%s", ins)
	}
}

// TestMCPInstructionsTruncateRowsNotCounts locks the same rule runBoard
// states at length and for the same reason: the count is over the whole
// queue, the rows are a prefix of it. A second Ready call with a limit, or
// one call limited for both, is how a count silently stops matching its own
// list -- and here it would be invisible, because nothing renders these two
// numbers side by side except an agent reading them.
func TestMCPInstructionsTruncateRowsNotCounts(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	const n = instructionRows + 3
	var ids []string
	for i := 0; i < n; i++ {
		// Descending value, so ready's own order is the creation order and
		// the last few added are the ones that must be cut.
		value := strconv.Itoa(10 - i)
		ids = append(ids, mustAdd(t, bin, repo, e, "--score", value+"/5/0", "ticket "+strconv.Itoa(i)))
	}

	got := initializeInstructions(t, bin, repo, e)
	if got == nil {
		t.Fatal("initialize carried no instructions field")
	}
	ins := *got

	if !strings.Contains(ins, "ready "+strconv.Itoa(n)+",") {
		t.Errorf("count is not the full queue of %d:\n%s", n, ins)
	}
	for _, id := range ids[:instructionRows] {
		if !strings.Contains(ins, id) {
			t.Errorf("row %s is within the first %d and must be shown:\n%s", id, instructionRows, ins)
		}
	}
	for _, id := range ids[instructionRows:] {
		if strings.Contains(ins, id) {
			t.Errorf("row %s is beyond %d rows and must be truncated away:\n%s", id, instructionRows, ins)
		}
	}
	if !strings.Contains(ins, "and 3 more") {
		t.Errorf("instructions do not say how many rows were withheld:\n%s", ins)
	}
}

// TestMCPInstructionsAbsentOnEmptyStore is the same silence
// plugin/scripts/session-queue.py keeps, at the other channel: an
// initialised but empty store has nothing to say, and announcing an empty
// queue every session trains a reader to skip the block -- which costs the
// field its only job.
//
// The assertion is on absence of the JSON key, not on an empty string,
// because `omitempty` is what makes those two the same thing on the wire.
func TestMCPInstructionsAbsentOnEmptyStore(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	if got := initializeInstructions(t, bin, repo, e); got != nil {
		t.Errorf("empty store carried instructions %q, want the field absent", *got)
	}
}
