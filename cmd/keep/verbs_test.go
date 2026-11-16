package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// next, why-not, export, import, migrate, and --all on the cross-repository
// verbs are tested here through a real built binary rather than only through
// internal/store.
//
// cmd/keep's job is exit codes, which stream
// output lands on, and flag interaction, and none of those exist at the
// store layer: `why-not` returning the right answer with the wrong exit code
// passes every store test in the repository. This package's tests run a
// subprocess for exactly that reason, and scripts/coverage.sh
// is what measures whether it actually got there.

// addScored files an item and returns its id.
func addScored(t *testing.T, bin, repo string, e []string, score, title string) string {
	t.Helper()
	stdout, stderr, code := run(t, bin, repo, e, "add", "--score", score, title)
	if code != 0 {
		t.Fatalf("add %q: exit %d, stderr=%s", title, code, stderr)
	}
	return strings.TrimSpace(stdout)
}

// TestNextClaimsTheTopOfTheQueue checks that `keep next` claims the top item,
// printing the item to stdout so it can be piped and the confirmation to
// stderr so it does not have to be stripped.
func TestNextClaimsTheTopOfTheQueue(t *testing.T) {
	bin := buildKeep(t, "")
	repo, home := tempDir(t), tempDir(t)
	e := env(home, "", "KEEP_SESSION=sess-next")
	mustInit(t, bin, repo, e)

	addScored(t, bin, repo, e, "2/2/9", "low")
	top := addScored(t, bin, repo, e, "9/9/1", "high")

	stdout, stderr, code := run(t, bin, repo, e, "next")
	if code != 0 {
		t.Fatalf("next: exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, top) {
		t.Errorf("next claimed something other than the top of the queue:\nstdout=%s", stdout)
	}
	if strings.Contains(stdout, "claimed by session") {
		t.Errorf("the confirmation is on stdout, where a caller piping the item would have to strip it:\n%s", stdout)
	}
	if !strings.Contains(stderr, "sess-next") {
		t.Errorf("stderr does not name the claiming session:\n%s", stderr)
	}

	// It is genuinely claimed, not merely reported.
	show, _, _ := run(t, bin, repo, e, "show", top, "--json")
	if !strings.Contains(show, "in_progress") {
		t.Errorf("the item is not in progress after next:\n%s", show)
	}
}

// TestNextOnAnEmptyQueueExitsZero checks that `keep next` on an empty queue
// exits 0, because "nothing to do" is an answer. A caller
// that had to treat it as failure could not tell it from a real one, and
// would either stop on a healthy store or retry forever on a broken one.
func TestNextOnAnEmptyQueueExitsZero(t *testing.T) {
	bin := buildKeep(t, "")
	repo, home := tempDir(t), tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "next")
	if code != 0 {
		t.Fatalf("next on an empty store: exit %d, want 0. stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("next printed to stdout with nothing to claim: %q", stdout)
	}
	if !strings.Contains(stderr, "nothing is workable") {
		t.Errorf("stderr does not say why nothing came back:\n%s", stderr)
	}
}

// TestWhyNotExitCodeIsTheAnswer checks why-not's exit codes: 0 workable, 1
// not, and an error for an unknown id, so a script can branch on it without
// parsing prose.
func TestWhyNotExitCodeIsTheAnswer(t *testing.T) {
	bin := buildKeep(t, "")
	repo, home := tempDir(t), tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	workable := addScored(t, bin, repo, e, "5/5/5", "workable")
	blocker := addScored(t, bin, repo, e, "5/5/5", "the blocker")
	blocked := addScored(t, bin, repo, e, "5/5/5", "blocked by the blocker")
	if _, stderr, code := run(t, bin, repo, e, "dep", blocked, "blocks", blocker); code != 0 {
		t.Fatalf("dep: exit %d, %s", code, stderr)
	}

	if stdout, _, code := run(t, bin, repo, e, "why-not", workable); code != 0 {
		t.Errorf("why-not on a workable item: exit %d, want 0. stdout=%s", code, stdout)
	} else if !strings.Contains(stdout, "workable now") {
		t.Errorf("stdout does not say it is workable: %s", stdout)
	}

	stdout, _, code := run(t, bin, repo, e, "why-not", blocked)
	if code != 1 {
		t.Errorf("why-not on a blocked item: exit %d, want 1", code)
	}
	if !strings.Contains(stdout, blocker) {
		t.Errorf("the reason does not name the blocker %s: %s", blocker, stdout)
	}

	if _, _, code := run(t, bin, repo, e, "why-not", "rnc-nosuch"); code != 1 {
		t.Errorf("why-not on an unknown id: exit %d, want 1", code)
	}

	// --json carries the same answer in a form a script can read.
	raw, _, _ := run(t, bin, repo, e, "why-not", blocked, "--json")
	var w struct {
		Ready     bool   `json:"ready"`
		BlockedBy string `json:"blocked_by"`
	}
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("why-not --json is not JSON: %v\n%s", err, raw)
	}
	if w.Ready || w.BlockedBy != blocker {
		t.Errorf("why-not --json = %+v, want ready=false blocked_by=%s", w, blocker)
	}
}

