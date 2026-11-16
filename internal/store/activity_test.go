package store

import (
	"context"
	"testing"
)

// TestLastActivityEmptyStore proves an empty store (no items, no events)
// returns ("", false, nil): a nil error and false, never an error and never
// a zero-valued timestamp string.
func TestLastActivityEmptyStore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	at, ok, err := s.LastActivity(ctx)
	if err != nil {
		t.Fatalf("LastActivity on empty store: unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("LastActivity on empty store: ok = true, want false")
	}
	if at != "" {
		t.Fatalf("LastActivity on empty store: at = %q, want \"\"", at)
	}
}

// TestLastActivityAfterOneAddItem proves LastActivity reports the single
// add event's own at value.
func TestLastActivityAfterOneAddItem(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	item := mustAdd(t, s, AddItemInput{Title: "solo item", Actor: "tester"})

	detail, err := s.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(detail.Events) != 1 {
		t.Fatalf("GetItem(%s).Events has %d rows, want exactly 1 (the add event)", item.ID, len(detail.Events))
	}
	wantAt := detail.Events[0].At

	at, ok, err := s.LastActivity(ctx)
	if err != nil {
		t.Fatalf("LastActivity: unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("LastActivity: ok = false, want true")
	}
	if at != wantAt {
		t.Fatalf("LastActivity = %q, want %q (the add event's at)", at, wantAt)
	}
}

// TestLastActivityAfterCreateDepStrictlyExceedsItemsUpdatedAt checks that
// after AddItem on two items followed by CreateDep, LastActivity is
// strictly greater than the maximum items.updated_at across both items.
// MAX(events.at) and MAX(items.updated_at) differ on this fixture because
// CreateDep (deps.go) writes exactly one events row and no UPDATE against
// items.
func TestLastActivityAfterCreateDepStrictlyExceedsItemsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	itemA := mustAdd(t, s, AddItemInput{Title: "item A", Actor: "tester"})
	itemB := mustAdd(t, s, AddItemInput{Title: "item B", Actor: "tester"})

	if err := s.CreateDep(ctx, DepInput{ItemID: itemA.ID, DependsOnID: itemB.ID, Type: "blocks", Actor: "tester"}); err != nil {
		t.Fatalf("CreateDep: %v", err)
	}

	items, err := s.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var maxItemsUpdatedAt string
	for _, it := range items {
		if it.UpdatedAt > maxItemsUpdatedAt {
			maxItemsUpdatedAt = it.UpdatedAt
		}
	}
	if maxItemsUpdatedAt == "" {
		t.Fatalf("computed max items.updated_at is empty -- fixture broken")
	}

	at, ok, err := s.LastActivity(ctx)
	if err != nil {
		t.Fatalf("LastActivity: unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("LastActivity: ok = false, want true")
	}

	if !(at > maxItemsUpdatedAt) {
		t.Fatalf("LastActivity = %q is NOT strictly greater than max(items.updated_at) = %q -- CreateDep's event should postdate both items' last update, since CreateDep never touches items", at, maxItemsUpdatedAt)
	}

	// Asserted positively: LastActivity equals the dep event's at, read back
	// from GetItem(itemA).Events (the last event whose Kind is "dep").
	detail, err := s.GetItem(ctx, itemA.ID)
	if err != nil {
		t.Fatalf("GetItem(%s): %v", itemA.ID, err)
	}
	var depAt string
	for _, e := range detail.Events {
		if e.Kind == "dep" {
			depAt = e.At
		}
	}
	if depAt == "" {
		t.Fatalf("no \"dep\" event found on %s's event history: %+v", itemA.ID, detail.Events)
	}
	if at != depAt {
		t.Fatalf("LastActivity = %q, want %q (the dep event's at)", at, depAt)
	}
}
