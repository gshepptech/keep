package dash

import (
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/gshepptech/rounce/internal/store"
)

// stderrLog is this package's one logger: a store error's detail is written
// HERE, never into a response body -- a store error message can echo
// agent-supplied text, and the response body is exactly what plan 05's scan
// inspects.
var stderrLog = slog.New(slog.NewTextHandler(os.Stderr, nil))

// routes returns the server's five-route table: the four views plus the
// implicit 405 http.ServeMux already produces for a wrong method on a
// registered pattern. {$} on the root pattern is Go 1.22+ ServeMux's own
// exact-match syntax -- without it, "/anything" would fall through to the
// ready-queue handler instead of 404ing.
//
// Each handler below makes exactly ONE store call and holds no rows or
// transaction across its render -- every store read method already
// materialises fully into Go values before returning (itemColumns/
// scanItems), so "call it, render the returned value" is the whole rule
// (05-RESEARCH.md Pitfall 5).
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleReady)
	mux.HandleFunc("GET /backlog", s.handleBacklog)

	// The same views, scoped to another project. One board serves every
	// project rounce knows; the URL says which, so two tabs can hold two
	// boards without a shared selection for them to fight over.
	mux.HandleFunc("GET /p/{project}", s.handleReady)
	mux.HandleFunc("GET /p/{project}/backlog", s.handleBacklog)
	mux.HandleFunc("GET /p/{project}/item/{id}", s.handleItem)
	mux.HandleFunc("GET /p/{project}/tree/{id}", s.handleTree)
	mux.HandleFunc("GET /all", s.handleAll)
	mux.HandleFunc("GET /new", s.handleNew)
	mux.HandleFunc("GET /projects", s.handleProjects)
	mux.HandleFunc("GET /item/{id}", s.handleItem)
	mux.HandleFunc("GET /tree/{id}", s.handleTree)
	s.routesMutating(mux)
	return mux
}

// handleReady renders the ready queue. The store call is UNLIMITED
// (limit 0) -- plan 06's DASH-07 agreement test asserts the board and
// `rounce ready` come from the same call, and a hard-coded limit that
// happens to match a fixture's size today is exactly the coincidental pass
// GATE-03 exists to catch.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	// readyItems, not s.st.Ready directly: the queue and /who must apply
	// the same Interlock filter, computed once, or the board will omit an
	// item that /who cannot explain.
	st, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	defer done()

	data, err := s.boardDataFrom(r, st)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "board", s.pageFor(r, "board", name, data))
}

// handleBacklog renders the raw backlog, grouped by type -- store.Backlog
// already does the grouping/ordering; this handler adds nothing.
func (s *Server) handleBacklog(w http.ResponseWriter, r *http.Request) {
	st, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	defer done()

	items, err := st.Backlog(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "backlog", s.pageFor(r, "backlog", name, backlogRows(items)))
}

// handleItem renders one item's full detail, keyed on the path's {id}
// wildcard -- a dot-containing id (every id below a root has dots) is
// captured whole, verified live during 05 planning and covered by this
// package's own handlers_test.go.
func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	st, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	defer done()

	d, err := st.GetItem(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "item", s.pageFor(r, "", name, d))
}

// handleTree renders the subtree beneath {id}.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	st, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	defer done()

	nodes, err := st.Tree(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "tree", s.pageFor(r, "", name, nodes))
}

// fail classifies err by errors.Is, never message text: store.ErrNotFound
// becomes 404; anything else becomes a generic 500, with the actual error
// logged to stderr only -- never echoed into the response body.
func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	stderrLog.Error("dash: handler error", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// writeHTML sets the content type explicitly -- never relies on
// http.DetectContentType -- then writes b.
func writeHTML(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// handleNew renders the filing form. Its own page rather than a drawer on
// the board: filing is deliberate, and a form open above the queue puts an
// empty text box where the answer to "what should I do" belongs.
func (s *Server) handleNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, "new", s.newPage(r, "", typeNames()))
}

// missOrFail answers 404 for a project that is not registered and 500 for
// anything else. A mistyped project name is an ordinary miss; treating it
// as a server failure would send a person looking for a bug that is not
// there.
func (s *Server) missOrFail(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoProject) {
		http.Error(w, "No project by that name. Check the list at /projects.", http.StatusNotFound)
		return
	}
	s.fail(w, err)
}