// TestExportImportThroughTheBinary checks the round trip as a person would
// do it: export one store to a file, import it into a fresh one, and ask
// both the same question.
func TestExportImportThroughTheBinary(t *testing.T) {
	bin := buildKeep(t, "")
	src, home := tempDir(t), tempDir(t)
	e := env(home, "")
	mustInit(t, bin, src, e)

	top := addScored(t, bin, src, e, "9/9/1", "Expire refresh tokens")
	addScored(t, bin, src, e, "3/3/7", "Something else")
	if _, stderr, code := run(t, bin, src, e, "capture", "Q3 roadmap", "--type", "roadmap"); code != 0 {
		t.Fatalf("capture: exit %d, %s", code, stderr)
	}

	dump := filepath.Join(tempDir(t), "backup.jsonl")
	if _, stderr, code := run(t, bin, src, e, "export", "--out", dump); code != 0 {
		t.Fatalf("export: exit %d, %s", code, stderr)
	}
	if _, stderr, code := run(t, bin, src, e, "export", "--out", dump); code == 0 {
		t.Errorf("export overwrote an existing file; a backup that destroys the previous backup is not one (stderr=%s)", stderr)
	}

	dst := tempDir(t)
	dstHome := tempDir(t)
	de := env(dstHome, "")
	mustInit(t, bin, dst, de)

	stdout, stderr, code := run(t, bin, dst, de, "import", dump)
	if code != 0 {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "imported 3 items") {
		t.Errorf("import did not report what it wrote: %s", stdout)
	}

	// The same question, both stores.
	srcReady, _, _ := run(t, bin, src, e, "ready", "--json")
	dstReady, _, _ := run(t, bin, dst, de, "ready", "--json")
	if srcReady != dstReady {
		t.Errorf("the restored store answers `ready` differently:\nsrc=%s\ndst=%s", srcReady, dstReady)
	}
	if !strings.Contains(dstReady, top) {
		t.Errorf("the restored queue is missing %s: %s", top, dstReady)
	}

	// And a second import is refused rather than doubling everything.
	if _, stderr, code := run(t, bin, dst, de, "import", dump); code == 0 {
		t.Error("importing into a populated store succeeded; it must refuse rather than merge")
	} else if !strings.Contains(stderr, "empty store") {
		t.Errorf("the refusal does not explain itself: %s", stderr)
	}
}

// TestMigrateReportsAndDoesNothingWhenCurrent checks that `keep migrate` on a
// current store succeeds and changes nothing, and that --check exits 0, so a
// script can run it unconditionally or use it as a gate.
func TestMigrateReportsAndDoesNothingWhenCurrent(t *testing.T) {
	bin := buildKeep(t, "")
	repo, home := tempDir(t), tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "migrate", "--check")
	if code != 0 {
		t.Fatalf("migrate --check on a current store: exit %d, want 0. stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Errorf("migrate --check does not say the store is current: %s", stdout)
	}

	if _, stderr, code := run(t, bin, repo, e, "migrate"); code != 0 {
		t.Fatalf("migrate on a current store: exit %d, want 0. stderr=%s", code, stderr)
	}

	// No backup file was left behind for a migration that did not happen.
	entries, err := os.ReadDir(filepath.Join(repo, ".rounce"))
	if err != nil {
		t.Fatalf("read .rounce: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".bak") {
			t.Errorf("a no-op migration left a backup file %s", entry.Name())
		}
	}

	// --json is machine-readable and says the same thing.
	raw, _, _ := run(t, bin, repo, e, "migrate", "--check", "--json")
	var rep struct {
		From       int      `json:"from"`
		To         int      `json:"to"`
		Migrations []string `json:"migrations"`
		Applied    bool     `json:"applied"`
	}
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatalf("migrate --json is not JSON: %v\n%s", err, raw)
	}
	if rep.From != rep.To || len(rep.Migrations) != 0 || rep.Applied {
		t.Errorf("migrate --check --json on a current store = %+v, want from==to and nothing pending", rep)
	}
}

