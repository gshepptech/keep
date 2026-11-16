package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/keep/internal/model"
)

// rawMCPConn drives a real `keep mcp` subprocess over its own stdin and
// stdout pipes, with no SDK client in between.
//
// mcpSession (mcp.CommandTransport + mcp.NewClient, cmd/keep/mcp_test.go)
// is the right tool for correctness assertions and is NOT replaced by
// this. This exists for the two properties the SDK's client cannot
// prove:
//
//   - pipelined calls not racing needs both requests WRITTEN before either
//     response is READ, which a request/response client will not do:
//     mcp.ClientSession's CallTool blocks until its own response arrives.
//   - "stdout is protocol bytes only" needs the raw stdout bytes, captured
//     independently of the
//     SDK's own decoder, which could otherwise absorb or misreport
//     pollution before a test ever saw it.
//
// rawOut is a tee'd copy of every byte read from the child's stdout,
// accumulated for exactly that assertion. It and stderr are only read by this
// file's tests AFTER the subprocess has fully exited (i.e. after closeAndWait,
// or after the t.Cleanup kill-and-Wait fallback below). os/exec documents that
// Wait does not return until the goroutines copying into a plain io.Writer
// (stderr here) or draining a Std*Pipe (stdout here, via this file's own read
// loop) have finished, so reading either buffer only after Wait has returned
// is race-free without an additional mutex. Reading either buffer WHILE the
// process might still be writing would be a genuine data race on
// *bytes.Buffer, not just a -race false positive, so no test in this file does
// that.
type rawMCPConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	rawOut *bytes.Buffer
	stderr *bytes.Buffer

	// done is set once closeAndWait (or the t.Cleanup fallback) has called
	// cmd.Wait, which signals that rawOut/stderr are stable and safe to read.
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

// toolCallResult is a tools/call response's Result payload, decoded from
// rawResponse.Result. Every tool call this file makes (successful ones,
// isError ones) shares this shape; a protocol error never has a Result at
// all (rawResponse.Error is set instead), so this type is never applied to
// that case.
type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// startRawMCP starts a real `keep mcp` subprocess (bin built by
// buildKeep, cmd/keep/cli_test.go) and wires stdin/stdout/stderr
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
	// whatever this file's own read loop (awaitID) does with it. The
	// byte-level assertion in TestMCPStdoutIsProtocolBytesOnly reads rawOut
	// directly, never a value that passed through awaitID's own parsing
	// first.
	var raw bytes.Buffer
	tee := io.TeeReader(stdoutPipe, &raw)

	// A plain io.Writer (not a *os.File pipe), so os/exec's own internal
	// copying goroutine finishes, and stderrBuf becomes safe to read, before
	// cmd.Wait returns. See the rawMCPConn doc comment above.
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start keep mcp: %v", err)
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
// single '\n', the framing mcp.StdioTransport's write side uses
// (newline delimited, one complete JSON-RPC message per line, never LSP's
// Content-Length framing).
func (c *rawMCPConn) send(t *testing.T, msg any) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		t.Fatalf("write to keep mcp stdin: %v", err)
	}
}

// awaitID reads lines from stdout until every id in ids has been seen,
// filing each decoded response by its own id, never assuming the next line
// answers the most recently sent request. Responses arrive out of
// order (six pipelined requests can be answered 1, 2, 4, 5, 3, 6), so
// matching by id is the only correct way to read a pipelined session.
//
// Each read attempt runs in its own goroutine with a single in-flight read
// at a time (the next attempt is only started after the previous one has
// completed and been consumed), so there is never more than one goroutine
// touching c.stdout concurrently, which keeps the harness -race-clean
// without a mutex around the bufio.Reader. A bounded 15s deadline (not an
// unbounded wait) makes a hang fail loudly instead of blocking the suite;
// the deadlock class described in isBlockedTx's doc comment
// (internal/store/transitions.go) is why unbounded waits are not trusted
// here.
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
				t.Fatalf("awaitID: error reading keep mcp stdout while waiting for ids %v (have %v so far): %v", ids, sortedIntKeys(got), res.err)
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

