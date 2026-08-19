package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
)

// runExport writes the whole store to stdout (or --out) as JSONL.
//
// This is a round-trip, not sync -- see store.Export. It exists because
// three things people needed had no answer at all: backing a store up,
// moving one to another machine, and reading what is in one without this
// binary. None of those requires merge resolution, and the README's refusal
// of multi-machine sync is untouched by them.
//
// Output goes to stdout by default so it composes: `keep export | gzip >
// backup.jsonl.gz` needs no flag. The progress line goes to stderr, because
// stdout is the file.
func runExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	out := fs.String("out", "", "write to this file instead of stdout")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep export [flags]")
		return 2
	}

	s, err := openStoreReadOnly(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	w := stdout
	if *out != "" {
		// O_EXCL: an export that silently overwrote yesterday's backup
		// would destroy the thing it exists to protect.
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			fmt.Fprintf(stderr, "keep: export: %v\n", err)
			return 1
		}
		defer f.Close()
		w = f
	}

	header, err := s.Export(context.Background(), w)
	if err != nil {
		fmt.Fprintf(stderr, "keep: export: %v\n", err)
		return 1
	}

	fmt.Fprintf(stderr, "keep: export: %d items, %d deps, %d events (schema_version %d)\n",
		header.Items, header.Deps, header.Events, header.SchemaVersion)
	return 0
}

// runImport reads an export back into an EMPTY store.
//
// It refuses a store that already holds items, and that refusal is the
// boundary rather than a missing feature: merging two stores means deciding
// what to do when both hold an item with the same id and different bodies,
// and every answer to that is a sync product. See store.Import.
func runImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: keep import [flags] [file]   (reads stdin when no file is given)")
		return 2
	}

	r := io.Reader(os.Stdin)
	if fs.NArg() == 1 && fs.Arg(0) != "-" {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "keep: import: %v\n", err)
			return 1
		}
		defer f.Close()
		r = f
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	summary, err := s.Import(context.Background(), r)
	if err != nil {
		return exitFor(stderr, err)
	}

	fmt.Fprintf(stdout, "imported %d items, %d deps, %d events\n", summary.Items, summary.Deps, summary.Events)
	return 0
}
