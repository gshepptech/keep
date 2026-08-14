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
	mux.HandleFunc("GET /all", s.handleAll)
	mux.HandleFunc("GET /who", s.handleInterlock)
	mux.HandleFunc("GET /item/{id}", s.handleItem)
	mux.HandleFunc("GET /tree/{id}", s.handleTree)
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
	items, _, err := s.readyItems(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	b, err := renderReady(items)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeHTML(w, b)
}

// handleBacklog renders the raw backlog, grouped by type -- store.Backlog
// already does the grouping/ordering; this handler adds nothing.
func (s *Server) handleBacklog(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.Backlog(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	b, err := renderBacklog(items)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeHTML(w, b)
}

// handleItem renders one item's full detail, keyed on the path's {id}
// wildcard -- a dot-containing id (every id below a root has dots) is
// captured whole, verified live during 05 planning and covered by this
// package's own handlers_test.go.
func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.GetItem(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	b, err := renderItem(d)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeHTML(w, b)
}

// handleTree renders the subtree beneath {id}.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.st.Tree(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	b, err := renderTree(nodes)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeHTML(w, b)
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
