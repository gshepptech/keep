package dash

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/gshepptech/rounce/internal/store"
)

// validateLoopback resolves hostport and refuses anything that is not a
// loopback address. This is DASH-03's whole mechanism, and it lives in the
// constructor rather than the flag parser on purpose (05-CONTEXT.md
// decision 4): the CLI exposes no --host flag at all, so there is no way to
// ASK for a non-loopback bind -- but DASH-03 requires the refusal be
// "asserted by attempting one and requiring an error", so the guard must
// exist somewhere a Go test can call directly.
//
// It checks the RESOLVED address, never the input string. That is what
// makes it correct for "localhost" regardless of how a given machine's
// /etc/hosts resolves it, and it is why a future --host flag would be safe
// to add without touching this function.
func validateLoopback(hostport string) (*net.TCPAddr, error) {
	addr, err := net.ResolveTCPAddr("tcp", hostport)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", hostport, err)
	}
	if addr.IP == nil {
		// ":0" and "" resolve with a nil IP, which net.Listen treats as the
		// unspecified address -- all interfaces. IsLoopback() on a nil IP
		// returns false, so the refusal below would happen anyway, but
		// silently and with a misleading message; naming it explicitly here
		// is what makes the failure legible. This branch is load-bearing,
		// not defensive padding.
		return nil, fmt.Errorf("refusing to bind %q: empty host is not loopback", hostport)
	}
	if !addr.IP.IsLoopback() {
		return nil, fmt.Errorf("refusing to bind %q: %s is not a loopback address", hostport, addr.IP)
	}
	return addr, nil
}

// Server is rounce's dashboard HTTP server: a bound, loopback-only listener
// and the route table (handlers.go) that answers requests against it.
type Server struct {
	st *store.Store

	// csrf is this server's form token, and actor is who its actions are
	// recorded as. Both are fixed for the process: the board has no accounts
	// and no sessions, so a token that varied would only mean a person's
	// open tab stopped working.
	csrf  string
	actor string

	// dbPath is what writable() reopens. The read path keeps its read-only
	// handle -- docs/design/phase-4.md made that true by construction and it
	// stays true for every GET; only a POST opens for writing, and only for
	// the length of the action.
	dbPath string

	// home is the directory holding the cross-repo registry
	// (~/.rounce/repos.json). Empty means no registry was supplied, which
	// leaves /all rendering its own empty state rather than 404ing -- the
	// nav link is unconditional, so a route that vanished would be a dead
	// link that only some builds have.
	home string

	ln  net.Listener
	srv *http.Server
}

// NewServer validates hostport, binds it, and returns a Server whose URL is
// already known -- the listener is opened HERE, not in Serve, because
// `--web` binds port 0 and must print the OS-assigned port before it starts
// serving (DASH-02). Validation runs before net.Listen, so a refused
// address never opens a socket at all.
// NewServer builds the board. dbPath and actor are what make it writable:
// pass an empty dbPath and every mutating route reports that the board is
// read-only, which is what a caller that has not opted in should get.
func NewServer(st *store.Store, hostport, home string, opts ...Option) (*Server, error) {
	if _, err := validateLoopback(hostport); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", hostport)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", hostport, err)
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	s := &Server{st: st, home: home, ln: ln, csrf: token, actor: "board"}
	for _, opt := range opts {
		opt(s)
	}
	s.srv = &http.Server{Handler: s.routes()}
	return s, nil
}

// URL reports the server's base URL, built from the listener's own bound
// address -- never hand-assembled from a host string and a port number, so
// an IPv6 loopback bind (whose net.TCPAddr.String() is already correctly
// bracketed, e.g. "[::1]:54321") comes out as a valid URL unmodified.
func (s *Server) URL() string {
	return "http://" + s.ln.Addr().String()
}

// Addr returns the bound address.
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// Handler returns the server's route table, for httptest-driven tests that
// never bind a real port.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// Serve blocks, serving over the already-bound listener.
func (s *Server) Serve() error {
	return s.srv.Serve(s.ln)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// Close closes the listener directly, for tests that construct a Server but
// never call Serve.
func (s *Server) Close() error {
	return s.ln.Close()
}

// Option configures a Server at construction. There are two, both about
// writing, and both default to off: a board built without them behaves
// exactly as the read-only one did.
type Option func(*Server)

// Writable lets the board act on the store. path is reopened per action, so
// the long-lived read handle stays read-only and a GET still cannot write.
func Writable(path string) Option {
	return func(s *Server) { s.dbPath = path }
}

// ActingAs sets the actor recorded on every event the board writes. It
// defaults to "board" rather than to $USER, because an event that says a
// person did something they did from a web page is less true than one that
// says the board did.
func ActingAs(actor string) Option {
	return func(s *Server) { s.actor = actor }
}

// writable opens a writing handle for one action.
func (s *Server) writable() (*store.Store, error) {
	if s.dbPath == "" {
		return nil, fmt.Errorf("this board is read-only: start it with `rounce board --web` from the repository that owns the store")
	}
	return store.Open(s.dbPath, store.OpenOptions{})
}

// CSRF is the token every mutating form must carry. Exported for the
// templates' page data, not for callers.
func (s *Server) CSRF() string { return s.csrf }
