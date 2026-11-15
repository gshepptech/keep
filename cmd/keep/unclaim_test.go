package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// TestUnclaimReturnsWorkToTheQueue drives the real binary, because the thing
// worth proving is the whole round trip: an item that has left `ready` comes
// back, and the exit codes a script would branch on are right.
func TestUnclaimReturnsWorkToTheQueue(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_SESSION=sess-dead")
	mustInit(t, bin, repo, e)

	id, _, code := run(t, bin, repo, e, "add", "--score", "9/8/2", "Expire refresh tokens")
	if code != 0 {
		t.Fatalf("add exited %d", code)
	}
	id = strings.TrimSpace(id)

	if _, _, code := run(t, bin, repo, e, "start", id); code != 0 {
		t.Fatalf("start exited %d", code)
	}
	if out, _, _ := run(t, bin, repo, e, "ready"); strings.Contains(out, id) {
		t.Fatal("a claimed item is still in ready; the rest of this test proves nothing")
	}

	// The rescuer is a different session, which is the whole case: the one
	// that made the claim is not coming back to release it.
	rescuer := env(home, "", "KEEP_SESSION=sess-human")
	out, stderr, code := run(t, bin, repo, rescuer, "unclaim", id, "--reason", "the agent crashed")
	if code != 0 {
		t.Fatalf("unclaim exited %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(out, id) {
		t.Errorf("unclaim said %q, which does not name the item", out)
	}

	if out, _, _ := run(t, bin, repo, e, "ready"); !strings.Contains(out, id) {
		t.Errorf("the item is not back in ready after unclaim:\n%s", out)
	}

	// The trail has to distinguish this from a close and from a drop.
	if out, _, _ := run(t, bin, repo, e, "log", "--kind", "unclaim"); !strings.Contains(out, id) {
		t.Errorf("no unclaim event in the log:\n%s", out)
	}
}

// TestUnclaimRefusesWithoutAReason holds the same line close and drop hold.
// A rescue with no reason records that a claim vanished and nothing about
// why, which is the audit hole this verb was added to avoid.
func TestUnclaimRefusesWithoutAReason(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_SESSION=sess-dead")
	mustInit(t, bin, repo, e)

	id, _, _ := run(t, bin, repo, e, "add", "A thing")
	id = strings.TrimSpace(id)
	run(t, bin, repo, e, "start", id)

	for _, args := range [][]string{
		{"unclaim", id},
		{"unclaim", id, "--reason", ""},
		{"unclaim", id, "--reason", "   "},
	} {
		stdout, stderr, code := run(t, bin, repo, e, args...)
		if code != 2 {
			t.Errorf("%v exited %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "reason") {
			t.Errorf("%v: stderr does not mention the reason: %q", args, stderr)
		}
		if strings.Contains(stdout, "returned") {
			t.Errorf("%v: the item was released anyway: %q", args, stdout)
		}
	}

	// And it is still claimed, so a refused rescue changed nothing.
	if out, _, _ := run(t, bin, repo, e, "ready"); strings.Contains(out, id) {
		t.Error("a refused unclaim released the item anyway")
	}
}

// TestStaleOffersUnclaimRatherThanDrop. `stale` must not tell the reader to
// "hand it back with keep drop", which is the exact false record unclaim
// exists to prevent: drop says the work was not worth doing, and a claim
// going quiet says nothing of the sort. The wording is the whole finding, so
// the wording is what is asserted.
func TestStaleOffersUnclaimRatherThanDrop(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_SESSION=sess-dead")
	mustInit(t, bin, repo, e)

	id, _, _ := run(t, bin, repo, e, "add", "A thing")
	id = strings.TrimSpace(id)
	run(t, bin, repo, e, "start", id)

	// --after 0 means "everything currently claimed" for this purpose.
	_, stderr, code := run(t, bin, repo, e, "stale", "--after", "1ns")
	if code != 0 {
		t.Fatalf("stale exited %d", code)
	}
	if !strings.Contains(stderr, "keep unclaim") {
		t.Errorf("stale does not offer unclaim:\n%s", stderr)
	}
	if strings.Contains(stderr, "hand it back with `keep drop") {
		t.Error("stale still calls drop 'handing it back', which records the work as abandoned")
	}
}

// TestVersionIsCitable checks that a binary built without -ldflags, as the
// documented `go install ./cmd/keep` does, still reports something a bug
// report can cite instead of "dev".
//
// Both paths are asserted because the explicit one must keep winning: a
// release build that passes -ldflags must not be overridden by something
// inferred from the build info.
func TestVersionIsCitable(t *testing.T) {
	t.Run("an explicit -ldflags version wins", func(t *testing.T) {
		bin := buildKeep(t, "v9.9.9")
		out, _, code := run(t, bin, tempDir(t), env(tempDir(t), ""), "version")
		if code != 0 {
			t.Fatalf("version exited %d", code)
		}
		if got := strings.TrimSpace(out); got != "v9.9.9" {
			t.Errorf("version = %q, want the ldflags value", got)
		}
	})

	// Every consumer, not just `keep version`. The other MCP tests build
	// with -ldflags set, so they cannot see an empty version in the
	// handshake; this case builds without it.
	t.Run("the MCP handshake reports it too", func(t *testing.T) {
		bin := buildKeep(t, "")
		repo := tempDir(t)
		home := tempDir(t)
		e := env(home, "")
		mustInit(t, bin, repo, e)

		reported := mcpServerVersion(t, bin, repo, e)
		if reported == "" {
			t.Error("the MCP server reports an empty version; a client cannot tell what it is talking to")
		}
		if reported == "dev" {
			t.Error(`the MCP server reports "dev"; resolveVersion is not reaching the handshake`)
		}
	})

	// No ldflags at all, which is what a plain `go install` does.
	t.Run("a plain build still identifies itself", func(t *testing.T) {
		bin := buildKeep(t, "")
		out, _, code := run(t, bin, tempDir(t), env(tempDir(t), ""), "version")
		if code != 0 {
			t.Fatalf("version exited %d", code)
		}
		got := strings.TrimSpace(out)
		if got == "" {
			t.Fatal("version printed nothing")
		}
		if got == "dev" {
			t.Error(`a plain build reports "dev", which no bug report can cite; build info carries a revision and it is not being read`)
		}
	})
}

// mcpServerVersion completes one initialize handshake against a real
// `keep mcp` process and returns the version it reports in serverInfo.
//
// It drives the binary rather than calling into internal/mcp because the
// failure it guards lives in the wiring between cmd/keep and the server,
// which neither package's own tests exercise.
func mcpServerVersion(t *testing.T, bin, dir string, environ []string) string {
	t.Helper()

	cmd := exec.Command(bin, "mcp")
	cmd.Dir = dir
	cmd.Env = environ
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })

	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-11-25","capabilities":{},` +
		`"clientInfo":{"name":"version-probe","version":"0"}}}` + "\n"
	if _, err := io.WriteString(stdin, req); err != nil {
		t.Fatalf("write initialize: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var msg struct {
			ID     int `json:"id"`
			Result struct {
				ServerInfo struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"serverInfo"`
			} `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.ID != 1 {
			continue
		}
		if msg.Result.ServerInfo.Name != "keep" {
			t.Errorf("serverInfo.name = %q, want keep", msg.Result.ServerInfo.Name)
		}
		return msg.Result.ServerInfo.Version
	}
	t.Fatal("no initialize response from keep mcp")
	return ""
}
