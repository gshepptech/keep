package main

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSessionAs is mcpSession with the client's advertised name under the
// test's control. That name is the input these tests are about, so it
// cannot be the fixed "rounce-test-client" mcpSession hard-codes.
func mcpSessionAs(t *testing.T, bin, dir string, environ []string, clientName string) *mcp.ClientSession {
	t.Helper()

	cmd := exec.Command(bin, "mcp")
	cmd.Dir = dir
	cmd.Env = environ

	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to rounce mcp as %q: %v", clientName, err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// addOverMCPAndReadActor files one item through an MCP session advertising
// clientName, then reads the actor off its `add` event through an ordinary
// CLI invocation -- never trusting the mutating path to report its own
// effect, the same rule TestCLIAndMCPWriteIdenticalEventRows follows.
func addOverMCPAndReadActor(t *testing.T, bin, dir string, environ []string, clientName string, args map[string]any) string {
	t.Helper()

	cs := mcpSessionAs(t, bin, dir, environ, clientName)
	if args == nil {
		args = map[string]any{}
	}
	args["title"] = "filed over MCP"

	var item struct {
		ID string `json:"id"`
	}
	decodeInto(t, callTool(t, cs, "add", args), &item)

	detail := showItemDetailJSON(t, bin, dir, environ, item.ID)
	if len(detail.Events) == 0 {
		t.Fatalf("item %s has no events", item.ID)
	}
	return detail.Events[0].Actor
}

// TestMCPActorComesFromTheClientName is the whole point: the two plugin
// manifests disagreed about ROUNCE_ACTOR and nothing caught it, because the
// only place it surfaces is a mixed event log read after the fact. The
// handshake already says who is calling, so neither manifest has to.
//
// $USER is set to something distinctive so a fallback that quietly won
// cannot pass by coincidence.
func TestMCPActorComesFromTheClientName(t *testing.T) {
	bin := buildRounce(t, "")
	dir := t.TempDir()
	environ := env(dir, "")
	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatal("init failed")
	}

	got := addOverMCPAndReadActor(t, bin, dir, environ, "claude-code", nil)
	if got != "claude-code" {
		t.Errorf("event actor = %q, want %q -- the client's own name, not $USER (%q)",
			got, "claude-code", "rounce-test")
	}
}

// TestMCPActorEnvOverridesClientName: $ROUNCE_ACTOR is how you say "record
// this as something other than the tool driving it" -- a named human, a CI
// identity, an agent behind a proxy. A client name that overrode it would
// make that unsayable, and would silently change the meaning of every
// existing setup, including the Codex registration documented before the
// client-name default existed.
func TestMCPActorEnvOverridesClientName(t *testing.T) {
	bin := buildRounce(t, "")
	dir := t.TempDir()
	environ := env(dir, "", "ROUNCE_ACTOR=explicitly-me")
	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatal("init failed")
	}

	got := addOverMCPAndReadActor(t, bin, dir, environ, "claude-code", nil)
	if got != "explicitly-me" {
		t.Errorf("event actor = %q, want %q -- an explicit $ROUNCE_ACTOR must beat the client name", got, "explicitly-me")
	}
}

// TestMCPActorFallsBackWhenClientIsAnonymous: a client that advertises no
// name must not produce an empty actor. An event row whose actor is "" is
// worse than one naming the wrong thing -- it reads as data loss.
func TestMCPActorFallsBackWhenClientIsAnonymous(t *testing.T) {
	bin := buildRounce(t, "")
	dir := t.TempDir()
	environ := env(dir, "")
	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatal("init failed")
	}

	// env() sets USER=rounce-test, which is resolveActor's last resort.
	got := addOverMCPAndReadActor(t, bin, dir, environ, "", nil)
	if got != "rounce-test" {
		t.Errorf("event actor = %q, want the $USER fallback %q", got, "rounce-test")
	}
}

// TestMCPActorPerCallStillWins locks the behaviour 04-CONTEXT.md decision 3
// established and this change must not quietly remove: actor is an optional
// argument on every mutating tool, defaulting to the server's own. The
// default is what changed here; the override is not.
func TestMCPActorPerCallStillWins(t *testing.T) {
	bin := buildRounce(t, "")
	dir := t.TempDir()
	environ := env(dir, "")
	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatal("init failed")
	}

	got := addOverMCPAndReadActor(t, bin, dir, environ, "claude-code",
		map[string]any{"actor": "per-call-actor"})
	if got != "per-call-actor" {
		t.Errorf("event actor = %q, want the per-call argument %q", got, "per-call-actor")
	}
}
