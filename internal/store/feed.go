package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gshepptech/keep/internal/model"
)

// FeedEntry is one event with enough of its item attached to be read on its
// own. The event log stores an item_id; a person reading a day's activity
// needs the title, because "close rnc-8f21ac" answers nothing.
type FeedEntry struct {
	model.Event
	Title  string `json:"title"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// FeedFilter narrows the activity feed.
type FeedFilter struct {
	// Since is an RFC3339 timestamp. Events at or after it are returned.
	// Empty means all of history.
	Since string `json:"since,omitempty"`
	// Actor returns only what one person or agent did.
	Actor string `json:"actor,omitempty"`
	// Kind returns only one kind of event (add, start, close, drop, ...).
	Kind  string `json:"kind,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// Feed returns events newest first, each carrying its item's title.
//
// This reads the events table, which EVENT-01 already guarantees is the
// complete append-only log of every mutation. Nothing new is recorded to
// support it and nothing can drift: an activity view derived from anything
// else would be a second, weaker record of the same facts.
//
// Ordering is `at DESC, id DESC`. The id tiebreak is load-bearing for the
// same reason it is in ListItems: several events written inside one
// transaction share a timestamp, and without it SQLite's order among them is
// unspecified -- a feed that reshuffles its own rows between two runs reads
// as a bug in the log rather than in the query.
func (s *Store) Feed(ctx context.Context, f FeedFilter) ([]FeedEntry, error) {
	var conds []string
	var args []any

	if f.Since != "" {
		conds = append(conds, "e.at >= ?")
		args = append(args, f.Since)
	}
	if f.Actor != "" {
		conds = append(conds, "e.actor = ?")
		args = append(args, f.Actor)
	}
	if f.Kind != "" {
		conds = append(conds, "e.kind = ?")
		args = append(args, f.Kind)
	}

	query := `SELECT e.id, e.item_id, e.kind, e.actor, e.session_id, e.at, e.payload,
	       i.title, i.type, i.status
	FROM events e JOIN items i ON i.id = e.item_id`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY e.at DESC, e.id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("Feed: %w", err)
	}
	defer rows.Close()

	var out []FeedEntry
	for rows.Next() {
		var e FeedEntry
		if err := rows.Scan(
			&e.ID, &e.ItemID, &e.Kind, &e.Actor, &e.SessionID, &e.At, &e.Payload,
			&e.Title, &e.Type, &e.Status,
		); err != nil {
			return nil, fmt.Errorf("Feed: %w", err)
		}
		e.Reason = reasonFromPayload(e.Payload)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Feed: %w", err)
	}
	return out, nil
}

// reasonFromPayload pulls the reason out of a close or drop payload. The
// payload is stored as JSON text and its shape varies by kind, so this
// reads the one field a reader of the feed actually wants and ignores the
// rest rather than growing a decoder per event kind. A payload that has no
// reason -- every other kind -- yields "".
func reasonFromPayload(payload *string) string {
	if payload == nil {
		return ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(*payload), &body); err != nil {
		return ""
	}
	return body.Reason
}
