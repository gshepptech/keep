package dash

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/gshepptech/rounce/internal/interlock"
	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// page is what every template receives.
//
// The view data hangs off Data rather than being the top-level value,
// because every page now needs three things that have nothing to do with
// what it is showing: the form token, whatever went wrong last action, and
// which nav item is current. Threading those through each render function's
// own type would mean six types that all grew the same three fields.
type page struct {
	// Nav is the current section, so the nav can mark it rather than making
	// a reader work out where they are from the heading.
	Nav string

	// CSRF goes in every mutating form on the page.
	CSRF string

	// Problem is the last action's failure, shown once at the top. Empty
	// most of the time; a page that always showed a status bar would train
	// people to stop reading it.
	Problem string

	// Writable is false when the board was started without a path to write
	// to. Actions are then not rendered at all rather than rendered and
	// refused -- a button that is going to say no is worse than no button.
	Writable bool

	Data any
}

// newPage builds the wrapper for one request.
func (s *Server) newPage(r *http.Request, nav string, data any) page {
	return page{
		Nav:      nav,
		CSRF:     s.csrf,
		Problem:  r.URL.Query().Get("problem"),
		Writable: s.dbPath != "",
		Data:     data,
	}
}

// render executes a template against a page and writes it.
func (s *Server) render(w http.ResponseWriter, name string, p page) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		s.fail(w, fmt.Errorf("render %s: %w", name, err))
		return
	}
	writeHTML(w, buf.Bytes())
}

// boardData is the unified board: the queue, and everything that explains
// why the queue looks the way it does.
//
// One page rather than two. The queue and the agents holding it up are the
// same question asked from two sides, and a person who has to switch pages
// to connect them is doing the join in their head.
type boardData struct {
	Ready    []model.Item
	Held     []interlock.Collision
	Progress []model.Item
	Blocked  []model.BlockedItem
	Backlog  int

	// Interlock is nil when it is not installed here, which is most
	// repositories. The board then shows the queue and says nothing about
	// agents, rather than showing an empty agents table.
	Interlock *interlock.Status

	Types  []string
	Repo   string
	Counts boardCounts
}

// boardCounts is the readout across the top.
type boardCounts struct {
	Ready, InProgress, Blocked, Backlog, Agents, Holding int
}

// boardData assembles the whole board in one pass.
func (s *Server) boardData(r *http.Request) (boardData, error) {
	ctx := r.Context()
	d := boardData{Types: typeNames()}

	ready, held, err := s.readyItems(ctx)
	if err != nil {
		return d, err
	}
	d.Ready, d.Held = ready, held

	if d.Progress, err = s.st.ListItems(ctx, storeInProgress()); err != nil {
		return d, err
	}
	if d.Blocked, err = s.st.Blocked(ctx); err != nil {
		return d, err
	}
	backlog, err := s.st.Backlog(ctx, "")
	if err != nil {
		return d, err
	}
	d.Backlog = len(backlog)

	if st, ok := interlock.Fetch(ctx, ""); ok {
		d.Interlock = &st
		for _, sess := range st.Sessions {
			if !sess.Expired {
				d.Counts.Agents++
			}
		}
		d.Counts.Holding = len(st.Leases)
	}

	d.Counts.Ready = len(d.Ready)
	d.Counts.InProgress = len(d.Progress)
	d.Counts.Blocked = len(d.Blocked)
	d.Counts.Backlog = d.Backlog
	return d, nil
}

// storeInProgress is the one filter the board uses twice.
func storeInProgress() store.ListFilter {
	return store.ListFilter{Status: model.StatusInProgress}
}

// typeNames is the type vocabulary the add form offers, in the order
// model declares it -- so the dropdown cannot drift from what the store
// accepts.
func typeNames() []string {
	out := make([]string, 0, len(model.TypesOrdered))
	for _, t := range model.TypesOrdered {
		out = append(out, string(t))
	}
	return out
}

// titled pairs a page with its browser title, so "head" can take one value.
type titledPage struct {
	Page  page
	Title string
	Data  any
}

// actionArgs is one POST button's data. A struct rather than six template
// arguments because a positional call of six strings is unreadable at the
// call site and silently wrong when two of them swap.
type actionArgs struct {
	CSRF, Verb, ID, Label, Return string
	Danger                        bool
}

// holdsFor names the files one session is holding, for the agents table.
// Presentation only: it joins facts Interlock already reported.
func holdsFor(st *interlock.Status, session string) string {
	if st == nil {
		return ""
	}
	var paths []string
	for _, l := range st.Leases {
		if l.SessionID == session {
			paths = append(paths, l.Path)
		}
	}
	if len(paths) == 0 {
		return "—"
	}
	return strings.Join(paths, ", ")
}
