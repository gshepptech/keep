package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// populatedStore builds a store with every shape the export has to carry:
// nullable columns both set and null, a captured body held verbatim, a
// parent-child tree, all three writable dep types, and events of several
// kinds including the ones with payloads.
func populatedStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)

	roadmap := mustAdd(t, s, AddItemInput{
		Title: "Q3 roadmap", Actor: "tester", Stage: model.StageRaw, Type: model.TypeRoadmap,
		Body: "# Q3\n\nA body with\nnewlines, \"quotes\", a tab\there and unicode: café ☕\n",
	})
	epic := mustAdd(t, s, AddItemInput{
		Title: "Ship the store", Actor: "tester", Type: model.TypeEpic,
		Value: intPtr(8), Confidence: intPtr(7), Effort: intPtr(3),
		Why: "internal/store/schema.go:41", Acceptance: "the ready view exists",
		Repo: "keep", Paths: `["internal/store/schema.go"]`, Assignee: "someone",
		DerivedFrom: []string{roadmap.ID},
	})
	child := mustAdd(t, s, AddItemInput{Title: "a child", Actor: "tester", ParentID: epic.ID})
	blocker := mustAdd(t, s, AddItemInput{Title: "the blocker", Actor: "tester"})
	related := mustAdd(t, s, AddItemInput{Title: "related work", Actor: "tester"})

	if err := s.CreateDep(ctx, DepInput{ItemID: epic.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(blocks): %v", err)
	}
	if err := s.CreateDep(ctx, DepInput{ItemID: child.ID, DependsOnID: related.ID, Type: model.DepRelated, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(related): %v", err)
	}

	// Events with payloads, and an item in every status the store can hold.
	if _, err := s.StartItem(ctx, StartItemInput{ID: blocker.ID, Actor: "tester", SessionID: "sess-1"}); err != nil {
		t.Fatalf("StartItem: %v", err)
	}
	if _, err := s.CloseItem(ctx, CloseItemInput{ID: blocker.ID, Reason: "merged in #42", Actor: "tester", SessionID: "sess-1"}); err != nil {
		t.Fatalf("CloseItem: %v", err)
	}
	if _, err := s.DropItem(ctx, DropItemInput{ID: related.ID, Reason: "not worth it", Actor: "tester"}); err != nil {
		t.Fatalf("DropItem: %v", err)
	}
	newTitle := "a child, renamed"
	if _, err := s.UpdateItem(ctx, UpdateItemInput{ID: child.ID, Actor: "tester", Title: &newTitle}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if _, err := s.StartItem(ctx, StartItemInput{ID: child.ID, Actor: "tester", SessionID: "sess-2"}); err != nil {
		t.Fatalf("StartItem(child): %v", err)
	}
	return s
}

// asJSON renders one row for comparison. See the note at its call site for
// why == is the wrong tool here.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal for comparison: %v", err)
	}
	return string(b)
}

// dumpStore reads every row of every table into comparable Go values. It is
// how the round-trip is checked: not "the same number of items" but the same
// rows, field for field, nullable columns included.
func dumpStore(t *testing.T, s *Store) ([]model.Item, []model.Dep, []model.Event) {
	t.Helper()
	ctx := context.Background()
	items, err := s.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	deps, err := s.allDeps(ctx)
	if err != nil {
		t.Fatalf("allDeps: %v", err)
	}
	events, err := s.allEvents(ctx)
	if err != nil {
		t.Fatalf("allEvents: %v", err)
	}
	return items, deps, events
}

