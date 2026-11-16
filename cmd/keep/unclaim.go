package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gshepptech/keep/internal/store"
)

// runUnclaim implements the `unclaim` subcommand: return a claimed item to
// the queue without saying anything about the work.
//
// This is the third exit from in_progress, and it exists because the other
// two both lie about a dead agent. `close` records that the work was done;
// `drop` records that it was abandoned as not worth doing. When a session
// crashes mid-item, neither is true, and without this the only options
// would be to pick one anyway or leave the item stuck out of `ready` forever.
//
// It is shaped like close.go and drop.go (same flag set, same required
// --reason, same exit codes) because it is the same kind of operation: a
// state change that has to say why. What differs is what it asserts, which
// is only that the claim ended.
//
// The MCP `unclaim` tool is the narrow form that releases only the
// caller's own claim (see store.UnclaimItem's doc comment). Releasing
// someone else's claim can take work away from a live agent, so it belongs
// with whoever can look at the terminal, not with another agent that has
// just been told the queue is empty.
func runUnclaim(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unclaim", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	actorFlag := addActorFlag(fs)
	reasonFlag := fs.String("reason", "", "why the claim is being released (required)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: keep unclaim [flags] <id> --reason <text>")
		return 2
	}
	id := fs.Arg(0)

	// Same rule as close and drop: an absent --reason and an explicit empty
	// one are indistinguishable through flag's *string default, and both are
	// exit 2. A rescue with no reason is a trail that records that a claim
	// vanished and nothing about why.
	if strings.TrimSpace(*reasonFlag) == "" {
		fmt.Fprintln(stderr, "keep: unclaim: --reason is required and cannot be empty")
		return 2
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: unclaim: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	item, err := s.UnclaimItem(context.Background(), store.UnclaimItemInput{
		ID:        id,
		Reason:    *reasonFlag,
		Actor:     actor,
		SessionID: resolveSession(),
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	fmt.Fprintf(stdout, "%s returned to the queue\n", item.ID)
	return 0
}
