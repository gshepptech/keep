package store

import (
	"context"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// typesOf returns the sequence of Type values in items, in order -- what
// TestBacklogGroupOrder compares against model.TypesOrdered.
func typesOf(items []model.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Type
	}
	return out
}

// TestBacklogOnlyRawNonDiscarded proves Backlog(ctx, "") returns every
// stage='raw' item whose status is not closed or dropped, and no
// ticket-stage item at all -- built through the real AddItem/CloseItem/
// DropItem, never raw SQL.
func TestBacklogOnlyRawNonDiscarded(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	open := mustAdd(t, s, AddItemInput{Title: "open raw", Actor: "tester", Stage: model.StageRaw})
	closedItem := mustAdd(t, s, AddItemInput{Title: "closed raw", Actor: "tester", Stage: model.StageRaw})
	dropped := mustAdd(t, s, AddItemInput{Title: "dropped raw", Actor: "tester", Stage: model.StageRaw})
	ticket := mustAdd(t, s, AddItemInput{Title: "an ordinary ticket", Actor: "tester"})

	if _, err := s.CloseItem(ctx, CloseItemInput{ID: closedItem.ID, Reason: "done", Actor: "tester"}); err != nil {
		t.Fatalf("CloseItem: %v", err)
	}
	if _, err := s.DropItem(ctx, DropItemInput{ID: dropped.ID, Reason: "no longer relevant", Actor: "tester"}); err != nil {
		t.Fatalf("DropItem: %v", err)
	}

	got, err := s.Backlog(ctx, "")
	if err != nil {
		t.Fatalf("Backlog(\"\"): %v", err)
	}
	wantIDs := sortedStrings([]string{open.ID})
	gotIDs := idsOfItems(got)
	if !equalStringSlices(gotIDs, wantIDs) {
		t.Fatalf("Backlog(\"\") ids = %v, want %v (closed/dropped/ticket excluded)", gotIDs, wantIDs)
	}
	for _, id := range []string{closedItem.ID, dropped.ID, ticket.ID} {
		for _, it := range got {
			if it.ID == id {
				t.Fatalf("Backlog(\"\") unexpectedly included %s", id)
			}
		}
	}
}

// TestBacklogInProgressRawIsPresent proves a raw item that is merely
// in_progress (not closed/dropped) is present.
func TestBacklogInProgressRawIsPresent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	inProgress := mustAdd(t, s, AddItemInput{Title: "in progress raw", Actor: "tester", Stage: model.StageRaw})
	execSQL(t, s, `UPDATE items SET status = ? WHERE id = ?`, model.StatusInProgress, inProgress.ID)

	got, err := s.Backlog(ctx, "")
	if err != nil {
		t.Fatalf("Backlog(\"\"): %v", err)
	}
	gotIDs := idsOfItems(got)
	if !equalStringSlices(gotIDs, []string{inProgress.ID}) {
		t.Fatalf("Backlog(\"\") ids = %v, want [%s] (in_progress raw item must be present)", gotIDs, inProgress.ID)
	}
}

// TestBacklogGroupOrder proves results are grouped by type in
// model.TypesOrdered order: one raw item of each of the five types,
// inserted in a deliberately scrambled order, comes back with a Type
// sequence exactly equal to model.TypesOrdered. The expected order is read
// from model.TypesOrdered itself, never re-typed here -- re-typing it would
// make this test and backlog.go's CASE two independent statements of the
// same order, defeating task 1's whole point.
func TestBacklogGroupOrder(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Deliberately scrambled insertion order: note, task, roadmap, bug, epic.
	mustAdd(t, s, AddItemInput{Title: "a note", Actor: "tester", Stage: model.StageRaw, Type: model.TypeNote})
	mustAdd(t, s, AddItemInput{Title: "a task", Actor: "tester", Stage: model.StageRaw, Type: model.TypeTask})
	mustAdd(t, s, AddItemInput{Title: "a roadmap", Actor: "tester", Stage: model.StageRaw, Type: model.TypeRoadmap})
	mustAdd(t, s, AddItemInput{Title: "a bug", Actor: "tester", Stage: model.StageRaw, Type: model.TypeBug})
	mustAdd(t, s, AddItemInput{Title: "an epic", Actor: "tester", Stage: model.StageRaw, Type: model.TypeEpic})

	got, err := s.Backlog(ctx, "")
	if err != nil {
		t.Fatalf("Backlog(\"\"): %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("Backlog(\"\") returned %d items, want 5: %v", len(got), got)
	}
	gotTypes := typesOf(got)
	if len(gotTypes) != len(model.TypesOrdered) {
		t.Fatalf("Backlog(\"\") type sequence = %v, want same length as model.TypesOrdered %v", gotTypes, model.TypesOrdered)
	}
	for i := range model.TypesOrdered {
		if gotTypes[i] != model.TypesOrdered[i] {
			t.Fatalf("Backlog(\"\") type sequence = %v, want %v (model.TypesOrdered)", gotTypes, model.TypesOrdered)
		}
	}
}

// TestBacklogNewestFirstWithinGroup proves, within one type's group, items
// come back created_at DESC.
func TestBacklogNewestFirstWithinGroup(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	orig := nowFunc
	t.Cleanup(func() { nowFunc = orig })

	nowFunc = func() string { return "2026-01-01T00:00:00.000000000Z" }
	first := mustAdd(t, s, AddItemInput{Title: "first note", Actor: "tester", Stage: model.StageRaw, Type: model.TypeNote})

	nowFunc = func() string { return "2026-01-02T00:00:00.000000000Z" }
	second := mustAdd(t, s, AddItemInput{Title: "second note", Actor: "tester", Stage: model.StageRaw, Type: model.TypeNote})

	got, err := s.Backlog(ctx, "note")
	if err != nil {
		t.Fatalf("Backlog(\"note\"): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Backlog(\"note\") returned %d items, want 2: %v", len(got), got)
	}
	if got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("Backlog(\"note\") ids = [%s, %s], want [%s, %s] (newest first)", got[0].ID, got[1].ID, second.ID, first.ID)
	}
}

