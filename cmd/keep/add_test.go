package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// mustInit runs `keep init` in repo via bin and fails the test immediately
// if it does not succeed -- every add/dep test needs a store in place before
// it can do anything else.
func mustInit(t *testing.T, bin, repo string, e []string) {
	t.Helper()
	_, stderr, code := run(t, bin, repo, e, "init")
	if code != 0 {
		t.Fatalf("init failed: exit %d, stderr=%q", code, stderr)
	}
}

var rootIDPattern = regexp.MustCompile(`^rnc-[0-9a-f]{6}$`)

func TestAddPrintsID(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "add", "a new item")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one line", stdout)
	}
	if !rootIDPattern.MatchString(lines[0]) {
		t.Fatalf("stdout = %q, want it to match %s", stdout, rootIDPattern)
	}
}

func TestAddChildIDs(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	epicOut, stderr, code := run(t, bin, repo, e, "add", "epic")
	if code != 0 {
		t.Fatalf("add epic: exit %d, stderr=%q", code, stderr)
	}
	epicID := strings.TrimSpace(epicOut)

	for i, suffix := range []string{".1", ".2", ".3"} {
		// Flags must precede the title: stdlib flag.Parse stops consuming
		// flags at the first non-flag argument, and runAdd deliberately
		// joins everything left in fs.Args() into the (possibly
		// multi-word, unquoted) title -- see add.go's runAdd doc comment.
		out, stderr, code := run(t, bin, repo, e, "add", "--parent", epicID, "child")
		if code != 0 {
			t.Fatalf("add child %d: exit %d, stderr=%q", i, code, stderr)
		}
		id := strings.TrimSpace(out)
		want := epicID + suffix
		if id != want {
			t.Fatalf("child %d id = %q, want %q", i, id, want)
		}
	}
}

func TestAddScoreValidation(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	// Each row's wantSubstr is the fragment the plan requires stderr to
	// name for that malformed input: a specific part label ("value",
	// "confidence", "effort") when parseScore got exactly three parts but
	// one was bad, or "part(s)" when the part count itself was wrong.
	cases := []struct {
		score      string
		wantSubstr string
	}{
		{"8//3", "confidence"},
		{"8/7", "part(s)"},
		{"8/7/3/2", "part(s)"},
		{"8/7/99", "effort"},
		{"a/7/3", "value"},
	}
	for _, tc := range cases {
		t.Run(tc.score, func(t *testing.T) {
			stdout, stderr, code := run(t, bin, repo, e, "add", "--score", tc.score, "x")
			if code != 2 {
				t.Fatalf("--score %q: exit code = %d, want 2 (stderr=%q)", tc.score, code, stderr)
			}
			if stdout != "" {
				t.Fatalf("--score %q: stdout = %q, want empty", tc.score, stdout)
			}
			if !strings.Contains(stderr, tc.wantSubstr) {
				t.Fatalf("--score %q: stderr = %q, want it to mention %q", tc.score, stderr, tc.wantSubstr)
			}
		})
	}
}

func TestAddJSON(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "add", "--score", "8/7/3", "--json", "scored item")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout, err)
	}
	for _, field := range []string{"id", "score", "value", "confidence", "effort"} {
		if _, ok := got[field]; !ok {
			t.Fatalf("json output missing field %q: %v", field, got)
		}
	}
}

func TestAddUnknownType(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "add", "--type", "nonsense", "x")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr=%q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	for _, want := range []string{"roadmap", "epic", "task", "bug", "note"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to list legal type %q", stderr, want)
		}
	}
}
