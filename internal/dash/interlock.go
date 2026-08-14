package dash

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/gshepptech/rounce/internal/interlock"
	"github.com/gshepptech/rounce/internal/model"
)

// interlockPage is /who's page data: who is working, and what they hold.
//
// It lives in its own file rather than render.go for the reason that file
// states about itself -- its only non-stdlib import is internal/model, which
// is what makes every function there fixturable from struct literals with no
// database and no wall clock. This view needs internal/interlock's types, so
// it takes its own file rather than quietly widening that claim. Same
// reasoning as repos.go.
type interlockPage struct {
	// Available is false when Interlock is not installed, not answering, or
	// speaking a version this build does not understand. The page then says
	// so plainly instead of rendering an empty table, which would read as
	// "nobody is working" -- the same lie an empty cross-repo queue would
	// tell about a deleted store.
	Available bool

	Status interlock.Status

	// Held pairs each lease with the queue item it is holding back, where
	// there is one. A lease on a file no item claims is still worth showing
	// -- an agent is editing something -- but a lease that is costing you a
	// workable ticket is the one you act on, so the two are distinguished
	// rather than merged into one list.
	Held []interlock.Collision
}

// handleInterlock renders who is working and what they hold.
func (s *Server) handleInterlock(w http.ResponseWriter, r *http.Request) {
	page := interlockPage{}

	st, ok := interlock.Fetch(r.Context(), "")
	page.Available = ok
	if ok {
		page.Status = st

		// The collisions shown here are computed from the SAME Exclude
		// call the queue view uses, over the same unlimited Ready result.
		// Two implementations of "is this held" would drift, and the drift
		// would show up as a board that omits an item from the queue while
		// this page cannot say why.
		items, err := s.st.Ready(r.Context(), 0)
		if err != nil {
			s.fail(w, err)
			return
		}
		_, page.Held = interlock.Exclude(items, st)
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "interlock", page); err != nil {
		s.fail(w, fmt.Errorf("render interlock: %w", err))
		return
	}
	writeHTML(w, buf.Bytes())
}

// readyItems is the queue view's store call and Interlock filter in one
// place, because handleReady and handleInterlock must not disagree about
// what is workable.
//
// It returns the kept items and what was held back. A caller that only
// wants the queue ignores the second value; nothing recomputes it.
func (s *Server) readyItems(ctx context.Context) ([]model.Item, []interlock.Collision, error) {
	items, err := s.st.Ready(ctx, 0)
	if err != nil {
		return nil, nil, err
	}
	st, ok := interlock.Fetch(ctx, "")
	if !ok {
		return items, nil, nil
	}
	kept, held := interlock.Exclude(items, st)
	return kept, held, nil
}
