package store

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/model"
)

// ExportFormat is the value every export header carries. It versions the
// FILE, separately from schema_version which versions the DATABASE, because
// the two change for different reasons: a new column bumps the schema, a
// change to how records are framed on a line bumps this. Conflating them
// would make either change break both readers.
const ExportFormat = 1

// Record kinds, one per table. `meta` is deliberately absent: the header
// already carries schema_version, and re-importing a meta table over a store
// Init has just written would let a file overwrite the version of the store
// reading it -- which is how an import turns into a corruption.
const (
	recordHeader = "header"
	recordItem   = "item"
	recordDep    = "dep"
	recordEvent  = "event"
)

// ExportHeader is the first line of every export. It is a separate record
// rather than a wrapping object so the format stays streamable: a store with
// a hundred thousand events must not have to be held in memory, at either
// end, to be read or written.
type ExportHeader struct {
	Kind          string `json:"kind"`
	Format        int    `json:"format"`
	SchemaVersion int    `json:"schema_version"`
	ExportedAt    string `json:"exported_at"`
	Items         int    `json:"items"`
	Deps          int    `json:"deps"`
	Events        int    `json:"events"`
}

// itemRecord, depRecord and eventRecord tag each row with its table so a
// reader needs no positional assumptions and a file stays readable when a
// future format appends a fourth kind.
type itemRecord struct {
	Kind string     `json:"kind"`
	Item model.Item `json:"item"`
}

type depRecord struct {
	Kind string    `json:"kind"`
	Dep  model.Dep `json:"dep"`
}

type eventRecord struct {
	Kind  string      `json:"kind"`
	Event model.Event `json:"event"`
}

// kindOnly reads just the kind off a line, so Import can dispatch without
// unmarshalling into the wrong shape first.
type kindOnly struct {
	Kind string `json:"kind"`
}

// Export writes the whole store to w as JSONL: one header line, then one
// line per item, dep and event.
//
// # What this is, and what it deliberately is not
//
// It is a round-trip. Export a store, import it into an empty one, and the
// two hold the same rows -- that is the entire contract, and
// TestExportImportRoundTripsEveryRow is what holds it.
//
// It is NOT sync. There is no merge, no conflict resolution, no
// three-way anything, and Import refuses a store that already has items in
// it rather than guessing what to do about a collision. Sharing a live store
// between machines stays out of scope; this covers the three things people
// actually need: backing a store up, moving one between machines, and
// reading what is in one without this binary.
//
// Ordering is deterministic -- items by id, deps by their primary key,
// events by their autoincrement id -- so two exports of an unchanged store
// are byte-identical and a diff between two exports means something.
//
// Events keep their ids. They are an append-only audit log whose ids are
// referenced by nothing, and preserving them keeps the imported log in the
// order it happened rather than in whatever order a fresh AUTOINCREMENT
// would assign.
func (s *Store) Export(ctx context.Context, w io.Writer) (ExportHeader, error) {
	items, err := s.ListItems(ctx, ListFilter{})
	if err != nil {
		return ExportHeader{}, fmt.Errorf("Export: read items: %w", err)
	}
	deps, err := s.allDeps(ctx)
	if err != nil {
		return ExportHeader{}, err
	}
	events, err := s.allEvents(ctx)
	if err != nil {
		return ExportHeader{}, err
	}

	version, err := storedSchemaVersion(ctx, s.db)
	if err != nil {
		return ExportHeader{}, err
	}

	header := ExportHeader{
		Kind:          recordHeader,
		Format:        ExportFormat,
		SchemaVersion: version,
		ExportedAt:    nowFunc(),
		Items:         len(items),
		Deps:          len(deps),
		Events:        len(events),
	}

	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	if err := enc.Encode(header); err != nil {
		return ExportHeader{}, fmt.Errorf("Export: write header: %w", err)
	}
	for _, it := range items {
		if err := enc.Encode(itemRecord{Kind: recordItem, Item: it}); err != nil {
			return ExportHeader{}, fmt.Errorf("Export: write item %s: %w", it.ID, err)
		}
	}
	for _, d := range deps {
		if err := enc.Encode(depRecord{Kind: recordDep, Dep: d}); err != nil {
			return ExportHeader{}, fmt.Errorf("Export: write dep %s->%s: %w", d.ItemID, d.DependsOnID, err)
		}
	}
	for _, e := range events {
		if err := enc.Encode(eventRecord{Kind: recordEvent, Event: e}); err != nil {
			return ExportHeader{}, fmt.Errorf("Export: write event %d: %w", e.ID, err)
		}
	}
	if err := bw.Flush(); err != nil {
		return ExportHeader{}, fmt.Errorf("Export: flush: %w", err)
	}
	return header, nil
}

