package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// captureFields is the subset of `show --json`'s output the capture tests
// assert on, decoded rather than grepped so a body containing the words
// "stage" or "type" cannot produce a false match.
type captureFields struct {
	Stage string `json:"stage"`
	Type  string `json:"type"`
	Body  string `json:"body"`
}

// showFields runs `keep show <id> --json` and decodes it into
// captureFields.
func showFields(t *testing.T, bin, dir string, e []string, id string) captureFields {
	t.Helper()
	stdout, stderr, code := run(t, bin, dir, e, "show", id, "--json")
	if code != 0 {
		t.Fatalf("show %s --json: exit %d, stderr=%q", id, code, stderr)
	}
	var got captureFields
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("show %s --json: json.Unmarshal(%q): %v", id, stdout, err)
	}
	return got
}

// docFixture is a multi-section markdown document with headings, a fenced
// code block, and a blank line: more structure than a bare string, so a
// naive line-trimming bug has something to fail on. It has no trailing
// newline, so the byte-identical contract is exercised in both directions.
const docFixture = `# Q3 roadmap

## Goals

- Ship capture
- Ship backlog

## Notes

` + "```" + `go
func main() {}
` + "```" + `
Last line, no trailing newline`

// TestCaptureCanonicalInvocation proves the canonical invocation (title
// first, flags after, a document piped in) exits 0, prints one id, and
// show --json reports stage=raw, the given type, and a body equal to the
// piped bytes.
func TestCaptureCanonicalInvocation(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := runStdin(t, bin, repo, e, docFixture,
		"capture", "Q3 roadmap", "--type", "roadmap", "--body", "-")
	if code != 0 {
		t.Fatalf(`capture "Q3 roadmap" --type roadmap --body -: exit %d, stderr=%q`, code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("capture: stdout = %q, want exactly one id line", stdout)
	}
	id := strings.TrimSpace(stdout)

	got := showFields(t, bin, repo, e, id)
	if got.Stage != "raw" {
		t.Fatalf("stage = %q, want %q", got.Stage, "raw")
	}
	if got.Type != "roadmap" {
		t.Fatalf("type = %q, want %q", got.Type, "roadmap")
	}
	if got.Body != docFixture {
		t.Fatalf("body = %q, want %q (the exact piped document)", got.Body, docFixture)
	}
}

// TestCaptureDefaultsToRawNote proves that with no --type and no --body,
// capture stores stage=raw, type=note (the default), and an empty body.
func TestCaptureDefaultsToRawNote(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "capture", "Rate limiter is per-process")
	if code != 0 {
		t.Fatalf("capture <title>: exit %d, stderr=%q", code, stderr)
	}
	id := strings.TrimSpace(stdout)

	got := showFields(t, bin, repo, e, id)
	if got.Stage != "raw" {
		t.Fatalf("stage = %q, want %q", got.Stage, "raw")
	}
	if got.Type != "note" {
		t.Fatalf("type = %q, want %q", got.Type, "note")
	}
	if got.Body != "" {
		t.Fatalf("body = %q, want empty", got.Body)
	}
}

// TestCaptureRefusesStageFlag proves --stage is refused by flag's own
// parser (unregistered, not hand-rejected): exit 2, stderr names "not
// defined", stdout is empty, and no item was created.
func TestCaptureRefusesStageFlag(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "capture", "--stage", "ticket", "x")
	if code != 2 {
		t.Fatalf("capture --stage ticket x: exit %d, want 2 (stderr=%q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("capture --stage ticket x: stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "not defined") {
		t.Fatalf("capture --stage ticket x: stderr = %q, want it to contain %q", stderr, "not defined")
	}

	// No item was created, so list --stage raw must be empty.
	listOut, listErr, listCode := run(t, bin, repo, e, "list", "--stage", "raw")
	if listCode != 0 {
		t.Fatalf("list --stage raw: exit %d, stderr=%q", listCode, listErr)
	}
	if listOut != "" {
		t.Fatalf("list --stage raw: stdout = %q, want empty -- --stage ticket must not have created anything", listOut)
	}
}

// TestCaptureEmptyStdinIsNotAnError proves --body - with an empty pipe
// exits 0 and stores an empty body, because io.ReadAll on an immediately-EOF
// reader is ("", nil), not an error.
func TestCaptureEmptyStdinIsNotAnError(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := runStdin(t, bin, repo, e, "", "capture", "empty body test", "--body", "-")
	if code != 0 {
		t.Fatalf("capture --body - (empty stdin): exit %d, stderr=%q", code, stderr)
	}
	id := strings.TrimSpace(stdout)

	got := showFields(t, bin, repo, e, id)
	if got.Body != "" {
		t.Fatalf("body = %q, want empty", got.Body)
	}
}

// TestAddBodyDashReadsStdin proves add --body - stores the piped bytes
// through the same resolveBody implementation capture uses, not a
// capture-only path.
func TestAddBodyDashReadsStdin(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := runStdin(t, bin, repo, e, docFixture,
		"add", "--body", "-", "a piped-body ticket")
	if code != 0 {
		t.Fatalf("add --body - <title>: exit %d, stderr=%q", code, stderr)
	}
	id := strings.TrimSpace(stdout)

	got := showFields(t, bin, repo, e, id)
	if got.Body != docFixture {
		t.Fatalf("body = %q, want %q (the exact piped document)", got.Body, docFixture)
	}
	// add's own default stage is "ticket", which confirms this path is
	// add's, not capture's, sharing only resolveBody.
	if got.Stage != "ticket" {
		t.Fatalf("stage = %q, want %q", got.Stage, "ticket")
	}
}

// TestCaptureRejectsUnknownType proves --type validation happens at the CLI
// boundary, exit 2, naming the legal types.
func TestCaptureRejectsUnknownType(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "capture", "--type", "nonsense", "x")
	if code != 2 {
		t.Fatalf("capture --type nonsense x: exit %d, want 2 (stderr=%q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("capture --type nonsense x: stdout = %q, want empty", stdout)
	}
	for _, want := range []string{"roadmap", "epic", "task", "bug", "note"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to list legal type %q", stderr, want)
		}
	}
}
