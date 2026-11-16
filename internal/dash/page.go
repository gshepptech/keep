package dash

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
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

	// Problem is the last action's failure and Done is its success, shown
	// once at the top. Empty most of the time; a page that always showed a
	// status bar would train people to stop reading it.
	Problem string
	Done    string

	// Writable is false when the board was started without a path to write
	// to. Actions are then not rendered at all rather than rendered and
	// refused, because a button that is going to say no is worse than none.
	Writable bool

	// Counts feed the sidebar pips, so every page shows the same two
	// numbers without each one having to fetch them.
	Counts navCounts

	// Project is what this board is serving: its name, and the directory a
	// person would cd into. Both, because a name alone is ambiguous across
	// machines and a path alone is unreadable at a glance.
	Project      string
	ProjectDir   string
	HasInterlock bool

	// Projects is the switcher in the sidebar. Every page carries it so
	// changing project is one click from anywhere, rather than a trip
	// through a list page.
	Projects []project

	// Prefix is what every in-page link is built from: empty for the
	// started-in project, /p/<name> for any other. Without it a link on
	// another project's board would silently navigate back to this one.
	Prefix string

	Data any
}

// navCounts is what the sidebar shows next to each section.
type navCounts struct {
	Ready, Backlog int
}

// newPage builds the wrapper for one request.
func (s *Server) newPage(r *http.Request, nav string, data any) page {
	return s.pageFor(r, nav, s.activeName(), data)
}

// pageFor builds the wrapper for one request against one project. The
// project's name and directory come from the request rather than from
// startup, so a board looking at another project says so everywhere
// (sidebar, links and all) instead of naming the one it was launched in.
func (s *Server) pageFor(r *http.Request, nav, project string, data any) page {
	p := page{
		Nav:          nav,
		CSRF:         s.csrf,
		Problem:      r.URL.Query().Get("problem"),
		Done:         r.URL.Query().Get("done"),
		Writable:     s.dbPath != "",
		Project:      project,
		ProjectDir:   repoDirOf(s.pathFor(project)),
		Prefix:       s.prefixFor(project),
		HasInterlock: interlock.Watches(repoDirOf(s.pathFor(project))),
		Data:         data,
	}
	p.Projects = s.projects(r.Context(), project)

	// Best effort: a sidebar count that cannot be read is omitted, never an
	// error. The page it decorates is still worth rendering.
	if st, done, _, err := s.storeFor(r); err == nil {
		defer done()
		if ready, err := st.Ready(r.Context(), 0); err == nil {
			p.Counts.Ready = len(ready)
		}
		if backlog, err := st.Backlog(r.Context(), ""); err == nil {
			p.Counts.Backlog = len(backlog)
		}
	}
	return p
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

	// FirstRun is a store nothing has ever happened in: no items, no
	// events, day one. It is worth its own screen: the ordinary empty
	// state says "nothing is workable", which is true and useless to
	// someone who has not yet learned what this holds or how work gets
	// into it.
	FirstRun bool

	// Stale is the claims nothing has happened to in a while. It sits on
	// the board rather than behind a command because the whole failure it
	// describes is one nobody thinks to go looking for.
	Stale []store.StaleClaim

	Types  []string
	Repo   string
	Counts boardCounts
}

// boardCounts is the readout across the top.
type boardCounts struct {
	Ready, InProgress, Blocked, Backlog, Agents, Holding int
}