// TestBacklogCreatedAtTieBrokenByIDAscending pins nowFunc so two items share
// an identical created_at, then proves the tie is broken by id ASC,
// deterministically on every run -- the exact reason cmd/keep/backlog.go's
// now-removed CLI-side grouping helper carried an ID-ascending tiebreak,
// now expressed in SQL.
func TestBacklogCreatedAtTieBrokenByIDAscending(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	orig := nowFunc
	t.Cleanup(func() { nowFunc = orig })
	const fixedNow = "2026-01-01T00:00:00.000000000Z"
	nowFunc = func() string { return fixedNow }

	a := mustAdd(t, s, AddItemInput{Title: "tied item alpha", Actor: "tester", Stage: model.StageRaw, Type: model.TypeNote})
	b := mustAdd(t, s, AddItemInput{Title: "tied item beta", Actor: "tester", Stage: model.StageRaw, Type: model.TypeNote})

	if a.CreatedAt != b.CreatedAt {
		t.Fatalf("fixture invalid: a.CreatedAt = %q, b.CreatedAt = %q, want equal (pinned nowFunc)", a.CreatedAt, b.CreatedAt)
	}

	wantFirst, wantSecond := a.ID, b.ID
	if wantFirst > wantSecond {
		wantFirst, wantSecond = wantSecond, wantFirst
	}

	got, err := s.Backlog(ctx, "note")
	if err != nil {
		t.Fatalf("Backlog(\"note\"): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Backlog(\"note\") returned %d items, want 2: %v", len(got), got)
	}
	if got[0].ID != wantFirst || got[1].ID != wantSecond {
		t.Fatalf("Backlog(\"note\") ids = [%s, %s], want [%s, %s] (id ASC tiebreak on a created_at tie)", got[0].ID, got[1].ID, wantFirst, wantSecond)
	}
}

// TestBacklogTypeFilter proves Backlog(ctx, "note") returns only note-type
// raw items, and an unknown filter value returns an empty result and no
// error.
func TestBacklogTypeFilter(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	note := mustAdd(t, s, AddItemInput{Title: "a note", Actor: "tester", Stage: model.StageRaw, Type: model.TypeNote})
	mustAdd(t, s, AddItemInput{Title: "a roadmap", Actor: "tester", Stage: model.StageRaw, Type: model.TypeRoadmap})

	got, err := s.Backlog(ctx, "note")
	if err != nil {
		t.Fatalf("Backlog(\"note\"): %v", err)
	}
	gotIDs := idsOfItems(got)
	if !equalStringSlices(gotIDs, []string{note.ID}) {
		t.Fatalf("Backlog(\"note\") ids = %v, want [%s]", gotIDs, note.ID)
	}

	gotNone, err := s.Backlog(ctx, "bogus-type")
	if err != nil {
		t.Fatalf("Backlog(\"bogus-type\"): %v, want no error for an unrecognized filter value", err)
	}
	if len(gotNone) != 0 {
		t.Fatalf("Backlog(\"bogus-type\") = %v, want empty (an unknown type matches nothing)", gotNone)
	}
}