// sortedIntKeys is a small logging helper for awaitID's failure messages;
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

// handshake sends `initialize` with protocolVersion "2025-11-25", awaits its
// response (id 1), then sends the `notifications/initialized` notification,
// which carries no id and gets no response. 2025-11-25 is the version the
// classic initialize method negotiates to even if the client requests a
// newer one, so asking for a newer value would change nothing.
func (c *rawMCPConn) handshake(t *testing.T) {
	t.Helper()

	c.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "keep-raw-test-client", "version": "0"},
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

// closeAndWait closes stdin (signalling the client is done; with no request
// left in flight, the server returns nil and exits 0) and waits for
// the process to exit, returning its exit code.
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
	t.Fatalf("keep mcp process did not exit cleanly: %v", err)
	return -1
}

// TestRawMCPHandshake proves the harness itself works end to end: it starts
// a real `keep mcp`, completes a handshake over raw pipes with no SDK
// client involved, and cleans up its subprocess via closeAndWait, which
// this test also uses to confirm a clean handshake-then-close exits 0 with
// nothing left in flight.
func TestRawMCPHandshake(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=raw-handshake-session")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	conn := startRawMCP(t, bin, dir, environ)
	conn.handshake(t)

	code := conn.closeAndWait(t)
	if code != 0 {
		t.Fatalf("keep mcp exit code = %d, want 0 (stderr: %s)", code, conn.stderr.String())
	}
}

