package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/rank"
)

// nowFunc is the seam AddItem (and any later mutation in this package) uses
// instead of calling model.Now() directly, so a test can pin "now" to a
// fixed value and make id-generation retries deterministic rather than
// depending on wall-clock timing or a sleep. Production code never
// overrides this; it is unexported so no caller outside the package can
// either.
var nowFunc = model.Now

// maxIDAttempts bounds both the root-id collision retry (SQLITE_CONSTRAINT_
// PRIMARYKEY, 1555) and the child-ordinal collision retry (SQLITE_CONSTRAINT_
// UNIQUE, 2067). 02-RESEARCH.md measured zero collisions of either kind
// across 320 real concurrent inserts under this DSN's BEGIN IMMEDIATE, so in
// practice each loop body runs exactly once; the bound exists so a genuine,
// permanent conflict returns ErrConflict instead of looping forever.
const maxIDAttempts = 8

// AddItemInput is the argument to AddItem. Actor is required -- EVENT-01 is
// unfalsifiable if actor can be silently empty, so AddItem rejects "" before
// doing any work. Type defaults to "task" and Stage to "ticket" when left
// unset: this phase creates tickets directly; Phase 3's `capture` is what
// creates stage=raw items.
type AddItemInput struct {
	Title       string
	Actor       string
	SessionID   string
	ParentID    string
	Type        string
	Stage       string
	Blocks      []string
	DerivedFrom []string
	Value       *int
	Confidence  *int
	Effort      *int
	Why         string
	Acceptance  string
	Body        string
	Repo        string
	Paths       string
}

// generateRootID derives a root item's id from a hash of its title,
// createdAt, and a retry attempt number, truncated to 3 bytes (6 hex chars,
// 24 bits = rnc-<6 hex>). This is not a security boundary -- sha256 is used
// only because its output is already a well-distributed byte stream needing
// no further mixing before truncation (02-RESEARCH.md "Standard Stack"). The
// retry loop this feeds is not defensive padding: birthday-bound collision
// probability crosses ~1% around 580 existing root items and ~50% around
// 4,820 (02-RESEARCH.md Pitfall 5).
func generateRootID(title, createdAt string, attempt int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", title, createdAt, attempt)))
	return fmt.Sprintf("rnc-%x", h[:3])
}