// TestCrossRepoFlagsMergeEveryRegisteredStore covers the four verbs that
// grew --all. They share one fan-out (cmd/keep/across.go), so the assertion
// that matters for each is the same: rows from BOTH repositories, tagged
// with which one they came from.
func TestCrossRepoFlagsMergeEveryRegisteredStore(t *testing.T) {
	bin := buildKeep(t, "")
	home := tempDir(t)
	e := env(home, "")

	// Two repositories, registered by their own `init`, with names taken
	// from their directory names.
	repoA, repoB := tempDir(t), tempDir(t)
	mustInit(t, bin, repoA, e)
	mustInit(t, bin, repoB, e)

	aTicket := addScored(t, bin, repoA, e, "9/9/1", "alpha token work")
	bTicket := addScored(t, bin, repoB, e, "8/8/1", "beta token work")
	if _, stderr, code := run(t, bin, repoA, e, "capture", "alpha parked idea"); code != 0 {
		t.Fatalf("capture in A: exit %d, %s", code, stderr)
	}
	if _, stderr, code := run(t, bin, repoB, e, "capture", "beta parked idea"); code != 0 {
		t.Fatalf("capture in B: exit %d, %s", code, stderr)
	}

	nameA, nameB := filepath.Base(repoA), filepath.Base(repoB)

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"search --all", []string{"search", "--all", "token"}, []string{aTicket, bTicket, nameA, nameB}},
		{"backlog --all", []string{"backlog", "--all"}, []string{"alpha parked idea", "beta parked idea", nameA, nameB}},
		{"log --all", []string{"log", "--all"}, []string{aTicket, bTicket, nameA, nameB}},
		{"board --all", []string{"board", "--all"}, []string{aTicket, bTicket, nameA, nameB}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Run from a third directory, so nothing can be answered from a
			// store that happens to be underfoot.
			stdout, stderr, code := run(t, bin, tempDir(t), e, tc.args...)
			if code != 0 {
				t.Fatalf("exit %d, stderr=%s", code, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want) {
					t.Errorf("output does not contain %q -- the merge is missing a repository:\n%s", want, stdout)
				}
			}
		})
	}

	// --db names one store, --all reads the registry. Picking one silently
	// would be worse than refusing.
	for _, verb := range []string{"search", "backlog", "log", "board"} {
		args := []string{verb, "--all", "--db", filepath.Join(repoA, ".rounce", "db.sqlite")}
		if verb == "search" {
			args = append(args, "token")
		}
		if _, stderr, code := run(t, bin, tempDir(t), e, args...); code != 2 {
			t.Errorf("%s --all --db: exit %d, want 2 (mutually exclusive). stderr=%s", verb, code, stderr)
		}
	}
}

// TestCrossRepoJSONAlwaysCarriesAnErrorsArray checks that cross-repo --json
// output always has an errors array. A consumer needs to know it saw a
// partial view, and an empty array is a positive statement that every
// registered store was read, which an absent field is not.
func TestCrossRepoJSONAlwaysCarriesAnErrorsArray(t *testing.T) {
	bin := buildKeep(t, "")
	home := tempDir(t)
	e := env(home, "")
	repo := tempDir(t)
	mustInit(t, bin, repo, e)
	addScored(t, bin, repo, e, "5/5/5", "something")

	for _, args := range [][]string{
		{"search", "--all", "--json", "something"},
		{"backlog", "--all", "--json"},
		{"log", "--all", "--json"},
		{"board", "--all", "--json"},
	} {
		t.Run(args[0], func(t *testing.T) {
			stdout, stderr, code := run(t, bin, tempDir(t), e, args...)
			if code != 0 {
				t.Fatalf("exit %d, stderr=%s", code, stderr)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, stdout)
			}
			raw, ok := payload["errors"]
			if !ok {
				t.Fatalf("no errors field; a consumer cannot tell a complete view from a partial one:\n%s", stdout)
			}
			if strings.TrimSpace(string(raw)) == "null" {
				t.Errorf("errors is null rather than []; an empty array is the positive statement:\n%s", stdout)
			}
		})
	}
}

