package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// TestNextItemClaimsTheTopOfTheQueue: `next` must claim exactly the row
// `ready` puts first, not "a workable item". If it ever picked differently
// the queue's ranking would stop meaning anything for the one caller that
// consumes it most.
func TestNextItemClaimsTheTopOfTheQueue(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	mustAdd(t, s, AddItemInput{Title: "low", Actor: "tester", Value: intPtr(1), Confidence: intPtr(1), Effort: intPtr(9)})
	top := mustAdd(t, s, AddItemInput{Title: "high", Actor: "tester", Value: intPtr(10), Confidence: intPtr(10), Effort: intPtr(0)})
	mustAdd(t, s, AddItemInput{Title: "middle", Actor: "tester", Value: intPtr(5), Confidence: intPtr(5), Effort: intPtr(5)})

	queue, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if queue[0].ID != top.ID {
		t.Fatalf("fixture is wrong: ready puts %s first, not %s", queue[0].ID, top.ID)
	}

	got, err := s.NextItem(ctx, NextInput{Actor: "tester", SessionID: "sess-a"})
	if err != nil {
		t.Fatalf("NextItem: %v", err)
	}
	if !got.Claimed {
		t.Fatal("NextItem returned Claimed=false with three workable items in the queue")
	}
	if got.Item.ID != top.ID {
		t.Errorf("NextItem claimed %s, want the top of the queue %s", got.Item.ID, top.ID)
	}
	if got.Item.Status != model.StatusInProgress {
		t.Errorf("claimed item status = %q, want %q", got.Item.Status, model.StatusInProgress)
	}

	// And it is genuinely out of the queue afterwards, not merely reported
	// as claimed.
	after, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready after: %v", err)
	}
	for _, it := range after {
		if it.ID == top.ID {
			t.Errorf("%s is still in ready after being claimed by next", top.ID)
		}
	}
}

// TestNextItemOnAnEmptyQueueIsAnAnswerNotAnError. An agent that receives an
// error for "there is nothing to do" has to tell it apart from a real
// failure before it can decide whether to retry, and every integration would
// have to get that right independently.
func TestNextItemOnAnEmptyQueueIsAnAnswerNotAnError(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	got, err := s.NextItem(ctx, NextInput{Actor: "tester", SessionID: "sess-a"})
	if err != nil {
		t.Fatalf("NextItem on an empty store = %v, want no error", err)
	}
	if got.Claimed {
		t.Error("NextItem claimed something from an empty store")
	}
	if got.Blocked != 0 {
		t.Errorf("Blocked = %d, want 0 -- nothing exists to be blocked", got.Blocked)
	}
}

// TestNextItemSaysHowMuchIsBlockedWhenTheQueueIsEmpty is the difference
// between "there is no work" and "there is work and something is in its
// way". They call for opposite responses -- stop, or go unblock something --
// and an empty list alone cannot tell them apart.
func TestNextItemSaysHowMuchIsBlockedWhenTheQueueIsEmpty(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	blocker := mustAdd(t, s, AddItemInput{Title: "the blocker", Actor: "tester"})
	a := mustAdd(t, s, AddItemInput{Title: "a", Actor: "tester"})
	b := mustAdd(t, s, AddItemInput{Title: "b", Actor: "tester"})
	for _, id := range []string{a.ID, b.ID} {
		if err := s.CreateDep(ctx, DepInput{ItemID: id, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
			t.Fatalf("CreateDep: %v", err)
		}
	}
	// Claim the blocker itself so nothing at all is workable.
	if _, err := s.StartItem(ctx, StartItemInput{ID: blocker.ID, Actor: "tester", SessionID: "sess-other"}); err != nil {
		t.Fatalf("StartItem(blocker): %v", err)
	}

	got, err := s.NextItem(ctx, NextInput{Actor: "tester", SessionID: "sess-a"})
	if err != nil {
		t.Fatalf("NextItem: %v", err)
	}
	if got.Claimed {
		t.Fatalf("NextItem claimed %s with everything blocked or held", got.Item.ID)
	}
	if got.Blocked != 2 {
		t.Errorf("Blocked = %d, want 2 (a and b are both waiting on the blocker)", got.Blocked)
	}
}

// TestConcurrentNextItemNeverHandsOutTheSameItemTwice is the whole reason
// this verb exists. The `ready` then `start` sequence it replaces has a
// window between the read and the write; two agents that read the same top
// row both try to claim it, and one gets ErrConflict and has to retry.
//
// Ten goroutines, ten items: every one must come back with a DIFFERENT item
// and no error. A single duplicate here means the window is still open.
func TestConcurrentNextItemNeverHandsOutTheSameItemTwice(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	const n = 10
	for i := 0; i < n; i++ {
		mustAdd(t, s, AddItemInput{Title: "workable", Actor: "tester", Value: intPtr(5), Confidence: intPtr(5), Effort: intPtr(1)})
	}

	var (
		mu      sync.Mutex
		claimed = map[string]string{} // item id -> session that got it
		errs    []error
	)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := "sess-" + string(rune('a'+i))
			got, err := s.NextItem(ctx, NextInput{Actor: "tester", SessionID: session})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if !got.Claimed {
				errs = append(errs, errors.New(session+" got an empty queue with "+string(rune('0'+n))+" items in it"))
				return
			}
			if prior, dup := claimed[got.Item.ID]; dup {
				errs = append(errs, errors.New("item "+got.Item.ID+" was handed to both "+prior+" and "+session))
				return
			}
			claimed[got.Item.ID] = session
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		t.Error(err)
	}
	if len(claimed) != n {
		t.Errorf("%d distinct items were claimed, want %d", len(claimed), n)
	}
}

