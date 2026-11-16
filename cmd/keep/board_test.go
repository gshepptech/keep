package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// boardFixture builds a store with several ready tickets (well above
// defaultBoardLimit, so a hard-coded limit fails the tests), one claimed
// (in-progress) item, one blocked epic with a child (transitively
// blocked), and one raw note (backlog, never ready).
type boardFixture struct {
	readyIDs []string // in `ready`'s own score-descending order
	claimed  string
	blocker  string
	epic     string
	child    string
	note     string
}

func buildBoardFixture(t *testing.T, bin, repo string, e []string) boardFixture {
	t.Helper()
	var f boardFixture

	// 12 ready tickets, deliberately more than defaultBoardLimit (10), which
	// TestBoardLimitTruncatesRowsNotCounts and TestBoardJSONShape rely on to
	// catch a hard-coded limit. value/confidence/effort are each clamped to
	// the CLI's 0-10 range; the expected order is derived from a live
	// `ready` call in each test, never asserted by construction here, so
	// exact score values (and any ties) do not matter.
	for i := 0; i < 12; i++ {
		value := 10 - (i % 11)
		id := mustAdd(t, bin, repo, e, "--score", strconv.Itoa(value)+"/5/5", "ready item "+strconv.Itoa(i))
		f.readyIDs = append(f.readyIDs, id)
	}

	f.claimed = mustAdd(t, bin, repo, e, "claimed item")
	if _, stderr, code := run(t, bin, repo, e, "start", f.claimed); code != 0 {
		t.Fatalf("start %s: exit %d, stderr=%q", f.claimed, code, stderr)
	}

	f.blocker = mustAdd(t, bin, repo, e, "the blocker")
	f.epic = mustAdd(t, bin, repo, e, "the epic")
	f.child = mustAdd(t, bin, repo, e, "--parent", f.epic, "the child")
	mustDep(t, bin, repo, e, f.epic, "blocks", f.blocker)

	f.note = mustBacklogCapture(t, bin, repo, e, "--type", "note", "a raw note")

	return f
}

// boardCountsLine parses "ready N   in progress M   blocked B   backlog K"
// by exact field extraction (awk-style), never a substring match.
func boardCountsLine(t *testing.T, line string) (ready, inProgress, blocked, backlog int) {
	t.Helper()
	fields := strings.Fields(line)
	// ["ready", "N", "in", "progress", "M", "blocked", "B", "backlog", "K"]
	if len(fields) != 9 || fields[0] != "ready" || fields[2] != "in" || fields[3] != "progress" || fields[5] != "blocked" || fields[7] != "backlog" {
		t.Fatalf("board counts line = %q, want the fixed ready/in progress/blocked/backlog shape", line)
	}
	var err error
	if ready, err = strconv.Atoi(fields[1]); err != nil {
		t.Fatalf("board counts line = %q, ready field not an int: %v", line, err)
	}
	if inProgress, err = strconv.Atoi(fields[4]); err != nil {
		t.Fatalf("board counts line = %q, in-progress field not an int: %v", line, err)
	}
	if blocked, err = strconv.Atoi(fields[6]); err != nil {
		t.Fatalf("board counts line = %q, blocked field not an int: %v", line, err)
	}
	if backlog, err = strconv.Atoi(fields[8]); err != nil {
		t.Fatalf("board counts line = %q, backlog field not an int: %v", line, err)
	}
	return
}

// jsonArrayLen runs args (which must end in --json and produce a flat JSON
// array) and returns its length. Every count in
// TestBoardTerminalCountsAndQueue is checked against it, so the test does
// not hand-derive an expected count from the fixture's construction (the
// fixture's "blocker" item is itself an ordinary open ticket, so it is also
// ready, and a hand-counted expectation would drift as the fixture grows).
func jsonArrayLen(t *testing.T, bin, repo string, e []string, args ...string) int {
	t.Helper()
	stdout, stderr, code := run(t, bin, repo, e, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d, stderr=%q", args, code, stderr)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatalf("%v: json.Unmarshal(%q): %v", args, stdout, err)
	}
	return len(items)
}

