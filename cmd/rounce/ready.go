package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/rounce/internal/model"
)

// runReady implements the `ready` subcommand (READY-01): the ranked queue of
// workable items, exactly as the shipped `ready` view computes it.
//
// cmd/rounce may not contain SQL or filtering/sorting logic (STORE-10,
// READY-03) -- runReady makes exactly one store.Ready call and renders its
// result unmodified. It does not re-sort, does not drop raw items, does not
// check status: every predicate lives in the view, per 02-CONTEXT.md's
// carried-forward decision and docs/design/phase-1.md's warning against
// re-adding a filter in a caller "for clarity".
func runReady(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	limitFlag := fs.Int("limit", 0, "maximum number of items to return (default: no limit)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: rounce ready [flags]")
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
	defer s.Close()

	items, err := s.Ready(context.Background(), *limitFlag)
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		// A nil []model.Item marshals to `null`, and an agent doing
		// `for item in $(jq '.[]')` breaks on null. An empty queue is
		// exit 0, not an error (RANK-02/CLI-01), so the JSON shape must
		// say so unambiguously: substitute an empty, non-nil slice
		// before encoding.
		if items == nil {
			items = []model.Item{}
		}
		if err := emitJSON(stdout, items); err != nil {
			fmt.Fprintf(stderr, "rounce: ready: %v\n", err)
			return 1
		}
		return 0
	}

	// renderItems is a no-op on an empty slice -- an empty queue prints
	// nothing and still exits 0.
	renderItems(stdout, items, renderOpts{})
	return 0
}