// TestReadyAllToolAnswersLikeTheFlag checks that the `ready_all` tool and
// `ready --all --json` produce the same bytes for the same registry, since
// both are handed the same function.
//
// It goes through a real `keep mcp` subprocess rather than calling the
// handler, because what is being checked is that cmd/keep wired it in at
// all: a ReadyAll left nil compiles, registers the tool, and reports "no
// registry access" forever.
func TestReadyAllToolAnswersLikeTheFlag(t *testing.T) {
	bin := buildKeep(t, "")
	home := tempDir(t)
	e := env(home, "")

	repoA, repoB := tempDir(t), tempDir(t)
	mustInit(t, bin, repoA, e)
	mustInit(t, bin, repoB, e)
	addScored(t, bin, repoA, e, "9/9/1", "alpha work")
	addScored(t, bin, repoB, e, "8/8/1", "beta work")

	fromFlag, stderr, code := run(t, bin, tempDir(t), e, "ready", "--all", "--json")
	if code != 0 {
		t.Fatalf("ready --all --json: exit %d, %s", code, stderr)
	}

	session := mcpSession(t, bin, repoA, e)
	fromTool := callTool(t, session, "ready_all", map[string]any{})

	var flagPayload, toolPayload any
	if err := json.Unmarshal([]byte(fromFlag), &flagPayload); err != nil {
		t.Fatalf("ready --all --json is not JSON: %v\n%s", err, fromFlag)
	}
	if err := json.Unmarshal([]byte(fromTool), &toolPayload); err != nil {
		t.Fatalf("the ready_all tool did not return JSON: %v\n%s", err, fromTool)
	}

	flagAgain, _ := json.Marshal(flagPayload)
	toolAgain, _ := json.Marshal(toolPayload)
	if string(flagAgain) != string(toolAgain) {
		t.Errorf("the tool and the flag disagree about the cross-repository queue:\nflag=%s\ntool=%s", flagAgain, toolAgain)
	}
	if !strings.Contains(fromTool, "alpha work") || !strings.Contains(fromTool, "beta work") {
		t.Errorf("the tool's answer is missing a repository:\n%s", fromTool)
	}
}

// TestMCPRefusesAnEmptyReason checks that the MCP `close` tool refuses an
// empty reason, as `keep close` does. The rule lives in the store, so the
// test drives the tool, not the flag.
func TestMCPRefusesAnEmptyReason(t *testing.T) {
	bin := buildKeep(t, "")
	repo, home := tempDir(t), tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	id := addScored(t, bin, repo, e, "5/5/5", "something to close")

	session := mcpSession(t, bin, repo, e)

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"omitted", map[string]any{"id": id}},
		{"empty", map[string]any{"id": id, "reason": ""}},
		{"whitespace", map[string]any{"id": id, "reason": "   "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := callToolExpectingToolError(t, session, "close", tc.args)
			if !strings.Contains(text, "reason is required") {
				t.Errorf("close with a %s reason returned %q, want a refusal naming the requirement", tc.name, text)
			}
		})
	}

	// It is still open: none of the three refusals closed it.
	show, _, _ := run(t, bin, repo, e, "show", id, "--json")
	if !strings.Contains(show, `"status":"open"`) && !strings.Contains(show, `"status": "open"`) {
		t.Errorf("the item is no longer open after three refused closes:\n%s", show)
	}

	// And with a reason it closes, so the refusals above are about the
	// reason rather than about the tool.
	if text := callTool(t, session, "close", map[string]any{"id": id, "reason": "merged in #42"}); strings.Contains(text, "reason is required") {
		t.Errorf("close with a real reason was refused: %s", text)
	}
}

// TestMCPRefusesAnInvalidTypeByName checks that an invalid type over MCP is
// refused with an error naming the value. Without the Go-side check it
// would reach the schema's CHECK constraint and come back as SQLite's own
// text, "constraint failed: CHECK constraint failed: type IN (...) (275)",
// forwarded as a generic protocol error that a model cannot tell apart
// from a broken store.
func TestMCPRefusesAnInvalidTypeByName(t *testing.T) {
	bin := buildKeep(t, "")
	repo, home := tempDir(t), tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	session := mcpSession(t, bin, repo, e)
	err := callToolExpectingError(t, session, "add", map[string]any{"title": "x", "type": "nonsense"})

	if !strings.Contains(err, "nonsense") {
		t.Errorf("the error does not name the offending value: %s", err)
	}
	if !strings.Contains(err, "task") {
		t.Errorf("the error does not name the legal values, so a model cannot correct it: %s", err)
	}
	if strings.Contains(err, "CHECK constraint") || strings.Contains(err, "275") {
		t.Errorf("the error is still SQLite's own text: %s", err)
	}
}

// callToolExpectingToolError returns the text of an isError:true tool
// result. It is the counterpart to callTool, which fatals on one. Both
// exist because the two outcomes are different things over MCP:
// an isError result is a legitimate answer a model reads and retries from,
// while a protocol error is a malformed call.
func callToolExpectingToolError(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: got a PROTOCOL error, want an isError result a model can read and retry from: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("tools/call %s: IsError = false, want the call refused. content = %+v", name, res.Content)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tools/call %s: Content[0] is %T, want *mcp.TextContent", name, res.Content[0])
	}
	return tc.Text
}

// callToolExpectingError returns the message of a JSON-RPC protocol error.
// A value outside a closed vocabulary is a malformed argument, which is the
// row ErrInvalidDepType has always occupied and which ErrInvalidType and
// ErrInvalidStage now share.
func callToolExpectingError(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err == nil {
		t.Fatalf("tools/call %s: no protocol error; result = %+v", name, res)
	}
	return err.Error()
}
