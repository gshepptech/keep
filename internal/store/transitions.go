package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/rank"
)

// ErrNoFieldsToUpdate is returned by UpdateItem when the caller sets no
// fields at all. A no-flag update is a caller error, not a silent no-op;
// the CLI maps this to a usage
// error (exit 2), the same treatment `add` with no title would get.
var ErrNoFieldsToUpdate = errors.New("update: no fields supplied")

// nullableString converts "" to a driver NULL and anything else to itself,
// the same "unset flag means SQL NULL, not empty string" convention
// insertItem (items.go) uses for repo/paths. StartItem uses it for
// claimed_by_session (an empty KEEP_SESSION means "no session", not a
// session literally named ""); UpdateItem uses it for repo/paths/assignee.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// innerInt unwraps UpdateItemInput's double pointer to the value being set,
// or nil when the field was left alone or is being cleared. Both of those
// mean "no number to check".
func innerInt(p **int) *int {
	if p == nil {
		return nil
	}
	return *p
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
// call it instead of IsBlocked: under this Store's
// SetMaxOpenConns(1) (schema.go), calling s.IsBlocked (which queries via
// s.db) from inside an already-open s.withTx transaction on the SAME *Store
// deadlocks: the pool has exactly one connection, already checked out by the
// open transaction, so the second query blocks forever waiting for a
// connection the pool is not allowed to create. With a deadline, the call
// only returns once the deadline expires, which forces database/sql to
// auto-rollback the transaction and free the connection. The pending query
// then succeeds, but the enclosing withTx fails with "sql:
// transaction has already been committed or rolled back".
// With no deadline on the caller's context (the normal case for a CLI
// invocation), this is a permanent hang, not a slow query.
//
// isBlockedTx embeds the same blockedTreeCTE constant (ready.go) IsBlocked
// does, not a second copy of the SQL, so the two can never disagree
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
// withTx (never split the read from the write across two transactions),
// so they share this one query shape rather than
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
// as every other mutation in this package (the audit trail means nothing if
// actor can be silently empty). SessionID is the caller's KEEP_SESSION,
// which the CLI and the board fill in for callers that do not set one;
// StartItem refuses an empty one (ErrNoSession).
//
// json tags: this struct doubles as the `start` MCP tool's wire type
// directly (internal/mcp). SessionID is `json:"-"` because session is a
// server-lifetime property (resolved once from KEEP_SESSION at server
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

// StartItem implements a six-row state table exactly. All six rows execute
// inside one withTx: the read (current status/stage/claim) and the write
// (the claim itself) must never be split across two transactions, or a
// concurrent StartItem could observe stale state.
//
// Check order inside the transaction matters, in this exact sequence:
//  1. Missing item -> ErrNotFound.
//  2. stage != ticket -> ErrInvalidTransition ("raw items are not
//     workable; promote it first").
//  3. status is closed or dropped -> ErrInvalidTransition.
//  4. status is in_progress: same claiming session -> idempotent no-op,
//     NO update, NO event, nil error; different session -> ErrConflict
//     naming the holder and claimed_at.
//  5. Blocked (directly or through the parent tree, via isBlockedTx) ->
//     ErrInvalidTransition naming the blocker.
//  6. Otherwise: claim it, with one UPDATE and one `start` event.
//
// Check 4 runs before check 5 deliberately: an agent retrying its own start
// on an item that has since become blocked by someone else's work must
// still get its idempotent success for the claim it already holds, rather
// than a confusing new failure it did nothing to cause. Checking blocking
// before claim status would break every agent's retry loop whenever a
// concurrent block lands between claiming and retrying.
func (s *Store) StartItem(ctx context.Context, in StartItemInput) (model.Item, error) {
	if in.Actor == "" {
		return model.Item{}, fmt.Errorf("StartItem: Actor is required")
	}

	var out model.Item
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = claimItemTx(ctx, tx, in)
		return err
	})
	if err != nil {
		return model.Item{}, err
	}
	return out, nil
}

