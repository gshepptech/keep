package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gshepptech/keep/internal/model"
)

// ErrParentChildViaDep, ErrSelfEdge, and ErrInvalidDepType are CreateDep's
// three validation sentinels. Each is checked in Go, before any SQL runs,
// and each is wrapped with fmt.Errorf's %w so a caller can errors.Is against
// it (02-RESEARCH.md's classify-by-code rule applies to SQLite failures;
// these are pure input-validation failures that never reach the driver at
// all, so they get their own sentinels rather than a SQL CHECK's extended
// code).
var (
	// ErrParentChildViaDep is returned when CreateDep is asked to write a
	// parent-child edge directly. Settled here (02-RESEARCH.md Open Question
	// 1, recommendation (a), adopted by 02-CONTEXT.md/this plan):
	// `add --parent` is the single writer of parent-child structure --
	// insertParentChildEdge in items.go is its only call site -- so
	// items.parent_id and the deps tree can never disagree. `dep` must name
	// the correct command rather than fail obscurely on a CHECK violation.
	ErrParentChildViaDep = errors.New("parent-child structure is created by `keep add --parent <id>`; `dep` does not reparent items")

	// ErrSelfEdge is returned when ItemID == DependsOnID, for every dep type
	// including parent-child (DEP-02, self-edge half).
	ErrSelfEdge = errors.New("dep: an item cannot depend on itself")

	// ErrInvalidDepType is returned when Type is outside the closed
	// vocabulary blocks/parent-child/related/derived-from (DEP-01). Checked
	// against model.ValidDepTypes before any SQL runs -- the schema's own
	// CHECK constraint would also refuse an invalid type, but only with
	// extended code 275 and a driver message that does not name the
	// vocabulary a caller needs to see.
	ErrInvalidDepType = errors.New("dep: type must be one of blocks, parent-child, related, derived-from")
)

// DepInput is the argument to CreateDep. Actor is required, same rule as
// AddItemInput (items.go) -- EVENT-01 is unfalsifiable if actor can be
// silently empty.
//
// json tags (04-02): this struct doubles as the `dep` MCP tool's wire type
// directly (internal/mcp). SessionID is `json:"-"` for the same reason as
// AddItemInput's: session is a server-lifetime property, never a per-call
// argument. Tags are additive and behaviourally inert for Go callers, all of
// which construct this struct with named fields.
type DepInput struct {
	ItemID      string `json:"item_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	Actor       string `json:"actor,omitempty"`
	SessionID   string `json:"-"`
}

// depEventPayload is the JSON body of the one `dep` event CreateDep writes,
// naming the edge created. ItemID is not repeated in the payload -- it is
// already the event's own item_id column.
type depEventPayload struct {
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// CreateDep writes one deps row and one events row, inside a single
// transaction, for any of the three types `dep` is allowed to write directly
// (blocks, related, derived-from). parent-child is deliberately refused --
// see ErrParentChildViaDep -- because insertParentChildEdge (items.go),
// reached only through AddItem's `--parent` handling, is that edge type's
// single writer.
//
// Validation order, all before the transaction opens: Actor required, Type
// in the closed vocabulary, Type != parent-child, ItemID != DependsOnID.
// Inside the transaction: both referents must exist (ErrNotFound naming
// whichever is missing), then the edge insert, classifying a PRIMARY KEY
// collision (item_id, depends_on_id, type) as ErrConflict rather than
// letting the raw 1555 escape.
func (s *Store) CreateDep(ctx context.Context, in DepInput) error {
	if in.Actor == "" {
		return fmt.Errorf("CreateDep: Actor is required")
	}
	if !model.ValidDepTypes[in.Type] {
		return fmt.Errorf("CreateDep: %q: %w", in.Type, ErrInvalidDepType)
	}
	if in.Type == model.DepParentChild {
		return fmt.Errorf("CreateDep: %w", ErrParentChildViaDep)
	}
	if in.ItemID == in.DependsOnID {
		return fmt.Errorf("CreateDep: %q depends on itself: %w", in.ItemID, ErrSelfEdge)
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		now := nowFunc()

		ok, err := itemExists(ctx, tx, in.ItemID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("CreateDep: item %q: %w", in.ItemID, ErrNotFound)
		}
		ok, err = itemExists(ctx, tx, in.DependsOnID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("CreateDep: depends-on %q: %w", in.DependsOnID, ErrNotFound)
		}

		if err := insertEdge(ctx, tx, in.ItemID, in.DependsOnID, in.Type, in.Actor, now); err != nil {
			if isConstraintPrimaryKey(err) {
				return fmt.Errorf("CreateDep: edge %s %s %s already exists: %w", in.ItemID, in.Type, in.DependsOnID, ErrConflict)
			}
			return err
		}

		// The event lands on ItemID, not DependsOnID: the edge is a fact
		// about the dependent item's workability, which is what `show <a>`
		// (a later plan) will want to display alongside the item itself.
		payload := depEventPayload{DependsOnID: in.DependsOnID, Type: in.Type}
		return writeEvent(ctx, tx, in.ItemID, "dep", in.Actor, in.SessionID, payload)
	})
}
