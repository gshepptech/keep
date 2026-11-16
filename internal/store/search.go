package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/gshepptech/keep/internal/model"
)

// SearchFilter is one text search over the fields a person would actually
// remember an item by.
type SearchFilter struct {
	// Query is matched case-insensitively as a substring. It is never
	// parsed: no operators, no quoting rules, no query language. A person
	// searching for "token expiry" means those characters, and a search
	// syntax is a thing to learn before you can find your own note.
	Query string `json:"query"`

	// IncludeClosed searches closed and dropped items too. Off by default:
	// the common search is for live work, and a store's whole history
	// buries it.
	IncludeClosed bool `json:"include_closed,omitempty"`

	Limit int `json:"limit,omitempty"`
}

// SearchResult is a matching item plus the field the match came from. The
// field matters to a reader: a result whose title does not contain the word
// you searched for looks like a mistake until you can see it matched the
// reason, or a path, or the body.
type SearchResult struct {
	model.Item
	// MatchedIn is one of searchFields: the highest-ranked field that
	// contained the query.
	MatchedIn string `json:"matched_in"`

	// Snippet is the text around the match, when the match is somewhere you
	// cannot already see. A captured document whose body is a whole plan
	// matches almost every query, and "matched in body" alone gives no way
	// to tell a real hit from a passing mention. A window of the actual text
	// does.
	Snippet string `json:"snippet,omitempty"`
}

// searchFields are the columns a match may come from, in the order that
// decides rank. Title first because an item whose TITLE says "token" is
// what you meant far more often than one that mentions "token" in a
// paragraph; body last for the same reason inverted.
//
// paths is searched too, and that is not padding: "what have I got open on
// internal/auth" is a question people ask constantly, and without paths it
// would need a separate command.
var searchFields = []string{"title", "why", "acceptance", "paths", "body"}

// SearchFieldRank returns where a SearchResult.MatchedIn value sits in the
// field order that decides rank: 0 for title, up to len(searchFields) for a
// value this package does not recognise.
//
// It is exported for one caller: `keep search --all`, which merges results
// from several stores and has to re-rank them together. That caller needs
// the same order this file's CASE expression uses. A copy of searchFields
// in cmd/keep would be a second definition of "title beats body" that
// would silently disagree as soon as a field is added here.
func SearchFieldRank(matchedIn string) int {
	for i, f := range searchFields {
		if f == matchedIn {
			return i
		}
	}
	return len(searchFields)
}

// Search finds items whose text contains Query, ranked by WHERE the match
// landed and then by the store's usual score order.
//
// # FTS5, and why it is a prefilter rather than the search
//
// Plain LIKE '%x%' scans look sufficient: a corpus of a few thousand rows
// scans in under a millisecond, and FTS5's virtual table, triggers and
// migration are three new ways for a work store to disagree with itself.
// Larger stores do not scan that fast. Searching a store of items with 120-word
// bodies with scans alone:
//
//	 1,000 items     14 ms
//	10,000 items    178 ms
//	50,000 items  1,008 ms
//
// A full second is not "under a millisecond", and a store an agent files
// into all day reaches ten thousand items without anybody deciding to let
// it. With the index, the same 50,000-item store answers a term that
// actually narrows it in 0.2 ms.
//
// The cost: a term matching nearly every item makes the index return
// nearly every row, so the lookup narrows nothing and is paid for on top of
// the scan. That case is about 55% slower than scanning alone.
// search_bench_test.go carries the full table and the reasoning for
// accepting it.
//
// So there is an items_fts index (internal/store/schema.go, migrations
// rung 2), and it is used as a candidate filter in front of the LIKE
// conditions rather than as a replacement for them. The LIKE clauses still
// decide what matches and the CASE still decides the rank; FTS5 only
// narrows which rows they run over. There are three reasons for that shape:
//
//  1. Semantics do not change. The index uses the trigram tokenizer, so
//     MATCH is an arbitrary case-insensitive substring search rather than a
//     word search, but keeping the LIKE means the answer is defined by one
//     plain expression. TestSearchFTSAgreesWithAFullScan asserts the two
//     agree.
//  2. The index can only return a superset. Trigram folds ASCII case and
//     LIKE does not fold non-ASCII case, so any row LIKE would match is a
//     row MATCH returns. Narrowing by a superset cannot lose a result.
//  3. Terms shorter than three characters have no trigram to look up. Those
//     queries skip the prefilter and scan.
//
// The ranking stays in SQL rather than moving to FTS5's bm25(), because the
// rank here is "which column matched", which is a decision about what a
// person meant, not about term frequency.
//
// The rank is computed in SQL rather than by sorting in Go so that Limit
// means "the best N", not "the first N found then sorted".
func (s *Store) Search(ctx context.Context, f SearchFilter) ([]SearchResult, error) {
	query := strings.TrimSpace(f.Query)
	if query == "" {
		return nil, fmt.Errorf("Search: a query is required")
	}

	// LIKE's own wildcards are escaped so a search for a literal "100%" or
	// an "_id" suffix finds those characters instead of matching
	// everything. The ESCAPE clause below is what makes the backslashes
	// mean escaping rather than backslashes.
	pattern := "%" + escapeLike(query) + "%"

	var conds []string
	var args []any
	// One rank expression and one match predicate, both built from the same
	// field order, so a field can never be searchable but unrankable.
	var rank []string
	for i, col := range searchFields {
		conds = append(conds, col+" LIKE ? ESCAPE '\\'")
		args = append(args, pattern)
		rank = append(rank, fmt.Sprintf("WHEN %s LIKE ? ESCAPE '\\' THEN %d", col, i))
	}

	where := "(" + strings.Join(conds, " OR ") + ")"

	// The prefilter. It is prepended to the WHERE rather than joined, so
	// SQLite evaluates the indexed lookup first and the LIKE conditions only
	// over what survives.
	var ftsArgs []any
	if match, ok := ftsMatchQuery(query); ok {
		where = "rowid IN (SELECT rowid FROM items_fts WHERE items_fts MATCH ?) AND " + where
		ftsArgs = append(ftsArgs, match)
	}

	if !f.IncludeClosed {
		where += " AND status NOT IN ('closed', 'dropped')"
	}

	// The rank arguments are bound before the match arguments because the
	// CASE appears in the SELECT list, which SQLite binds before WHERE.
	rankArgs := make([]any, len(searchFields))
	for i := range rankArgs {
		rankArgs[i] = pattern
	}

	sqlText := `SELECT ` + itemColumns + `, field_rank FROM (
	SELECT ` + itemColumns + `, CASE ` + strings.Join(rank, " ") + ` ELSE ` +
		fmt.Sprint(len(searchFields)) + ` END AS field_rank
	FROM items WHERE ` + where + `
) ORDER BY field_rank ASC, score DESC, created_at ASC, id ASC`

	if f.Limit > 0 {
		sqlText += " LIMIT ?"
	}

	all := append(rankArgs, ftsArgs...)
	all = append(all, args...)
	if f.Limit > 0 {
		all = append(all, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, sqlText, all...)
	if err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}
	defer rows.Close()

	out := []SearchResult{}
	for rows.Next() {
		var r SearchResult
		var rank int
		if err := rows.Scan(
			&r.ID, &r.ParentID, &r.Ordinal, &r.Stage, &r.Type, &r.Title,
			&r.Body, &r.Acceptance, &r.Status, &r.Value, &r.Confidence,
			&r.Effort, &r.Score, &r.Why, &r.Repo, &r.Paths,
			&r.ClaimedBySession, &r.ClaimedAt, &r.Assignee, &r.ExternalRef,
			&r.CreatedAt, &r.UpdatedAt, &r.ClosedAt, &rank,
		); err != nil {
			return nil, fmt.Errorf("Search: %w", err)
		}
		r.MatchedIn = fieldName(rank)
		r.Snippet = snippetAround(r.fieldValue(r.MatchedIn), query)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}
	return out, nil
}

