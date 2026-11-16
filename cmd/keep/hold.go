package main

import (
	"context"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/model"
)

// heldBack asks Interlock which of items collide with a lease it currently
// holds, and returns the two halves.
//
// It is the one place in cmd/keep that pairs interlock.Fetch with
// interlock.Exclude. `ready`, `next` and `start` all go through it, so the
// three cannot disagree about what "held" means. A queue that hides an item
// and a claim that allows it are worse together than either alone, because
// the agent that ignores the queue and claims by id gets the collision the
// queue existed to prevent.
//
// Where Interlock is absent, unusable, or speaking a version this keep does
// not know, every item comes back kept and nothing comes back held: keep
// works alone, and a join that failed loudly where the peer is missing
// would be worse than no join.
//
// The empty dir means "this process's working directory", which is where
// the agent is working, not wherever --db points. `interlock status`
// resolves its own repository root by walking up from where it runs, exactly
// as `keep ready` resolves its store.
func heldBack(ctx context.Context, items []model.Item) ([]model.Item, []interlock.Collision) {
	st, ok := interlock.Fetch(ctx, "")
	if !ok {
		return items, nil
	}
	return interlock.Exclude(items, st)
}

// explainHolds writes one line per held item to stderr, naming the file, the
// agent and the session.
//
// stderr, never stdout: stdout carries the queue or the item, and an agent
// piping it must not have to strip prose. Naming the holder is the point --
// an item that merely vanished would be indistinguishable from one that was
// closed.
func explainHolds(stderr io.Writer, verb string, held []interlock.Collision) {
	for _, c := range held {
		fmt.Fprintf(stderr, "keep: %s: %s held back -- %s is held by %s (session %s)\n",
			verb, c.Item.ID, c.Path, c.Lease.Agent, c.Lease.SessionID)
	}
}

// heldIDs is heldBack's answer as the id list store.NextInput.Skip takes.
func heldIDs(held []interlock.Collision) []string {
	out := make([]string, len(held))
	for i, c := range held {
		out[i] = c.Item.ID
	}
	return out
}