// claimItemTx is StartItem's six-row state table, lifted out so NextItem can
// run the identical claim inside its own transaction rather than carrying a
// second copy of it. Both entry points therefore refuse a raw item, a
// terminal item, another session's claim and a blocked item in the same way,
// and a change to the table is a change to one function.
//
// It takes a *sql.Tx rather than opening one, because NextItem must
// choose the item and claim it in ONE transaction.
func claimItemTx(ctx context.Context, tx *sql.Tx, in StartItemInput) (model.Item, error) {
	if in.SessionID == "" {
		return model.Item{}, fmt.Errorf("StartItem: item %q: %w -- set $KEEP_SESSION, or claim through a surface that derives one", in.ID, ErrNoSession)
	}

	item, err := getItemForUpdateTx(ctx, tx, in.ID)
	if err != nil {
		return model.Item{}, fmt.Errorf("StartItem: %w", err)
	}

	if item.Stage != model.StageTicket {
		return model.Item{}, fmt.Errorf("StartItem: item %q: raw items are not workable; promote it first: %w", in.ID, ErrInvalidTransition)
	}
	if item.Status == model.StatusClosed || item.Status == model.StatusDropped {
		return model.Item{}, fmt.Errorf("StartItem: item %q: status %q cannot be started: %w", in.ID, item.Status, ErrInvalidTransition)
	}

	if item.Status == model.StatusInProgress {
		holder := ""
		if item.ClaimedBySession != nil {
			holder = *item.ClaimedBySession
		}
		// A legacy anonymous claim (claimed_by_session NULL, written by an
		// older binary) reads back as "" and cannot match here, because the
		// check at the top of this function guarantees in.SessionID is not
		// "". So it falls through to the ErrConflict below rather than being
		// handed to whoever asked next as their own retry, and needs no
		// `holder != ""` guard of its own.
		if holder == in.SessionID {
			// Idempotent no-op: this is the same session retrying its own
			// claim. No update and no event: this is the deliberate
			// exception to "every mutation writes one event", because a
			// retried event would put a claim in the audit trail that never
			// happened.
			return item, nil
		}
		claimedAt := ""
		if item.ClaimedAt != nil {
			claimedAt = *item.ClaimedAt
		}
		return model.Item{}, fmt.Errorf("StartItem: item %q already claimed by session %q since %s: %w", in.ID, holder, claimedAt, ErrConflict)
	}

	// item.Status == open here: every other status was handled above.
	blockerID, blocked, err := isBlockedTx(ctx, tx, in.ID)
	if err != nil {
		return model.Item{}, fmt.Errorf("StartItem: %w", err)
	}
	if blocked {
		return model.Item{}, fmt.Errorf("StartItem: item %q is blocked by %q: %w", in.ID, blockerID, ErrInvalidTransition)
	}

	now := nowFunc()

	// A claim says someone picked this up, so it records who. Without this
	// the item is in progress with an empty assignee, and every view asking
	// "who is working on this?" has to answer "nobody" for something a
	// person just claimed.
	//
	// An assignee already on the item is left alone: assigning work to
	// someone and then claiming it yourself is a real sequence, and the
	// event trail already names who did the claiming.
	assignee := item.Assignee
	if assignee == nil || *assignee == "" {
		assignee = &in.Actor
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE items SET status = ?, claimed_by_session = ?, claimed_at = ?, assignee = ?, updated_at = ? WHERE id = ?`,
		model.StatusInProgress, nullableString(in.SessionID), now, nullableString(*assignee), now, in.ID,
	); err != nil {
		return model.Item{}, err
	}

	item.Assignee = assignee
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

	payload := startEventPayload{SessionID: in.SessionID}
	if err := writeEvent(ctx, tx, in.ID, "start", in.Actor, in.SessionID, payload); err != nil {
		return model.Item{}, err
	}
	return item, nil
}

// UnclaimItemInput is the argument to UnclaimItem. Actor and SessionID
// record who performed the release, not who held the claim.
//
// json tags: this struct doubles as the `unclaim` MCP tool's wire type.
// RequireOwnClaim is `json:"-"` and unreachable over the wire: the MCP
// handler pre-seeds it true and no argument can turn it off. UnclaimItem's
// doc comment explains why that restriction is what makes the MCP tool
// safe.
type UnclaimItemInput struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"-"`

	// RequireOwnClaim refuses to release a claim held by a session other
	// than SessionID. False for a person at a terminal, who is the one who
	// can see that the holding agent is gone; true for another agent.
	RequireOwnClaim bool `json:"-"`
}

// unclaimEventPayload names both halves of the rescue: the session whose
// claim was released, and why. Held holds the session that lost the claim so
// the trail answers "whose work was this?" after the claim itself is gone.
type unclaimEventPayload struct {
	Held   string `json:"held_by,omitempty"`
	Reason string `json:"reason"`
}

// UnclaimItem returns an in-progress item to the queue, undoing a claim
// without asserting anything about the work.
//
// # Why this verb has to exist
//
// `start` moves open -> in_progress, and without this only `close` and
// `drop` could move it out again. An agent that dies mid-item, which is the
// ordinary failure for the agents this store serves, would leave
// the item stuck in progress, out of `ready`, with two exits that both
// record something false: `close` says it was done, `drop` says it was
// abandoned as not worth doing.
//
// `close` and `drop` are kept distinguishable so abandoned work is never
// counted as done. That guarantee is worth nothing if the only way to rescue
// a dead agent's item is to pick one of them and lie. This is the third exit,
// and it asserts only what actually happened: the claim ended.
//
// # Why any session may do it
//
// `start` refuses a claim held by a different session (ErrConflict), because
// two agents on one item is the collision it exists to prevent. Unclaim
// does not check the holder by default: the holder is, by
// definition of the case this serves, not coming back to release it itself.
// The event records who did the releasing, so a rescue is never anonymous.
//
// That openness is also why `stale` only reports (internal/store/stale.go)
// and why the unrestricted form is not an MCP tool: a verb that can take
// work away from a live agent belongs with the person who can look at the
// terminal, not in the hands of another agent that has just been told the
// queue is empty.
//
// That argument holds for someone else's claim and does not hold for your
// own. An agent that claimed an item and then found it could not do the
// work would otherwise have only close, which says it was done, and drop,
// which says it was not worth doing, with no way to say it is putting the
// item back. So the wire surface is the narrow half of this verb
// (RequireOwnClaim true, releasing your own claim and nothing else), and
// the unrestricted rescue stays with the person at the terminal.
//
// The status returns to open and the claim fields clear. Assignee is left
// alone: "who has this right now" and "who should do this" are different
// questions, and only the first one ended.
func (s *Store) UnclaimItem(ctx context.Context, in UnclaimItemInput) (model.Item, error) {
	// Reason is validated in the store, as it is for close, drop and
	// reopen; see validate.go.
	if in.Actor == "" {
		return model.Item{}, fmt.Errorf("UnclaimItem: Actor is required")
	}
	if err := requireReason("UnclaimItem", in.Reason); err != nil {
		return model.Item{}, err
	}

	var out model.Item
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		item, err := getItemForUpdateTx(ctx, tx, in.ID)
		if err != nil {
			return fmt.Errorf("UnclaimItem: %w", err)
		}

		if item.Status != model.StatusInProgress {
			return fmt.Errorf("UnclaimItem: item %q: status %q is not claimed, so there is nothing to release: %w",
				in.ID, item.Status, ErrInvalidTransition)
		}

		held := ""
		if item.ClaimedBySession != nil {
			held = *item.ClaimedBySession
		}

		// The narrow half of the verb: release your own claim only. Set by
		// the MCP handler and unreachable over the wire, so an agent cannot
		// opt out of it by supplying an argument.
		// An empty session on either side means the same thing it means for
		// a claim: nobody in particular. Comparing two of them would let
		// any anonymous caller release any anonymous claim, which is the
		// restriction inverted rather than enforced.
		if in.RequireOwnClaim && (in.SessionID == "" || held == "" || held != in.SessionID) {
			return fmt.Errorf("UnclaimItem: item %q is claimed by session %q, not by this one -- releasing another session's claim is a `keep unclaim` a person runs, not something an agent does on its behalf: %w",
				in.ID, held, ErrConflict)
		}

		now := nowFunc()
		if _, err := tx.ExecContext(ctx,
			`UPDATE items SET status = ?, claimed_by_session = NULL, claimed_at = NULL, updated_at = ? WHERE id = ?`,
			model.StatusOpen, now, in.ID,
		); err != nil {
			return err
		}

		item.Status = model.StatusOpen
		item.ClaimedBySession = nil
		item.ClaimedAt = nil
		item.UpdatedAt = now
		out = item

		return writeEvent(ctx, tx, in.ID, "unclaim", in.Actor, in.SessionID,
			unclaimEventPayload{Held: held, Reason: in.Reason})
	})
	if err != nil {
		return model.Item{}, err
	}
	return out, nil
}

