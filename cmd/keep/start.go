package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// runStart implements the `start` subcommand: claim an item for
// the calling session. Session id comes from $KEEP_SESSION via
// resolveClaimSession, never from a flag. Interlock sets that variable for
// hook invocations, which is how the two line up without either side
// knowing about the other.
//
// Exit codes follow StartItem's six-row state table exactly, and
// store.StartItem's doc comment (internal/store/transitions.go) enumerates
// the checks in the order the store performs them: a same-session repeat
// exits 0 (an idempotent no-op; the store writes no second event); every
// other refusal (different session, raw stage, blocked, closed/dropped)
// exits 1, not 2, because these are operational failures, not usage
// errors. exitFor passes the store's own message through unmodified: it
// already names the holding session and claimed_at (ErrConflict) or the
// blocker (ErrInvalidTransition), and restating any of that here could
// drift from what the store decided.
//
// There is no flag to steal an existing claim: taking work away from a
// session belongs to Interlock's lease handling and to `keep unclaim`.
func runStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	actorFlag := addActorFlag(fs)
	forceFlag := fs.Bool("force", false, "claim even if another agent holds a file this item declares")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: keep start [flags] <id>")
		return 2
	}
	id := fs.Arg(0)

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: start: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	ctx := context.Background()

	// The join, at the point of claiming. `ready` omits items whose
	// declared files another agent holds, but that would not stop an agent
	// claiming one by id, and an agent that already knows the id it wants
	// is the one that never sees the queue's omission. The
	// check is here rather than in internal/store because it means running
	// another program, which the store does not do, and must never do from
	// inside the transaction that holds the write lock.
	//
	// --force exists because this is advisory. A person who can see that
	// the holding agent is gone should not have to release someone else's
	// lease to get on with their own work; what they should not do is claim
	// it without being told.
	if !*forceFlag {
		detail, err := s.GetItem(ctx, id)
		if err != nil {
			return exitFor(stderr, err)
		}
		if _, held := heldBack(ctx, []model.Item{detail.Item}); len(held) > 0 {
			c := held[0]
			fmt.Fprintf(stderr, "keep: start: %s declares %s, which is held by %s (session %s) -- wait for it, or pass --force\n",
				c.Item.ID, c.Path, c.Lease.Agent, c.Lease.SessionID)
			return 1
		}
	}

	session := resolveClaimSession()
	item, err := s.StartItem(ctx, store.StartItemInput{
		ID:        id,
		Actor:     actor,
		SessionID: session,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, item); err != nil {
			fmt.Fprintf(stderr, "keep: start: %v\n", err)
			return 1
		}
		return 0
	}

	// A same-session retry returns success with the item UNCHANGED.
	// StartItem deliberately does not report which case fired (fresh claim
	// vs idempotent retry), since telling the two apart would require the
	// store to return state it does not track (restated here because the CLI
	// is where the temptation to print "already claimed by you" would
	// otherwise show up). Both cases print the identical confirmation line
	// below.
	fmt.Fprintf(stdout, "%s claimed by session %q\n", item.ID, session)
	return 0
}
