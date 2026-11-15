package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	internalmcp "github.com/gshepptech/keep/internal/mcp"
	"github.com/gshepptech/keep/internal/store"
)

// codeServerClosing is jsonrpc2.ErrServerClosing's code. The SDK does not
// export it (only the five standard JSON-RPC codes are in the public
// jsonrpc package), so it is named here rather than string-matched -- the
// same classify-by-code-never-by-message rule internal/store/errors.go
// states for SQLite and cmd/keep/flags.go's exitFor follows for exit
// codes.
//
// It is what Run returns when the client closes stdin while a request is
// still in flight, which is an ordinary disconnect and not a keep
// failure. Piping a batch of requests and closing immediately produces
// *fmt.wrapError wrapping *jsonrpc.Error{Code: -32004, Message: "server is
// closing"}, message "server is closing: EOF". The same batch with stdin
// held open until every response is written returns nil. Without this
// classification `keep mcp` would exit 1 on a perfectly normal client
// shutdown.
const codeServerClosing = -32004

// runMCP serves the MCP protocol over stdio against the store the same
// four-rule resolver every other command uses. It blocks until the client
// disconnects -- the one runX that does not return promptly -- but keeps
// every other runX's (args, stdout, stderr) int shape.
//
// No --json: the protocol IS the output format. No --actor: actor is a
// per-call tool argument with a server default: a
// --actor flag here would be a third place the default could come from,
// alongside $KEEP_ACTOR and $USER.
func runMCP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbFlag := addDBFlag(fs)
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "keep mcp: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	actor, err := resolveActor("")
	if err != nil {
		fmt.Fprintf(stderr, "keep mcp: %v\n", err)
		return 2
	}
	session := resolveMCPSession()

	s, err := openStore(*dbFlag)
	if err != nil {
		// This is where store.ErrSchemaVersion lands: an unusable store
		// fails server startup rather than surfacing through a single tool
		// call, which is exactly why internal/mcp's resultFor does not
		// classify it (internal/mcp/errors.go).
		fmt.Fprintf(stderr, "keep mcp: %v\n", err)
		return 1
	}
	defer s.Close()

	// The SDK's default Logger is slog.New(slog.DiscardHandler) -- it
	// writes nowhere. Set one explicitly, pointed at stderr (never
	// os.Stdout, never the stdout parameter): stdout carries protocol
	// bytes only, and an MCP server with no diagnostics at all is
	// hard to debug.
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	// $KEEP_ACTOR is read here rather than inside mcpActor so the
	// "explicit wins" rule is decided once, at startup, from the process
	// environment -- not re-read per session from whatever the client's
	// handshake happens to contain.
	envActor := store.Env("ACTOR")

	srv, err := internalmcp.NewServer(s, internalmcp.Options{
		Actor:        actor,
		Session:      session,
		Version:      resolveVersion(),
		Logger:       logger,
		Instructions: mcpInstructions(context.Background(), s),
		ActorFor: func(p *mcp.InitializeParams) string {
			return mcpActor(envActor, p, actor)
		},
		// The cross-repository queue, injected rather than computed in
		// internal/mcp: that package holds one store and must not grow a
		// registry fan-out. This is the same function `keep ready --all`
		// renders from, so the tool and the flag cannot disagree.
		ReadyAll: func(ctx context.Context) (any, error) {
			// Warnings go through the SDK logger, which is pointed at
			// stderr: stdout carries protocol bytes only, and a
			// malformed registry is worth saying out loud rather than
			// dropping just because this surface has nowhere obvious to
			// put it.
			return readyAllPayload(ctx, loggerWriter{logger})
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "keep mcp: %v\n", err)
		return 1
	}

	err = srv.Run(context.Background(), &mcp.StdioTransport{})
	if err == nil {
		return 0
	}
	var we *jsonrpc.Error
	if errors.As(err, &we) && we.Code == codeServerClosing {
		return 0
	}
	fmt.Fprintf(stderr, "keep mcp: %v\n", err)
	return 1
}

// resolveMCPSession returns the session id this server's tools claim under.
//
// $KEEP_SESSION wins, as everywhere else. Absent it, this derives a
// PER-PROCESS id -- which is the opposite of resolveClaimSession's stable
// one, and correct for the opposite reason. One MCP server process serves
// one agent session (Options.Session says so), so two agents on this machine
// are two processes and must not share an identity; a stable per-user id
// would put them both under the same session and re-open the collapse this
// exists to close: each would be handed the other's claim as its own retry,
// and either could release the other's through the own-claim-only path.
//
// The pid alone is not enough. Pids are reused, and a stale claim left by a
// dead server could collide with a new one that happened to inherit its
// number -- rare, silent, and exactly the class of thing that only shows up
// under the concurrency this is meant to make safe. The random half removes
// it; crypto/rand rather than math/rand because a guessable session id is a
// session id another process can impersonate.
func resolveMCPSession() string {
	if s := store.Env("SESSION"); s != "" {
		return s
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cannot happen on any supported platform, and if it somehow does,
		// a pid-only id is still non-empty and still unique among LIVE
		// processes -- which is better than refusing to start.
		return fmt.Sprintf("mcp:%d", os.Getpid())
	}
	return fmt.Sprintf("mcp:%d:%s", os.Getpid(), hex.EncodeToString(b[:]))
}

// loggerWriter adapts a *slog.Logger to io.Writer so cmd/keep helpers that
// warn to a writer can be reused by `keep mcp`, where writing to stdout
// would corrupt the protocol stream.
type loggerWriter struct{ l *slog.Logger }

func (w loggerWriter) Write(p []byte) (int, error) {
	w.l.Warn(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
