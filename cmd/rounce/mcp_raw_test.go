package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

// rawMCPConn drives a real `rounce mcp` subprocess over its own stdin and
// stdout pipes, with no SDK client in between.
//
// Plan 04's mcpSession (mcp.CommandTransport + mcp.NewClient, cmd/rounce/
// mcp_test.go) is the right tool for correctness assertions and is NOT
// replaced by this. This exists for the two requirements the SDK's client
// structurally cannot prove:
//
//   - MCP-06 needs both requests WRITTEN before either response is READ,
//     which a request/response client will not do -- mcp.ClientSession's
//     CallTool blocks until its own response arrives.
//   - MCP-07 needs the raw stdout bytes, captured independently of the
//     SDK's own decoder, which could otherwise absorb or misreport
//     pollution before a test ever saw it.
//
// rawOut is a tee'd copy of every byte read from the child's stdout,
// accumulated for exactly that assertion. It and stderr are only read by
// this file's tests AFTER the subprocess has fully exited (i.e. after
// closeAndWait, or after the t.Cleanup kill-and-Wait fallback below) --
// os/exec's own documented guarantee is that Wait does not return until
// the goroutines copying into a plain io.Writer (stderr here) or draining a
// Std*Pipe (stdout here, via this file's own read loop) have finished, so
// reading either buffer only after Wait has returned is race-free without
// an additional mutex. Reading either buffer WHILE the process might still
// be writing would be a genuine data race on *bytes.Buffer, not just a
// -race false positive, so no test in this file does that.
type rawMCPConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	rawOut *bytes.Buffer
	stderr *bytes.Buffer

	// done is set once closeAndWait (or the t.Cleanup fallback) has called
	// cmd.Wait -- the signal that rawOut/stderr are now stable and safe to
	// read.
	done bool
}

// rawResponse is the minimal JSON-RPC response shape this file decodes.
// Result is left as json.RawMessage rather than a concrete type because
// different requests (initialize, tools/list, tools/call) all have
// different result shapes, and awaitID is generic across all of them.
type rawResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// startRawMCP starts a real `rounce mcp` subprocess (bin built by
// buildRounce, cmd/rounce/cli_test.go) and wires stdin/stdout/stderr
// directly, with no SDK transport in between.
func startRawMCP(t *testing.T, bin, dir string, environ []string) *rawMCPConn {
	t.Helper()

	cmd := exec.Command(bin, "mcp")
	cmd.Dir = dir
	cmd.Env = environ

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}

	// rawOut accumulates every byte read from stdoutPipe, independently of
	// whatever this file's own read loop (awaitID/readLine) does with it --
	// this is MCP-07's load-bearing property: the byte-level assertion in
	// TestMCPStdoutIsProtocolBytesOnly reads rawOut directly, never a value
	// that passed through readLine/awaitID's own parsing first.
	var raw bytes.Buffer
	tee := io.TeeReader(stdoutPipe, &raw)

	// A plain io.Writer (not a *os.File pipe), so os/exec's own internal
	// copying goroutine finishes -- and stderrBuf becomes safe to read --
	// before cmd.Wait returns. See the rawMCPConn doc comment above.
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start rounce mcp: %v", err)
	}

	c := &rawMCPConn{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(tee),
		rawOut: &raw,
		stderr: &stderrBuf,
	}

	// If the test returns (by success or t.Fatal) without ever calling
	// closeAndWait, the subprocess must not be left running.
	t.Cleanup(func() {
		if c.done {
			return
		}
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
		c.done = true
	})

	return c
}

// send marshals msg and writes it to the child's stdin, terminated with a
// single '\n' -- the exact framing mcp.StdioTransport's write side uses
// (verified live, 04-RESEARCH.md: "newline delimited", one complete
// JSON-RPC message per line, never LSP's Content-Length framing).
func (c *rawMCPConn) send(t *testing.T, msg any) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		t.Fatalf("write to rounce mcp stdin: %v", err)
	}
}

