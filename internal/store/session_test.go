package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// TestAnonymousClaimsAreRefused checks that claiming without a session id
// fails with ErrNoSession. An anonymous claim would not merely be
// unattributed: every anonymous worker would become the same worker,
// because "" equals "". Two StartItem calls with different actors and no
// session would both succeed, and the second actor could then release the
// first's claim through the own-claim-only path.
func TestAnonymousClaimsAreRefused(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	item := mustAdd(t, s, AddItemInput{Title: "contested", Actor: "agent-a"})

	_, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent-a", SessionID: ""})
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("StartItem with no session = %v, want ErrNoSession", err)
	}
	if !strings.Contains(err.Error(), "KEEP_SESSION") {
		t.Errorf("the error does not name what to set: %v", err)
	}

	// Nothing was claimed.
	detail, err := s.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if detail.Item.Status != model.StatusOpen {
		t.Errorf("status = %q after a refused claim, want %q", detail.Item.Status, model.StatusOpen)
	}

	// next is the same claim by another route, and refuses before it reads
	// the queue, so a session that cannot hold a claim does not consume the
	// top of somebody's queue and fail afterwards.
	if _, err := s.NextItem(ctx, NextInput{Actor: "agent-a", SessionID: ""}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("NextItem with no session = %v, want ErrNoSession", err)
	}
	if ready, err := s.Ready(ctx, 0); err != nil || len(ready) != 1 {
		t.Errorf("the queue is %d items after a refused next, want 1 -- it must not have been consumed", len(ready))
	}
}

// TestASecondWorkerIsNeverHandedTheFirstsClaim checks that a second worker
// is refused the first worker's claim even when no session ids are set, so
// the idempotent-retry path cannot swallow the conflict.
func TestASecondWorkerIsNeverHandedTheFirstsClaim(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	item := mustAdd(t, s, AddItemInput{Title: "contested", Actor: "agent-a"})

	if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent-a", SessionID: "sess-a"}); err != nil {
		t.Fatalf("agent-a start: %v", err)
	}

	_, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent-b", SessionID: "sess-b"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("agent-b start on agent-a's claim = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "sess-a") {
		t.Errorf("the conflict does not name the holder: %v", err)
	}

	// And agent-a's own retry is still an idempotent no-op: the session
	// check must not turn a retry into a conflict.
	before := countEvents(t, s)
	if _, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent-a", SessionID: "sess-a"}); err != nil {
		t.Fatalf("agent-a retrying its own claim = %v, want nil", err)
	}
	if after := countEvents(t, s); after != before {
		t.Errorf("an idempotent retry wrote %d event(s), want 0", after-before)
	}
}

// TestOwnClaimGuardTreatsAnonymousAsNobody checks that RequireOwnClaim never
// matches an anonymous claim. RequireOwnClaim is what makes `unclaim` safe
// to expose over MCP, and comparing two empty sessions would let any
// anonymous caller release any anonymous claim.
//
// The store refuses to create anonymous claims, but stores written by older
// binaries can hold them, so the guard handles them.
func TestOwnClaimGuardTreatsAnonymousAsNobody(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	item := mustAdd(t, s, AddItemInput{Title: "held anonymously", Actor: "agent-a"})

	// An anonymous claim: in progress, with a NULL session. It is written
	// directly, because the store refuses to create one.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE items SET status = ?, claimed_by_session = NULL, claimed_at = ? WHERE id = ?`,
		model.StatusInProgress, model.Now(), item.ID); err != nil {
		t.Fatalf("plant a legacy anonymous claim: %v", err)
	}

	// Nobody may release it through the own-claim path, whatever they call
	// themselves.
	for _, session := range []string{"", "sess-b"} {
		_, err := s.UnclaimItem(ctx, UnclaimItemInput{
			ID: item.ID, Reason: "mine now", Actor: "agent-b",
			SessionID: session, RequireOwnClaim: true,
		})
		if !errors.Is(err, ErrConflict) {
			t.Errorf("UnclaimItem(RequireOwnClaim, session=%q) on an anonymous claim = %v, want ErrConflict", session, err)
		}
	}

	// A person at a terminal still can, which is what unrestricted unclaim
	// is for, so a legacy anonymous claim can always be released.
	if _, err := s.UnclaimItem(ctx, UnclaimItemInput{
		ID: item.ID, Reason: "the agent is long gone", Actor: "a person", SessionID: "cli:someone@here",
	}); err != nil {
		t.Fatalf("unrestricted unclaim of a legacy anonymous claim = %v, want nil", err)
	}
}

// TestALegacyAnonymousClaimIsNotHandedToTheNextCaller checks that a claim
// with a NULL session is not treated as the caller's own retry.
// StartItem's idempotent arm compares holder to SessionID, a NULL holder
// reads back as "", and every caller has a non-empty session, so the arm
// sees a real id against an empty holder, which must not match.
func TestALegacyAnonymousClaimIsNotHandedToTheNextCaller(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	item := mustAdd(t, s, AddItemInput{Title: "held anonymously", Actor: "agent-a"})

	if _, err := s.db.ExecContext(ctx,
		`UPDATE items SET status = ?, claimed_by_session = NULL, claimed_at = ? WHERE id = ?`,
		model.StatusInProgress, model.Now(), item.ID); err != nil {
		t.Fatalf("plant a legacy anonymous claim: %v", err)
	}

	_, err := s.StartItem(ctx, StartItemInput{ID: item.ID, Actor: "agent-b", SessionID: "sess-b"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("start against a legacy anonymous claim = %v, want ErrConflict -- it must not be read as this caller's own retry", err)
	}
}
