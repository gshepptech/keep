package main

import (
	"strings"
	"testing"
)

// mustCapture runs `keep capture <title> --body -` (the canonical
// invocation order: title first, flags after, proving permuteArgs handles
// it) with stdin as the body, and fails
// the test immediately if it does not succeed, returning the trimmed id.
func mustCapture(t *testing.T, bin, repo string, e []string, stdin, title string) string {
	t.Helper()
	stdout, stderr, code := runStdin(t, bin, repo, e, stdin, "capture", title, "--body", "-")
	if code != 0 {
		t.Fatalf("capture %q --body -: exit %d, stderr=%q", title, code, stderr)
	}
	return strings.TrimSpace(stdout)
}

// firstDiffOffset returns the byte offset of the first difference between a
// and b, or the length of the shorter string when one is a prefix of the
// other (the append/truncation point). Used only to report a mismatch --
// dumping a full 1MB document into a test log is unreadable, and length plus
// offset is what actually identifies a trailing-newline or CRLF bug.
func firstDiffOffset(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// buildLargeMarkdownDoc generates a deterministic document of at least 1MB,
// ending with a distinct marker line so a silent truncation at the tail
// (the class of bug most likely from a buffer size limit) would be caught
// by the byte-length/content check, not just an approximate size check.
func buildLargeMarkdownDoc() string {
	var b strings.Builder
	b.WriteString("# Large document\n\n")
	line := "The quick brown fox jumps over the lazy dog. 0123456789.\n"
	for b.Len() < 1024*1024 {
		b.WriteString(line)
	}
	b.WriteString("## End marker, do not truncate me\n")
	return b.String()
}

// TestCaptureShowBodyRoundTripsByteIdentical: a real document, piped into
// `capture --body -`, must come back out of `show --body` byte-identical --
// no normalisation in either direction. Every document shape that
// round-trips at the storage layer is exercised here again at the
// CLI/process boundary, through a real pipe and the real binary.
func TestCaptureShowBodyRoundTripsByteIdentical(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	markdown := "# Q3 roadmap\n\n" +
		"## Goals\n\n" +
		"Ship capture and the byte-identical round trip.\n\n" +
		"```go\n" +
		"func main() {\n" +
		"\tfmt.Println(\"hi\")\n" +
		"}\n" +
		"```\n\n" +
		"## Notes\n\n" +
		"- item one\n" +
		"- item two\n" +
		"- item three\n\n" +
		"Final paragraph.\n"
	withoutTrailingNewline := strings.TrimSuffix(markdown, "\n")
	crlf := strings.ReplaceAll(markdown, "\n", "\r\n")
	// The NUL/invalid-UTF-8 case is a regression lock, not an open
	// question: it round-trips with zero transform through TEXT NOT NULL on
	// the pinned driver. It exists to
	// catch someone later adding "defensive" encoding, not to discover new
	// behavior.
	nulAndInvalidUTF8 := "before\x00after " + string([]byte{0xff, 0xfe, 0xfd}) + " end\x00"
	large := buildLargeMarkdownDoc()

	cases := []struct {
		name string
		body string
	}{
		{"markdown with trailing newline", markdown},
		{"markdown without trailing newline", withoutTrailingNewline},
		{"CRLF line endings", crlf},
		{"empty document", ""},
		{"NUL byte and invalid UTF-8", nulAndInvalidUTF8},
		{"large document (>=1MB)", large},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := mustCapture(t, bin, repo, e, tc.body, "roundtrip: "+tc.name)

			stdout, stderr, code := run(t, bin, repo, e, "show", "--body", id)
			if code != 0 {
				t.Fatalf("show --body: exit %d, stderr=%q", code, stderr)
			}
			if stderr != "" {
				t.Fatalf("show --body: stderr = %q, want empty", stderr)
			}

			// No trimming or normalising on either side of this comparison --
			// that would hide precisely the class of bug (an added or
			// stripped trailing newline, mangled CRLF) this test exists to
			// catch. A raw string != is deliberately stricter than a
			// diff-based check.
			if stdout != tc.body {
				offset := firstDiffOffset(stdout, tc.body)
				t.Fatalf("show --body byte mismatch: len(got)=%d len(want)=%d, first differing byte at offset %d", len(stdout), len(tc.body), offset)
			}
		})
	}
}

// TestShowBodyAndJSONAreMutuallyExclusive proves --body and --json cannot be
// combined: two different output contracts, combining them means nothing
// in combination.
func TestShowBodyAndJSONAreMutuallyExclusive(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "an item")

	stdout, stderr, code := run(t, bin, repo, e, "show", "--body", "--json", id)
	if code != 2 {
		t.Fatalf("show --body --json: exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("show --body --json: stdout = %q, want empty", stdout)
	}
}

// TestShowBodyEmptyBodyPrintsNothing proves an item added with no body
// prints exactly zero bytes under --body and still exits 0 -- the empty
// case is success, not an error.
func TestShowBodyEmptyBodyPrintsNothing(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "an item with no body")

	stdout, stderr, code := run(t, bin, repo, e, "show", "--body", id)
	if code != 0 {
		t.Fatalf("show --body: exit %d, stderr=%q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("show --body: stdout = %q, want zero bytes", stdout)
	}
	if stderr != "" {
		t.Fatalf("show --body: stderr = %q, want empty", stderr)
	}
}
