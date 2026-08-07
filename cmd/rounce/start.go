package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/rounce/internal/store"
)

// runStart implements the `start` subcommand (ITEM-06): claim an item for
// the calling session. Session id comes from $ROUNCE_SESSION via
// resolveSession, NEVER from a flag -- Interlock already sets that variable
// for hook invocations, which is how the two line up without either side
// knowing about the other.
//
// Exit codes follow 02-CONTEXT.md decision 3's locked six-row state table
// exactly, and store.StartItem's own doc comment (internal/store/
// transitions.go) enumerates the checks in the load-bearing order the store
// performs them: same-session repeat exits 0 (idempotent no-op -- the store
// writes NO second event); every other refusal (different session, raw
// stage, blocked, closed/dropped) exits 1, NOT 2 -- these are operational
// failures, not usage errors (02-CONTEXT.md says so explicitly). exitFor
// passes the store's own message through unmodified: it already names the
// holding session and claimed_at (ErrConflict) or the blocker
// (ErrInvalidTransition), and restating any of that here would only risk
// drifting from what the store actually decided.
//
// There is deliberately no flag to steal an existing claim: that belongs to
// Interlock's lease work (02-CONTEXT.md decision 3's "Claude's Discretion"
// / deferred list), not here. Do not add one "for testing".
func runStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	actorFlag := addActorFlag(fs)

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: rounce start [flags] <id>")
		return 2
	}
	id := fs.Arg(0)

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: start: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
	defer s.Close()

	session := resolveSession()
	item, err := s.StartItem(context.Background(), store.StartItemInput{
		ID:        id,
		Actor:     actor,
		SessionID: session,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, item); err != nil {
			fmt.Fprintf(stderr, "rounce: start: %v\n", err)
			return 1
		}
		return 0
	}

	// A same-session retry returns success with the item UNCHANGED --
	// StartItem deliberately does not report which case fired (fresh claim
	// vs idempotent retry), since telling the two apart would require the
	// store to return state it does not track (02-06-SUMMARY.md's own
	// reasoning, restated here because the CLI is where the temptation to
	// print "already claimed by you" would otherwise show up). Both cases
	// print the identical confirmation line below.
	fmt.Fprintf(stdout, "%s claimed by session %q\n", item.ID, session)
	return 0
}
