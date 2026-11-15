package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/registry"
	"github.com/gshepptech/keep/internal/store"
)

// runSearch implements the `search` subcommand: find an item by a word you
// remember.
//
// `list` answers structural questions -- which tickets are open, what is
// under this parent. It cannot answer "where is that thing about tokens",
// which is the question anyone with more than a screenful of items actually
// asks. The two are kept separate rather than adding a --text flag to list:
// a filter that matches nothing is a normal `list` result, while a search
// that matches nothing is worth saying out loud.
//
// One store.Search call and no re-filtering in Go, the same discipline
// every other read command follows.
func runSearch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	limitFlag := fs.Int("limit", 0, "at most n results")
	closedFlag := fs.Bool("closed", false, "search closed and dropped items too")
	allFlag := fs.Bool("all", false, "search every registered store (see keep repos)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: keep search [flags] <text>")
		return 2
	}
	if *allFlag && *dbFlag != "" {
		fmt.Fprintln(stderr, "keep: search: --db and --all are mutually exclusive (--db names one store; --all reads the registry)")
		return 2
	}

	// Every remaining argument joins into one query, so `keep search token
	// expiry` works without quoting. Quoting to search for two words is the
	// kind of papercut that makes people stop using a search.
	query := joinArgs(fs.Args())

	if *allFlag {
		return runSearchAll(query, *closedFlag, *limitFlag, *jsonFlag, stdout, stderr)
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	results, err := s.Search(context.Background(), store.SearchFilter{
		Query:         query,
		IncludeClosed: *closedFlag,
		Limit:         *limitFlag,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		out := results
		if out == nil {
			out = []store.SearchResult{}
		}
		if err := emitJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "keep: search: %v\n", err)
			return 1
		}
		return 0
	}

	// A search that found nothing says so, on stderr, so a script reading
	// stdout still gets a clean empty result. Silence would be
	// indistinguishable from a store with nothing in it.
	if len(results) == 0 {
		fmt.Fprintf(stderr, "nothing matches %q%s\n", query,
			map[bool]string{true: "", false: " -- add --closed to search finished work too"}[*allFlag])
		return 0
	}

	// Same renderer as ready and blocked, so search results line up with
	// every other list this CLI prints, and the field the match came from
	// goes after the title -- a result whose title lacks the word you
	// searched for reads as a mistake until it says it matched the reason.
	items := make([]model.Item, len(results))
	matched := make(map[string]string, len(results))
	for i, r := range results {
		items[i] = r.Item
		// A title match needs no explanation -- the title is right there.
		// Anything else does, and the snippet is what makes the row
		// judgeable without opening the item.
		if r.MatchedIn == "title" {
			continue
		}
		note := "in " + r.MatchedIn
		if r.Snippet != "" {
			note += ": " + r.Snippet
		}
		matched[r.ID] = note
	}
	renderItems(stdout, items, renderOpts{note: matched})
	return 0
}

// joinArgs joins the remaining arguments with single spaces.
func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// searchAllJSON is `search --all --json`'s payload.
type searchAllJSON struct {
	Results []registry.Tagged[store.SearchResult] `json:"results"`
	Errors  []readyAllError                       `json:"errors"`
}

// runSearchAll searches every registered store and re-ranks the results
// together.
//
// The cross-store rank is the single-store rank, applied again over the
// merged set: which field matched first, then score, then repository and id
// to make the order total. It uses store.SearchFieldRank rather than a copy
// of the field order, so "title beats body" is defined once -- a second copy
// here would silently disagree the first time a field is added to the
// search.
//
// Each store is searched UNLIMITED and --limit slices the merged result, the
// same discipline ready --all follows: a per-store limit would let each
// store's own tiebreak decide which rows even reach the cross-store ranking.
func runSearchAll(query string, includeClosed bool, limit int, asJSON bool, stdout, stderr io.Writer) int {
	rows, results, err := acrossRepos(context.Background(), stderr, "search",
		func(ctx context.Context, st *store.Store) ([]store.SearchResult, error) {
			return st.Search(ctx, store.SearchFilter{Query: query, IncludeClosed: includeClosed})
		},
		func(a, b registry.Tagged[store.SearchResult]) bool {
			ra, rb := store.SearchFieldRank(a.Row.MatchedIn), store.SearchFieldRank(b.Row.MatchedIn)
			if ra != rb {
				return ra < rb
			}
			if a.Row.Score != b.Row.Score {
				return a.Row.Score > b.Row.Score
			}
			if a.Repository != b.Repository {
				return a.Repository < b.Repository
			}
			return a.Row.ID < b.Row.ID
		},
	)
	if err != nil {
		fmt.Fprintf(stderr, "keep: search: %v\n", err)
		return 1
	}

	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}

	if asJSON {
		if err := emitJSON(stdout, searchAllJSON{Results: rows, Errors: repoErrorsJSON(results)}); err != nil {
			fmt.Fprintf(stderr, "keep: search --all: %v\n", err)
			return 1
		}
		return 0
	}

	renderTagged(stdout, rows, func(r store.SearchResult) []string {
		return []string{r.ID, r.MatchedIn, r.Title}
	})
	return 0
}