// boardDataFrom assembles the whole board from one project's store, in one
// pass.
//
// The store is a parameter rather than read off the server, because two
// requests can be in flight against two different projects and a field
// swapped for the duration of one would be visible to the other. That is
// not a hypothetical with a board someone keeps open in tabs.
func (s *Server) boardDataFrom(r *http.Request, st *store.Store, path string) (boardData, error) {
	ctx := r.Context()
	d := boardData{Types: typeNames()}

	ready, held, err := s.readyItemsFrom(ctx, st, repoDirOf(path))
	if err != nil {
		return d, err
	}
	d.Ready, d.Held = ready, held

	if d.Progress, err = st.ListItems(ctx, storeInProgress()); err != nil {
		return d, err
	}
	if d.Blocked, err = st.Blocked(ctx); err != nil {
		return d, err
	}
	backlog, err := st.Backlog(ctx, "")
	if err != nil {
		return d, err
	}
	d.Backlog = len(backlog)

	// Scoped to this project's directory and gated on it being set up.
	// Fetch alone answers from any directory, including one Interlock has
	// never been initialised in, so asking it "is there coordination here"
	// gets a confident yes about a repository it is not watching, and the
	// board would show an empty agents table on a project that has no
	// coordination at all.
	if dir := repoDirOf(path); interlock.Watches(dir) {
		if st, ok := interlock.Fetch(ctx, dir); ok {
			d.Interlock = &st
			for _, sess := range st.Sessions {
				if !sess.Expired {
					d.Counts.Agents++
				}
			}
			d.Counts.Holding = len(st.Leases)
		}
	}

	if d.Stale, err = st.StaleClaims(ctx, 0); err != nil {
		return d, err
	}

	// The events table is the complete record of every mutation, so "has
	// anything ever happened here" is one query against it rather than a
	// count of four separate lists, and unlike those, it stays correct for
	// a repository whose work has all been closed.
	_, everUsed, err := st.LastActivity(ctx)
	if err != nil {
		return d, err
	}
	d.FirstRun = !everUsed

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
// model declares it, so the dropdown cannot drift from what the store
// accepts.
func typeNames() []string {
	out := make([]string, 0, len(model.TypesOrdered))
	for _, t := range model.TypesOrdered {
		out = append(out, string(t))
	}
	return out
}

// titledPage pairs a page with its browser title, so "head" can take one
// value.
type titledPage struct {
	Page page

	// Title is the browser tab. Heading is the page's own <h1>, and Lede is
	// the one line under it. They are separate because the tab wants
	// "Board" while the page wants to say what a board is for.
	Title, Heading, Lede string

	Data any
}

// actionArgs is one POST button's data. A struct rather than six template
// arguments because a positional call of six strings is unreadable at the
// call site and silently wrong when two of them swap.
type actionArgs struct {
	CSRF, Verb, ID, Label, Return string

	// Project and Prefix ride along on every action so a click on one
	// project's board cannot act on another's store, and lands back where
	// it was clicked.
	Project, Prefix string

	Danger bool
}

// itemView is one item's page: its detail, and the lease holding it back
// if there is one. The board hides a held item from the queue, so without
// this the item page would offer Claim on it as though it were free, and
// say nothing about why it vanished from the list you came from.
type itemView struct {
	model.ItemDetail
	Held *interlock.Collision
}

// stateTag is one state marker: the semantic slug that picks its glyph and
// colour, and the word printed beside it.
type stateTag struct {
	Slug  string
	Label string
}

// holdCount is how many files a session is holding right now. The Agents
// panel asks before drawing Release: a button offered against nothing
// teaches you that the button does nothing.
func holdCount(st *interlock.Status, session string) int {
	if st == nil {
		return 0
	}
	n := 0
	for _, l := range st.Leases {
		if l.SessionID == session {
			n++
		}
	}
	return n
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

// statCard is one figure in the readout.
type statCard struct {
	Label, Badge, Caption, Accent string
	Value                         int
}

// statCardFor builds one figure, and suppresses its accent at zero.
//
// A red 0 next to "Blocked" reads as an alarm when it is the opposite: no
// blocked work is the good outcome. Colour here means "this many things
// need you", so at zero there is nothing to colour.
func statCardFor(label string, value int, badge, caption, accent string) statCard {
	if value == 0 {
		accent = ""
	}
	return statCard{Label: label, Value: value, Badge: badge, Caption: caption, Accent: accent}
}

// initial is the one letter an avatar shows. Presentation only, and
// deliberately not a colour hash: two agents named claude and codex would
// get different colours from the same first letter, which reads as meaning
// when it is noise.
func initial(name string) string {
	if name == "" {
		return "?"
	}
	return strings.ToUpper(string([]rune(name)[:1]))
}

// base is the last element of a path, for the sidebar's repository line.
// The full path stays on the title attribute for anyone who needs it.
func base(p string) string {
	if p == "" {
		return "no store"
	}
	return filepath.Base(p)
}

// pathFor is the store path of a named project, falling back to the one
// this board was started in.
func (s *Server) pathFor(project string) string {
	if project == "" || project == s.activeName() {
		return s.dbPath
	}
	if path, ok := s.pathOf(project); ok {
		return path
	}
	return s.dbPath
}

// prefixFor is the URL prefix a project's own pages live under.
func (s *Server) prefixFor(project string) string {
	if project == "" || project == s.activeName() {
		return ""
	}
	return "/p/" + project
}