// TestBoardTerminalCountsAndQueue proves the count line's four numbers and
// the queue rows' first fields, cross-checked against `ready`/`blocked`/
// `backlog`/`list --status in_progress`'s own output from the same store.
func TestBoardTerminalCountsAndQueue(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	buildBoardFixture(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "board")
	if code != 0 {
		t.Fatalf("board: exit %d, stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("board: stderr = %q, want empty", stderr)
	}

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("board stdout = %q, want at least a counts line and a blank line", stdout)
	}
	ready, inProgress, blocked, backlog := boardCountsLine(t, lines[0])

	wantReady := jsonArrayLen(t, bin, repo, e, "ready", "--json")
	wantInProgress := jsonArrayLen(t, bin, repo, e, "list", "--status", "in_progress", "--json")
	wantBlocked := jsonArrayLen(t, bin, repo, e, "blocked", "--json")
	wantBacklog := jsonArrayLen(t, bin, repo, e, "backlog", "--json")

	if ready != wantReady {
		t.Fatalf("board: ready count = %d, want %d (ready --json)", ready, wantReady)
	}
	if ready <= defaultBoardLimit {
		t.Fatalf("board: fixture's ready count = %d, want it above defaultBoardLimit (%d) so the queue-truncation assertion below is meaningful", ready, defaultBoardLimit)
	}
	if inProgress != wantInProgress {
		t.Fatalf("board: in-progress count = %d, want %d (list --status in_progress --json)", inProgress, wantInProgress)
	}
	if blocked != wantBlocked {
		t.Fatalf("board: blocked count = %d, want %d (blocked --json)", blocked, wantBlocked)
	}
	if backlog != wantBacklog {
		t.Fatalf("board: backlog count = %d, want %d (backlog --json)", backlog, wantBacklog)
	}
	if lines[1] != "" {
		t.Fatalf("board stdout line 2 = %q, want a blank line", lines[1])
	}

	// The queue rows (from line 3 onward) must be a prefix of `ready`'s own
	// order, at most defaultBoardLimit (10) of them.
	rows := lines[2:]
	if len(rows) != defaultBoardLimit {
		t.Fatalf("board: got %d queue rows, want %d (defaultBoardLimit)", len(rows), defaultBoardLimit)
	}
	readyStdout, readyStderr, readyCode := run(t, bin, repo, e, "ready")
	if readyCode != 0 {
		t.Fatalf("ready: exit %d, stderr=%q", readyCode, readyStderr)
	}
	wantIDs := idsFromLines(readyStdout)
	for i, row := range rows {
		fields := strings.Fields(row)
		if len(fields) == 0 {
			t.Fatalf("board queue row %d = %q, want a rendered item row", i, row)
		}
		if fields[0] != wantIDs[i] {
			t.Fatalf("board queue row %d first field = %q, want %q (ready's own order)", i, fields[0], wantIDs[i])
		}
	}
}

// TestBoardLimitTruncatesRowsNotCounts proves --limit 2 yields exactly 2
// rows while all four counts stay identical to --limit 0's.
func TestBoardLimitTruncatesRowsNotCounts(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	buildBoardFixture(t, bin, repo, e)

	allStdout, allStderr, allCode := run(t, bin, repo, e, "board", "--limit", "0")
	if allCode != 0 {
		t.Fatalf("board --limit 0: exit %d, stderr=%q", allCode, allStderr)
	}
	allLines := strings.Split(strings.TrimRight(allStdout, "\n"), "\n")
	wantReady, wantInProgress, wantBlocked, wantBacklog := boardCountsLine(t, allLines[0])
	if len(allLines)-2 != wantReady {
		t.Fatalf("board --limit 0: got %d rows, want %d (all ready items, unlimited)", len(allLines)-2, wantReady)
	}

	limStdout, limStderr, limCode := run(t, bin, repo, e, "board", "--limit", "2")
	if limCode != 0 {
		t.Fatalf("board --limit 2: exit %d, stderr=%q", limCode, limStderr)
	}
	limLines := strings.Split(strings.TrimRight(limStdout, "\n"), "\n")
	gotReady, gotInProgress, gotBlocked, gotBacklog := boardCountsLine(t, limLines[0])
	if gotReady != wantReady || gotInProgress != wantInProgress || gotBlocked != wantBlocked || gotBacklog != wantBacklog {
		t.Fatalf("board --limit 2 counts = (%d,%d,%d,%d), want (%d,%d,%d,%d) -- limit must not change counts",
			gotReady, gotInProgress, gotBlocked, gotBacklog, wantReady, wantInProgress, wantBlocked, wantBacklog)
	}
	rows := limLines[2:]
	if len(rows) != 2 {
		t.Fatalf("board --limit 2: got %d rows, want 2", len(rows))
	}
}

