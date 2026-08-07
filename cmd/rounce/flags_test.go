package main

import (
	"flag"
	"os"
	"reflect"
	"testing"
	"time"
)

// newTestFlagSet registers the same three flag shapes 03-RESEARCH.md Pattern
// 2 verified permuteArgs against: a string flag (--type), a bool flag
// (--json), and a repeatable stringSlice (--blocks). Every subtest below
// gets a fresh FlagSet -- flag state must not leak between table rows.
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

	// This is the silent-corruption regression lock (03-RESEARCH.md Pitfall
	// 1): a naive pre-pass that unconditionally inserts "--" ahead of
	// positionals sets a missing value to the literal string "--", silently,
	// with exit 0. Do not "tidy this up" as a weird edge case later -- it is
	// the exact defect this plan exists to prevent.
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
		// The tail stays in fs.Args() exactly as flag would have left it
		// before this change: flag.Parse on the un-permuted
		// ["title", "--type"] stops at "title" and never touches --type.
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

// TestResolveBody exercises resolveBody as a pure function against a real
// os.Stdin swap -- the process-level proof that a real pipe round-trips
// through the real binary lives in capture_test.go's
// TestAddBodyDashReadsStdin, but this table proves resolveBody's own gating
// logic directly and fast, including the "never touches stdin unless the
// flag is literally -" contract (03-RESEARCH.md Pitfall 4).
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
			w.WriteString(want)
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
		// non-"-" value, this test would hang rather than fail cleanly --
		// exactly the signal that matters here.
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