// TestExportImportRoundTripsEveryRow is the whole contract. Anything less
// than field-for-field equality means a store that came back from a backup
// is quietly not the store that went into it -- and the fields most likely
// to be dropped are exactly the ones nobody looks at until they matter:
// why, acceptance, paths, external_ref, claimed_at.
func TestExportImportRoundTripsEveryRow(t *testing.T) {
	ctx := context.Background()
	src := populatedStore(t)

	var buf bytes.Buffer
	header, err := src.Export(ctx, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	wantItems, wantDeps, wantEvents := dumpStore(t, src)
	if header.Items != len(wantItems) || header.Deps != len(wantDeps) || header.Events != len(wantEvents) {
		t.Fatalf("header says %d/%d/%d items/deps/events, store holds %d/%d/%d",
			header.Items, header.Deps, header.Events, len(wantItems), len(wantDeps), len(wantEvents))
	}
	if len(wantItems) == 0 || len(wantDeps) == 0 || len(wantEvents) == 0 {
		t.Fatal("the fixture is empty; this test would pass without exporting anything")
	}

	dst, err := Init(filepath.Join(t.TempDir(), "restored.sqlite"))
	if err != nil {
		t.Fatalf("Init(dst): %v", err)
	}
	defer dst.Close()

	summary, err := dst.Import(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Items != len(wantItems) || summary.Deps != len(wantDeps) || summary.Events != len(wantEvents) {
		t.Fatalf("imported %d/%d/%d, exported %d/%d/%d",
			summary.Items, summary.Deps, summary.Events, len(wantItems), len(wantDeps), len(wantEvents))
	}

	gotItems, gotDeps, gotEvents := dumpStore(t, dst)

	if len(gotItems) != len(wantItems) {
		t.Fatalf("restored %d items, want %d", len(gotItems), len(wantItems))
	}
	// Compared as JSON, not with ==. model.Item has ten pointer fields, and
	// == on a struct compares those pointers, so a struct-equality check
	// here would fail on two identical rows and pass on nothing -- verified
	// by writing it that way first and watching every row "differ" with
	// byte-identical contents.
	for i := range wantItems {
		if w, g := asJSON(t, wantItems[i]), asJSON(t, gotItems[i]); w != g {
			t.Errorf("item %d differs.\nwant %s\ngot  %s", i, w, g)
		}
	}
	for i := range wantDeps {
		if w, g := asJSON(t, wantDeps[i]), asJSON(t, gotDeps[i]); w != g {
			t.Errorf("dep %d differs.\nwant %s\ngot  %s", i, w, g)
		}
	}
	for i := range wantEvents {
		if w, g := asJSON(t, wantEvents[i]), asJSON(t, gotEvents[i]); w != g {
			t.Errorf("event %d differs.\nwant %s\ngot  %s", i, w, g)
		}
	}
}

// TestRestoredStoreAnswersTheSameQueries. Equal rows is necessary and not
// sufficient: the ready view is computed from deps, and an import that wrote
// items faithfully while dropping a parent-child edge would pass the row
// comparison and hand back a store whose queue is wrong.
func TestRestoredStoreAnswersTheSameQueries(t *testing.T) {
	ctx := context.Background()
	src := populatedStore(t)

	var buf bytes.Buffer
	if _, err := src.Export(ctx, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	dst, err := Init(filepath.Join(t.TempDir(), "restored.sqlite"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer dst.Close()
	if _, err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Import: %v", err)
	}

	wantReady, err := src.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready(src): %v", err)
	}
	gotReady, err := dst.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready(dst): %v", err)
	}
	if len(gotReady) != len(wantReady) {
		t.Fatalf("restored ready has %d rows, original has %d", len(gotReady), len(wantReady))
	}
	for i := range wantReady {
		if gotReady[i].ID != wantReady[i].ID {
			t.Errorf("ready row %d = %s, want %s", i, gotReady[i].ID, wantReady[i].ID)
		}
	}

	wantBlocked, err := src.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked(src): %v", err)
	}
	gotBlocked, err := dst.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked(dst): %v", err)
	}
	if len(gotBlocked) != len(wantBlocked) {
		t.Fatalf("restored blocked has %d rows, original has %d -- a dropped dep edge shows up here and nowhere else", len(gotBlocked), len(wantBlocked))
	}
	for i := range wantBlocked {
		if gotBlocked[i].ID != wantBlocked[i].ID || gotBlocked[i].BlockedBy != wantBlocked[i].BlockedBy {
			t.Errorf("blocked row %d = %s blocked by %s, want %s blocked by %s",
				i, gotBlocked[i].ID, gotBlocked[i].BlockedBy, wantBlocked[i].ID, wantBlocked[i].BlockedBy)
		}
	}
}