// readLine reads one newline-terminated message from the child's stdout,
// trimmed of its trailing newline. Every read here also lands in rawOut via
// the TeeReader startRawMCP installed.
func (c *rawMCPConn) readLine(t *testing.T) string {
	t.Helper()
	line, err := c.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("readLine: read from rounce mcp stdout: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// awaitID reads lines from stdout until every id in ids has been seen,
// filing each decoded response by its own id -- never assuming the next
// line answers the most recently sent request. This is the load-bearing
// helper: responses arrive out of order (observed live, 04-RESEARCH.md /
// 04-CONTEXT.md: six pipelined requests answered 1, 2, 4, 5, 3, 6), so
// matching by id rather than arrival order is not a defensive nicety, it is
// the only correct way to read a pipelined session.
//
// Each read attempt runs in its own goroutine with a single in-flight read
// at a time (the next attempt is only started after the previous one has
// completed and been consumed), so there is never more than one goroutine
// touching c.stdout concurrently -- this keeps the harness -race-clean
// without needing a mutex around the bufio.Reader. A bounded 15s deadline
// (not an unbounded wait) makes a genuine hang fail loudly instead of
// blocking the suite -- internal/store's own known deadlock class
// (isBlockedTx's doc comment, transitions.go) is exactly the lesson this
// codebase already learned about trusting unbounded waits.
func (c *rawMCPConn) awaitID(t *testing.T, ids ...int) map[int]rawResponse {
	t.Helper()

	want := make(map[int]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	got := make(map[int]rawResponse, len(ids))

	type lineResult struct {
		line string
		err  error
	}

	deadline := time.Now().Add(15 * time.Second)

	for len(got) < len(want) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("awaitID: timed out waiting for ids %v, got ids %v so far", ids, sortedIntKeys(got))
		}

		ch := make(chan lineResult, 1)
		go func() {
			line, err := c.stdout.ReadString('\n')
			ch <- lineResult{line, err}
		}()

		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("awaitID: error reading rounce mcp stdout while waiting for ids %v (have %v so far): %v", ids, sortedIntKeys(got), res.err)
			}
			line := strings.TrimRight(res.line, "\r\n")
			if line == "" {
				continue
			}
			var resp rawResponse
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				t.Fatalf("awaitID: stdout line is not valid JSON while waiting for ids %v: %q: %v", ids, line, err)
			}
			if want[resp.ID] {
				got[resp.ID] = resp
			}
		case <-time.After(remaining):
			t.Fatalf("awaitID: timed out after 15s waiting for ids %v, got ids %v so far", ids, sortedIntKeys(got))
		}
	}
	return got
}

// sortedIntKeys is a small logging helper for awaitID's failure messages --
// deterministic output makes a flaky-looking failure easier to read twice
// and compare.
func sortedIntKeys(m map[int]rawResponse) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// handshake sends `initialize` with protocolVersion "2025-11-25" -- the
// version the classic initialize handshake negotiates to regardless of what
// is requested (04-RESEARCH.md: "the classic initialize method caps the
// negotiated version at 2025-11-25 even if the client requests
// 2026-07-28"), so asking for the newer value here would be pointless --
// awaits its response (id 1), then sends the `notifications/initialized`
// notification, which carries no id and gets no response.
func (c *rawMCPConn) handshake(t *testing.T) {
	t.Helper()

	c.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "rounce-raw-test-client", "version": "0"},
		},
	})
	resp := c.awaitID(t, 1)[1]
	if resp.Error != nil {
		t.Fatalf("initialize: protocol error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	c.send(t, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
}

// closeAndWait closes stdin (signalling the client is done, exactly the
// "holding stdin open until every response is written returns nil and
// exits 0" path 04-RESEARCH.md verified live, 10/10 runs, when there is no
// request left in flight) and waits for the process to exit, returning its
// exit code.
func (c *rawMCPConn) closeAndWait(t *testing.T) int {
	t.Helper()

	if err := c.stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	err := c.cmd.Wait()
	c.done = true
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("rounce mcp process did not exit cleanly: %v", err)
	return -1
}

// TestRawMCPHandshake proves the harness itself works end to end: it starts
// a real `rounce mcp`, completes a handshake over raw pipes with no SDK
// client involved, and cleans up its subprocess via closeAndWait, which
// this test also uses to confirm a clean handshake-then-close exits 0 with
// nothing left in flight.
func TestRawMCPHandshake(t *testing.T) {
	bin := buildRounce(t, "")
	dir := t.TempDir()
	environ := env(dir, "", "ROUNCE_SESSION=raw-handshake-session")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	conn := startRawMCP(t, bin, dir, environ)
	conn.handshake(t)

	code := conn.closeAndWait(t)
	if code != 0 {
		t.Fatalf("rounce mcp exit code = %d, want 0 (stderr: %s)", code, conn.stderr.String())
	}
}
