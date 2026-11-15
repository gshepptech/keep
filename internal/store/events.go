package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// writeEvent is the ONLY place in this codebase that inserts into events.
// It takes a *sql.Tx and not a *sql.DB deliberately: the event
// write must land in the same transaction as the mutation it records, or a
// crash between the two produces a store whose audit trail disagrees with its
// data. Actor and session_id are caller-supplied rather than derived from
// the row because they are properties of who is asking, not of what is
// stored.
//
// An empty sessionID stores SQL NULL, not the empty string, so "no session"
// (agent/human ran a command with no KEEP_SESSION set) and "session named
// the empty string" are never conflated into the same row.
//
// payload is marshalled with encoding/json when non-nil; a nil payload
// stores NULL. at is always nowFunc() at the moment of the write -- the
// same clock indirection items.go uses, so a test that pins the clock pins
// the whole store rather than half of it; a staleness query needs exactly
// that control. See items.go's nowFunc seam for how callers keep a single
// mutation's
// created_at/updated_at/deps.created_at/event.at values internally
// consistent by threading one timestamp through, rather than calling
// writeEvent's own clock read.
func writeEvent(ctx context.Context, tx *sql.Tx, itemID, kind, actor, sessionID string, payload any) error {
	var payloadJSON sql.NullString
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("writeEvent: marshal payload for kind %q item %q: %w", kind, itemID, err)
		}
		payloadJSON = sql.NullString{String: string(b), Valid: true}
	}

	session := sql.NullString{String: sessionID, Valid: sessionID != ""}

	_, err := tx.ExecContext(ctx,
		`INSERT INTO events (item_id, kind, actor, session_id, at, payload) VALUES (?, ?, ?, ?, ?, ?)`,
		itemID, kind, actor, session, nowFunc(), payloadJSON,
	)
	if err != nil {
		return fmt.Errorf("writeEvent: insert kind %q for item %q: %w", kind, itemID, err)
	}
	return nil
}
