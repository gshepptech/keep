package dash

import (
	"testing"

	"github.com/gshepptech/keep/internal/interlock"
)

// TestHoldCountIsWhatTheReleaseButtonAsks.
//
// Found on a real board: an agent that had only just started -- a session
// registered, no file touched -- was offered "Release holds". Interlock
// registers a session at SessionStart, so a session with no leases is the
// normal state of every agent for its first few seconds, and the button
// would have released nothing. The Agents panel branches on this count.
func TestHoldCountIsWhatTheReleaseButtonAsks(t *testing.T) {
	st := &interlock.Status{
		Sessions: []interlock.Session{{ID: "fresh"}, {ID: "busy"}},
		Leases: []interlock.Lease{
			{Path: "a.go", SessionID: "busy"},
			{Path: "b.go", SessionID: "busy"},
		},
	}

	if got := holdCount(st, "fresh"); got != 0 {
		t.Errorf("holdCount(fresh) = %d, want 0 -- a just-started agent holds nothing", got)
	}
	if got := holdCount(st, "busy"); got != 2 {
		t.Errorf("holdCount(busy) = %d, want 2", got)
	}
	if got := holdCount(nil, "busy"); got != 0 {
		t.Errorf("holdCount(nil) = %d, want 0 -- no Interlock, no holds", got)
	}
}
