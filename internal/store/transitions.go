package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gshepptech/rounce/internal/model"
)

// nullableString converts "" to a driver NULL and anything else to itself,
// the same "unset flag means SQL NULL, not empty string" convention
// insertItem (items.go) uses for repo/paths. StartItem uses it for
// claimed_by_session (an empty ROUNCE_SESSION means "no session", not a
// session literally named ""); UpdateItem (task 2) uses it for
// repo/paths/assignee.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isBlockedTx is the tx-scoped twin of IsBlocked (ready.go). StartItem must
// call it instead of IsBlocked -- confirmed empirically (this plan's own
// probe, not by reasoning about it) that under this Store's
// SetMaxOpenConns(1) (schema.go), calling s.IsBlocked (which queries via
// s.db) from inside an already-open s.withTx transaction on the SAME *Store
// deadlocks: the pool has exactly one connection, already checked out by the
// open transaction, so the second query blocks forever waiting for a
// connection the pool is not allowed to create. In the probe, the call only
// ever returned because the outer context's deadline expired, which forces
// database/sql to auto-rollback the transaction and free the connection --
// at which point the pending query succeeded, but the enclosing withTx then
// failed with "sql: transaction has already been committed or rolled back".
// With no deadline on the caller's context (the normal case for a CLI
// invocation), this is a permanent hang, not a slow query.
//
// isBlockedTx embeds the SAME blockedTreeCTE constant (ready.go) IsBlocked
// does -- not a second copy of the SQL -- so the two can never disagree
// about which items are blocked; it differs only in running the query
// against tx instead of s.db. BEGIN IMMEDIATE (this Store's DSN) already
// serializes every writer against this transaction, so there is no
// TOCTOU window between this read and StartItem's later UPDATE.
func isBlockedTx(ctx context.Context, tx *sql.Tx, id string) (string, bool, error) {
	query := blockedTreeCTE + `
SELECT CASE WHEN bt.depth = 0 THEN bt.blocker_id ELSE bt.nearest_blocked_id END AS blocked_by
FROM blocked_tree bt
WHERE bt.item_id = ?
LIMIT 1`

	var blockedBy string
	err := tx.QueryRowContext(ctx, query, id).Scan(&blockedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("isBlockedTx: %w", err)
	}
	return blockedBy, true, nil
}

