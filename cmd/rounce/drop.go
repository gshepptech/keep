package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/rounce/internal/store"
)

// runDrop implements the `drop` subcommand (ITEM-07/08): mark an item
// dropped, recording why. See close.go's runClose for the near-identical
// closed path and its doc comment for why the two are separate files
// rather than one shared runCloseOrDrop.
func runDrop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("drop", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	actorFlag := addActorFlag(fs)
	reasonFlag := fs.String("reason", "", "why this item is being dropped (required)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: rounce drop [flags] <id> --reason <text>")
		return 2
	}
	id := fs.Arg(0)

	// Same rule as close: an absent --reason and --reason "" are both
	// exit 2 -- see close.go's identical check for the reasoning.
	if *reasonFlag == "" {
		fmt.Fprintln(stderr, "rounce: drop: --reason is required and cannot be empty")
		return 2
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: drop: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
	defer s.Close()

	item, err := s.DropItem(context.Background(), store.DropItemInput{
		ID:        id,
		Reason:    *reasonFlag,
		Actor:     actor,
		SessionID: resolveSession(),
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	fmt.Fprintf(stdout, "%s dropped\n", item.ID)
	return 0
}
