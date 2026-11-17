package store

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// idsOfItems returns the sorted ids of items, for table-test comparisons
// that must catch "the wrong three of five rows" rather than just a count.
func idsOfItems(items []model.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	sort.Strings(out)
	return out
}

func sortedStrings(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsDep reports whether deps contains an edge exactly matching
// itemID/dependsOnID/typ.
func containsDep(deps []model.Dep, itemID, dependsOnID, typ string) bool {
	for _, d := range deps {
		if d.ItemID == itemID && d.DependsOnID == dependsOnID && d.Type == typ {
			return true
		}
	}
	return false
}

// TestGetItemBothDependencyDirections proves each edge appears on exactly
// the right side of exactly the right item, including the negative case: the
// parent's DependsOn must NOT contain the child's own parent-child edge. A
// test that only checks presence would pass even if both directions were
// populated from the same query.
func TestGetItemBothDependencyDirections(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	parent := mustAdd(t, s, AddItemInput{Title: "parent", Actor: "tester"})
	child := mustAdd(t, s, AddItemInput{Title: "child", Actor: "tester", ParentID: parent.ID})
	blockTarget := mustAdd(t, s, AddItemInput{Title: "block-target", Actor: "tester"})
	derivedFrom := mustAdd(t, s, AddItemInput{Title: "derived-from-source", Actor: "tester"})

	if err := s.CreateDep(ctx, DepInput{ItemID: child.ID, DependsOnID: blockTarget.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(blocks): %v", err)
	}
	if err := s.CreateDep(ctx, DepInput{ItemID: child.ID, DependsOnID: derivedFrom.ID, Type: model.DepDerivedFrom, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(derived-from): %v", err)
	}

	childDetail, err := s.GetItem(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetItem(child): %v", err)
	}
	if !containsDep(childDetail.DependsOn, child.ID, parent.ID, model.DepParentChild) {
		t.Fatalf("child.DependsOn = %+v, want it to contain the parent-child edge to %s", childDetail.DependsOn, parent.ID)
	}
	if !containsDep(childDetail.DependsOn, child.ID, blockTarget.ID, model.DepBlocks) {
		t.Fatalf("child.DependsOn = %+v, want it to contain the blocks edge to %s", childDetail.DependsOn, blockTarget.ID)
	}
	if !containsDep(childDetail.DependsOn, child.ID, derivedFrom.ID, model.DepDerivedFrom) {
		t.Fatalf("child.DependsOn = %+v, want it to contain the derived-from edge to %s", childDetail.DependsOn, derivedFrom.ID)
	}
	if len(childDetail.DependedOnBy) != 0 {
		t.Fatalf("child.DependedOnBy = %+v, want empty -- nothing depends on child", childDetail.DependedOnBy)
	}

	parentDetail, err := s.GetItem(ctx, parent.ID)
	if err != nil {
		t.Fatalf("GetItem(parent): %v", err)
	}
	if !containsDep(parentDetail.DependedOnBy, child.ID, parent.ID, model.DepParentChild) {
		t.Fatalf("parent.DependedOnBy = %+v, want it to contain child's parent-child edge", parentDetail.DependedOnBy)
	}
	// The negative assertion: the parent-child edge is item_id=child,
	// depends_on_id=parent, so it must not also appear in the parent's own
	// DependsOn (that would mean the two directions were populated from the
	// same query, or the direction predicate got swapped).
	if containsDep(parentDetail.DependsOn, child.ID, parent.ID, model.DepParentChild) {
		t.Fatalf("parent.DependsOn = %+v, must NOT contain the child's parent-child edge", parentDetail.DependsOn)
	}
	if len(parentDetail.DependsOn) != 0 {
		t.Fatalf("parent.DependsOn = %+v, want empty -- parent has no items.parent_id and depends on nothing", parentDetail.DependsOn)
	}
}

// TestGetItemEventHistoryOldestFirst proves three mutations against one item
// produce three events in the order they happened: oldest first, by id.
func TestGetItemEventHistoryOldestFirst(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	item := mustAdd(t, s, AddItemInput{Title: "item", Actor: "tester"}) // event 1: add
	target1 := mustAdd(t, s, AddItemInput{Title: "target1", Actor: "tester"})
	target2 := mustAdd(t, s, AddItemInput{Title: "target2", Actor: "tester"})

	if err := s.CreateDep(ctx, DepInput{ItemID: item.ID, DependsOnID: target1.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(1): %v", err) // event 2: dep
	}
	if err := s.CreateDep(ctx, DepInput{ItemID: item.ID, DependsOnID: target2.ID, Type: model.DepRelated, Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep(2): %v", err) // event 3: dep
	}

	detail, err := s.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(detail.Events) != 3 {
		t.Fatalf("GetItem(item).Events has %d entries, want exactly 3: %+v", len(detail.Events), detail.Events)
	}
	if detail.Events[0].Kind != "add" {
		t.Fatalf("Events[0].Kind = %q, want %q (the add event happened first)", detail.Events[0].Kind, "add")
	}
	if detail.Events[1].Kind != "dep" || detail.Events[2].Kind != "dep" {
		t.Fatalf("Events[1:3].Kind = %q, %q, want both %q", detail.Events[1].Kind, detail.Events[2].Kind, "dep")
	}
	if detail.Events[0].ID >= detail.Events[1].ID || detail.Events[1].ID >= detail.Events[2].ID {
		t.Fatalf("Events ids = %d, %d, %d, want strictly increasing", detail.Events[0].ID, detail.Events[1].ID, detail.Events[2].ID)
	}
}

// TestGetItemNotFound proves an unknown id returns ErrNotFound via
// errors.Is, not a zero ItemDetail with a nil error.
func TestGetItemNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, err := s.GetItem(ctx, "rnc-000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem(unknown id) = %v, want ErrNotFound", err)
	}
}

// TestGetItemBodyVerbatim proves a multi-line body with trailing whitespace
// and a UTF-8 character round-trips byte-identically. `capture` and
// `show --body` depend on this holding.
func TestGetItemBodyVerbatim(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	body := "first line\nsecond line with trailing spaces   \nthird line with a café\n"
	item := mustAdd(t, s, AddItemInput{Title: "verbatim-body", Actor: "tester", Body: body})

	detail, err := s.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if detail.Body != body {
		t.Fatalf("GetItem(item).Body = %q, want %q (byte-identical round-trip)", detail.Body, body)
	}
}

// listFiltersFixture is the shared five-item fixture TestListItemsFilters'
// table runs against, built once.
type listFiltersFixture struct {
	// stage=raw, type=task, status=open (AddItem always inserts status='open')
	raw          model.Item
	epic         model.Item // stage=ticket, type=epic, status=open
	closedTicket model.Item // stage=ticket, type=task, status=closed, written with execSQL
	child        model.Item // stage=ticket, type=task, parent=epic, status=open
	repoTicket   model.Item // stage=ticket, type=bug, repo="myrepo", status=open
}

func buildListFiltersFixture(t *testing.T, s *Store) listFiltersFixture {
	t.Helper()
	f := listFiltersFixture{}
	f.raw = mustAdd(t, s, AddItemInput{Title: "raw-item", Actor: "tester", Stage: model.StageRaw, Type: model.TypeTask})
	f.epic = mustAdd(t, s, AddItemInput{Title: "epic-item", Actor: "tester", Type: model.TypeEpic})
	f.closedTicket = mustAdd(t, s, AddItemInput{Title: "closed-item", Actor: "tester", Type: model.TypeTask})
	execSQL(t, s, `UPDATE items SET status = ? WHERE id = ?`, model.StatusClosed, f.closedTicket.ID)
	f.child = mustAdd(t, s, AddItemInput{Title: "child-item", Actor: "tester", Type: model.TypeTask, ParentID: f.epic.ID})
	f.repoTicket = mustAdd(t, s, AddItemInput{Title: "repo-item", Actor: "tester", Type: model.TypeBug, Repo: "myrepo"})
	return f
}

// TestListItemsFilters is a table test over each single filter and at least
// two AND combinations, asserting exact sorted id sets. A count assertion
// alone would pass when a filter returns the wrong three of five rows.
func TestListItemsFilters(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	f := buildListFiltersFixture(t, s)

	allIDs := []string{f.raw.ID, f.epic.ID, f.closedTicket.ID, f.child.ID, f.repoTicket.ID}

	cases := []struct {
		name    string
		filter  ListFilter
		wantIDs []string
	}{
		{"no filter returns all five", ListFilter{}, allIDs},
		{"stage=ticket excludes raw", ListFilter{Stage: model.StageTicket}, []string{f.epic.ID, f.closedTicket.ID, f.child.ID, f.repoTicket.ID}},
		{"stage=raw returns only raw", ListFilter{Stage: model.StageRaw}, []string{f.raw.ID}},
		{"status=open excludes closed", ListFilter{Status: model.StatusOpen}, []string{f.raw.ID, f.epic.ID, f.child.ID, f.repoTicket.ID}},
		{"status=closed returns only closed", ListFilter{Status: model.StatusClosed}, []string{f.closedTicket.ID}},
		{"type=epic returns only the epic", ListFilter{Type: model.TypeEpic}, []string{f.epic.ID}},
		{"type=bug returns only the repo item", ListFilter{Type: model.TypeBug}, []string{f.repoTicket.ID}},
		{"parent=epic returns only the child", ListFilter{ParentID: f.epic.ID}, []string{f.child.ID}},
		{"repo=myrepo returns only the repo item", ListFilter{Repo: "myrepo"}, []string{f.repoTicket.ID}},
		{"AND: stage=ticket + status=open excludes raw and closed", ListFilter{Stage: model.StageTicket, Status: model.StatusOpen}, []string{f.epic.ID, f.child.ID, f.repoTicket.ID}},
		{"AND: stage=ticket + type=epic returns only the epic", ListFilter{Stage: model.StageTicket, Type: model.TypeEpic}, []string{f.epic.ID}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListItems(ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListItems(%+v): %v", tc.filter, err)
			}
			gotIDs := idsOfItems(got)
			wantIDs := sortedStrings(tc.wantIDs)
			if !equalStringSlices(gotIDs, wantIDs) {
				t.Fatalf("ListItems(%+v) ids = %v, want %v", tc.filter, gotIDs, wantIDs)
			}
		})
	}
}

// TestListItemsExcludeClosed proves ListFilter.ExcludeClosed's scope: given
// four raw items, one each in status open/in_progress/closed/dropped (the
// latter two set through the real CloseItem/DropItem methods, not raw SQL,
// so the test exercises the same rows the CLI will), ExcludeClosed: true
// returns exactly the open and in_progress items, the zero value (unset)
// still returns all four, and ExcludeClosed composes with Type/Stage as AND
// like every other ListFilter field.
func TestListItemsExcludeClosed(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	open := mustAdd(t, s, AddItemInput{Title: "open-raw", Actor: "tester", Stage: model.StageRaw})
	inProgress := mustAdd(t, s, AddItemInput{Title: "in-progress-raw", Actor: "tester", Stage: model.StageRaw})
	closedItem := mustAdd(t, s, AddItemInput{Title: "closed-raw", Actor: "tester", Stage: model.StageRaw})
	dropped := mustAdd(t, s, AddItemInput{Title: "dropped-raw", Actor: "tester", Stage: model.StageRaw})

	execSQL(t, s, `UPDATE items SET status = ? WHERE id = ?`, model.StatusInProgress, inProgress.ID)
	if _, err := s.CloseItem(ctx, CloseItemInput{ID: closedItem.ID, Reason: "done", Actor: "tester"}); err != nil {
		t.Fatalf("CloseItem: %v", err)
	}
	if _, err := s.DropItem(ctx, DropItemInput{ID: dropped.ID, Reason: "no longer relevant", Actor: "tester"}); err != nil {
		t.Fatalf("DropItem: %v", err)
	}

	got, err := s.ListItems(ctx, ListFilter{Stage: model.StageRaw, ExcludeClosed: true})
	if err != nil {
		t.Fatalf("ListItems(ExcludeClosed: true): %v", err)
	}
	wantIDs := sortedStrings([]string{open.ID, inProgress.ID})
	gotIDs := idsOfItems(got)
	if !equalStringSlices(gotIDs, wantIDs) {
		t.Fatalf("ListItems(Stage: raw, ExcludeClosed: true) ids = %v, want %v", gotIDs, wantIDs)
	}

	// The zero value (ExcludeClosed unset) applies no filter: all four raw
	// items still come back.
	allGot, err := s.ListItems(ctx, ListFilter{Stage: model.StageRaw})
	if err != nil {
		t.Fatalf("ListItems(Stage: raw): %v", err)
	}
	allWant := sortedStrings([]string{open.ID, inProgress.ID, closedItem.ID, dropped.ID})
	allGotIDs := idsOfItems(allGot)
	if !equalStringSlices(allGotIDs, allWant) {
		t.Fatalf("ListItems(Stage: raw) ids = %v, want %v (zero value must change nothing)", allGotIDs, allWant)
	}

	// ExcludeClosed composes with Type as AND: a Type filter that matches
	// none of the fixture's items combined with ExcludeClosed must return
	// nothing, not fall back to ignoring the Type filter.
	none, err := s.ListItems(ctx, ListFilter{Stage: model.StageRaw, Type: model.TypeBug, ExcludeClosed: true})
	if err != nil {
		t.Fatalf("ListItems(Type: bug, ExcludeClosed: true): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("ListItems(Type: bug, ExcludeClosed: true) = %v, want empty (fixture has no bug-type items)", idsOfItems(none))
	}
}

// TestListItemsUnfilteredIncludesRaw proves a raw item is present with no
// filter, and absent when Stage: "ticket" is applied. ListItems must query
// items directly, never the ready view, or a raw item would silently never
// appear regardless of filter.
func TestListItemsUnfilteredIncludesRaw(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	raw := mustAdd(t, s, AddItemInput{Title: "raw-only", Actor: "tester", Stage: model.StageRaw})

	unfiltered, err := s.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListItems({}): %v", err)
	}
	if !containsID(idsOfItems(unfiltered), raw.ID) {
		t.Fatalf("ListItems({}) ids = %v, want %s present", idsOfItems(unfiltered), raw.ID)
	}

	filtered, err := s.ListItems(ctx, ListFilter{Stage: model.StageTicket})
	if err != nil {
		t.Fatalf("ListItems({Stage: ticket}): %v", err)
	}
	if containsID(idsOfItems(filtered), raw.ID) {
		t.Fatalf("ListItems({Stage: ticket}) ids = %v, want %s absent", idsOfItems(filtered), raw.ID)
	}
}
