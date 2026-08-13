package dash

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// openTestStore initialises a fresh, empty store in t.TempDir() for
// internal/dash's own tests -- the same three lines as internal/store's own
// testhelp_test.go helper of the same name, which internal/dash cannot
// import (it is unexported and lives in another package's _test.go file).
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sqlite")
	s, err := store.Init(path)
	if err != nil {
		t.Fatalf("openTestStore: Init: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("openTestStore: Close: %v", err)
		}
	})
	return s
}

// mustAdd calls st.AddItem and fails the test immediately on error, for
// tests that only need a valid fixture item.
func mustAdd(t *testing.T, st *store.Store, in store.AddItemInput) model.Item {
	t.Helper()
	it, err := st.AddItem(context.Background(), in)
	if err != nil {
		t.Fatalf("mustAdd: AddItem(%+v): %v", in, err)
	}
	return it
}

// TestNewServerRefusesNonLoopback is DASH-03's own wording: attempt a
// non-loopback bind and require an error. Only the error's presence is
// asserted -- never message text (server.go's own comment explains why: the
// addresses that get past validation would fail at Listen with OS-dependent
// text, and coupling to that text is how this test rots).
func TestNewServerRefusesNonLoopback(t *testing.T) {
	st := openTestStore(t)
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.5:0", "::1:0"} {
		if s, err := NewServer(st, addr, ""); err == nil {
			s.Close()
			t.Errorf("NewServer(%q): want error, got nil", addr)
		}
	}
}

// TestNewServerBindsLoopbackEphemeralPort proves the two accepted forms
// bind a real, OS-assigned ephemeral port and report a URL url.Parse
// accepts, with a non-zero port.
func TestNewServerBindsLoopbackEphemeralPort(t *testing.T) {
	st := openTestStore(t)
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		s, err := NewServer(st, addr, "")
		if err != nil {
			t.Fatalf("NewServer(%q): unexpected error: %v", addr, err)
		}
		t.Cleanup(func() { s.Close() })

		u, err := url.Parse(s.URL())
		if err != nil {
			t.Fatalf("NewServer(%q): URL() = %q, not parseable: %v", addr, s.URL(), err)
		}
		if u.Port() == "" || u.Port() == "0" {
			t.Errorf("NewServer(%q): URL() = %q, want a non-zero port", addr, s.URL())
		}
	}
}

// TestNewServerLeaksNoListenerOnFailure calls NewServer 50 times with a bad
// address, then confirms a real loopback bind still succeeds afterward -- a
// failed validation must never open (and leak) a socket.
func TestNewServerLeaksNoListenerOnFailure(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 50; i++ {
		if s, err := NewServer(st, "0.0.0.0:0", ""); err == nil {
			s.Close()
			t.Fatalf("NewServer(bad address) attempt %d: want error, got nil", i)
		}
	}
	s, err := NewServer(st, "127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("NewServer(127.0.0.1:0) after 50 failed attempts: %v", err)
	}
	s.Close()
}
