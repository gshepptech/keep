package main

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"
	"time"
)

// newTestFlagSet registers the three flag shapes permuteArgs must handle:
// a string flag (--type), a bool flag (--json), and a repeatable
// stringSlice (--blocks). Every subtest below gets a fresh FlagSet, so flag
// state cannot leak between table rows.
func newTestFlagSet() (*flag.FlagSet, *string, *bool, *stringSlice) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	typeFlag := fs.String("type", "", "item type")
	jsonFlag := fs.Bool("json", false, "json output")
	var blocksFlag stringSlice
	fs.Var(&blocksFlag, "blocks", "blocking id, repeatable")
	return fs, typeFlag, jsonFlag, &blocksFlag
}

// TestPermuteArgs asserts through a real flag.FlagSet after Parse, not on
// permuteArgs's raw output alone: what matters is the parsed result, and
// asserting only the intermediate slice would let a permutation that is
// textually plausible but semantically wrong pass.
func TestPermuteArgs(t *testing.T) {
	t.Run("title then flags -- the canonical capture-shaped invocation", func(t *testing.T) {
		fs, typeFlag, _, _ := newTestFlagSet()
		if err := fs.Parse(permuteArgs(fs, []string{"Q3 roadmap", "--type", "roadmap"})); err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if got, want := fs.Args(), []string{"Q3 roadmap"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fs.Args() = %v, want %v", got, want)
		}
		if *typeFlag != "roadmap" {
			t.Errorf("type = %q, want %q", *typeFlag, "roadmap")
		}
	})

	t.Run("dash-dash unchanged shape -- weird title still works", func(t *testing.T) {
		fs, _, _, _ := newTestFlagSet()
		if err := fs.Parse(permuteArgs(fs, []string{"--", "--weird-title"})); err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if got, want := fs.Args(), []string{"--weird-title"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fs.Args() = %v, want %v", got, want)
		}
	})

	t.Run("bool flag after positional moves ahead alone, taking no value", func(t *testing.T) {
		fs, _, jsonFlag, _ := newTestFlagSet()
		if err := fs.Parse(permuteArgs(fs, []string{"title", "--json"})); err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if got, want := fs.Args(), []string{"title"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fs.Args() = %v, want %v", got, want)
		}
		if !*jsonFlag {
			t.Errorf("json = false, want true")
		}
	})

	t.Run("equals form after positional moves ahead intact", func(t *testing.T) {
		fs, typeFlag, _, _ := newTestFlagSet()
		if err := fs.Parse(permuteArgs(fs, []string{"title", "--type=roadmap"})); err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if got, want := fs.Args(), []string{"title"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fs.Args() = %v, want %v", got, want)
		}
		if *typeFlag != "roadmap" {
			t.Errorf("type = %q, want %q", *typeFlag, "roadmap")
		}
	})

	t.Run("already-correct order is unchanged -- permutation is a no-op", func(t *testing.T) {
		fs, typeFlag, _, _ := newTestFlagSet()
		if err := fs.Parse(permuteArgs(fs, []string{"--type", "roadmap", "title"})); err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if got, want := fs.Args(), []string{"title"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fs.Args() = %v, want %v", got, want)
		}
		if *typeFlag != "roadmap" {
			t.Errorf("type = %q, want %q", *typeFlag, "roadmap")
		}
	})

	// A pre-pass that unconditionally inserts "--" ahead of positionals sets
	// a missing value to the literal string "--", silently, with exit 0.
	// permuteArgs is written to avoid that, and this case checks it does.
	t.Run("missing value regression lock -- value-taking flag with nothing after it", func(t *testing.T) {
		fs, typeFlag, _, _ := newTestFlagSet()
		if err := fs.Parse(permuteArgs(fs, []string{"title", "--type"})); err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if *typeFlag == "--" {
			t.Fatalf("type = %q -- the flag silently swallowed \"--\" as its value", *typeFlag)
		}
		if *typeFlag != "" {
			t.Errorf("type = %q, want empty (flag.Parse's own native handling of the malformed tail)", *typeFlag)
		}
		// The tail stays in fs.Args() exactly as flag alone would leave it:
		// flag.Parse on the un-permuted ["title", "--type"] stops at "title"
		// and never touches --type.
		if got, want := fs.Args(), []string{"title", "--type"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fs.Args() = %v, want %v", got, want)
		}
	})

	t.Run("repeatable flag -- both occurrences keep their values", func(t *testing.T) {
		fs, _, _, blocksFlag := newTestFlagSet()
		if err := fs.Parse(permuteArgs(fs, []string{"title", "--blocks", "a", "--blocks", "b"})); err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if got, want := fs.Args(), []string{"title"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fs.Args() = %v, want %v", got, want)
		}
		if got, want := []string(*blocksFlag), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
			t.Errorf("blocks = %v, want %v", got, want)
		}
	})
}

