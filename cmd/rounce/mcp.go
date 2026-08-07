package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	internalmcp "github.com/gshepptech/rounce/internal/mcp"
)

// codeServerClosing is jsonrpc2.ErrServerClosing's code. The SDK does not
// export it (only the five standard JSON-RPC codes are in the public
// jsonrpc package), so it is named here rather than string-matched -- the
// same classify-by-code-never-by-message rule internal/store/errors.go
// states for SQLite and cmd/rounce/flags.go's exitFor follows for exit
// codes.
//
// It is what Run returns when the client closes stdin while a request is
// still in flight, which is an ordinary disconnect and not a rounce
// failure. Verified live, 10 runs out of 10 (04-RESEARCH.md/04-04-PLAN.md):
// piping a batch of requests and closing immediately produces
// *fmt.wrapError wrapping *jsonrpc.Error{Code: -32004, Message: "server is
// closing"}, message "server is closing: EOF". The same batch with stdin
// held open until every response is written returns nil. Without this
// classification `rounce mcp` would exit 1 on a perfectly normal client
// shutdown.
const codeServerClosing = -32004

// runMCP serves the MCP protocol over stdio against the store the same
// four-rule resolver every other command uses. It blocks until the client
// disconnects -- the one runX that does not return promptly -- but keeps
// every other runX's (args, stdout, stderr) int shape (04-CONTEXT.md:
// "registration follows the same shape").
//
// No --json: the protocol IS the output format. No --actor: 04-CONTEXT.md
// decision 3 makes actor a per-call tool argument with a server default: a
// --actor flag here would be a third place the default could come from,
// alongside $ROUNCE_ACTOR and $USER.
func runMCP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbFlag := addDBFlag(fs)
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "rounce mcp: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	actor, err := resolveActor("")
	if err != nil {
		fmt.Fprintf(stderr, "rounce mcp: %v\n", err)
		return 2
	}
	session := resolveSession()

	s, err := openStore(*dbFlag)
	if err != nil {
		// This is where store.ErrSchemaVersion lands: an unusable store
		// fails server startup rather than surfacing through a single tool
		// call, which is exactly why internal/mcp's resultFor does not
		// classify it (internal/mcp/errors.go).
		fmt.Fprintf(stderr, "rounce mcp: %v\n", err)
		return 1
	}
	defer s.Close()

	// The SDK's default Logger is slog.New(slog.DiscardHandler) -- it
	// writes nowhere. Set one explicitly, pointed at stderr (never
	// os.Stdout, never the stdout parameter): stdout carries protocol
	// bytes only (MCP-07), and an MCP server with no diagnostics at all is
	// hard to debug.
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	srv, err := internalmcp.NewServer(s, internalmcp.Options{
		Actor:   actor,
		Session: session,
		Version: version,
		Logger:  logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "rounce mcp: %v\n", err)
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
	fmt.Fprintf(stderr, "rounce mcp: %v\n", err)
	return 1
}