// boardJSONOut mirrors board.go's boardJSON, kept independent (the test
// drives a separate binary) so the test proves the wire shape, not a shared
// Go type.
type boardJSONOut struct {
	Counts struct {
		Ready      int `json:"ready"`
		InProgress int `json:"in_progress"`
		Blocked    int `json:"blocked"`
		Backlog    int `json:"backlog"`
	} `json:"counts"`
	Items []map[string]any `json:"items"`
}

// TestBoardJSONShape unmarshals the WHOLE stdout buffer and asserts
// counts.ready against `ready --json`'s own item count, and that items is
// [] (never null) on an empty store.
func TestBoardJSONShape(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	buildBoardFixture(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "board", "--json")
	if code != 0 {
		t.Fatalf("board --json: exit %d, stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("board --json: stderr = %q, want empty", stderr)
	}
	var out boardJSONOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("board --json: json.Unmarshal(entire stdout %q): %v", stdout, err)
	}

	readyStdout, readyStderr, readyCode := run(t, bin, repo, e, "ready", "--json")
	if readyCode != 0 {
		t.Fatalf("ready --json: exit %d, stderr=%q", readyCode, readyStderr)
	}
	var readyItems []map[string]any
	if err := json.Unmarshal([]byte(readyStdout), &readyItems); err != nil {
		t.Fatalf("ready --json: json.Unmarshal: %v", err)
	}
	if out.Counts.Ready != len(readyItems) {
		t.Fatalf("board --json counts.ready = %d, want %d (len(ready --json))", out.Counts.Ready, len(readyItems))
	}
	if out.Items == nil {
		t.Fatalf("board --json items = nil, want a non-nil (possibly empty) slice")
	}
	if len(out.Items) != defaultBoardLimit {
		t.Fatalf("board --json: got %d items, want %d (defaultBoardLimit)", len(out.Items), defaultBoardLimit)
	}

	// Empty store: items must be [], never null.
	emptyRepo := tempDir(t)
	emptyHome := tempDir(t)
	emptyEnv := env(emptyHome, "")
	mustInit(t, bin, emptyRepo, emptyEnv)
	emptyStdout, emptyStderr, emptyCode := run(t, bin, emptyRepo, emptyEnv, "board", "--json")
	if emptyCode != 0 {
		t.Fatalf("board --json (empty store): exit %d, stderr=%q", emptyCode, emptyStderr)
	}
	if !strings.Contains(emptyStdout, `"items": []`) && !strings.Contains(emptyStdout, `"items":[]`) {
		t.Fatalf("board --json (empty store) stdout = %q, want items to be [] literally", emptyStdout)
	}
	var emptyOut boardJSONOut
	if err := json.Unmarshal([]byte(emptyStdout), &emptyOut); err != nil {
		t.Fatalf("board --json (empty store): json.Unmarshal: %v", err)
	}
	if emptyOut.Items == nil {
		t.Fatalf("board --json (empty store): items = nil, want non-nil empty slice")
	}
	if len(emptyOut.Items) != 0 {
		t.Fatalf("board --json (empty store): items = %v, want empty", emptyOut.Items)
	}
}

// freePort binds 127.0.0.1:0, reads the OS-assigned port, and closes the
// listener before returning, the standard "obtain a free port" technique.
// It is racy on a busy machine (another process could grab the same port
// between Close and the subprocess's own bind), which is accepted because
// a hard-coded port collides as soon as two test runs (or two repos'
// `board --web` sessions) overlap.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("freePort: close: %v", err)
	}
	return port
}

// webProc wraps a real `board --web` subprocess. cmd.Wait() must be called
// at most once (calling it twice concurrently, say from a background
// goroutine and from the test, is a data race per os/exec's documentation),
// so wait() is the one way to reap it; waited tracks whether that has
// happened so t.Cleanup's guard never races with it either.
type webProc struct {
	cmd    *exec.Cmd
	waited atomic.Bool
}

// wait calls cmd.Wait() exactly once and records that it happened.
func (p *webProc) wait() error {
	err := p.cmd.Wait()
	p.waited.Store(true)
	return err
}