// fieldValue reads back the field a match came from, so the snippet is cut
// from the text that actually matched rather than from whichever field is
// most convenient.
func (r SearchResult) fieldValue(name string) string {
	switch name {
	case "title":
		return r.Title
	case "why":
		return r.Why
	case "acceptance":
		return r.Acceptance
	case "paths":
		if r.Paths == nil {
			return ""
		}
		return *r.Paths
	case "body":
		return r.Body
	}
	return ""
}

// snippetWindow is how much text to show either side of a match. Enough for
// a sentence fragment to be recognisable, short enough to stay on one line
// beside everything else in the row.
const snippetWindow = 44

// snippetAround returns the text surrounding the first case-insensitive
// occurrence of query, with ellipses where it was cut. A field short enough
// to read whole is returned whole rather than decorated with ellipses that
// hide nothing.
func snippetAround(field, query string) string {
	if field == "" {
		return ""
	}
	flat := strings.Join(strings.Fields(field), " ")
	at := strings.Index(strings.ToLower(flat), strings.ToLower(query))
	if at < 0 || len(flat) <= snippetWindow*2 {
		return flat
	}

	start := max(at-snippetWindow, 0)
	end := min(at+len(query)+snippetWindow, len(flat))
	out := flat[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(flat) {
		out += "…"
	}
	return out
}

// fieldName turns the rank the query computed back into the field it came
// from. A rank past the end means the CASE fell through, which the WHERE
// clause makes impossible, so it is reported as unknown rather than guessed.
func fieldName(rank int) string {
	if rank < 0 || rank >= len(searchFields) {
		return "unknown"
	}
	return searchFields[rank]
}

// ftsMinTermLength is the trigram tokenizer's floor: it indexes every run of
// three characters, so a term shorter than that has no trigram to look up
// and MATCH returns nothing for it. Such a query skips the prefilter and
// scans, which is what searching for "id" did before the index existed.
const ftsMinTermLength = 3

// ftsMatchQuery renders a user's query as an FTS5 phrase, and reports
// whether the index can serve it at all.
//
// The query is wrapped in double quotes so FTS5 reads it as a phrase rather
// than as its query language: an unquoted "token OR expiry" would be parsed
// as an operator expression, and SearchFilter.Query's contract is that there
// is no query language, so those characters are what the person is
// looking for. Internal double quotes are doubled, FTS5's own escape.
func ftsMatchQuery(query string) (string, bool) {
	if len([]rune(query)) < ftsMinTermLength {
		return "", false
	}
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`, true
}

// escapeLike neutralises the three characters LIKE treats as syntax, so a
// query is matched as the literal text a person typed. The backslash is
// escaped first; doing it later would double the backslashes this function
// itself just added.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}