// CloseItemInput is the argument to CloseItem.
//
// json tags: this struct doubles as the `close` MCP tool's wire type
// directly (internal/mcp). SessionID is `json:"-"`, for the same reason as
// every other mutating *Input struct in this package: session is a
// server-lifetime property, never a per-call argument. Tags are additive
// and behaviourally inert for Go callers.
type CloseItemInput struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"-"`
}

// DropItemInput is the argument to DropItem. It has the same shape and
// json-tag reasoning as CloseItemInput.
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

// closeOrDrop is CloseItem's and DropItem's shared implementation. Abandoned
// work must never be counted as done, which only holds if `close` and
// `drop` stay distinguishable in both items.status and events.kind: the two
// parameters (status, kind) the two exported wrappers vary. Two
// near-duplicate implementations could drift into doing the same thing;
// one shared body parameterized by the two constants that must never match
// prevents that.
//
// Closing (or dropping) an item that is already closed or dropped returns
// ErrInvalidTransition: terminal states do not transition again, in either
// direction, through either verb.
func (s *Store) closeOrDrop(ctx context.Context, id, status, kind, reason, actor, sessionID string) (model.Item, error) {
	if actor == "" {
		return model.Item{}, fmt.Errorf("%s: Actor is required", kind)
	}
	if err := requireReason(kind, reason); err != nil {
		return model.Item{}, err
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

// ReopenItemInput is the argument to ReopenItem.
//
// json tags: this struct doubles as the `reopen` MCP tool's wire type.
// Agents are the ones who close the wrong id, and they notice within the
// same turn; without the tool they would have to file the work again,
// losing the id, the history and every dependency edge pointing at the
// original.
//
// Reopen is also the safest verb to expose: it destroys nothing, the
// original close stays in the events table (events are never edited),
// and the item returns to open rather than to anybody's claim.
type ReopenItemInput struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"-"`
}

