package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
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
// One store.Search call and no re-filtering in Go, the same STORE-10
// discipline every other read command follows.
func runSearch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	limitFlag := fs.Int("limit", 0, "at most n results")
	allFlag := fs.Bool("closed", false, "search closed and dropped items too")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: rounce search [flags] <text>")
		return 2
	}

	// Every remaining argument joins into one query, so `rounce search token
	// expiry` works without quoting. Quoting to search for two words is the
	// kind of papercut that makes people stop using a search.
	query := joinArgs(fs.Args())

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
	defer s.Close()

	results, err := s.Search(context.Background(), store.SearchFilter{
		Query:         query,
		IncludeClosed: *allFlag,
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
			fmt.Fprintf(stderr, "rounce: search: %v\n", err)
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
