package store

import (
	"context"
	"strings"
	"testing"
)

// TestSearchFindsAnItemByAnyOfItsText. The whole point: you remember a word,
// not an id.
func TestSearchFindsAnItemByAnyOfItsText(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	// The title match scores LOWEST on purpose. Rank by field is the only
	// thing that can put it first; without it, score order wins and the
	// body match leads. Equal scores would let the default sort produce
	// the expected order even with the ranking removed.
	nine, five := 9, 1
	mustAdd(t, st, AddItemInput{Title: "Expire refresh tokens", Value: &five, Actor: "a"})
	mustAdd(t, st, AddItemInput{Title: "Tidy the flag help", Why: "the token parser is duplicated", Value: &nine, Actor: "a"})
	mustAdd(t, st, AddItemInput{Title: "Close the pool", Paths: "internal/auth/token.go", Value: &nine, Actor: "a"})
	mustAdd(t, st, AddItemInput{Title: "Unrelated work", Actor: "a"})

	got, err := st.Search(ctx, SearchFilter{Query: "token"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Search(token) found %d items, want 3 (title, why, paths)", len(got))
	}
	// Title match ranks above a match buried in another field.
	if got[0].Title != "Expire refresh tokens" {
		t.Errorf("first result is %q, want the title match first", got[0].Title)
	}
}

// TestSearchIsCaseInsensitive. Nobody remembers the capitalisation.
func TestSearchIsCaseInsensitive(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	mustAdd(t, st, AddItemInput{Title: "Expire Refresh TOKENS", Actor: "a"})

	got, err := st.Search(ctx, SearchFilter{Query: "tokens"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Search(tokens) found %d, want 1 -- case must not matter", len(got))
	}
}

// TestSearchTreatsWildcardsAsLiteralText checks that % and _ in a query match
// only themselves.
//
// LIKE gives % and _ meaning. A person searching for "100%" means those
// four characters; unescaped, the % matches everything and the search
// silently returns the whole store, which reads as "no filter applied"
// rather than as a bug.
func TestSearchTreatsWildcardsAsLiteralText(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	mustAdd(t, st, AddItemInput{Title: "Cover 100% of the parser", Actor: "a"})
	mustAdd(t, st, AddItemInput{Title: "Something else entirely", Actor: "a"})

	got, err := st.Search(ctx, SearchFilter{Query: "100%"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search(100%%) found %d items, want 1 -- %% must be literal", len(got))
	}

	// And the bare wildcard finds nothing rather than everything.
	got, err = st.Search(ctx, SearchFilter{Query: "%"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Search(%%) found %d items, want 1 (the one literally containing %%)", len(got))
	}
}

// TestSearchLeavesClosedWorkOutUnlessAsked. The common search is for live
// work; a store's whole history buries it.
func TestSearchLeavesClosedWorkOutUnlessAsked(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	live := mustAdd(t, st, AddItemInput{Title: "token work, live", Actor: "a"})
	done := mustAdd(t, st, AddItemInput{Title: "token work, finished", Actor: "a"})
	if _, err := st.CloseItem(ctx, CloseItemInput{ID: done.ID, Reason: "done", Actor: "a"}); err != nil {
		t.Fatalf("CloseItem: %v", err)
	}

	got, err := st.Search(ctx, SearchFilter{Query: "token"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].ID != live.ID {
		t.Errorf("Search found %d items, want only the live one", len(got))
	}

	got, err = st.Search(ctx, SearchFilter{Query: "token", IncludeClosed: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Search(IncludeClosed) found %d, want 2", len(got))
	}
}

// TestSearchRefusesAnEmptyQuery. An empty search is a request for the whole
// store, which `list` already answers; answering it here would make an
// accidental empty box look like a working search.
func TestSearchRefusesAnEmptyQuery(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	mustAdd(t, st, AddItemInput{Title: "anything", Actor: "a"})

	if _, err := st.Search(ctx, SearchFilter{Query: "   "}); err == nil {
		t.Error("Search(whitespace) succeeded, want an error")
	} else if !strings.Contains(err.Error(), "query is required") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}

// TestSearchSaysWhereTheMatchCameFrom checks that each search result names
// the field it matched in.
//
// A result whose title does not contain the word you searched for reads as
// a mistake until it can tell you it matched the reason, or a path, or the
// body. Found by looking at the rendered page: the one hit was an item
// whose title had nothing to do with the query.
func TestSearchSaysWhereTheMatchCameFrom(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	mustAdd(t, st, AddItemInput{Title: "token expiry", Actor: "a"})
	mustAdd(t, st, AddItemInput{Title: "unrelated title", Why: "the token parser is duplicated", Actor: "a"})
	mustAdd(t, st, AddItemInput{Title: "also unrelated", Paths: "internal/auth/token.go", Actor: "a"})

	got, err := st.Search(ctx, SearchFilter{Query: "token"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := map[string]string{
		"token expiry":    "title",
		"unrelated title": "why",
		"also unrelated":  "paths",
	}
	if len(got) != 3 {
		t.Fatalf("Search found %d, want 3", len(got))
	}
	for _, r := range got {
		if w := want[r.Title]; r.MatchedIn != w {
			t.Errorf("%q matched in %q, want %q", r.Title, r.MatchedIn, w)
		}
	}
}

// TestSearchShowsTheTextAroundAMatch checks that a search result carries the
// text around the match.
//
// Found by running search against a real store: one captured document whose
// body was an entire roadmap matched almost every query, and "matched in
// body" gave no way at all to tell a real hit from a passing mention. The
// window of surrounding text is what makes a result judgeable without
// opening it.
func TestSearchShowsTheTextAroundAMatch(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	long := strings.Repeat("padding text that is not interesting. ", 12) +
		"the token parser is duplicated in three places. " +
		strings.Repeat("more padding afterwards. ", 12)
	mustAdd(t, st, AddItemInput{Title: "a captured document", Body: long, Actor: "a"})

	got, err := st.Search(ctx, SearchFilter{Query: "token parser"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search found %d, want 1", len(got))
	}
	if !strings.Contains(got[0].Snippet, "token parser") {
		t.Errorf("snippet = %q, want it to contain the matched text", got[0].Snippet)
	}
	if len(got[0].Snippet) >= len(long) {
		t.Errorf("snippet is %d chars of a %d-char body -- it must be a window, not the whole thing",
			len(got[0].Snippet), len(long))
	}
	if !strings.HasPrefix(got[0].Snippet, "…") || !strings.HasSuffix(got[0].Snippet, "…") {
		t.Errorf("snippet = %q, want ellipses showing it was cut on both sides", got[0].Snippet)
	}
}

// TestSearchDoesNotEllipsiseWhatItShowedWhole. A short field is returned
// whole; decorating it with ellipses would claim text was hidden when none
// was.
func TestSearchDoesNotEllipsiseWhatItShowedWhole(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	mustAdd(t, st, AddItemInput{Title: "short one", Why: "tokens expire", Actor: "a"})

	got, err := st.Search(ctx, SearchFilter{Query: "tokens"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got[0].Snippet != "tokens expire" {
		t.Errorf("snippet = %q, want the whole short field with no ellipses", got[0].Snippet)
	}
}
