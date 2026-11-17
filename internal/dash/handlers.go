package dash

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"

	"github.com/gshepptech/keep/internal/store"
)

// stderrLog is this package's one logger: a store error's detail is written
// here, never into a response body, because a store error message can echo
// agent-supplied text, and the response body is what the off-host
// reference scan inspects.
var stderrLog = slog.New(slog.NewTextHandler(os.Stderr, nil))

// routes returns the server's five-route table: the four views plus the
// implicit 405 http.ServeMux already produces for a wrong method on a
// registered pattern. {$} on the root pattern is Go 1.22+ ServeMux's own
// exact-match syntax; without it, "/anything" would fall through to the
// ready-queue handler instead of 404ing.
//
// Each handler below makes exactly one store call and holds no rows or
// transaction across its render. Every store read method materialises
// fully into Go values before returning (itemColumns/scanItems), so each
// handler calls the store and renders the returned value.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleReady)
	mux.HandleFunc("GET /backlog", s.handleBacklog)

	// The same views, scoped to another project. One board serves every
	// project keep knows; the URL says which, so two tabs can hold two
	// boards without a shared selection for them to fight over.
	mux.HandleFunc("GET /p/{project}", s.handleReady)
	mux.HandleFunc("GET /p/{project}/backlog", s.handleBacklog)
	mux.HandleFunc("GET /p/{project}/item/{id}", s.handleItem)
	mux.HandleFunc("GET /p/{project}/tree/{id}", s.handleTree)
	mux.HandleFunc("GET /all", s.handleAll)
	mux.HandleFunc("GET /new", s.handleNew)
	mux.HandleFunc("GET /p/{project}/new", s.handleNew)
	mux.HandleFunc("GET /projects", s.handleProjects)
	mux.HandleFunc("GET /confirm", s.handleConfirm)
	mux.HandleFunc("GET /p/{project}/confirm", s.handleConfirm)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /p/{project}/search", s.handleSearch)
	mux.HandleFunc("GET /activity", s.handleActivity)
	mux.HandleFunc("GET /p/{project}/activity", s.handleActivity)
	mux.HandleFunc("GET /item/{id}", s.handleItem)
	mux.HandleFunc("GET /tree/{id}", s.handleTree)
	s.routesMutating(mux)
	return mux
}

// handleReady renders the ready queue. The store call is unlimited (limit
// 0): the agreement test asserts the board and `keep ready` come from the
// same call, and a hard-coded limit that happened to match a fixture's size
// would pass it by coincidence.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	// boardDataFrom, not s.st.Ready directly: it goes through
	// readyItemsFrom so the queue and the "held back" section apply the
	// same Interlock filter, computed once, or the board will omit an item
	// that the section below it cannot explain.
	st, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	defer done()

	data, err := s.boardDataFrom(r, st, s.pathFor(name))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "board", s.pageFor(r, "board", name, data))
}

// handleBacklog renders the raw backlog, grouped by type. store.Backlog
// does the grouping and ordering; this handler adds nothing.
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
// wildcard. A dot-containing id (every id below a root has dots) is
// captured whole, as handlers_test.go checks.
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
	path := s.pathFor(r.PathValue("project"))
	view := itemView{ItemDetail: d, Held: s.heldFor(r.Context(), d.Item, repoDirOf(path))}
	s.render(w, "item", s.pageFor(r, "item", name, view))
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
	s.render(w, "tree", s.pageFor(r, "tree", name, nodes))
}

// fail classifies err by errors.Is, never message text: store.ErrNotFound
// becomes 404; anything else becomes a generic 500, with the actual error
// logged to stderr only and never echoed into the response body.
//
// fail renders a real page rather than http.Error's bare text.
//
// http.Error writes an unstyled sentence with no nav and no way back, and a
// board whose URLs carry ids is a board people edit URLs on. Landing on
// "not found" as plain text is a dead end; landing on it as a page still
// has the rail, so the way out is where it always is.
func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.failPage(w, http.StatusNotFound, "Nothing here",
			"That id does not match anything in this project. It may have been dropped, or it may belong to another project.")
		return
	}
	stderrLog.Error("dash: handler error", "err", err)
	s.failPage(w, http.StatusInternalServerError, "Something went wrong",
		"The board could not finish that. The reason was written to the terminal running keep.")
}

// failPage renders the error inside the normal shell. It deliberately does
// not echo the underlying error: those are written to the terminal, and an
// internal error string on a page is noise to the reader and detail to
// anyone else.
func (s *Server) failPage(w http.ResponseWriter, code int, heading, body string) {
	// Executed directly rather than through render(): render reports a
	// failure by calling fail(), which is this function, and would recurse.
	req := (&http.Request{URL: &url.URL{Path: "/"}}).WithContext(context.Background())
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "failure", s.pageFor(req, "", s.activeName(), errorData{Heading: heading, Body: body})); err != nil {
		stderrLog.Error("dash: failure page did not render", "err", err)
		http.Error(w, heading, code)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes()) // headers are sent; a client that hung up cannot be told
}

// errorData is what the failure page renders.
type errorData struct {
	Heading string
	Body    string
}

// writeHTML sets the content type explicitly, rather than relying on
// http.DetectContentType, then writes b.
func writeHTML(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b) // a client that hung up cannot be told
}

// handleNew renders the filing form. Its own page rather than a drawer on
// the board: filing is deliberate, and a form open above the queue puts an
// empty text box where the answer to "what should I do" belongs.
func (s *Server) handleNew(w http.ResponseWriter, r *http.Request) {
	_, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	done()
	s.render(w, "new", s.pageFor(r, "new", name, typeNames()))
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
