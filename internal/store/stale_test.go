package store

import (
	"context"
	"testing"
	"time"
)

// atClock runs fn with the store's clock pinned to at.
func atClock(t *testing.T, at string, fn func()) {
	t.Helper()
	orig := nowFunc
	defer func() { nowFunc = orig }()
	nowFunc = func() string { return at }
	fn()
}

// TestStaleClaimsFindsWorkThatWentQuiet.
//
// The gap this closes: an agent claims an item, the session dies, and the
// item sits in_progress for ever. Nothing ever asked about it, so nobody
// found out until they wondered why the queue was short.
func TestStaleClaimsFindsWorkThatWentQuiet(t *testing.T) {
	s, ctx := openTestStore(t), context.Background()

	var quiet, busy string
	atClock(t, "2026-01-01T00:00:00.000000000Z", func() {
		a := mustAdd(t, s, AddItemInput{Title: "claimed then abandoned", Actor: "claude"})
		b := mustAdd(t, s, AddItemInput{Title: "claimed and still going", Actor: "claude"})
		quiet, busy = a.ID, b.ID
		if _, err := s.StartItem(ctx, StartItemInput{ID: quiet, Actor: "claude", SessionID: "sess-quiet"}); err != nil {
			t.Fatalf("StartItem: %v", err)
		}
	})

	// Six hours later the second item is claimed -- so it has a recent
	// event -- and we ask what has gone quiet.
	atClock(t, "2026-01-01T06:00:00.000000000Z", func() {
		if _, err := s.StartItem(ctx, StartItemInput{ID: busy, Actor: "claude", SessionID: "sess-busy"}); err != nil {
			t.Fatalf("StartItem: %v", err)
		}

		got, err := s.StaleClaims(ctx, 2*time.Hour)
		if err != nil {
			t.Fatalf("StaleClaims: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("StaleClaims found %d, want 1 -- only the abandoned one", len(got))
		}
		if got[0].ID != quiet {
			t.Errorf("StaleClaims named %s, want %s", got[0].ID, quiet)
		}
		if got[0].Silent < 5*time.Hour {
			t.Errorf("Silent = %v, want about six hours", got[0].Silent)
		}
		if got[0].LastKind != "start" {
			t.Errorf("LastKind = %q, want start -- the last thing that happened", got[0].LastKind)
		}
	})
}

// TestStaleClaimsMeasuresSilenceNotAge.
//
// An agent can work one item for a day. What makes a claim stale is that
// NOTHING IS HAPPENING, not that it started a long time ago -- measuring
// from claimed_at would flag every long piece of work while it was being
// actively worked on, which is the fastest way to teach someone to ignore
// a warning.
func TestStaleClaimsMeasuresSilenceNotAge(t *testing.T) {
	s, ctx := openTestStore(t), context.Background()

	var id string
	atClock(t, "2026-01-01T00:00:00.000000000Z", func() {
		it := mustAdd(t, s, AddItemInput{Title: "long but live", Actor: "claude"})
		id = it.ID
		if _, err := s.StartItem(ctx, StartItemInput{ID: id, Actor: "claude", SessionID: "sess-silence"}); err != nil {
			t.Fatalf("StartItem: %v", err)
		}
	})

	// Claimed eight hours ago, but touched a minute ago.
	atClock(t, "2026-01-01T07:59:00.000000000Z", func() {
		if _, err := s.UpdateItem(ctx, UpdateItemInput{ID: id, Why: strptr("still going"), Actor: "claude"}); err != nil {
			t.Fatalf("UpdateItem: %v", err)
		}
	})

	atClock(t, "2026-01-01T08:00:00.000000000Z", func() {
		got, err := s.StaleClaims(ctx, 2*time.Hour)
		if err != nil {
			t.Fatalf("StaleClaims: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("StaleClaims flagged %d items, want 0 -- it was touched a minute ago", len(got))
		}
	})
}

// TestStaleClaimsIgnoresWorkNobodyClaimed. An open item is not a stale
// claim; it is just work waiting, which the queue already shows.
func TestStaleClaimsIgnoresWorkNobodyClaimed(t *testing.T) {
	s, ctx := openTestStore(t), context.Background()
	atClock(t, "2026-01-01T00:00:00.000000000Z", func() {
		mustAdd(t, s, AddItemInput{Title: "nobody has touched this", Actor: "a"})
	})
	atClock(t, "2026-02-01T00:00:00.000000000Z", func() {
		got, err := s.StaleClaims(ctx, 2*time.Hour)
		if err != nil {
			t.Fatalf("StaleClaims: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("StaleClaims flagged %d unclaimed items, want 0", len(got))
		}
	})
}

func strptr(s string) *string { return &s }
