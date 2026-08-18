package dash

import (
	"context"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// readyItemsFrom is one project's store call and Interlock filter in one
// place, because the board's queue and its "held back" section must not
// disagree about what is workable.
//
// It returns the kept items and what was held back from the same Exclude
// call. A caller that only wants the queue ignores the second value;
// nothing recomputes it.
func (s *Server) readyItemsFrom(ctx context.Context, st *store.Store, dir string) ([]model.Item, []interlock.Collision, error) {
	items, err := st.Ready(ctx, 0)
	if err != nil {
		return nil, nil, err
	}
	// Only a project Interlock is set up for can have leases worth
	// filtering against, and only its own directory can answer for it.
	if !interlock.Watches(dir) {
		return items, nil, nil
	}
	status, ok := interlock.Fetch(ctx, dir)
	if !ok {
		return items, nil, nil
	}
	kept, held := interlock.Exclude(items, status)
	return kept, held, nil
}

// heldFor reports the collision holding one item back, if any.
//
// It runs the item through interlock.Exclude -- the same call the queue
// uses -- rather than comparing paths itself. A second rule for "is this
// held" could disagree with the board's, which is exactly the split
// DASH-07 exists to prevent: the item page would offer Claim on work the
// queue had already hidden, and neither page could explain the other.
func (s *Server) heldFor(ctx context.Context, item model.Item, dir string) *interlock.Collision {
	if !interlock.Watches(dir) {
		return nil
	}
	status, ok := interlock.Fetch(ctx, dir)
	if !ok {
		return nil
	}
	_, held := interlock.Exclude([]model.Item{item}, status)
	if len(held) == 0 {
		return nil
	}
	return &held[0]
}

// interlockRelease is the one Interlock mutation this board offers.
func interlockRelease(ctx context.Context, session string) error {
	return interlock.Release(ctx, session)
}
