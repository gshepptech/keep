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
	st  *store.Store
	ln  net.Listener
	srv *http.Server
}

// NewServer validates hostport, binds it, and returns a Server whose URL is
// already known -- the listener is opened HERE, not in Serve, because
// `--web` binds port 0 and must print the OS-assigned port before it starts
// serving (DASH-02). Validation runs before net.Listen, so a refused
// address never opens a socket at all.
func NewServer(st *store.Store, hostport string) (*Server, error) {
	if _, err := validateLoopback(hostport); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", hostport)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", hostport, err)
	}
	s := &Server{st: st, ln: ln}
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

// routes is a placeholder returning an empty mux, so NewServer and this
// task's own tests build and pass in isolation. Task 2 (handlers.go)
// replaces this function with the real five-route table; it is not called
// from anywhere but NewServer, so there is exactly one definition to
// replace, not two to keep in sync.
func (s *Server) routes() *http.ServeMux {
	return http.NewServeMux()
}