// getItemForUpdateTx reads id's full row via itemColumns/scanItems inside an
// already-open transaction, returning ErrNotFound on no rows. StartItem,
// closeOrDrop (task 2), and UpdateItem (task 2) all read-then-write the same
// row inside one withTx (02-RESEARCH.md Pitfall 2: never split the read from
// the write across two transactions), so they share this one query shape
// rather than each writing its own SELECT.
func getItemForUpdateTx(ctx context.Context, tx *sql.Tx, id string) (model.Item, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id = ?`, id)
	if err != nil {
		return model.Item{}, err
	}
	items, err := scanItems(rows)
	rows.Close()
	if err != nil {
		return model.Item{}, err
	}
	if len(items) == 0 {
		return model.Item{}, fmt.Errorf("item %q: %w", id, ErrNotFound)
	}
	return items[0], nil
}

// StartItemInput is the argument to StartItem. Actor is required, same rule
// as every other mutation in this package (EVENT-01 is unfalsifiable if
// actor can be silently empty). SessionID is the caller's ROUNCE_SESSION,
// which may legitimately be empty -- there is no requirement that an agent
// set one.
type StartItemInput struct {
	ID        string
	Actor     string
	SessionID string
}

// startEventPayload is the JSON body of the one `start` event StartItem
// writes on a genuine (non-idempotent) claim.
type startEventPayload struct {
	SessionID string `json:"session_id,omitempty"`
}

// StartItem implements the locked six-row state table (02-CONTEXT.md
// decision 3) exactly. All six rows execute inside one withTx -- the read
// (current status/stage/claim) and the write (the claim itself) must never
// be split across two transactions, or a concurrent StartItem could observe
// stale state (02-RESEARCH.md Pitfall 2).
//
// Check order inside the transaction is load-bearing, in this exact
// sequence:
//  1. Missing item -> ErrNotFound.
//  2. stage != ticket -> ErrInvalidTransition ("raw items are not
//     workable; promote it first" -- README principle 4).
//  3. status is closed or dropped -> ErrInvalidTransition.
//  4. status is in_progress: same claiming session -> idempotent no-op,
//     NO update, NO event, nil error; different session -> ErrConflict
//     naming the holder and claimed_at.
//  5. Blocked (directly or through the parent tree, via isBlockedTx) ->
//     ErrInvalidTransition naming the blocker.
//  6. Otherwise: claim it -- one UPDATE, one `start` event.
//
// Check 4 runs BEFORE check 5 deliberately: an agent retrying its own start
// on an item that has since become blocked by someone else's work must
// still get its idempotent success for the claim it already holds, rather
// than a confusing new failure it did nothing to cause. A later refactor
// that "simplifies" this by reordering the checks (e.g. to check blocking
// unconditionally before claim status) would silently break every agent's
// retry loop the moment a race window opens between claiming and a
// concurrent block -- this comment is the load-bearing part, not the code
// shape.
func (s *Store) StartItem(ctx context.Context, in StartItemInput) (model.Item, error) {
	if in.Actor == "" {
		return model.Item{}, fmt.Errorf("StartItem: Actor is required")
	}

	var out model.Item
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		item, err := getItemForUpdateTx(ctx, tx, in.ID)
		if err != nil {
			return fmt.Errorf("StartItem: %w", err)
		}

		if item.Stage != model.StageTicket {
			return fmt.Errorf("StartItem: item %q: raw items are not workable; promote it first (README principle 4): %w", in.ID, ErrInvalidTransition)
		}
		if item.Status == model.StatusClosed || item.Status == model.StatusDropped {
			return fmt.Errorf("StartItem: item %q: status %q cannot be started: %w", in.ID, item.Status, ErrInvalidTransition)
		}

		if item.Status == model.StatusInProgress {
			holder := ""
			if item.ClaimedBySession != nil {
				holder = *item.ClaimedBySession
			}
			if holder == in.SessionID {
				// Idempotent no-op: this is the same session retrying its
				// own claim. NO update, NO event -- the deliberate
				// exception to "every mutation writes one event"
				// (02-CONTEXT.md decision 3: a retried event would put a
				// claim in the audit trail that never happened).
				out = item
				return nil
			}
			claimedAt := ""
			if item.ClaimedAt != nil {
				claimedAt = *item.ClaimedAt
			}
			return fmt.Errorf("StartItem: item %q already claimed by session %q since %s: %w", in.ID, holder, claimedAt, ErrConflict)
		}

		// item.Status == open here: every other status was handled above.
		blockerID, blocked, err := isBlockedTx(ctx, tx, in.ID)
		if err != nil {
			return fmt.Errorf("StartItem: %w", err)
		}
		if blocked {
			return fmt.Errorf("StartItem: item %q is blocked by %q: %w", in.ID, blockerID, ErrInvalidTransition)
		}

		now := nowFunc()
		if _, err := tx.ExecContext(ctx,
			`UPDATE items SET status = ?, claimed_by_session = ?, claimed_at = ?, updated_at = ? WHERE id = ?`,
			model.StatusInProgress, nullableString(in.SessionID), now, now, in.ID,
		); err != nil {
			return err
		}

		item.Status = model.StatusInProgress
		if in.SessionID != "" {
			session := in.SessionID
			item.ClaimedBySession = &session
		} else {
			item.ClaimedBySession = nil
		}
		claimedAt := now
		item.ClaimedAt = &claimedAt
		item.UpdatedAt = now
		out = item

		payload := startEventPayload{SessionID: in.SessionID}
		return writeEvent(ctx, tx, in.ID, "start", in.Actor, in.SessionID, payload)
	})
	if err != nil {
		return model.Item{}, err
	}
	return out, nil
}