// reopenEventPayload names the status the item came back FROM. Without it
// the log cannot distinguish a close that was wrong from a drop that was
// wrong, and those are different mistakes: one said the work was done, the
// other said it was not worth doing.
type reopenEventPayload struct {
	From   string `json:"from"`
	Reason string `json:"reason"`
}

// ReopenItem returns a closed or dropped item to the queue.
//
// Without it, close and drop would be terminal statuses with no way out,
// making a mis-typed id unrecoverable: the work would have to be filed
// again, losing the id, the event history, and every dependency edge
// pointing at the original. UnclaimItem solves the same problem for
// in_progress, and this solves it the same way: not by editing the close,
// since events are never edited, but by appending a `reopen` event that
// says the close was wrong and when.
//
// It returns the item to open, never to in_progress. A claim is a live
// assertion that some session is working on it, and reopening cannot make
// that true again: the session that held it is long gone. Whoever wants it
// calls start.
//
// closed_at is cleared, because a reopened item has no close date and
// leaving a stale one would make `stale` and any date-ordered read lie.
// The original close is still in the events table, which is where the
// history belongs.
//
// The claim columns are cleared for the same reason. close and drop leave
// claimed_by_session set, because on a finished item it records who did the
// work, which is useful. On an open one it is a contradiction: open means
// nobody holds this, and a leftover session id would show a phantom holder on
// the board and in `stale`. UnclaimItem clears them on the same transition for
// the same reason.
func (s *Store) ReopenItem(ctx context.Context, in ReopenItemInput) (model.Item, error) {
	if in.Actor == "" {
		return model.Item{}, fmt.Errorf("ReopenItem: Actor is required")
	}
	if err := requireReason("ReopenItem", in.Reason); err != nil {
		return model.Item{}, err
	}

	var out model.Item
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		item, err := getItemForUpdateTx(ctx, tx, in.ID)
		if err != nil {
			return fmt.Errorf("ReopenItem: %w", err)
		}

		if item.Status != model.StatusClosed && item.Status != model.StatusDropped {
			return fmt.Errorf("ReopenItem: item %q: status %q is not finished, so there is nothing to reopen: %w",
				in.ID, item.Status, ErrInvalidTransition)
		}
		from := item.Status

		now := nowFunc()
		if _, err := tx.ExecContext(ctx,
			`UPDATE items SET status = ?, closed_at = NULL, claimed_by_session = NULL, claimed_at = NULL, updated_at = ? WHERE id = ?`,
			model.StatusOpen, now, in.ID,
		); err != nil {
			return err
		}

		item.Status = model.StatusOpen
		item.ClosedAt = nil
		item.ClaimedBySession = nil
		item.ClaimedAt = nil
		item.UpdatedAt = now
		out = item

		return writeEvent(ctx, tx, in.ID, "reopen", in.Actor, in.SessionID,
			reopenEventPayload{From: from, Reason: in.Reason})
	})
	if err != nil {
		return model.Item{}, err
	}
	return out, nil
}