// TestMCPPipelinedCallsDoNotRace proves the MCP process serializes its own
// store access, so pipelined tools/call requests within one session cannot
// race.
//
// Why one process and not several: the other concurrency tests spawn
// several real binaries because OS-level fcntl/flock locking is invisible
// to in-process tests. That is about inter-process contention: two keep
// processes, two *Store values, two *sql.DB pools, arbitrated by SQLite's
// file locks. This is the opposite hazard: intra-process, one *Store, one
// connection pool, one *sql.DB, with the SDK dispatching handlers
// concurrently. Two `keep mcp` processes would not share a *Store and so
// would not exercise it. It still runs against the real compiled binary:
// one spawned `keep mcp`, requests written before any response is read,
// over its real stdio pipes, not the SDK's in-memory-transports
// constructor, which is an in-process net.Pipe. The multi-process form is
// covered by TestConcurrentAddChildrenNoOrdinalCollision
// (cmd/keep/concurrent_test.go). This test writes four requests before
// reading any response rather than two, because two pipelined requests can
// pass by accident on a machine that happens to serialize them, and the
// ordinal invariant this test asserts only bites with real contention.
func TestMCPPipelinedCallsDoNotRace(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=pipeline-session")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	parentOut, parentErr, code := run(t, bin, dir, environ, "add", "--type", "epic", "parent")
	if code != 0 {
		t.Fatalf("add parent (fixture): exit %d, stderr=%s", code, parentErr)
	}
	parentID := strings.TrimSpace(parentOut)

	conn := startRawMCP(t, bin, dir, environ)
	conn.handshake(t)

	// Serialization here is SetMaxOpenConns(1) (internal/store/schema.go's
	// open()) and nothing else; there is no package-level mutex in this
	// codebase. One would add nothing the pool does not already guarantee,
	// and this test checks that the pool is enough.
	//
	// The SDK dispatches every tools/call concurrently (jsonrpc2.Async,
	// called on every request). Six pipelined requests can be answered in
	// the order 1, 2, 4, 5, 3, 6, which is also why awaitID (above) files
	// responses by id instead of assuming arrival order.

	// Write all four requests to stdin with NO read between them. Inserting
	// an awaitID call between these four sends would silently turn this into
	// four SEQUENTIAL calls and prove nothing about concurrent dispatch, and
	// the test would still pass.
	for i, id := range []int{10, 11, 12, 13} {
		conn.send(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "add",
				"arguments": map[string]any{
					"title":     fmt.Sprintf("child %d", i),
					"parent_id": parentID,
				},
			},
		})
	}

	responses := conn.awaitID(t, 10, 11, 12, 13)

	ordinals := map[int]bool{}
	ids := map[string]bool{}
	for _, reqID := range []int{10, 11, 12, 13} {
		resp := responses[reqID]
		if resp.Error != nil {
			t.Fatalf("tools/call add (raw id %d): protocol error: code=%d message=%s", reqID, resp.Error.Code, resp.Error.Message)
		}
		var result toolCallResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("decode tools/call result for raw id %d: %v (raw result: %s)", reqID, err, resp.Result)
		}
		if result.IsError {
			t.Fatalf("tools/call add (raw id %d): isError = true, content = %+v", reqID, result.Content)
		}
		if len(result.Content) != 1 {
			t.Fatalf("tools/call add (raw id %d): len(content) = %d, want 1", reqID, len(result.Content))
		}
		var item model.Item
		if err := json.Unmarshal([]byte(result.Content[0].Text), &item); err != nil {
			t.Fatalf("decode item for raw id %d: %v (text: %s)", reqID, err, result.Content[0].Text)
		}
		if item.Ordinal == nil {
			t.Fatalf("raw id %d: item %s has a nil ordinal, want one of 1..4", reqID, item.ID)
		}
		// Assert as sets (map[int]bool, map[string]bool), not as a count or
		// a maximum, for the same reason as
		// TestConcurrentAddChildrenNoOrdinalCollision: a count of 4 also
		// passes if two children share an ordinal and a third is missing.
		ordinals[*item.Ordinal] = true
		ids[item.ID] = true
	}

	if len(ordinals) != 4 {
		t.Fatalf("got %d distinct ordinals %v from four responses, want 4 distinct values {1,2,3,4} -- no gaps, no duplicates", len(ordinals), ordinals)
	}
	for i := 1; i <= 4; i++ {
		if !ordinals[i] {
			t.Fatalf("ordinal %d missing from the set %v -- want exactly {1,2,3,4}", i, ordinals)
		}
	}
	if len(ids) != 4 {
		t.Fatalf("got %d distinct ids %v, want 4", len(ids), ids)
	}
	for i := 1; i <= 4; i++ {
		wantID := fmt.Sprintf("%s.%d", parentID, i)
		if !ids[wantID] {
			t.Fatalf("id %q missing from the set %v -- want exactly {%s.1 .. %s.4}", wantID, ids, parentID, parentID)
		}
	}

	// Confirm the rows are really on disk, not just present in four
	// optimistic tools/call responses: an ordinary CLI invocation,
	// afterwards, over the same store.
	listOut, listErr, listCode := run(t, bin, dir, environ, "list", "--parent", parentID, "--json")
	if listCode != 0 {
		t.Fatalf("list --parent %s --json: exit %d, stderr=%s", parentID, listCode, listErr)
	}
	var onDisk []model.Item
	if err := json.Unmarshal([]byte(listOut), &onDisk); err != nil {
		t.Fatalf("decode list --json: %v (raw: %s)", err, listOut)
	}
	if len(onDisk) != 4 {
		t.Fatalf("list --parent %s --json returned %d items, want exactly 4", parentID, len(onDisk))
	}

	code2 := conn.closeAndWait(t)
	if code2 != 0 {
		t.Fatalf("keep mcp exit code = %d, want 0 (stderr: %s)", code2, conn.stderr.String())
	}
}

