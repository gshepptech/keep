package main

import (
	"strings"
	"testing"
)

func TestDepVocabulary(t *testing.T) {
	bin := buildRounce(t, "")
	repo := t.TempDir()
	home := t.TempDir()
	e := env(home, "")
	mustInit(t, bin, repo, e)

	aOut, stderr, code := run(t, bin, repo, e, "add", "a")
	if code != 0 {
		t.Fatalf("add a: exit %d, stderr=%q", code, stderr)
	}
	a := strings.TrimSpace(aOut)
	bOut, stderr, code := run(t, bin, repo, e, "add", "b")
	if code != 0 {
		t.Fatalf("add b: exit %d, stderr=%q", code, stderr)
	}
	b := strings.TrimSpace(bOut)

	for _, depType := range []string{"blocks", "related", "derived-from"} {
		t.Run(depType, func(t *testing.T) {
			stdout, stderr, code := run(t, bin, repo, e, "dep", a, depType, b)
			if code != 0 {
				t.Fatalf("dep %s %s %s: exit %d, stderr=%q", a, depType, b, code, stderr)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
			if strings.TrimSpace(stdout) == "" {
				t.Fatalf("stdout is empty, want a confirmation line")
			}
		})
	}

	t.Run("parent-child", func(t *testing.T) {
		stdout, stderr, code := run(t, bin, repo, e, "dep", a, "parent-child", b)
		if code != 2 {
			t.Fatalf("exit code = %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "add --parent") {
			t.Fatalf("stderr = %q, want it to name `add --parent`", stderr)
		}
	})

	t.Run("nonsense", func(t *testing.T) {
		stdout, stderr, code := run(t, bin, repo, e, "dep", a, "nonsense", b)
		if code != 2 {
			t.Fatalf("exit code = %d, want 2 (stderr=%q)", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want empty", stdout)
		}
		for _, want := range []string{"blocks", "parent-child", "related", "derived-from"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr = %q, want it to list legal type %q", stderr, want)
			}
		}
	})
}

func TestDepSelfEdge(t *testing.T) {
	bin := buildRounce(t, "")
	repo := t.TempDir()
	home := t.TempDir()
	e := env(home, "")
	mustInit(t, bin, repo, e)

	aOut, stderr, code := run(t, bin, repo, e, "add", "a")
	if code != 0 {
		t.Fatalf("add a: exit %d, stderr=%q", code, stderr)
	}
	a := strings.TrimSpace(aOut)

	stdout, stderr, code := run(t, bin, repo, e, "dep", a, "blocks", a)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
}