// TestWhyNotAgreesWithReadyForEveryItem is the property that makes WhyNot
// worth trusting: for every item in the store, WhyNot's `ready` flag must
// match whether the item is actually in the ready view. A WhyNot that
// reasoned about the predicate independently could drift from the view, and
// an explanation that disagrees with the thing it explains is worse than no
// explanation.
func TestWhyNotAgreesWithReadyForEveryItem(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	raw := mustAdd(t, s, AddItemInput{Title: "captured", Actor: "tester", Stage: model.StageRaw})
	blocker := mustAdd(t, s, AddItemInput{Title: "blocker", Actor: "tester"})
	blockedItem := mustAdd(t, s, AddItemInput{Title: "blocked", Actor: "tester"})
	epic := mustAdd(t, s, AddItemInput{Title: "epic", Actor: "tester"})
	child := mustAdd(t, s, AddItemInput{Title: "child of a blocked epic", Actor: "tester", ParentID: epic.ID})
	inProgress := mustAdd(t, s, AddItemInput{Title: "held", Actor: "tester"})
	closed := mustAdd(t, s, AddItemInput{Title: "finished", Actor: "tester"})
	workable := mustAdd(t, s, AddItemInput{Title: "workable", Actor: "tester"})

	for _, id := range []string{blockedItem.ID, epic.ID} {
		if err := s.CreateDep(ctx, DepInput{ItemID: id, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: "tester"}); err != nil {
			t.Fatalf("CreateDep: %v", err)
		}
	}
	if _, err := s.StartItem(ctx, StartItemInput{ID: inProgress.ID, Actor: "tester", SessionID: "sess-x"}); err != nil {
		t.Fatalf("StartItem: %v", err)
	}
	if _, err := s.CloseItem(ctx, CloseItemInput{ID: closed.ID, Reason: "done", Actor: "tester"}); err != nil {
		t.Fatalf("CloseItem: %v", err)
	}

	inReady := map[string]bool{}
	queue, err := s.Ready(ctx, 0)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, it := range queue {
		inReady[it.ID] = true
	}

	all, err := s.ListItems(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(all) != 8 {
		t.Fatalf("fixture has %d items, want 8", len(all))
	}

	for _, it := range all {
		w, err := s.WhyNot(ctx, it.ID)
		if err != nil {
			t.Fatalf("WhyNot(%s): %v", it.ID, err)
		}
		if w.Ready != inReady[it.ID] {
			t.Errorf("WhyNot(%s %q).Ready = %v, but ready view membership is %v -- the explanation disagrees with the view it explains",
				it.ID, it.Title, w.Ready, inReady[it.ID])
		}
		if w.Ready && w.Reason != "" {
			t.Errorf("WhyNot(%s) is ready but still gave a reason %q", it.ID, w.Reason)
		}
		if !w.Ready && w.Reason == "" {
			t.Errorf("WhyNot(%s) is not ready and gave no reason", it.ID)
		}
	}

	// And the specific reasons, since "some non-empty string" is not an
	// explanation.
	cases := []struct {
		id       string
		contains string
	}{
		{raw.ID, "raw"},
		{blockedItem.ID, blocker.ID},
		{child.ID, epic.ID},
		{inProgress.ID, "in_progress"},
		{closed.ID, "closed"},
	}
	for _, c := range cases {
		w, err := s.WhyNot(ctx, c.id)
		if err != nil {
			t.Fatalf("WhyNot(%s): %v", c.id, err)
		}
		if !strings.Contains(w.Reason, c.contains) && !strings.Contains(w.BlockedBy, c.contains) {
			t.Errorf("WhyNot(%s).Reason = %q, BlockedBy = %q; want one of them to name %q", c.id, w.Reason, w.BlockedBy, c.contains)
		}
	}

	if w, err := s.WhyNot(ctx, workable.ID); err != nil || !w.Ready {
		t.Errorf("WhyNot(workable item) = %+v, %v; want Ready", w, err)
	}
}

// TestWhyNotOnAnUnknownIDIsNotFound: a typo must say so rather than
// answering "not ready" about an item that does not exist, which reads as a
// real finding.
func TestWhyNotOnAnUnknownIDIsNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.WhyNot(context.Background(), "rnc-nosuch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("WhyNot(unknown id) = %v, want ErrNotFound", err)
	}
}
