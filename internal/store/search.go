package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/gshepptech/rounce/internal/model"
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
	// MatchedIn is one of searchFields -- the highest-ranked field that
	// contained the query.
	MatchedIn string `json:"matched_in"`

	// Snippet is the text around the match, when the match is somewhere you
	// cannot already see. Found by using this against a real store: one
	// captured document whose body is a whole roadmap matched almost every
	// query, and "matched in body" gave no way to tell a real hit from a
	// passing mention. A window of the actual text does.
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

// Search finds items whose text contains Query, ranked by WHERE the match
// landed and then by the store's usual score order.
//
// Substring matching over five columns rather than SQLite's FTS5, and that
// is a deliberate trade rather than the easy road. FTS5 would rank better
// and scan faster, but it needs its own virtual table, its own triggers to
// stay in step with items, and a migration for every store that already
// exists -- three new ways for a work store to disagree with itself, bought
// for a corpus that is a few thousand rows of one person's tickets. A LIKE
// scan over that is under a millisecond. If a store ever grows to where
// that stops being true, the fix is FTS5 behind this same signature, and
// nothing above this line has to know.
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

	all := append(rankArgs, args...)
	if f.Limit > 0 {
		all = append(all, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, sqlText, all...)
	if err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
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
// clause makes impossible -- it is reported honestly rather than guessed at.
func fieldName(rank int) string {
	if rank < 0 || rank >= len(searchFields) {
		return "unknown"
	}
	return searchFields[rank]
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