// isConstraintPrimaryKey and isConstraintUnique classify two DISTINCT SQLite
// extended result codes that both look like "a uniqueness violation" in
// prose but require different recoveries (02-RESEARCH.md Pattern 1): an
// items.id collision (1555, SQLITE_CONSTRAINT_PRIMARYKEY) means regenerate
// the hash and retry; an idx_items_parent_ordinal collision (2067,
// SQLITE_CONSTRAINT_UNIQUE) means re-read MAX(ordinal) and retry.
// Deliberately two separate functions, not one "isConstraint" that matches
// both -- collapsing them into one was this plan's documented GATE-03 break.
func isConstraintPrimaryKey(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

func isConstraintUnique(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// itemExists reports whether id names a row in items. AddItem uses this to
// turn a missing --parent/--blocks/--derived-from referent into ErrNotFound
// instead of letting the foreign key fire and surface extended code 787 and
// a driver message with no sentinel the CLI can switch on.
func itemExists(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM items WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// insertParentChildEdge writes the deps row that makes a parent-child
// relationship visible to the ready view. THE SHIPPED ready VIEW READS ONLY
// deps (WHERE type = 'parent-child'); IT NEVER CONSULTS items.parent_id
// (02-RESEARCH.md Pattern 3). AddItem sets items.parent_id/ordinal for id
// derivation and ordinal allocation ONLY -- omitting this row leaves
// items.parent_id looking correct while READY-02's blocking propagation
// silently does nothing for this child, with no error anywhere. add --parent
// must be this helper's single caller; plan 03 adds a cycle guard to this
// same helper rather than duplicating the insert at a second call site.
func insertParentChildEdge(ctx context.Context, tx *sql.Tx, childID, parentID, actor, createdAt string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO deps (item_id, depends_on_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, ?)`,
		childID, parentID, createdAt, actor,
	)
	return err
}

// insertEdge writes one deps row of the given type -- 'blocks' or
// 'derived-from' here (parent-child goes through insertParentChildEdge,
// which is deliberately the only writer of that type). Direction is fixed
// by the shipped ready view, not by English: a row (item_id=A,
// depends_on_id=B, type='blocks') means "A is blocked until B closes"
// (02-RESEARCH.md, ready view's blocked_directly CTE). So add --blocks <id>
// means the NEW item is blocked until <id> closes, and inserts
// (item_id=new, depends_on_id=<id>, type='blocks').
func insertEdge(ctx context.Context, tx *sql.Tx, itemID, dependsOnID, depType, actor, createdAt string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO deps (item_id, depends_on_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		itemID, dependsOnID, depType, createdAt, actor,
	)
	return err
}

// addEventPayload is the JSON body of the one `add` event AddItem writes,
// naming the parent (if any) and every edge created alongside the item.
type addEventPayload struct {
	ParentID    string   `json:"parent_id,omitempty"`
	Blocks      []string `json:"blocks,omitempty"`
	DerivedFrom []string `json:"derived_from,omitempty"`
}

// AddItem creates a new item -- root or child -- inside one transaction:
// hierarchical id allocation, the dual parent-child write, blocks/
// derived-from edges, the computed score, and exactly one `add` event. The
// returned model.Item is constructed from the values just written rather
// than re-read with a SELECT, since every value is already known at that
// point and no other writer can have touched the row inside this same
// transaction.
func (s *Store) AddItem(ctx context.Context, in AddItemInput) (model.Item, error) {
	if in.Actor == "" {
		return model.Item{}, fmt.Errorf("AddItem: Actor is required")
	}

	itemType := in.Type
	if itemType == "" {
		itemType = model.TypeTask
	}
	stage := in.Stage
	if stage == "" {
		stage = model.StageTicket
	}

	var out model.Item
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := nowFunc()

		// Missing referents become ErrNotFound before any insert is
		// attempted, rather than relying on the foreign key to fire
		// (02-RESEARCH.md "Missing referents").
		if in.ParentID != "" {
			ok, err := itemExists(ctx, tx, in.ParentID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("AddItem: parent %q: %w", in.ParentID, ErrNotFound)
			}
		}
		for _, b := range in.Blocks {
			ok, err := itemExists(ctx, tx, b)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("AddItem: blocks target %q: %w", b, ErrNotFound)
			}
		}
		for _, d := range in.DerivedFrom {
			ok, err := itemExists(ctx, tx, d)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("AddItem: derived-from target %q: %w", d, ErrNotFound)
			}
		}

		score := rank.Score(in.Value, in.Confidence, in.Effort)

		var id string
		var ordinal *int

		if in.ParentID == "" {
			// Root id allocation and insert, retrying on a PRIMARYKEY
			// collision against already-committed data (02-RESEARCH.md
			// Pattern 1). BEGIN IMMEDIATE means a collision here can only
			// be against committed data, never a concurrent in-flight id.
			for attempt := 0; ; attempt++ {
				if attempt >= maxIDAttempts {
					return fmt.Errorf("AddItem: generate root id: exhausted %d attempts: %w", maxIDAttempts, ErrConflict)
				}
				id = generateRootID(in.Title, now, attempt)
				execErr := insertItem(ctx, tx, id, nil, nil, stage, itemType, in.Title, in.Body, in.Acceptance, in.Value, in.Confidence, in.Effort, score, in.Why, in.Repo, in.Paths, now)
				if execErr == nil {
					break
				}
				if isConstraintPrimaryKey(execErr) {
					continue
				}
				return execErr
			}
		} else {
			// Child id and ordinal allocation. The read (MAX(ordinal)) and
			// the insert happen in this same transaction -- never split
			// across two withTx calls (02-RESEARCH.md Pitfall 2). Retrying
			// on SQLITE_CONSTRAINT_UNIQUE (2067) exists so a future
			// refactor that splits the read from the write fails loudly
			// instead of allocating a duplicate ordinal; 02-RESEARCH.md
			// measured zero hits of this branch across 320 real concurrent
			// inserts, because BEGIN IMMEDIATE blocks a second writer
			// inside its own BeginTx until the first commits. Do not
			// delete this branch as dead code.
			for attempt := 0; ; attempt++ {
				if attempt >= maxIDAttempts {
					return fmt.Errorf("AddItem: allocate child ordinal: exhausted %d attempts: %w", maxIDAttempts, ErrConflict)
				}
				var maxOrdinal sql.NullInt64
				if err := tx.QueryRowContext(ctx, `SELECT MAX(ordinal) FROM items WHERE parent_id = ?`, in.ParentID).Scan(&maxOrdinal); err != nil {
					return err
				}
				next := 1
				if maxOrdinal.Valid {
					next = int(maxOrdinal.Int64) + 1
				}
				ordinal = &next
				id = fmt.Sprintf("%s.%d", in.ParentID, next)
				execErr := insertItem(ctx, tx, id, &in.ParentID, ordinal, stage, itemType, in.Title, in.Body, in.Acceptance, in.Value, in.Confidence, in.Effort, score, in.Why, in.Repo, in.Paths, now)
				if execErr == nil {
					break
				}
				if isConstraintUnique(execErr) {
					continue
				}
				return execErr
			}

			// THE DUAL PARENT-CHILD WRITE. items.parent_id/ordinal (above)
			// are structural only -- the ready view never reads them. This
			// deps row is the only thing ready/blocked actually consult
			// for tree-shaped blocking propagation (02-RESEARCH.md Pattern
			// 3). Omitting it is the single highest-consequence miss in
			// this phase: it silently disables READY-02 with no error.
			if err := insertParentChildEdge(ctx, tx, id, in.ParentID, in.Actor, now); err != nil {
				return err
			}
		}

		for _, b := range in.Blocks {
			if err := insertEdge(ctx, tx, id, b, model.DepBlocks, in.Actor, now); err != nil {
				return err
			}
		}
		for _, d := range in.DerivedFrom {
			if err := insertEdge(ctx, tx, id, d, model.DepDerivedFrom, in.Actor, now); err != nil {
				return err
			}
		}

		out = model.Item{
			ID:         id,
			Ordinal:    ordinal,
			Stage:      stage,
			Type:       itemType,
			Title:      in.Title,
			Body:       in.Body,
			Acceptance: in.Acceptance,
			Status:     model.StatusOpen,
			Value:      in.Value,
			Confidence: in.Confidence,
			Effort:     in.Effort,
			Score:      score,
			Why:        in.Why,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if in.ParentID != "" {
			parentID := in.ParentID
			out.ParentID = &parentID
		}
		if in.Repo != "" {
			repo := in.Repo
			out.Repo = &repo
		}
		if in.Paths != "" {
			paths := in.Paths
			out.Paths = &paths
		}

		payload := addEventPayload{
			ParentID:    in.ParentID,
			Blocks:      in.Blocks,
			DerivedFrom: in.DerivedFrom,
		}
		return writeEvent(ctx, tx, id, "add", in.Actor, in.SessionID, payload)
	})
	if err != nil {
		return model.Item{}, err
	}
	return out, nil
}

// insertItem is the one INSERT INTO items statement AddItem uses for both
// the root and child cases -- parentID/ordinal are nil for a root. repo and
// paths are converted from "" to a driver nil so an unset flag stores SQL
// NULL rather than the empty string.
func insertItem(ctx context.Context, tx *sql.Tx, id string, parentID *string, ordinal *int, stage, itemType, title, body, acceptance string, value, confidence, effort *int, score int, why, repo, paths, now string) error {
	var repoVal, pathsVal any
	if repo != "" {
		repoVal = repo
	}
	if paths != "" {
		pathsVal = paths
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO items (
    id, parent_id, ordinal, stage, type, title, body, acceptance, status,
    value, confidence, effort, score, why, repo, paths,
    claimed_by_session, claimed_at, assignee, external_ref,
    created_at, updated_at, closed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL, ?, ?, NULL)`,
		id, parentID, ordinal, stage, itemType, title, body, acceptance,
		value, confidence, effort, score, why, repoVal, pathsVal,
		now, now,
	)
	return err
}