// UpdateItemInput is the argument to UpdateItem. Every field is a pointer
// so "not supplied" (nil) and "supplied" (non-nil, even if the pointed-to
// value is the zero value) are different requests: clearing a title to ""
// and leaving the title alone are not the same operation.
//
// There is deliberately no Status field: open -> in_progress ->
// closed/dropped moves only through StartItem/CloseItem/DropItem, so the
// event log always records why a status changed.
// There is deliberately no ParentID field either: nothing reparents an
// item, which is what lets AddItem's --parent handling (insertParentChildEdge,
// items.go) stay the single writer of parent-child structure.
//
// Value/Confidence/Effort are double pointers: the outer pointer is
// "touched or not" (nil = leave this scoring input alone); the inner
// pointer is the value itself, which may legitimately be nil (clear this
// input back to unscored). "Unscored" (column NULL) and "scored 0" must
// remain distinguishable, and a single *int cannot express both "leave it"
// and "clear it" at once. The `update` CLI's `--score` handling is written
// against this encoding.
//
// The json tags are what puts this type on the wire: `update` is an MCP
// tool, and internal/mcp reuses this struct unmodified so its handler stays
// the same branchless three lines as every other tool's (a handler may hold
// no conditional, which rules out a conversion function).
//
// One thing the wire cannot express: clearing a score back to unscored.
// encoding/json gives a **int field nil for both an absent key and an
// explicit null, so "leave it alone" and "clear it" arrive identically. The
// CLI keeps that ability through `--score none`, which has somewhere to put
// the distinction. Setting a score works over both.
type UpdateItemInput struct {
	ID        string `json:"id"`
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"-"`

	Title      *string `json:"title,omitempty"`
	Body       *string `json:"body,omitempty"`
	Acceptance *string `json:"acceptance,omitempty"`
	Why        *string `json:"why,omitempty"`
	Type       *string `json:"type,omitempty"`
	Stage      *string `json:"stage,omitempty"`
	Repo       *string `json:"repo,omitempty"`
	Paths      *string `json:"paths,omitempty"`
	Assignee   *string `json:"assignee,omitempty"`

	Value      **int `json:"value,omitempty"`
	Confidence **int `json:"confidence,omitempty"`
	Effort     **int `json:"effort,omitempty"`
}

// updateEventPayload is the JSON body of the one `update` event UpdateItem
// writes. It names the changed fields, not their values, because the values
// are already in the items row the event refers to.
type updateEventPayload struct {
	Fields []string `json:"fields"`
}

// UpdateItem applies only the fields the caller set, leaves every other
// column untouched, always bumps updated_at, and writes one `update` event
// naming the changed fields. Score is recomputed in the same transaction
// whenever any of value/confidence/effort was supplied, using rank.Score on
// the item's post-update triple rather than on the raw deltas the caller
// passed, since a caller may set only one of the three and expect the other
// two to keep contributing to the recomputed score.
//
// Supplying no fields at all is a caller error (ErrNoFieldsToUpdate), not a
// silent no-op, and is returned before any write is attempted.
func (s *Store) UpdateItem(ctx context.Context, in UpdateItemInput) (model.Item, error) {
	if in.Actor == "" {
		return model.Item{}, fmt.Errorf("UpdateItem: Actor is required")
	}
	// A nil Title means "leave it alone"; a non-nil empty one means "blank
	// it", which AddItem already refuses to create. Closing one door and
	// leaving the other open would just make the hole take two commands.
	if in.Title != nil && strings.TrimSpace(*in.Title) == "" {
		return model.Item{}, fmt.Errorf("UpdateItem: %w", ErrEmptyTitle)
	}
	// The outer pointer is "touched or not" and the inner is the value, which
	// may legitimately be nil (clear this input back to unscored). Only an
	// input being SET carries a number to check.
	if err := checkScoreRange(innerInt(in.Value), innerInt(in.Confidence), innerInt(in.Effort)); err != nil {
		return model.Item{}, fmt.Errorf("UpdateItem: %w", err)
	}
	// Closed vocabularies. The schema's CHECK constraints would refuse these
	// too, several statements later and in SQLite's own words; naming them
	// here is what lets a caller be told which value was wrong.
	if in.Type != nil {
		if err := checkType(*in.Type); err != nil {
			return model.Item{}, fmt.Errorf("UpdateItem: %w", err)
		}
	}
	if in.Stage != nil {
		if err := checkStage(*in.Stage); err != nil {
			return model.Item{}, fmt.Errorf("UpdateItem: %w", err)
		}
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
