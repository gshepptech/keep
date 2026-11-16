package store

import (
	"context"
	"testing"
	"time"
)

// TestFeedAnswersWhatHappened checks that the feed lists every event newest
// first, each with its item's title and reason.
//
// The question a person actually asks is "what did the agents do?", and
// the feed answers it without opening items one at a time.
func TestFeedAnswersWhatHappened(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	a := mustAdd(t, st, AddItemInput{Title: "Expire refresh tokens", Actor: "claude"})
	mustAdd(t, st, AddItemInput{Title: "Something else", Actor: "dev"})
	if _, err := st.StartItem(ctx, StartItemInput{ID: a.ID, Actor: "claude", SessionID: "sess-feed"}); err != nil {
		t.Fatalf("StartItem: %v", err)
	}
	if _, err := st.CloseItem(ctx, CloseItemInput{ID: a.ID, Reason: "shipped it", Actor: "claude"}); err != nil {
		t.Fatalf("CloseItem: %v", err)
	}

	got, err := st.Feed(ctx, FeedFilter{})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("Feed returned %d events, want 4 (two adds, a start, a close)", len(got))
	}

	// Newest first.
	if got[0].Kind != "close" {
		t.Errorf("first entry is %q, want the close -- the feed reads newest first", got[0].Kind)
	}
	// Each entry carries its item's title: "close rnc-8f21ac" answers nothing.
	if got[0].Title != "Expire refresh tokens" {
		t.Errorf("entry has title %q, want the item's title", got[0].Title)
	}
	// And the reason a thing was closed, which is the useful half of a close.
	if got[0].Reason != "shipped it" {
		t.Errorf("close entry reason = %q, want %q", got[0].Reason, "shipped it")
	}
}

// TestFeedNarrowsByActorAndKind checks that the actor and kind filters each
// narrow the feed to matching events, which answer "what did claude do" and
// "what got closed".
func TestFeedNarrowsByActorAndKind(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	mustAdd(t, st, AddItemInput{Title: "by claude", Actor: "claude"})
	mustAdd(t, st, AddItemInput{Title: "by a person", Actor: "dev"})

	got, err := st.Feed(ctx, FeedFilter{Actor: "claude"})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(got) != 1 || got[0].Actor != "claude" {
		t.Errorf("Feed(actor=claude) returned %d entries, want only claude's", len(got))
	}

	got, err = st.Feed(ctx, FeedFilter{Kind: "close"})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Feed(kind=close) returned %d entries, want 0 -- nothing was closed", len(got))
	}
}

// TestFeedSinceCutsOffHistory checks that Since drops events older than the
// cutoff, which answers the daily "since yesterday" question.
func TestFeedSinceCutsOffHistory(t *testing.T) {
	st, ctx := openTestStore(t), context.Background()
	mustAdd(t, st, AddItemInput{Title: "old news", Actor: "a"})

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	got, err := st.Feed(ctx, FeedFilter{Since: future})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Feed(since=the future) returned %d entries, want 0", len(got))
	}
}
