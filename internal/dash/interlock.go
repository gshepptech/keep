package dash

import (
	"context"

	"github.com/gshepptech/rounce/internal/interlock"
	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// readyItems is the queue view's store call and Interlock filter in one
// place, because handleReady and handleInterlock must not disagree about
// what is workable.
//
// It returns the kept items and what was held back. A caller that only
// wants the queue ignores the second value; nothing recomputes it.
func (s *Server) readyItems(ctx context.Context) ([]model.Item, []interlock.Collision, error) {
	return s.readyItemsFrom(ctx, s.st, repoDirOf(s.dbPath))
}

// readyItemsFrom is the same for one named project's store.
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

// interlockRelease is the one Interlock mutation this board offers.
func interlockRelease(ctx context.Context, session string) error {
	return interlock.Release(ctx, session)
}
