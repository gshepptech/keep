package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gshepptech/keep/internal/model"
)

// NextInput is the argument to NextItem.
//
// json tags: this struct doubles as the `next` MCP tool's wire type.
// SessionID is `json:"-"` for the same reason every other mutating input in
// this package has it -- session is a server-lifetime property, never a
// per-call argument.
type NextInput struct {
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"-"`

	// Skip names item ids NextItem must not claim, however high they rank.
	//
	// It exists for the Interlock join and is deliberately expressed as
	// data rather than as a hook: internal/store must not shell out to
	// another program, least of all from inside a transaction that holds
	// the write lock. The caller asks Interlock which files are held, works
	// out which items collide using the same interlock.Exclude that `ready`
	// uses, and passes the ids. The store's job stays "give me the top of
	// the queue that is not one of these".
	//
	// `json:"-"`: an agent must not be able to steer which item it is given
	// by naming ids to skip. The lease state is the caller's to determine,
	// not the caller's to assert.
	Skip []string `json:"-"`
}

// NextResult is what NextItem answers with. An empty queue is a result, not
// an error: "nothing is workable right now" is the correct and common answer
// to "what should I do", and an agent that received it as a failure would
// have to distinguish it from a real one to avoid retrying forever.
type NextResult struct {
	// Item is the item now claimed by the calling session. Only meaningful
	// when Claimed is true.
	Item model.Item `json:"item,omitzero"`

	// Claimed is false when the queue had nothing in it.
	Claimed bool `json:"claimed"`

	// Blocked is how many open tickets exist that `ready` is holding back.
	// It is the difference between "there is no work" and "there is work and
	// something is in its way", which is the first thing anyone asks when
	// the queue comes back empty.
	Blocked int `json:"blocked"`
}

// NextItem takes the top of the ready queue and claims it, in one
// transaction.
//
// The two-call sequence it replaces -- `ready`, then `start` on the top row
// -- has a window between the read and the write in which another agent can
// claim the same item. The loser gets ErrConflict and has to re-read and
// retry, which is a retry loop every single agent integration has to write
// correctly and independently. Doing both inside one BEGIN IMMEDIATE closes
// the window at the only layer that can: this store serializes writers, so
// two concurrent NextItem calls get two different items, or one gets an item
// and the other gets an empty queue.
//
// It claims exactly the row `ready` puts first. It does not re-rank, filter
// or skip -- the ordering lives in the view (README principle 2), and a
// "pick the best one for me" heuristic here would be the store reasoning
// about the work, which is the one thing it does not do.
func (s *Store) NextItem(ctx context.Context, in NextInput) (NextResult, error) {
	if in.Actor == "" {
		return NextResult{}, fmt.Errorf("NextItem: Actor is required")
	}

	var out NextResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var id string
		query, args := topOfQueueQuery(in.Skip)
		err := tx.QueryRowContext(ctx, query, args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			blocked, cerr := countBlockedTx(ctx, tx)
			out = NextResult{Claimed: false, Blocked: blocked}
			return cerr
		}
		if err != nil {
			return fmt.Errorf("NextItem: read the queue: %w", err)
		}

		item, err := claimItemTx(ctx, tx, StartItemInput{ID: id, Actor: in.Actor, SessionID: in.SessionID})
		if err != nil {
			return err
		}
		out = NextResult{Item: item, Claimed: true}
		return nil
	})
	if err != nil {
		return NextResult{}, err
	}
	return out, nil
}

// topOfQueueQuery selects the first row of `ready` the caller has not asked
// to skip. With nothing to skip it is the bare query and no bind arguments,
// so the common path stays exactly what it was.
//
// The placeholders are generated rather than the ids interpolated: an item
// id comes from the store, but a caller could pass anything, and a query
// built by string concatenation from caller data is the one shape this
// package does not allow anywhere else either.
func topOfQueueQuery(skip []string) (string, []any) {
	if len(skip) == 0 {
		return `SELECT id FROM ready LIMIT 1`, nil
	}
	placeholders := make([]string, len(skip))
	args := make([]any, len(skip))
	for i, id := range skip {
		placeholders[i] = "?"
		args[i] = id
	}
	return `SELECT id FROM ready WHERE id NOT IN (` + strings.Join(placeholders, ",") + `) LIMIT 1`, args
}

// countBlockedTx counts the open tickets `ready` is holding back, using the
// same blockedTreeCTE every other blocking answer in this package uses. It
// is the one number that makes an empty queue interpretable.
func countBlockedTx(ctx context.Context, tx *sql.Tx) (int, error) {
	query := blockedTreeCTE + `
SELECT COUNT(DISTINCT bt.item_id)
FROM blocked_tree bt
JOIN items i ON i.id = bt.item_id
WHERE i.stage = 'ticket' AND i.status = 'open'`

	var n int
	if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("countBlockedTx: %w", err)
	}
	return n, nil
}

// Workability is WhyNot's answer: whether one item is workable right now,
// and if not, the single reason it is not.
type Workability struct {
	ID     string `json:"id"`
	Ready  bool   `json:"ready"`
	Stage  string `json:"stage"`
	Status string `json:"status"`

	// Reason is a sentence for a person or a model to read. Empty when
	// Ready is true.
	Reason string `json:"reason,omitempty"`

	// BlockedBy names the nearest item in the way, when that is the reason.
	// Empty otherwise -- including when the item is not workable for some
	// other reason, so a caller cannot mistake "not blocked" for "workable".
	BlockedBy string `json:"blocked_by,omitempty"`
}

// WhyNot answers the question an empty `ready` always provokes: this item is
// not in the queue -- why not?
//
// Before this, the only way to find out was to read `show`, compare stage and
// status against the view's predicate by eye, and then check `blocked`
// separately for the propagated case, which a person can do and an agent
// reliably cannot. Every reason `ready` can omit an item for is enumerated
// here, in the view's own order, so the answer cannot disagree with the view
// about anything except by this function being wrong -- and it reads its
// blocking answer from IsBlocked, which embeds the same CTE the view does.
//
// The reasons, in the order the view applies them:
//
//  1. stage is raw. Parked, captured or unrefined; README principle 4 says
//     raw is not workable, and promotion is what changes that.
//  2. status is not open. In progress means someone holds it; closed and
//     dropped mean it is finished with.
//  3. blocked, directly or through a blocked ancestor. Names the nearest
//     blocker.
//
// An item that passes all three is in the queue, and Ready is true.
func (s *Store) WhyNot(ctx context.Context, id string) (Workability, error) {
	detail, err := s.GetItem(ctx, id)
	if err != nil {
		return Workability{}, err
	}
	item := detail.Item

	out := Workability{ID: item.ID, Stage: item.Stage, Status: item.Status}

	if item.Stage != model.StageTicket {
		out.Reason = fmt.Sprintf("stage is %q: raw items never appear in ready. Refine it into a ticket (add --parent %s --derived-from %s, or update --stage ticket).", item.Stage, item.ID, item.ID)
		return out, nil
	}
	if item.Status != model.StatusOpen {
		out.Reason = fmt.Sprintf("status is %q: ready lists open tickets only.", item.Status)
		return out, nil
	}

	blockerID, blocked, err := s.IsBlocked(ctx, id)
	if err != nil {
		return Workability{}, err
	}
	if blocked {
		out.BlockedBy = blockerID
		out.Reason = fmt.Sprintf("blocked by %s: it or an ancestor waits on something that is not closed.", blockerID)
		return out, nil
	}

	out.Ready = true
	return out, nil
}
