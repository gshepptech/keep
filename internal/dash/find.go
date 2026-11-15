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
		d.Results = withoutRedundantSnippets(results)
	}
	s.render(w, "search", s.pageFor(r, "search", name, d))
}

// withoutRedundantSnippets blanks the match snippet on any result whose
// row already displays the text it would show.
//
// The snippet exists to answer "why is this in my results?" when the answer
// is somewhere you cannot see -- a match in the reason, the acceptance
// criteria, or the body. A row already shows its title and its paths, so a
// match in either produces a snippet that repeats a line an inch above it.
// The rendered result was:
//
//	Replace the hand-rolled retry loop with a backoff library
//	open · matched in paths · internal/payments/refund.go · rnc-616bf0
//	internal/payments/refund.go
//
// Substring containment states the rule once instead of once per field,
// and it also
// catches the case a field list cannot: a short body whose entire text is
// the title again.
//
// It runs here rather than in store.Search because it is a fact about what
// this page draws, not about the search. The CLI prints no paths column, so
// it still needs the same snippet.
func withoutRedundantSnippets(results []store.SearchResult) []store.SearchResult {
	for i, r := range results {
		if r.Snippet == "" {
			continue
		}
		snip := strings.TrimSpace(strings.Trim(r.Snippet, "…"))
		if snip == "" {
			results[i].Snippet = ""
			continue
		}
		shown := r.Title
		if r.Paths != nil {
			shown += " " + *r.Paths
		}
		if strings.Contains(shown, snip) {
			results[i].Snippet = ""
		}
	}
	return results
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
