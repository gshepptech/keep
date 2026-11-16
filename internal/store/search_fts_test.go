package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// likeOnlyMatches is the oracle: the search predicate without the items_fts
// prefilter, five LIKE '%x%' conditions and nothing else, executed here
// rather than through Search.
//
// It is written out again on purpose. Reusing Search with a flag to turn the
// prefilter off would make the comparison circular: the same expression
// would produce both sides of it, and this test has to rule out that the
// prefilter changed what the expression means.
func likeOnlyMatches(t *testing.T, s *Store, query string, includeClosed bool) []string {
	t.Helper()
	pattern := "%" + escapeLike(query) + "%"

	var conds []string
	var args []any
	for _, col := range searchFields {
		conds = append(conds, col+" LIKE ? ESCAPE '\\'")
		args = append(args, pattern)
	}
	sqlText := "SELECT id FROM items WHERE (" + strings.Join(conds, " OR ") + ")"
	if !includeClosed {
		sqlText += " AND status NOT IN ('closed', 'dropped')"
	}
	sqlText += " ORDER BY id"

	rows, err := s.db.QueryContext(context.Background(), sqlText, args...)
	if err != nil {
		t.Fatalf("oracle query: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("oracle scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// TestSearchFTSAgreesWithAFullScan checks that Search with the FTS5
// prefilter returns exactly what a plain LIKE scan returns. An index in
// front of a query is only safe if it changes how long the answer takes and
// not what the answer is, and the failure when it does not is invisible:
// search returns results, they are just missing the one you wanted.
//
// The queries below are the cases where a token-based index would silently
// diverge from substring matching: an infix, a case difference, a
// punctuation-only fragment, a LIKE metacharacter, a term shorter than a
// trigram, a query with FTS5 operator syntax in it, and non-ASCII text.
func TestSearchFTSAgreesWithAFullScan(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	fixtures := []AddItemInput{
		{Title: "Expire refresh tokens after 30 days", Actor: "t"},
		{Title: "TOKEN rotation", Actor: "t"},
		{Title: "unrelated", Actor: "t", Why: "internal/auth/token.go:42"},
		{Title: "percent literal", Actor: "t", Body: "the retry rate hit 100% on Tuesday"},
		{Title: "underscore literal", Actor: "t", Body: "the column is claimed_by_session"},
		{Title: "café ☕ and a Ünicode heading", Actor: "t"},
		{Title: "quotes", Actor: "t", Body: `he said "not yet" and left`},
		{Title: "operators", Actor: "t", Body: "token OR expiry AND NOT rotation"},
		{Title: "paths", Actor: "t", Paths: `["internal/payments/refund.go"]`},
		{Title: "acceptance", Actor: "t", Acceptance: "the queue drops the item"},
		{Title: "short", Actor: "t", Body: "id ok no"},
	}
	for i, in := range fixtures {
		in.Title = fmt.Sprintf("%s [%d]", in.Title, i)
		if _, err := s.AddItem(ctx, in); err != nil {
			t.Fatalf("AddItem(%q): %v", in.Title, err)
		}
	}
	closed := mustAdd(t, s, AddItemInput{Title: "closed token work", Actor: "t"})
	if _, err := s.CloseItem(ctx, CloseItemInput{ID: closed.ID, Reason: "done", Actor: "t"}); err != nil {
		t.Fatalf("CloseItem: %v", err)
	}

	queries := []string{
		"token",       // plain
		"oken",        // infix: the case a word tokenizer gets wrong
		"TOKEN",       // case fold
		"Token",       // mixed case
		"100%",        // LIKE metacharacter
		"_by_",        // LIKE metacharacter, infix
		"café",        // non-ASCII
		"Ünicode",     // non-ASCII with case
		`"not yet"`,   // embedded double quotes: FTS5's own escape character
		"OR expiry",   // FTS5 operator syntax as literal text
		"refund.go",   // punctuation inside a term
		"id",          // shorter than a trigram: prefilter must be skipped
		"o",           // one character
		"the queue",   // phrase with a space
		"nothingness", // no matches at all
	}

	for _, includeClosed := range []bool{false, true} {
		for _, q := range queries {
			name := q
			if includeClosed {
				name += " (with closed)"
			}
			t.Run(name, func(t *testing.T) {
				want := likeOnlyMatches(t, s, q, includeClosed)

				got, err := s.Search(ctx, SearchFilter{Query: q, IncludeClosed: includeClosed})
				if err != nil {
					t.Fatalf("Search(%q): %v", q, err)
				}
				gotIDs := make([]string, len(got))
				for i, r := range got {
					gotIDs[i] = r.ID
				}

				if len(gotIDs) != len(want) {
					t.Fatalf("Search(%q) returned %d results %v, the full scan finds %d %v -- the index changed the answer",
						q, len(gotIDs), gotIDs, len(want), want)
				}
				inWant := map[string]bool{}
				for _, id := range want {
					inWant[id] = true
				}
				for _, id := range gotIDs {
					if !inWant[id] {
						t.Errorf("Search(%q) returned %s, which the full scan does not match", q, id)
					}
				}
			})
		}
	}
}

// TestFTSIndexTracksEveryWriteToItems checks that new and edited items are
// reflected in search. External-content FTS5 does not
// update itself; three triggers do. A missing or wrong trigger produces a
// store where search silently answers from stale text, finding a title
// that was renamed or failing to find one that was just filed.
func TestFTSIndexTracksEveryWriteToItems(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// INSERT: findable immediately.
	item := mustAdd(t, s, AddItemInput{Title: "aardvark husbandry", Actor: "t"})
	if got := searchIDs(t, s, "aardvark"); len(got) != 1 || got[0] != item.ID {
		t.Fatalf("after add, search(aardvark) = %v, want [%s] -- the insert trigger is not indexing", got, item.ID)
	}

	// UPDATE: the new text is findable and the old text is not.
	newTitle := "zebra husbandry"
	if _, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "t", Title: &newTitle}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if got := searchIDs(t, s, "zebra"); len(got) != 1 || got[0] != item.ID {
		t.Fatalf("after rename, search(zebra) = %v, want [%s] -- the update trigger is not re-indexing", got, item.ID)
	}
	if got := searchIDs(t, s, "aardvark"); len(got) != 0 {
		t.Fatalf("after rename, search(aardvark) still returns %v -- the update trigger is not removing the old text, so search answers from a title that no longer exists", got)
	}

	// The body column too, not just title: the triggers name five columns
	// and a missing one would show up on exactly one of them.
	body := "the pangolin clause"
	if _, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "t", Body: &body}); err != nil {
		t.Fatalf("UpdateItem(body): %v", err)
	}
	if got := searchIDs(t, s, "pangolin"); len(got) != 1 {
		t.Fatalf("search(pangolin) = %v, want the item -- body is not indexed", got)
	}
}