// TestResolveBody checks resolveBody's gating logic as a pure function
// against a real os.Stdin swap, including the "never touches stdin unless
// the flag is literally -" contract. The process-level check that a real
// pipe round-trips through the real binary is capture_test.go's
// TestAddBodyDashReadsStdin.
func TestResolveBody(t *testing.T) {
	t.Run(`"-" reads everything from stdin, byte for byte`, func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		orig := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = orig }()

		want := "line one\nline two, no trailing newline"
		go func() {
			// A short write shows up as a mismatch in the comparison below.
			_, _ = w.WriteString(want)
			w.Close()
		}()

		got, err := resolveBody("-")
		if err != nil {
			t.Fatalf("resolveBody(\"-\"): %v", err)
		}
		if got != want {
			t.Fatalf("resolveBody(\"-\") = %q, want %q", got, want)
		}
	})

	t.Run("non-dash values are returned unchanged and never touch stdin", func(t *testing.T) {
		// A pipe whose writer is deliberately never closed: any real read
		// from it blocks forever. If resolveBody touched stdin for a
		// non-"-" value, this test would hang rather than fail cleanly,
		// which is the signal that matters here.
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		defer w.Close()
		orig := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = orig }()

		for _, in := range []string{"some text", ""} {
			done := make(chan struct{})
			var got string
			var gotErr error
			go func() {
				got, gotErr = resolveBody(in)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("resolveBody(%q) blocked -- it touched stdin", in)
			}
			if gotErr != nil {
				t.Fatalf("resolveBody(%q): %v", in, gotErr)
			}
			if got != in {
				t.Fatalf("resolveBody(%q) = %q, want %q", in, got, in)
			}
		}
	})
}

// TestResolveActor proves --actor > $KEEP_ACTOR > $USER, including the
// ""-set-but-empty-behaves-as-unset case every nullable convention in this
// codebase already uses, and the terminal error case with no third
// fallback.
func TestResolveActor(t *testing.T) {
	cases := []struct {
		name       string
		flagActor  string
		roundActor string
		user       string
		want       string
		wantErr    bool
	}{
		{
			name:       "--actor wins over both env vars",
			flagActor:  "alice",
			roundActor: "bob",
			user:       "carol",
			want:       "alice",
		},
		{
			name:       "no --actor: KEEP_ACTOR wins over USER",
			flagActor:  "",
			roundActor: "bob",
			user:       "carol",
			want:       "bob",
		},
		{
			name:       "no --actor, no KEEP_ACTOR: falls back to USER",
			flagActor:  "",
			roundActor: "",
			user:       "carol",
			want:       "carol",
		},
		{
			name:       "nothing set: error, no third fallback",
			flagActor:  "",
			roundActor: "",
			user:       "",
			wantErr:    true,
		},
		{
			name:       "KEEP_ACTOR set but empty behaves as unset, falls to USER",
			flagActor:  "",
			roundActor: "",
			user:       "carol",
			want:       "carol",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEEP_ACTOR", tc.roundActor)
			t.Setenv("USER", tc.user)

			got, err := resolveActor(tc.flagActor)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveActor(%q) = %q, nil, want an error", tc.flagActor, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveActor(%q): %v", tc.flagActor, err)
			}
			if got != tc.want {
				t.Fatalf("resolveActor(%q) = %q, want %q", tc.flagActor, got, tc.want)
			}
		})
	}
}

// TestResolveActorReadsKEEP_ACTORCrossProcess drives the real compiled binary,
// because an in-process table test can prove resolveActor's own logic but
// cannot prove the real binary actually reads the variable end to end. It sets
// KEEP_ACTOR=mcp-actor (USER is "keep-test", per env()'s base set) and asserts
// the add event's actor field is "mcp-actor", not "keep-test".
func TestResolveActorReadsKEEP_ACTORCrossProcess(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "", "KEEP_ACTOR=mcp-actor")
	mustInit(t, bin, repo, e)

	id := mustAdd(t, bin, repo, e, "an item added with KEEP_ACTOR set")

	stdout, stderr, code := run(t, bin, repo, e, "show", id, "--json")
	if code != 0 {
		t.Fatalf("show %s --json: exit %d, stderr=%q", id, code, stderr)
	}

	var detail struct {
		Events []struct {
			Kind  string `json:"kind"`
			Actor string `json:"actor"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(stdout), &detail); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout, err)
	}

	var addEvent *struct {
		Kind  string `json:"kind"`
		Actor string `json:"actor"`
	}
	for i := range detail.Events {
		if detail.Events[i].Kind == "add" {
			addEvent = &detail.Events[i]
			break
		}
	}
	if addEvent == nil {
		t.Fatalf("show %s --json: no %q event in %v", id, "add", detail.Events)
	}
	if addEvent.Actor != "mcp-actor" {
		t.Fatalf("add event actor = %q, want %q (KEEP_ACTOR, not $USER=%q)", addEvent.Actor, "mcp-actor", "keep-test")
	}
}