// allDeps reads every deps row, ordered by its primary key so the output is
// stable.
func (s *Store) allDeps(ctx context.Context) ([]model.Dep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT item_id, depends_on_id, type, created_at, created_by FROM deps
		 ORDER BY item_id, depends_on_id, type`)
	if err != nil {
		return nil, fmt.Errorf("Export: read deps: %w", err)
	}
	defer rows.Close()

	out := []model.Dep{}
	for rows.Next() {
		var d model.Dep
		if err := rows.Scan(&d.ItemID, &d.DependsOnID, &d.Type, &d.CreatedAt, &d.CreatedBy); err != nil {
			return nil, fmt.Errorf("Export: scan dep: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// allEvents reads every events row in id order, which is the order they
// happened.
func (s *Store) allEvents(ctx context.Context) ([]model.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, item_id, kind, actor, session_id, at, payload FROM events ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("Export: read events: %w", err)
	}
	defer rows.Close()

	out := []model.Event{}
	for rows.Next() {
		var e model.Event
		if err := rows.Scan(&e.ID, &e.ItemID, &e.Kind, &e.Actor, &e.SessionID, &e.At, &e.Payload); err != nil {
			return nil, fmt.Errorf("Export: scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ImportSummary is what Import managed to write.
type ImportSummary struct {
	Items  int `json:"items"`
	Deps   int `json:"deps"`
	Events int `json:"events"`
}

// Import reads an export produced by Export into this store.
//
// # It refuses a store that is not empty
//
// Merging two stores means deciding what to do when both contain an item
// with the same id and different bodies, and there is no answer to that
// which is not a sync product. Refusing is not a limitation to be lifted
// later by relaxing this check -- it is the boundary that keeps this a
// round-trip. See ErrStoreNotEmpty.
//
// # Everything lands in one transaction
//
// A half-imported store is worse than a failed import: it looks like a
// working store and is missing rows nothing will tell you about. Foreign
// keys are deferred for the duration -- items reference their own parents
// and deps reference items, so no ordering of the input can satisfy every
// constraint row by row, and an import that required one would reject valid
// exports of valid stores.
//
// The rows are written EXACTLY as they appear, through plain INSERTs.
// Import does not call AddItem or CreateDep: those allocate ids, derive
// scores, write events and enforce transitions, all of which would rewrite
// the very data being restored. This is the one place in the package that
// writes an items row without going through the item lifecycle, and it is
// correct here for the same reason it would be wrong anywhere else -- the
// lifecycle already ran, on the machine this file came from.
func (s *Store) Import(ctx context.Context, r io.Reader) (ImportSummary, error) {
	empty, err := s.isEmpty(ctx)
	if err != nil {
		return ImportSummary{}, err
	}
	if !empty {
		return ImportSummary{}, fmt.Errorf("Import: this store already holds items; import writes into an empty store only, because merging two stores means resolving collisions and that is a sync product keep is not: %w", ErrStoreNotEmpty)
	}

	version, err := storedSchemaVersion(ctx, s.db)
	if err != nil {
		return ImportSummary{}, err
	}

	var summary ImportSummary
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		// Deferred rather than off: an import that referenced a parent no
		// line ever defined would still be caught, at COMMIT, instead of
		// producing a store with a dangling parent_id.
		if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
			return fmt.Errorf("Import: defer foreign keys: %w", err)
		}

		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), maxExportLine)

		line := 0
		seenHeader := false
		for sc.Scan() {
			line++
			raw := sc.Bytes()
			if len(raw) == 0 {
				continue
			}
			var k kindOnly
			if err := json.Unmarshal(raw, &k); err != nil {
				return fmt.Errorf("Import: line %d is not JSON: %w", line, err)
			}

			switch k.Kind {
			case recordHeader:
				var h ExportHeader
				if err := json.Unmarshal(raw, &h); err != nil {
					return fmt.Errorf("Import: line %d: %w", line, err)
				}
				if err := checkHeader(h, version); err != nil {
					return err
				}
				seenHeader = true
			case recordItem:
				var rec itemRecord
				if err := json.Unmarshal(raw, &rec); err != nil {
					return fmt.Errorf("Import: line %d: %w", line, err)
				}
				if err := insertItemRow(ctx, tx, rec.Item); err != nil {
					return fmt.Errorf("Import: line %d, item %s: %w", line, rec.Item.ID, err)
				}
				summary.Items++
			case recordDep:
				var rec depRecord
				if err := json.Unmarshal(raw, &rec); err != nil {
					return fmt.Errorf("Import: line %d: %w", line, err)
				}
				if err := insertDepRow(ctx, tx, rec.Dep); err != nil {
					return fmt.Errorf("Import: line %d, dep %s->%s: %w", line, rec.Dep.ItemID, rec.Dep.DependsOnID, err)
				}
				summary.Deps++
			case recordEvent:
				var rec eventRecord
				if err := json.Unmarshal(raw, &rec); err != nil {
					return fmt.Errorf("Import: line %d: %w", line, err)
				}
				if err := insertEventRow(ctx, tx, rec.Event); err != nil {
					return fmt.Errorf("Import: line %d, event %d: %w", line, rec.Event.ID, err)
				}
				summary.Events++
			default:
				// An unknown kind is refused, not skipped. Skipping would
				// silently drop whatever a newer keep wrote, and the
				// resulting store would look complete.
				return fmt.Errorf("Import: line %d: unknown record kind %q: %w", line, k.Kind, ErrBadExport)
			}
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("Import: read: %w", err)
		}
		if !seenHeader {
			return fmt.Errorf("Import: no header record; this is not a keep export: %w", ErrBadExport)
		}
		return nil
	})
	if err != nil {
		return ImportSummary{}, err
	}
	return summary, nil
}

// maxExportLine bounds one JSONL line. A captured roadmap lives in
// items.body verbatim and can easily run to tens of kilobytes, so the
// bound has to be generous; 16MB is far past any document a person pastes
// and far short of exhausting memory on a line that is actually corrupt.
const maxExportLine = 16 << 20

// checkHeader refuses a file this binary cannot faithfully restore.
func checkHeader(h ExportHeader, storeVersion int) error {
	if h.Format != ExportFormat {
		return fmt.Errorf("Import: export format %d, this keep reads %d: %w", h.Format, ExportFormat, ErrBadExport)
	}
	if h.SchemaVersion != storeVersion {
		return fmt.Errorf("Import: export was taken from a store at schema_version %d and this store is at %d -- import into a store of the matching version, or migrate the source store before exporting: %w", h.SchemaVersion, storeVersion, ErrSchemaVersion)
	}
	return nil
}

func insertItemRow(ctx context.Context, tx *sql.Tx, it model.Item) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO items (id, parent_id, ordinal, stage, type, title, body, acceptance, status,
                   value, confidence, effort, score, why, repo, paths,
                   claimed_by_session, claimed_at, assignee, external_ref,
                   created_at, updated_at, closed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ID, it.ParentID, it.Ordinal, it.Stage, it.Type, it.Title, it.Body, it.Acceptance, it.Status,
		it.Value, it.Confidence, it.Effort, it.Score, it.Why, it.Repo, it.Paths,
		it.ClaimedBySession, it.ClaimedAt, it.Assignee, it.ExternalRef,
		it.CreatedAt, it.UpdatedAt, it.ClosedAt)
	return err
}

func insertDepRow(ctx context.Context, tx *sql.Tx, d model.Dep) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO deps (item_id, depends_on_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		d.ItemID, d.DependsOnID, d.Type, d.CreatedAt, d.CreatedBy)
	return err
}

func insertEventRow(ctx context.Context, tx *sql.Tx, e model.Event) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO events (id, item_id, kind, actor, session_id, at, payload) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ItemID, e.Kind, e.Actor, e.SessionID, e.At, e.Payload)
	return err
}

// isEmpty reports whether the store holds no items. Deps and events cannot
// exist without an item to hang off, so items alone is the whole question.
func (s *Store) isEmpty(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM items").Scan(&n); err != nil {
		return false, fmt.Errorf("Import: count items: %w", err)
	}
	return n == 0, nil
}
