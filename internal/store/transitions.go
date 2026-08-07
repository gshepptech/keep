package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/rank"
)

// ErrNoFieldsToUpdate is returned by UpdateItem when the caller sets no
// fields at all. 02-CONTEXT.md decision 4 requires a no-flag update to be a
// caller error, not a silent no-op -- the CLI (plan 09) maps this to a usage
// error (exit 2), the same treatment `add` with no title would get.
var ErrNoFieldsToUpdate = errors.New("update: no fields supplied")

// nullableString converts "" to a driver NULL and anything else to itself,
// the same "unset flag means SQL NULL, not empty string" convention
// insertItem (items.go) uses for repo/paths. StartItem uses it for
// claimed_by_session (an empty ROUNCE_SESSION means "no session", not a
// session literally named ""); UpdateItem uses it for repo/paths/assignee.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// derefIntPtr converts a possibly-nil *int into a driver value: nil becomes
// SQL NULL, a non-nil pointer becomes its dereferenced value. Used by
// UpdateItem's score-input columns, which are cleared (set NULL) rather than
// zeroed when the caller supplies an explicit nil inner pointer.
func derefIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
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
// closeOrDrop, and UpdateItem all read-then-write the same row inside one
// withTx (02-RESEARCH.md Pitfall 2: never split the read from the write
// across two transactions), so they share this one query shape rather than
// each writing its own SELECT.
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
//
// json tags (04-02): this struct doubles as the `start` MCP tool's wire type
// directly (internal/mcp). SessionID is `json:"-"` -- session is a
// server-lifetime property (resolved once from ROUNCE_SESSION at server
// start), never a per-call argument, matching every other mutating *Input
// struct in this package. Tags are additive and behaviourally inert for Go
// callers, all of which construct this struct with named fields.
type StartItemInput struct {
	ID        string `json:"id"`
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"-"`
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

// CloseItemInput is the argument to CloseItem.
//
// json tags (04-02): this struct doubles as the `close` MCP tool's wire type
// directly (internal/mcp). SessionID is `json:"-"`, same reason as every
// other mutating *Input struct in this package -- session is a
// server-lifetime property, never a per-call argument. Split into one field
// per line (rather than the previous combined declaration) so each can carry
// its own tag; tags are additive and behaviourally inert for Go callers.
type CloseItemInput struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"-"`
}

