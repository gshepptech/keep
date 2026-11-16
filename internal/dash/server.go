package dash

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/gshepptech/keep/internal/registry"
	"github.com/gshepptech/keep/internal/store"
)

// validateLoopback resolves hostport and refuses anything that is not a
// loopback address. This is the whole loopback-only mechanism, and it lives
// in the constructor rather than the flag parser on purpose: the CLI exposes
// no --host flag at all, so there is no way to ask for a non-loopback bind,
// but the refusal must be asserted by attempting one and requiring an
// error, so the guard must exist somewhere a Go test can call directly.
//
// It checks the resolved address, never the input string. That makes it
// correct for "localhost" regardless of how a given machine's /etc/hosts
// resolves it, and means a --host flag could be added without touching this
// function.
func validateLoopback(hostport string) (*net.TCPAddr, error) {
	addr, err := net.ResolveTCPAddr("tcp", hostport)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", hostport, err)
	}
	if addr.IP == nil {
		// ":0" and "" resolve with a nil IP, which net.Listen treats as the
		// unspecified address, meaning all interfaces. IsLoopback() on a nil
		// IP returns false, so the refusal below would happen anyway, but
		// with a misleading message; naming it explicitly here makes the
		// failure legible.
		return nil, fmt.Errorf("refusing to bind %q: empty host is not loopback", hostport)
	}
	if !addr.IP.IsLoopback() {
		return nil, fmt.Errorf("refusing to bind %q: %s is not a loopback address", hostport, addr.IP)
	}
	return addr, nil
}

// Server is keep's dashboard HTTP server: a bound, loopback-only listener
// and the route table (handlers.go) that answers requests against it.
type Server struct {
	st *store.Store

	// csrf is this server's form token, and actor is who its actions are
	// recorded as. Both are fixed for the process: the board has no accounts
	// and no sessions, so a token that varied would only mean a person's
	// open tab stopped working.
	csrf  string
	actor string

	// session is who the board's claims are held by. It is fixed for the
	// process and derived at startup, never empty: store.StartItem refuses
	// an anonymous claim (store.ErrNoSession), because an empty session id
	// makes every anonymous worker the same worker: the second one to
	// claim an item is handed it as its own retry, and either can release
	// the other's hold.
	//
	// Without its own session, a person clicking Claim and an agent that had
	// not set $KEEP_SESSION would be, to every rule that reads the id back,
	// indistinguishable.
	//
	// Per-process rather than stable: two boards are two people at two
	// terminals, and the id exists to tell them apart. A person's own
	// repeat click is idempotent anyway, because the same board serves
	// it.
	session string

	// dbPath is what writableAt reopens. The read path keeps its
	// read-only handle for every GET; only a POST opens for writing, and
	// only for the length of the action.
	dbPath string

	// home is the directory holding the cross-repo registry
	// (~/.rounce/repos.json). Empty means no registry was supplied, which
	// leaves /all rendering its own empty state rather than 404ing. The
	// nav link is unconditional, so a route that vanished would leave a
	// dead link.
	home string

	ln  net.Listener
	srv *http.Server
}

// NewServer validates hostport, binds it, and returns a Server whose URL is
// already known. The listener is opened here, not in Serve, because `--web`
// binds port 0 and must print the OS-assigned port before it starts
// serving. Validation runs before net.Listen, so a refused address never
// opens a socket at all.
//
// The Writable and ActingAs options are what make the board writable.
// Without Writable, every mutating route reports that the board is
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
	sessionToken, err := newToken()
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("derive a board session: %w", err)
	}

	s := &Server{st: st, home: home, ln: ln, csrf: token, actor: "board", session: "board:" + sessionToken}
	for _, opt := range opts {
		opt(s)
	}
	s.srv = &http.Server{Handler: s.routes()}
	return s, nil
}

// URL reports the server's base URL, built from the listener's own bound
// address, never hand-assembled from a host string and a port number, so
// an IPv6 loopback bind (whose net.TCPAddr.String() is already correctly
// bracketed, e.g. "[::1]:54321") comes out as a valid URL unmodified.
func (s *Server) URL() string {
	return "http://" + s.ln.Addr().String()
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
// writing, and both default to off: without them the board is read-only.
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

// errNoProject is a miss rather than a failure, so handlers can answer 404.
var errNoProject = errors.New("no project by that name is registered here")

// storeFor resolves the store this request is about.
//
// A board serves every project keep knows, not only the directory it was
// started in: switching should not mean restarting a server. The project
// comes from the URL, so two tabs can hold two different boards without a
// shared "current project" for them to fight over, as a server-side
// selection would have.
//
// The returned closer is nil for the started-in store, whose handle is
// long-lived and must not be closed per request.
func (s *Server) storeFor(r *http.Request) (*store.Store, func(), string, error) {
	name := r.PathValue("project")
	if name == "" || name == s.activeName() {
		return s.st, func() {}, s.activeName(), nil
	}
	path, ok := s.pathOf(name)
	if !ok {
		// Not found, not broken: a mistyped or forgotten project is an
		// ordinary miss and must not read as the board having failed.
		return nil, nil, "", errNoProject
	}
	// Read-only: a board looking at another project reads it. Mutating
	// actions resolve their own writable handle, and only for the project
	// they were posted from.
	st, err := store.Open(path, store.OpenOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, "", err
	}
	return st, func() { st.Close() }, name, nil
}

// pathOf finds a registered project's store by name.
func (s *Server) pathOf(name string) (string, bool) {
	env, _ := registry.Load(s.home)
	for _, repo := range env.Repos {
		if repo.Name == name {
			return repo.Path, true
		}
	}
	return "", false
}

// writableAt opens a writing handle for one action against one project.
func (s *Server) writableAt(path string) (*store.Store, error) {
	if path == "" {
		return nil, fmt.Errorf("this board is read-only: start it with `keep board --web` from the repository that owns the store")
	}
	return store.Open(path, store.OpenOptions{})
}
