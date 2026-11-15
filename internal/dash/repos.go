package dash

import (
	"context"
	"net/http"

	"github.com/gshepptech/keep/internal/registry"
)

// allPage is /all's page data.
//
// It lives here rather than in render.go because render.go states, and a
// reader relies on, that its only non-stdlib import is internal/model --
// which is what makes every function there fixturable from struct literals
// with no database and no wall clock. The cross-repo view genuinely needs
// registry's types, so it takes its own file rather than quietly widening
// that claim.
type allPage struct {
	// Items is already merged and ranked ACROSS stores by registry.Merge.
	// This package does not re-sort it, the same rule renderReady follows
	// for store.Ready.
	Items []registry.MergedItem

	// Errors are the repositories that could not be read. They are page
	// content, not a failure: an unreadable store is a visible row-level
	// error and the other repositories render anyway, so a store that has
	// been deleted or chmod-ed to nothing costs you one row, never the
	// page.
	Errors []repoError

	// RepoCount is every registered repository, readable or not, so the
	// heading does not quietly shrink when one of them breaks.
	RepoCount int
}

// repoError is one unreadable repository, flattened for the template.
// Carrying the error value itself would put an error string built from a
// filesystem path in front of html/template as an error rather than as a
// string; flattening it here keeps the template's data plain and keeps the
// escaping ordinary.
type repoError struct {
	Repo    string
	Message string
}

// handleAll renders the ready queue merged across every registered store.
//
// It is the one handler that does not make exactly one store call, because
// it makes none: it never opens this repository's store at all. The fan-out
// opens each registered store read-only through internal/registry, which is
// where the read-only open and failure classification already live -- duplicating
// any of that here would be a second implementation of the rule that a
// missing store must not be fabricated as an empty one.
func (s *Server) handleAll(w http.ResponseWriter, r *http.Request) {
	data, err := s.allData(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "all", s.newPage(r, "all", data))
}

// allData gathers the cross-repo queue.
func (s *Server) allData(ctx context.Context) (allPage, error) {
	env, err := registry.Load(s.home)
	if err != nil {
		return allPage{}, err
	}

	results := registry.FetchReady(ctx, env.Repos)

	page := allPage{
		Items:     registry.Merge(results),
		RepoCount: len(env.Repos),
	}
	for _, res := range results {
		if res.Err == nil {
			continue
		}
		page.Errors = append(page.Errors, repoError{Repo: res.Repo, Message: res.Err.Error()})
	}

	return page, nil
}