func searchIDs(t *testing.T, s *Store, query string) []string {
	t.Helper()
	res, err := s.Search(context.Background(), SearchFilter{Query: query})
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.ID
	}
	return out
}

// TestMigratedStoreFindsItemsFiledBeforeTheIndexExisted checks that search
// finds items filed before the migration that adds items_fts. The triggers
// only fire on writes from that rung on, so without the rebuild, search
// would return nothing for older items while still returning results for
// newer ones, a failure nobody would notice.
func TestMigratedStoreFindsItemsFiledBeforeTheIndexExisted(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/old.sqlite"

	old, err := initWithDDL(path, schemaDDLv1, baseSchemaVersion)
	if err != nil {
		t.Fatalf("initWithDDL(v1): %v", err)
	}
	before, err := old.AddItem(ctx, AddItemInput{Title: "filed before the index existed", Actor: "t", Body: "quokka"})
	if err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	old.Close()

	if _, err := Migrate(ctx, path); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	s, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	after := mustAdd(t, s, AddItemInput{Title: "filed after", Actor: "t", Body: "quokka"})

	got := searchIDs(t, s, "quokka")
	if len(got) != 2 {
		t.Fatalf("search after migrating found %d items %v, want both %s (pre-existing) and %s (new) -- the rung's rebuild did not index what was already there",
			len(got), got, before.ID, after.ID)
	}
}
