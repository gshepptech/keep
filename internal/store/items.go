package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/rank"
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
//
// json tags (04-02): this struct doubles as the `add`/`capture` MCP tools'
// wire type directly (internal/mcp), which is why every field carries a
// snake_case tag. This changes nothing for Go callers -- every construction
// site in this module and cmd/keep uses named fields, which struct tags do
// not affect. Two tags are `json:"-"`, deliberately: SessionID is a
// server-lifetime property (one MCP server process serves one agent
// session), never a per-call argument, so it can never be supplied over the
// wire under any spelling. Stage is also `json:"-"` because the VERB chooses
// the stage, not an argument: `capture` pre-seeds Stage: model.StageRaw;
// `add` leaves it zero and AddItem defaults it to "ticket". If `stage` were
// wire-settable, `capture` -- whose whole contract is "this is raw" -- could
// be told to make a ticket, collapsing the two tools into one with a mode
// argument. This is an accepted CLI/MCP surface difference: the CLI's `add`
// has a `--stage` flag; the MCP `add` tool does not. An agent that wants a
// raw item calls `capture`, which is the verb for it.
type AddItemInput struct {
	Title       string   `json:"title"`
	Actor       string   `json:"actor,omitempty"`
	SessionID   string   `json:"-"`
	ParentID    string   `json:"parent_id,omitempty"`
	Type        string   `json:"type,omitempty"`
	Stage       string   `json:"-"`
	Blocks      []string `json:"blocks,omitempty"`
	DerivedFrom []string `json:"derived_from,omitempty"`
	Value       *int     `json:"value,omitempty"`
	Confidence  *int     `json:"confidence,omitempty"`
	Effort      *int     `json:"effort,omitempty"`
	Why         string   `json:"why,omitempty"`
	Acceptance  string   `json:"acceptance,omitempty"`
	Body        string   `json:"body,omitempty"`
	Repo        string   `json:"repo,omitempty"`
	Paths       string   `json:"paths,omitempty"`
	// Assignee is stored via the same "" -> SQL NULL convention insertItem
	// already uses for Repo/Paths (02-07: plan 07's `add --assignee` flag
	// needs somewhere to land; the assignee column has existed since Phase
	// 1's schema, and UpdateItemInput already supports setting it -- AddItem
	// lacking it was a gap, not a deliberate omission).
	Assignee string `json:"assignee,omitempty"`
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

// parentChildCycleCheckSQL is the recursive-CTE cycle guard, verified
// against a real 4-node tree (02-RESEARCH.md Pattern 4, positive and
// negative case both confirmed) -- used verbatim, not re-derived. :b (first
// placeholder) is the prospective PARENT, :a (second placeholder) the
// prospective CHILD: it asks "is the prospective child already an ancestor
// of the prospective parent?" -- if so, the candidate edge would close the
// loop. The argument order is the whole test; swapping it silently allows
// every cycle it exists to catch (this is this plan's GATE-03 break 1). The
// depth < 50 bound matches the ready view's own guard (schema.go) and stays
// for the same reason: the cost of being wrong is an infinite loop inside a
// user's shell.
const parentChildCycleCheckSQL = `
WITH RECURSIVE ancestors(id, depth) AS (
  SELECT depends_on_id, 1 FROM deps WHERE item_id = ? AND type = 'parent-child'
  UNION ALL
  SELECT d.depends_on_id, anc.depth + 1
  FROM ancestors anc
  JOIN deps d ON d.item_id = anc.id AND d.type = 'parent-child'
  WHERE anc.depth < 50
)
SELECT 1 FROM ancestors WHERE id = ? LIMIT 1`

// insertParentChildEdge writes the deps row that makes a parent-child
// relationship visible to the ready view. THE SHIPPED ready VIEW READS ONLY
// deps (WHERE type = 'parent-child'); IT NEVER CONSULTS items.parent_id
// (02-RESEARCH.md Pattern 3). AddItem sets items.parent_id/ordinal for id
// derivation and ordinal allocation ONLY -- omitting this row leaves
// items.parent_id looking correct while READY-02's blocking propagation
// silently does nothing for this child, with no error anywhere.
//
// Before writing, it runs parentChildCycleCheckSQL and returns ErrCycle,
// writing nothing, if childID is already an ancestor of parentID. This guard
// is reachable ONLY through add --parent today: CreateDep (deps.go) refuses
// to write parent-child edges directly (ErrParentChildViaDep), which is
// exactly what makes this the only call site that can ever place a
// parent-child row. If a reparent command is ever added, the guard must move
// with this helper, not be re-derived at a second call site. BEGIN IMMEDIATE
// (this Store's DSN) fully serializes writers, so there is no TOCTOU window
// between this check and the insert below -- both run inside the same
// transaction the caller's withTx provides.
func insertParentChildEdge(ctx context.Context, tx *sql.Tx, childID, parentID, actor, createdAt string) error {
	var found int
	err := tx.QueryRowContext(ctx, parentChildCycleCheckSQL, parentID, childID).Scan(&found)
	if err == nil {
		return fmt.Errorf("insertParentChildEdge: %s as parent of %s would close a parent-child cycle: %w", parentID, childID, ErrCycle)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	_, err = tx.ExecContext(ctx,
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

// ErrEmptyTitle is returned when a write would leave an item with nothing
// to call it by.
//
// The title is the only field every surface displays: `ready`, `list`,
// `search`, the board's rows, the MCP tools' output. An item without one is
// not a partially-filled record, it is a hole in the queue that can only be
// referred to by id -- and the id is deliberately an opaque handle nobody
// reads. Every other field on an item is optional, and this one cannot be.
//
// Guarded in the store rather than in each command because there are five
// places that build an AddItemInput -- add, capture, two MCP tools and the
// board -- and the rule should not be four-fifths true.
//
// Whitespace counts as empty. A title of "   " is exactly as unreadable as
// "" in every list that renders it, and accepting one while rejecting the
// other would be a distinction nobody could see.
var ErrEmptyTitle = errors.New("an item needs a title: it is the only field every list, search result and board row shows")

// ErrScoreOutOfRange is returned when a scoring input falls outside
// model.ScoreMin..ScoreMax.
//
// It is enforced HERE rather than at each surface because the surfaces do not
// all validate: cmd/keep's flag parser did, and the MCP tools did not, so an
// agent could file effort=100 and land the item below unscored work. The MCP
// schema now advertises the bounds, but the SDK does not validate arguments
// against InputSchema at all -- a published enum was confirmed ignored on a
// real call (internal/mcp/input.go) -- so the schema steers and this refuses.
var ErrScoreOutOfRange = fmt.Errorf("value, confidence and effort must each be %d-%d", model.ScoreMin, model.ScoreMax)

// checkScoreRange refuses any supplied scoring input outside the range. A nil
// input is "unscored" and always fine. Named per input so the message says
// which one to fix rather than that something was wrong.
func checkScoreRange(value, confidence, effort *int) error {
	for _, in := range []struct {
		name string
		v    *int
	}{{"value", value}, {"confidence", confidence}, {"effort", effort}} {
		if in.v == nil {
			continue
		}
		if *in.v < model.ScoreMin || *in.v > model.ScoreMax {
			return fmt.Errorf("%s is %d: %w", in.name, *in.v, ErrScoreOutOfRange)
		}
	}
	return nil
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
	if strings.TrimSpace(in.Title) == "" {
		return model.Item{}, fmt.Errorf("AddItem: %w", ErrEmptyTitle)
	}
	if err := checkScoreRange(in.Value, in.Confidence, in.Effort); err != nil {
		return model.Item{}, fmt.Errorf("AddItem: %w", err)
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
				execErr := insertItem(ctx, tx, id, nil, nil, stage, itemType, in.Title, in.Body, in.Acceptance, in.Value, in.Confidence, in.Effort, score, in.Why, in.Repo, in.Paths, in.Assignee, now)
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
				execErr := insertItem(ctx, tx, id, &in.ParentID, ordinal, stage, itemType, in.Title, in.Body, in.Acceptance, in.Value, in.Confidence, in.Effort, score, in.Why, in.Repo, in.Paths, in.Assignee, now)
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
		if in.Assignee != "" {
			assignee := in.Assignee
			out.Assignee = &assignee
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
// the root and child cases -- parentID/ordinal are nil for a root. repo,
// paths, and assignee are converted from "" to a driver nil so an unset
// flag stores SQL NULL rather than the empty string.
func insertItem(ctx context.Context, tx *sql.Tx, id string, parentID *string, ordinal *int, stage, itemType, title, body, acceptance string, value, confidence, effort *int, score int, why, repo, paths, assignee, now string) error {
	var repoVal, pathsVal, assigneeVal any
	if repo != "" {
		repoVal = repo
	}
	if paths != "" {
		pathsVal = paths
	}
	if assignee != "" {
		assigneeVal = assignee
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO items (
    id, parent_id, ordinal, stage, type, title, body, acceptance, status,
    value, confidence, effort, score, why, repo, paths,
    claimed_by_session, claimed_at, assignee, external_ref,
    created_at, updated_at, closed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, NULL, ?, ?, NULL)`,
		id, parentID, ordinal, stage, itemType, title, body, acceptance,
		value, confidence, effort, score, why, repoVal, pathsVal, assigneeVal,
		now, now,
	)
	return err
}