// startBoardWeb starts `keep board --web [args...]` as a real subprocess,
// reads its first stdout line, and returns the parsed URL and the running
// process. t.Cleanup's Process.Kill() guard only fires if wait() has not
// already been called, never as a raw kill of an uncertain PID (and never
// kill(0), which would signal this test binary's own process group).
func startBoardWeb(t *testing.T, bin, repo string, e []string, extraArgs ...string) (*url.URL, *webProc, *bufio.Reader) {
	t.Helper()

	args := append([]string{"board", "--web"}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = repo
	cmd.Env = e
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start board --web: %v", err)
	}
	p := &webProc{cmd: cmd}

	t.Cleanup(func() {
		if !p.waited.Load() {
			cmd.Process.Kill()
		}
	})

	r := bufio.NewReader(stdout)
	line, err := r.ReadString('\n')
	if err != nil {
		cmd.Process.Kill()
		t.Fatalf("board --web: reading first stdout line: %v (stderr so far: %q)", err, stderrBuf.String())
	}
	u, err := url.Parse(strings.TrimSpace(line))
	if err != nil {
		cmd.Process.Kill()
		t.Fatalf("board --web: first stdout line %q is not a URL: %v", line, err)
	}

	return u, p, r
}

// TestBoardWebPrintsURLThenServes proves the URL is genuinely the first
// (and, this test asserts, only necessary) line, names 127.0.0.1 as the
// host, and that all four views answer 200 over real HTTP requests --
// reading the URL alone would only prove a string was printed, not that
// anything is listening.
func TestBoardWebPrintsURLThenServes(t *testing.T) {
	// Windows has no SIGINT to send a process, so "Ctrl-C exits cleanly"
	// is not a statement that can be made there. The board's own shutdown
	// path is exercised on the other two platforms.
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT does not exist on Windows")
	}
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	f := buildBoardFixture(t, bin, repo, e)

	u, p, _ := startBoardWeb(t, bin, repo, e)
	if u.Hostname() != "127.0.0.1" {
		t.Fatalf("board --web URL host = %q, want 127.0.0.1", u.Hostname())
	}

	client := &http.Client{Timeout: 3 * time.Second}

	for _, path := range []string{"/", "/backlog", "/item/" + f.child, "/tree/" + f.epic} {
		resp, err := client.Get(u.String() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", path, resp.StatusCode)
		}
	}

	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal SIGINT: %v", err)
	}
	if err := p.wait(); err != nil {
		t.Fatalf("board --web: want clean exit after SIGINT, got %v", err)
	}
}

// TestBoardWebCtrlCExitsZero checks that `board --web` exits 0 on SIGINT:
// after a real 200, it sends os.Interrupt and requires cmd.Wait() to return
// nil.
func TestBoardWebCtrlCExitsZero(t *testing.T) {
	// Windows has no SIGINT to send a process, so "Ctrl-C exits cleanly"
	// is not a statement that can be made there. The board's own shutdown
	// path is exercised on the other two platforms.
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT does not exist on Windows")
	}
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	buildBoardFixture(t, bin, repo, e)

	u, p, _ := startBoardWeb(t, bin, repo, e)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(u.String() + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", resp.StatusCode)
	}

	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal SIGINT: %v", err)
	}
	if err := p.wait(); err != nil {
		t.Fatalf("cmd.Wait() after SIGINT = %v, want nil (exit 0)", err)
	}
}

// TestBoardWebPortOverride proves --port <n> produces a URL naming that
// exact port.
func TestBoardWebPortOverride(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	port := freePort(t)
	u, p, _ := startBoardWeb(t, bin, repo, e, "--port", strconv.Itoa(port))
	if u.Port() != strconv.Itoa(port) {
		t.Fatalf("board --web --port %d: URL = %q, want port %d named in it", port, u.String(), port)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(u.String() + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()

	p.cmd.Process.Signal(os.Interrupt)
	p.wait()
}

// TestBoardWebRejectsJSON proves --web --json exits 2 and prints nothing on
// stdout.
func TestBoardWebRejectsJSON(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "board", "--web", "--json")
	if code != 2 {
		t.Fatalf("board --web --json: exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("board --web --json: stdout = %q, want empty", stdout)
	}
}

// TestBoardPortWithoutWebRejected proves --port without --web exits 2
// before the store opens.
func TestBoardPortWithoutWebRejected(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	// Deliberately not mustInit-ed: the rejection must happen before the
	// store is ever opened.
	stdout, stderr, code := run(t, bin, repo, e, "board", "--port", "9999")
	if code != 2 {
		t.Fatalf("board --port 9999 (no --web): exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("board --port 9999 (no --web): stdout = %q, want empty", stdout)
	}
}
