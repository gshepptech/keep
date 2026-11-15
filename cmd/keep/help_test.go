package main

import (
	"strings"
	"testing"
)

// TestAskingForHelpDoesNotFileAnItem.
//
// `keep add --help` must print usage, not file an item titled "--help"
// into the real store. The hazard is permuteArgs: -h and --help are
// handled by the flag package itself and so are absent from fs.VisitAll,
// so a pre-pass that classified them as value-taking flags would send them,
// last on the line with no value after them, down the "dump the rest
// verbatim" branch into the positionals, to arrive at flag.Parse behind a
// `--` and become the title.
//
// Two things make this worth a cross-process test rather than a unit test
// on permuteArgs. It is a whole-binary behaviour -- parse, then write, then
// exit code -- and the damage was to a store, so the assertion that matters
// is that the store is still empty afterwards. A test that only checked the
// exit code would have passed on a version that printed usage AND filed the
// item.
//
// Checked on every subcommand that takes a positional argument, because the
// pre-pass is shared: a fix in one of them alone would not be a fix.
func TestAskingForHelpDoesNotFileAnItem(t *testing.T) {
	bin := buildKeep(t, "")

	for _, flag := range []string{"--help", "-h"} {
		for _, cmd := range []string{"add", "capture"} {
			t.Run(cmd+" "+flag, func(t *testing.T) {
				repo := tempDir(t)
				home := tempDir(t)
				e := env(home, "")
				mustInit(t, bin, repo, e)

				stdout, stderr, code := run(t, bin, repo, e, cmd, flag)

				if code != 2 {
					t.Errorf("exit code = %d, want 2 (asking for help is not success)", code)
				}
				if strings.Contains(stdout, "rnc-") {
					t.Errorf("stdout printed an item id: %q", stdout)
				}
				// The flag list goes to stderr, which is where this
				// FlagSet's output is pointed.
				if !strings.Contains(stderr, "-acceptance") && !strings.Contains(stderr, "-type") {
					t.Errorf("stderr does not look like a flag list: %q", stderr)
				}

				// The point of the whole test: nothing was written.
				out, _, listCode := run(t, bin, repo, e, "list", "--json")
				if listCode != 0 {
					t.Fatalf("list exited %d", listCode)
				}
				if trimmed := strings.TrimSpace(out); trimmed != "[]" {
					t.Errorf("the store is not empty after asking for help: %s", trimmed)
				}
			})
		}
	}
}

// TestATitleThatLooksLikeAFlagStillWorks holds up the other side of the
// fix. `keep add -- --weird-title` is documented and must keep working:
// everything after `--` is positional by definition, including something
// spelled like a flag. A guard that refused it would have traded one bug
// for a smaller one.
func TestATitleThatLooksLikeAFlagStillWorks(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "add", "--", "--weird-title")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "rnc-") {
		t.Fatalf("no id printed: %q", stdout)
	}

	out, _, _ := run(t, bin, repo, e, "list", "--json")
	if !strings.Contains(out, "--weird-title") {
		t.Fatalf("the title was not stored verbatim: %s", out)
	}
}

// TestAnEmptyTitleIsRefusedAtTheCommandLine is the CLI end of the store's
// ErrEmptyTitle. The store test proves the rule; this proves the exit code
// a script would branch on, and that the message names the problem rather
// than reporting an internal error.
func TestAnEmptyTitleIsRefusedAtTheCommandLine(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)

	for _, title := range []string{"", "   "} {
		t.Run("title="+title, func(t *testing.T) {
			stdout, stderr, code := run(t, bin, repo, e, "add", title)
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (a usage error, not a crash)", code)
			}
			if strings.Contains(stdout, "rnc-") {
				t.Errorf("an id was printed for an untitled item: %q", stdout)
			}
			if !strings.Contains(stderr, "title") {
				t.Errorf("stderr does not mention the title: %q", stderr)
			}
		})
	}
}