// DropItemInput is the argument to DropItem. See CloseItemInput's doc
// comment -- identical shape, identical json-tag reasoning.
type DropItemInput struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"-"`
}

// closeDropEventPayload is the JSON body of the one `close` or `drop` event
// closeOrDrop writes, carrying the caller-supplied reason.
type closeDropEventPayload struct {
	Reason string `json:"reason"`
}

// CloseItem marks id closed, recording reason. See closeOrDrop for the
// shared implementation; CloseItem and DropItem differ only in the status
// and event kind they pass.
func (s *Store) CloseItem(ctx context.Context, in CloseItemInput) (model.Item, error) {
	return s.closeOrDrop(ctx, in.ID, model.StatusClosed, "close", in.Reason, in.Actor, in.SessionID)
}

// DropItem marks id dropped, recording reason. See closeOrDrop for the
// shared implementation; CloseItem and DropItem differ only in the status
// and event kind they pass.
func (s *Store) DropItem(ctx context.Context, in DropItemInput) (model.Item, error) {
	return s.closeOrDrop(ctx, in.ID, model.StatusDropped, "drop", in.Reason, in.Actor, in.SessionID)
}

// closeOrDrop is CloseItem's and DropItem's shared implementation. ITEM-08
// exists so abandoned work is never counted as done -- that only holds if
// `close` and `drop` stay distinguishable in both items.status and
// events.kind, which is exactly the two parameters (status, kind) the two
// exported wrappers vary. Two independent near-duplicate implementations is
// how `close` and `drop` would eventually drift into doing the same thing;
// one shared body parameterized by the two constants that must never match
// is what keeps that from happening.
//
// Closing (or dropping) an item that is already closed or dropped returns
// ErrInvalidTransition -- terminal states do not transition again, in
// either direction, through either verb.
func (s *Store) closeOrDrop(ctx context.Context, id, status, kind, reason, actor, sessionID string) (model.Item, error) {
	if actor == "" {
		return model.Item{}, fmt.Errorf("%s: Actor is required", kind)
	}

	var out model.Item
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		item, err := getItemForUpdateTx(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}

		if item.Status == model.StatusClosed || item.Status == model.StatusDropped {
			return fmt.Errorf("%s: item %q: already %s: %w", kind, id, item.Status, ErrInvalidTransition)
		}

		now := nowFunc()
		if _, err := tx.ExecContext(ctx,
			`UPDATE items SET status = ?, closed_at = ?, updated_at = ? WHERE id = ?`,
			status, now, now, id,
		); err != nil {
			return err
		}

		item.Status = status
		closedAt := now
		item.ClosedAt = &closedAt
		item.UpdatedAt = now
		out = item

		payload := closeDropEventPayload{Reason: reason}
		return writeEvent(ctx, tx, id, kind, actor, sessionID, payload)
	})
	if err != nil {
		return model.Item{}, err
	}
	return out, nil
}

// UpdateItemInput is the argument to UpdateItem. Every field is a pointer
// so "not supplied" (nil) and "supplied" (non-nil, even if the pointed-to
// value is the zero value) are different requests -- clearing a title to ""
// and leaving the title alone are not the same operation.
//
// There is deliberately no Status field: open -> in_progress ->
// closed/dropped moves only through StartItem/CloseItem/DropItem, so the
// event log always records WHY a status changed (02-CONTEXT.md decision 4).
// There is deliberately no ParentID field either: nothing reparents an
// item, which is what lets AddItem's --parent handling (insertParentChildEdge,
// items.go) stay the single writer of parent-child structure (02-RESEARCH.md
// Open Question 1).
//
// Value/Confidence/Effort are double pointers: the outer pointer is
// "touched or not" (nil = leave this scoring input alone); the inner
// pointer is the value itself, which may legitimately be nil (clear this
// input back to unscored) -- RANK-02 requires "unscored" (column NULL) and
// "scored 0" to remain distinguishable, and a single *int cannot express
// both "leave it" and "clear it" at once. This encoding is what plan 09's
// `update` CLI is written against; if it ever changes, that CLI's `--score`
// handling must change with it.
type UpdateItemInput struct {
	ID               string
	Actor, SessionID string

	Title, Body, Acceptance *string
	Why, Type, Stage        *string
	Repo, Paths, Assignee   *string

	Value, Confidence, Effort **int
}

// updateEventPayload is the JSON body of the one `update` event UpdateItem
// writes. It names the CHANGED FIELDS, not their values -- the values are
// already in the items row the event refers to (02-CONTEXT.md decision 4).
type updateEventPayload struct {
	Fields []string `json:"fields"`
}

// UpdateItem applies only the fields the caller set, leaves every other
// column untouched, always bumps updated_at, and writes one `update` event
// naming the changed fields. Score is recomputed (RANK-01) in the same
// transaction whenever any of value/confidence/effort was supplied -- using
// rank.Score on the item's POST-UPDATE triple, not on the raw deltas the
// caller passed, since a caller may set only one of the three and expect
// the other two to keep contributing to the recomputed score.
//
// Supplying no fields at all is a caller error (ErrNoFieldsToUpdate), not a
// silent no-op (02-CONTEXT.md decision 4) -- checked, and returned, before
// any write is attempted.
func (s *Store) UpdateItem(ctx context.Context, in UpdateItemInput) (model.Item, error) {
	if in.Actor == "" {
		return model.Item{}, fmt.Errorf("UpdateItem: Actor is required")
	}

	var out model.Item
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		item, err := getItemForUpdateTx(ctx, tx, in.ID)
		if err != nil {
			return fmt.Errorf("UpdateItem: %w", err)
		}

		var setClauses []string
		var args []any
		var changed []string

		addStringSet := func(col string, val *string, target *string, name string) {
			if val == nil {
				return
			}
			setClauses = append(setClauses, col+" = ?")
			args = append(args, *val)
			*target = *val
			changed = append(changed, name)
		}
		addStringSet("title", in.Title, &item.Title, "title")
		addStringSet("body", in.Body, &item.Body, "body")
		addStringSet("acceptance", in.Acceptance, &item.Acceptance, "acceptance")
		addStringSet("why", in.Why, &item.Why, "why")
		addStringSet("type", in.Type, &item.Type, "type")
		addStringSet("stage", in.Stage, &item.Stage, "stage")

		addNullableStringSet := func(col string, val *string, target **string, name string) {
			if val == nil {
				return
			}
			setClauses = append(setClauses, col+" = ?")
			args = append(args, nullableString(*val))
			if *val == "" {
				*target = nil
			} else {
				v := *val
				*target = &v
			}
			changed = append(changed, name)
		}
		addNullableStringSet("repo", in.Repo, &item.Repo, "repo")
		addNullableStringSet("paths", in.Paths, &item.Paths, "paths")
		addNullableStringSet("assignee", in.Assignee, &item.Assignee, "assignee")

		scoreTouched := in.Value != nil || in.Confidence != nil || in.Effort != nil
		if in.Value != nil {
			setClauses = append(setClauses, "value = ?")
			args = append(args, derefIntPtr(*in.Value))
			item.Value = *in.Value
			changed = append(changed, "value")
		}
		if in.Confidence != nil {
			setClauses = append(setClauses, "confidence = ?")
			args = append(args, derefIntPtr(*in.Confidence))
			item.Confidence = *in.Confidence
			changed = append(changed, "confidence")
		}
		if in.Effort != nil {
			setClauses = append(setClauses, "effort = ?")
			args = append(args, derefIntPtr(*in.Effort))
			item.Effort = *in.Effort
			changed = append(changed, "effort")
		}

		if len(setClauses) == 0 {
			return fmt.Errorf("UpdateItem: item %q: %w", in.ID, ErrNoFieldsToUpdate)
		}

		if scoreTouched {
			newScore := rank.Score(item.Value, item.Confidence, item.Effort)
			setClauses = append(setClauses, "score = ?")
			args = append(args, newScore)
			item.Score = newScore
		}

		now := nowFunc()
		setClauses = append(setClauses, "updated_at = ?")
		args = append(args, now)
		item.UpdatedAt = now

		args = append(args, in.ID)
		query := `UPDATE items SET ` + strings.Join(setClauses, ", ") + ` WHERE id = ?`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}

		out = item
		payload := updateEventPayload{Fields: changed}
		return writeEvent(ctx, tx, in.ID, "update", in.Actor, in.SessionID, payload)
	})
	if err != nil {
		return model.Item{}, err
	}
	return out, nil
}
