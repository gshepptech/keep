package dash

import (
	"net/http"
	"strings"

	"github.com/gshepptech/keep/internal/store"
)

// searchData is what the search page renders: the box, and whatever the box
// found. Query is echoed back so the field still holds what was typed after
// the round trip -- a search box that empties itself on submit makes you
// retype to refine.
type searchData struct {
	Query   string
	Results []store.SearchResult
	Ran     bool
}

// handleSearch renders the search page.
//
// GET with a q parameter rather than POST, deliberately, and it is the only
// place on this board that reads a query string. A search is a read: it
// belongs in a URL you can bookmark, share and reload, and the CSRF and
// POST-only discipline the rest of the board follows exists to protect
// writes, which this is not.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	st, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	defer done()

	d := searchData{Query: strings.TrimSpace(r.URL.Query().Get("q"))}
	if d.Query != "" {
		d.Ran = true
		results, err := st.Search(r.Context(), store.SearchFilter{
			Query:         d.Query,
			IncludeClosed: r.URL.Query().Get("closed") != "",
			Limit:         100,
		})
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Results = results
	}
	s.render(w, "search", s.pageFor(r, "search", name, d))
}

// handleActivity renders what has happened, newest first.
//
// The board's other pages answer "what is true now". This one answers "what
// did they do", which for a tool whose premise is agents working unattended
// is the question you have when you sit back down.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	st, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	defer done()

	entries, err := st.Feed(r.Context(), store.FeedFilter{Limit: 200})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "activity", s.pageFor(r, "activity", name, entries))
}
