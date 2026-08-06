package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/rounce/internal/store"
)

// runClose implements the `close` subcommand (ITEM-07/08): mark an item
// closed, recording why. See drop.go's runDrop for the near-identical
// dropped path.
//
// runClose and runDrop are two small files rather than one shared
// runCloseOrDrop: they are two commands with two help texts and two usage
// lines, and the shared logic that actually must never drift
// (store.closeOrDrop, internal/store/transitions.go) already lives in the
// store, reachable identically by this CLI and, later, Phase 4's MCP
// surface. Collapsing the two into one Go function here would buy nothing
// this package needs and would risk the opposite of ITEM-08's point: close
// and drop staying genuinely distinct.
func runClose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	actorFlag := addActorFlag(fs)
	reasonFlag := fs.String("reason", "", "why this item is being closed (required)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: rounce close [flags] <id> --reason <text>")
		return 2
	}
	id := fs.Arg(0)

	// An absent --reason and an explicit --reason "" are indistinguishable
	// through flag's *string default (both leave *reasonFlag == "") --
	// both must exit 2. ITEM-07 says the command records WHY; a blank
	// reason is an audit trail that records that something happened and
	// nothing else.
	if *reasonFlag == "" {
		fmt.Fprintln(stderr, "rounce: close: --reason is required and cannot be empty")
		return 2
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: close: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
	defer s.Close()

	item, err := s.CloseItem(context.Background(), store.CloseItemInput{
		ID:        id,
		Reason:    *reasonFlag,
		Actor:     actor,
		SessionID: resolveSession(),
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	fmt.Fprintf(stdout, "%s closed\n", item.ID)
	return 0
}