// TestMCPStdoutIsProtocolBytesOnly proves every byte the
// `keep mcp` process writes to stdout is a complete JSON-RPC 2.0 message
// on its own line, and every diagnostic lands on stderr instead. It is
// asserted at the byte level, over conn.rawOut (the tee'd copy of every
// byte read from stdout), never through awaitID's already-parsed return
// value or the SDK client, so it sees what the process wrote before
// anything tolerant could normalise it.
//
// Other code in this process that could write to stdout: the SDK's mcp
// package references os.Stdout in one place (StdioTransport.Connect's own
// wiring); the pinned modernc.org/sqlite references it nowhere; the pinned
// modernc.org/libc's references are C-library emulation unreachable for
// this project's usage; Go's log package defaults to os.Stderr. The likely
// accidental source is a bare fmt.Println in this project's code, which is
// what this test catches.
func TestMCPStdoutIsProtocolBytesOnly(t *testing.T) {
	bin := buildKeep(t, "")
	dir := tempDir(t)
	environ := env(dir, "", "KEEP_SESSION=stdout-purity-session")

	if _, _, code := run(t, bin, dir, environ, "init"); code != 0 {
		t.Fatalf("init exit code = %d, want 0", code)
	}

	conn := startRawMCP(t, bin, dir, environ)
	conn.handshake(t) // id 1

	nextID := 1
	send := func(method string, params map[string]any) int {
		nextID++
		msg := map[string]any{"jsonrpc": "2.0", "id": nextID, "method": method}
		if params != nil {
			msg["params"] = params
		}
		conn.send(t, msg)
		return nextID
	}

	// tools/list exercises the handler-free SDK dispatch path.
	listID := send("tools/list", nil)
	if resp := conn.awaitID(t, listID)[listID]; resp.Error != nil {
		t.Fatalf("tools/list: protocol error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	// A successful capture.
	captureID := send("tools/call", map[string]any{
		"name":      "capture",
		"arguments": map[string]any{"title": "stdout purity doc", "body": "hello"},
	})
	captureResp := conn.awaitID(t, captureID)[captureID]
	if captureResp.Error != nil {
		t.Fatalf("tools/call capture: protocol error: code=%d message=%s", captureResp.Error.Code, captureResp.Error.Message)
	}
	var captureResult toolCallResult
	if err := json.Unmarshal(captureResp.Result, &captureResult); err != nil {
		t.Fatalf("decode capture result: %v (raw: %s)", err, captureResp.Result)
	}
	if captureResult.IsError || len(captureResult.Content) != 1 {
		t.Fatalf("tools/call capture: isError=%v content=%+v, want a clean single-content success", captureResult.IsError, captureResult.Content)
	}

	// A successful add.
	addID := send("tools/call", map[string]any{
		"name":      "add",
		"arguments": map[string]any{"title": "stdout purity ticket"},
	})
	addResp := conn.awaitID(t, addID)[addID]
	if addResp.Error != nil {
		t.Fatalf("tools/call add: protocol error: code=%d message=%s", addResp.Error.Code, addResp.Error.Message)
	}
	var addResult toolCallResult
	if err := json.Unmarshal(addResp.Result, &addResult); err != nil {
		t.Fatalf("decode add result: %v (raw: %s)", err, addResp.Result)
	}
	if addResult.IsError || len(addResult.Content) != 1 {
		t.Fatalf("tools/call add: isError=%v content=%+v, want a clean single-content success", addResult.IsError, addResult.Content)
	}
	var ticket model.Item
	if err := json.Unmarshal([]byte(addResult.Content[0].Text), &ticket); err != nil {
		t.Fatalf("decode added ticket: %v (text: %s)", err, addResult.Content[0].Text)
	}

	// A successful start.
	startID := send("tools/call", map[string]any{
		"name":      "start",
		"arguments": map[string]any{"id": ticket.ID},
	})
	if resp := conn.awaitID(t, startID)[startID]; resp.Error != nil {
		t.Fatalf("tools/call start: protocol error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	// An isError result: show on a missing id.
	showID := send("tools/call", map[string]any{
		"name":      "show",
		"arguments": map[string]any{"id": "rnc-doesnotexist"},
	})
	showResp := conn.awaitID(t, showID)[showID]
	if showResp.Error != nil {
		t.Fatalf("tools/call show (missing id): unexpected protocol error, want a nil error with isError=true: code=%d message=%s", showResp.Error.Code, showResp.Error.Message)
	}
	var showResult toolCallResult
	if err := json.Unmarshal(showResp.Result, &showResult); err != nil {
		t.Fatalf("decode show result: %v (raw: %s)", err, showResp.Result)
	}
	if !showResult.IsError {
		t.Fatalf("tools/call show (missing id): isError = false, want true")
	}

	// A protocol error: add with title sent as a number, not a string.
	badID := send("tools/call", map[string]any{
		"name":      "add",
		"arguments": map[string]any{"title": 123},
	})
	badResp := conn.awaitID(t, badID)[badID]
	if badResp.Error == nil {
		t.Fatalf("tools/call add {\"title\":123}: got a result, want a protocol error")
	}

	exitCode := conn.closeAndWait(t)
	if exitCode != 0 {
		t.Fatalf("keep mcp exit code = %d, want 0 (stdin closed with nothing in flight)", exitCode)
	}

	// --- The byte-level assertions, over conn.rawOut ---

	// Every id-bearing request this test sent, in the order sent. The
	// initialize handshake's id (1) is included: rawOut is a tee'd copy of
	// EVERY byte ever read from stdout, including bytes read during
	// handshake(), which ran before this test took over sending ids
	// itself.
	sentIDs := []int{1, listID, captureID, addID, startID, showID, badID}

	rawBytes := conn.rawOut.String()
	lines := strings.Split(rawBytes, "\n")
	parsedResponseCount := 0

	for i, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		if line == "" {
			// The trailing newline after the final message produces one
			// empty trailing element from strings.Split; every other
			// empty line would itself be pollution (an actually-blank
			// line is not a valid JSON-RPC message), but strings.Split
			// cannot distinguish "trailing newline" from "blank line in
			// the middle" by position alone here, so this loop counts
			// non-empty lines only, and separately below asserts the
			// exact expected id-bearing count, which a spurious blank
			// line in the middle would silently violate by making the
			// message-count-vs-response-count arithmetic not line up
			// with what was actually sent.
			continue
		}

		var probe struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("stdout line %d is not valid JSON (protocol pollution): %q: %v", i, line, err)
		}
		if probe.JSONRPC != "2.0" {
			t.Fatalf("stdout line %d is valid JSON but jsonrpc = %q, want \"2.0\": %q", i, probe.JSONRPC, line)
		}
		if probe.ID == nil {
			// A notification from the server: none expected in this
			// session, but not itself a violation of the "protocol
			// bytes only" claim if one arrived; only a non-JSON or
			// non-jsonrpc-2.0 line is pollution.
			continue
		}

		hasResult := len(probe.Result) > 0 && string(probe.Result) != "null"
		hasError := len(probe.Error) > 0 && string(probe.Error) != "null"
		if hasResult == hasError {
			t.Fatalf("stdout line %d (id=%d) has result-present=%v error-present=%v, want exactly one -- a complete but malformed message: %q", i, *probe.ID, hasResult, hasError, line)
		}
		parsedResponseCount++
	}

	if parsedResponseCount != len(sentIDs) {
		t.Fatalf("stdout carried %d id-bearing responses, want %d (one per id-bearing request sent: %v) -- a silently dropped or duplicated response is as much a protocol failure as an extra line", parsedResponseCount, len(sentIDs), sentIDs)
	}

	// stderr is only read now, after closeAndWait's cmd.Wait has returned;
	// the rawMCPConn doc comment explains why this ordering makes the read
	// race-free.
	stderrText := conn.stderr.String()
	if stderrText == "" {
		t.Fatalf("stderr is empty -- the server's diagnostics never landed anywhere, so the \"diagnostics go to stderr\" half is unproven")
	}
	// slog.NewTextHandler's own output format (cmd/keep/mcp.go's runMCP
	// sets one explicitly, pointed at stderr). It checks a distinctive
	// substring, not the whole buffer, since the whole buffer also contains
	// timestamps that would never appear verbatim anyway.
	const slogMarker = "level=INFO"
	if !strings.Contains(stderrText, slogMarker) {
		t.Errorf("stderr does not contain %q -- expected slog.NewTextHandler output naming the server's own diagnostics", slogMarker)
	}
	if strings.Contains(rawBytes, slogMarker) {
		t.Fatalf("stdout contains %q -- a stderr byte leaked onto stdout", slogMarker)
	}
}
