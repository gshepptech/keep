package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gshepptech/keep/internal/store"
)

// runReopen implements the `reopen` subcommand: bring a closed or dropped
// item back into the queue.
//
// Without it, close and drop would be statuses with no way out. An
// agent that closed the wrong id could not undo it: the work would have to
// be filed again under a new id, losing the event history and every
// dependency edge that pointed at the original. Filing a duplicate is not a
// recovery, it is a second record of the same work with the first one still
// claiming to be done.
//
// It is shaped like unclaim.go, close.go and drop.go: same flag set, same
// required --reason, same exit codes. Four verbs that change status and
// have to say why should not each look different.
//
// The MCP `reopen` tool calls the same store method, so an agent that
// closed the wrong id can undo it the same way.
func runReopen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reopen", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	actorFlag := addActorFlag(fs)
	reasonFlag := fs.String("reason", "", "why the item is coming back (required)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: keep reopen [flags] <id> --reason <text>")
		return 2
	}
	id := fs.Arg(0)

	// Same rule as close, drop and unclaim: an absent --reason and an
	// explicit empty one are indistinguishable through flag's *string
	// default, and both are exit 2. Reopening without a reason leaves a log
	// that says a finished item came back and nothing about what was wrong
	// with finishing it.
	if strings.TrimSpace(*reasonFlag) == "" {
		fmt.Fprintln(stderr, "keep: reopen: --reason is required and cannot be empty")
		return 2
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: reopen: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	item, err := s.ReopenItem(context.Background(), store.ReopenItemInput{
		ID:        id,
		Reason:    *reasonFlag,
		Actor:     actor,
		SessionID: resolveSession(),
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	fmt.Fprintf(stdout, "%s reopened\n", item.ID)
	return 0
}
