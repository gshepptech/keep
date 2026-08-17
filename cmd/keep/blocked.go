package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/model"
)

// runBlocked implements the `blocked` subcommand (READY-04): the structural
// inverse of `ready`, each item naming the nearest item blocking it.
//
// One store.Blocked call, no re-filtering in Go (same STORE-10/READY-03
// discipline runReady follows). The blockedBy map built from the results is
// passed through renderOpts so `blocked` shares renderItems with `ready`
// rather than a second, independently-drifting formatter.
func runBlocked(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("blocked", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep blocked [flags]")
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	blockedItems, err := s.Blocked(context.Background())
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		// Same null-vs-empty-array discipline as ready --json.
		out := blockedItems
		if out == nil {
			out = []model.BlockedItem{}
		}
		if err := emitJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "keep: blocked: %v\n", err)
			return 1
		}
		return 0
	}

	items := make([]model.Item, len(blockedItems))
	blockedBy := make(map[string]string, len(blockedItems))
	for i, bi := range blockedItems {
		items[i] = bi.Item
		blockedBy[bi.Item.ID] = bi.BlockedBy
	}
	renderItems(stdout, items, renderOpts{blockedBy: blockedBy})
	return 0
}
