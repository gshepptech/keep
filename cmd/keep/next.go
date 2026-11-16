package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// runNext implements the `next` subcommand: take the top of the queue and
// claim it, in one call.
//
// The two-call sequence of `ready`, then `start` on the top row, has a
// window between the read and the write in which another agent can
// take the same item. store.NextItem closes it by doing both inside one
// transaction; see its doc comment. This function does no choosing of its
// own: the ranking lives in the ready view, as every other read here does.
//
// An empty queue exits 0, not 1. "Nothing is workable right now" is a
// correct answer to "what should I do next", and a caller that had to treat
// it as failure could not tell it apart from a real one. The count of
// blocked items goes with it, because "no work" and "work, all of it
// blocked" call for opposite responses.
func runNext(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	actorFlag := addActorFlag(fs)

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep next [flags]")
		return 2
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: next: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	ctx := context.Background()
	session := resolveClaimSession()

	// The join, applied where it matters most. `ready` already omits items
	// whose files another agent holds; `next` claims one, so offering a held
	// item here would hand an agent the collision the queue exists to
	// prevent, and to the agent that trusted the tool most, the one that
	// stopped reading the queue and asked for work.
	//
	// The queue is read unlimited, filtered, and the survivors' ids are what
	// NextItem is told to avoid. Reading it first costs one extra query and
	// is the only way to know which ids to skip; the store still chooses,
	// inside its transaction, from what is workable at that moment.
	queue, err := s.Ready(ctx, 0)
	if err != nil {
		return exitFor(stderr, err)
	}
	_, held := heldBack(ctx, queue)
	explainHolds(stderr, "next", held)

	res, err := s.NextItem(ctx, store.NextInput{Actor: actor, SessionID: session, Skip: heldIDs(held)})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, res); err != nil {
			fmt.Fprintf(stderr, "keep: next: %v\n", err)
			return 1
		}
		return 0
	}

	if !res.Claimed {
		// stderr, not stdout: stdout is the item, and a caller piping it
		// must not have to strip prose to find out there wasn't one. Same
		// rule `ready` follows for its held-back diagnostics.
		fmt.Fprintf(stderr, "keep: next: nothing is workable (%d open ticket(s) blocked, %d held by another agent)\n",
			res.Blocked, len(held))
		return 0
	}

	renderItems(stdout, []model.Item{res.Item}, renderOpts{})
	fmt.Fprintf(stderr, "keep: next: claimed by session %q\n", session)
	return 0
}