// TestExportIsDeterministic: two exports of an unchanged store must be
// byte-identical apart from the timestamp in the header, so a diff between
// two exports says something happened rather than that the rows came back
// in a different order.
func TestExportIsDeterministic(t *testing.T) {
	ctx := context.Background()
	s := populatedStore(t)

	var a, b bytes.Buffer
	if _, err := s.Export(ctx, &a); err != nil {
		t.Fatalf("Export(a): %v", err)
	}
	if _, err := s.Export(ctx, &b); err != nil {
		t.Fatalf("Export(b): %v", err)
	}

	// Drop the header line from each; it carries exported_at.
	bodyA := a.String()[strings.Index(a.String(), "\n")+1:]
	bodyB := b.String()[strings.Index(b.String(), "\n")+1:]
	if bodyA != bodyB {
		t.Error("two exports of an unchanged store differ; the row ordering is not stable")
	}
}

// TestImportRefusesANonEmptyStore. Merging two stores means resolving id
// collisions, and there is no answer to that which is not a sync product.
// Refusing is the boundary, not a limitation waiting to be relaxed.
func TestImportRefusesANonEmptyStore(t *testing.T) {
	ctx := context.Background()
	src := populatedStore(t)
	var buf bytes.Buffer
	if _, err := src.Export(ctx, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}

	dst := openTestStore(t)
	existing := mustAdd(t, dst, AddItemInput{Title: "already here", Actor: "tester"})

	if _, err := dst.Import(ctx, bytes.NewReader(buf.Bytes())); !errors.Is(err, ErrStoreNotEmpty) {
		t.Fatalf("Import into a non-empty store = %v, want ErrStoreNotEmpty", err)
	}

	items, err := dst.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != existing.ID {
		t.Fatalf("a refused import changed the store: %d items %v", len(items), items)
	}
}

// TestImportRejectsWhatItCannotFaithfullyRestore covers the three ways an
// input can be wrong. Each must refuse rather than import what it
// understands and drop the rest, because a partially-restored store looks
// like a complete one.
func TestImportRejectsWhatItCannotFaithfullyRestore(t *testing.T) {
	ctx := context.Background()
	src := populatedStore(t)
	var buf bytes.Buffer
	if _, err := src.Export(ctx, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	good := buf.String()
	lines := strings.Split(strings.TrimRight(good, "\n"), "\n")

	cases := []struct {
		name  string
		input string
		want  error
	}{
		{
			name:  "no header",
			input: strings.Join(lines[1:], "\n") + "\n",
			want:  ErrBadExport,
		},
		{
			name:  "a record kind this binary does not know",
			input: lines[0] + "\n" + `{"kind":"attachment","blob":"..."}` + "\n",
			want:  ErrBadExport,
		},
		{
			name:  "a newer file format",
			input: `{"kind":"header","format":99,"schema_version":1}` + "\n",
			want:  ErrBadExport,
		},
		{
			name:  "taken from a store at a different schema_version",
			input: `{"kind":"header","format":1,"schema_version":99}` + "\n",
			want:  ErrSchemaVersion,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst, err := Init(filepath.Join(t.TempDir(), "dst.sqlite"))
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			defer dst.Close()

			if _, err := dst.Import(ctx, strings.NewReader(tc.input)); !errors.Is(err, tc.want) {
				t.Fatalf("Import = %v, want %v", err, tc.want)
			}

			items, err := dst.ListItems(ctx, ListFilter{})
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			if len(items) != 0 {
				t.Errorf("a refused import left %d items behind; it must be all or nothing", len(items))
			}
		})
	}
}

// TestImportIsAtomic: a file that is valid for a hundred lines and corrupt
// on the hundred-and-first must leave the store empty, not holding the first
// hundred. That store would open, answer queries, and be missing rows
// nothing would ever tell you about.
func TestImportIsAtomic(t *testing.T) {
	ctx := context.Background()
	src := populatedStore(t)
	var buf bytes.Buffer
	if _, err := src.Export(ctx, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}

	truncated := buf.String() + `{"kind":"item","item":{"id":` + "\n"

	dst, err := Init(filepath.Join(t.TempDir(), "dst.sqlite"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer dst.Close()

	if _, err := dst.Import(ctx, strings.NewReader(truncated)); err == nil {
		t.Fatal("Import of a file with a corrupt final line = nil, want an error")
	}

	items, err := dst.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("a failed import left %d items in the store; the whole file must land in one transaction", len(items))
	}
}
