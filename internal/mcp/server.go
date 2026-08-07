package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// Options carries everything the server must not read for itself.
// internal/mcp reads no environment variable and opens no store:
// cmd/rounce/mcp.go resolves actor and session through the SAME
// resolveActor/resolveSession helpers every CLI verb uses, then passes them
// in. That is what makes MCP-03's identical-events-rows claim a property of
// shared code rather than of two implementations agreeing.
type Options struct {
	// Actor is the server's default actor -- resolved once at startup
	// (ROUNCE_ACTOR, else $USER, via cmd/rounce/flags.go's resolveActor). A
	// caller may override it per call on any mutating tool (04-CONTEXT.md
	// decision 3: "actor is an optional argument on every mutating tool,
	// defaulting to the server's own actor"). The pre-seed pattern in every
	// handler below is what lets that override happen with zero
	// conditionals.
	Actor string

	// Session is fixed for the process lifetime -- one MCP server process
	// serves one agent session (04-CONTEXT.md decision 3). It is never a
	// per-call argument: every mutating *Input struct's SessionID field is
	// json:"-" (plan 02), unreachable over the wire under any spelling,
	// verified live (04-RESEARCH.md) even under an exact-Go-field-name
	// injection attempt.
	Session string

	// Version is reported in the initialize handshake.
	Version string

	// Logger MUST NOT write to stdout (MCP-07) -- stdout carries protocol
	// bytes only, and anything else on it is fatal to an MCP stdio session.
	// A nil Logger is safe here: the SDK's own default is
	// slog.New(slog.DiscardHandler), which writes nowhere.
	Logger *slog.Logger
}

// NewServer builds the rounce MCP server: all ten tools, wired to st,
// registered through the low-level mcp.Server.AddTool -- never the SDK's
// generic AddTool[In, Out].
//
// # Why not the generic AddTool[In, Out]
//
// It is the SDK's ergonomic generic API. It is what the SDK's own
// documentation shows first. It is what "reuse the store's *Input structs as
// typed tool inputs" naturally suggests. And it cannot satisfy MCP-04.
// Verified live, twice (04-RESEARCH.md): it routes both a JSON type
// mismatch and an entirely legitimate call that omitted an optional field
// into an isError:true tool result -- reached through a code path
// (toolForErr's generated handler) that never calls the handler at all,
// because it does `errRes.SetError(...); return &errRes, nil` on any
// validation or unmarshal failure. That nil Go error is why the dispatcher
// would emit a normal successful response with no top-level "error" key.
//
// MCP-04 requires a genuine JSON-RPC protocol error. Only the low-level
// mcp.Server.AddTool(*Tool, ToolHandler), paired with a manual
// json.Unmarshal and a plain (or *jsonrpc.Error) Go error return on
// failure, produces one -- verified live to emit exactly
// {"jsonrpc":"2.0","id":N,"error":{"code":-32602,"message":"..."}}. Used
// uniformly for all ten tools; mixing the two styles would make error
// behaviour differ per tool for no benefit.
func NewServer(st *store.Store, opts Options) (*mcp.Server, error) {
	sc, err := BuildSchemas()
	if err != nil {
		return nil, err
	}

	s := mcp.NewServer(
		&mcp.Implementation{Name: "rounce", Version: opts.Version},
		&mcp.ServerOptions{Logger: opts.Logger},
	)

	// Registration order matches 04-CONTEXT.md decision 2's list -- the
	// order tools/list returns and the order plan 04's MCP-01 test asserts:
	// capture, add, ready, backlog, show, list, dep, start, close, drop.

	s.AddTool(&mcp.Tool{
		Name:        "capture",
		Description: "Store a raw note or document verbatim. Raw items never appear in `ready` until they are refined into tickets.",
		InputSchema: sc["capture"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.AddItemInput{Actor: opts.Actor, SessionID: opts.Session, Stage: model.StageRaw}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.AddItem(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "add",
		Description: "Create a workable ticket. Use `parent_id` to place it in the tree and `derived_from` to record the document it was refined out of.",
		InputSchema: sc["add"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.AddItemInput{Actor: opts.Actor, SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.AddItem(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "ready",
		Description: "List the open, unblocked tickets ranked by score -- what to work on right now.",
		InputSchema: sc["ready"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := ReadyInput{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.Ready(ctx, in.Limit))
	})

	s.AddTool(&mcp.Tool{
		Name:        "backlog",
		Description: "List raw, unrefined items (captured documents and notes) waiting to be turned into tickets.",
		InputSchema: sc["backlog"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := BacklogInput{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.Backlog(ctx, in.Type))
	})

	s.AddTool(&mcp.Tool{
		Name:        "show",
		Description: "Show one item's full detail: fields, dependencies in both directions, and its event history.",
		InputSchema: sc["show"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := ShowInput{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.GetItem(ctx, in.ID))
	})

	s.AddTool(&mcp.Tool{
		Name:        "list",
		Description: "List items with optional filters -- stage, status, type, parent, repo -- including raw and closed/dropped items ready/backlog omit.",
		InputSchema: sc["list"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.ListFilter{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.ListItems(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "dep",
		Description: "Create a dependency edge between two items: blocks, related, or derived-from. Parent-child structure is created by `add`'s parent_id, not this tool.",
		InputSchema: sc["dep"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.DepInput{Actor: opts.Actor, SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(in, st.CreateDep(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "start",
		Description: "Claim an item and mark it in progress. Refused if the item is raw, already terminal, or blocked.",
		InputSchema: sc["start"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.StartItemInput{Actor: opts.Actor, SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.StartItem(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "close",
		Description: "Mark an item finished, recording why. Distinct from `drop`: close means done, drop means abandoned and must never be counted as done.",
		InputSchema: sc["close"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.CloseItemInput{Actor: opts.Actor, SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.CloseItem(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "drop",
		Description: "Mark an item abandoned, recording why. Distinct from `close`: drop means abandoned and must never be counted as done.",
		InputSchema: sc["drop"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.DropItemInput{Actor: opts.Actor, SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.DropItem(ctx, in))
	})

	return s, nil
}

// Three accepted surface differences from the CLI, recorded here per this
// plan's own instruction (plan 07 carries them into docs/design/phase-3.md):
//
//  1. `blocked` and `update` have no tool. The CLI has thirteen verbs; the
//     locked ten omit `init` (an MCP client cannot usefully create the store
//     it was already pointed at), `blocked`, and `update` --
//     04-CONTEXT.md decision 2's list verbatim, not extended here.
//  2. `add` cannot set `stage`. The CLI's `add --stage raw` has no MCP
//     equivalent; `capture` is the verb for a raw item (input.go, plan 02).
//  3. An invalid `type`/`stage` on `add`/`capture` produces a raw SQLite
//     constraint message, because AddItem does not pre-validate against the
//     closed vocabulary in Go (only cmd/rounce/add.go does, at the CLI
//     boundary). The published enum steers a model away from it; when one
//     arrives anyway it falls to resultFor's default arm and becomes a
//     protocol error carrying driver text (04-RESEARCH.md Open Question 5).
//     Moving that validation into AddItem would improve both surfaces and is
//     out of this phase's locked scope.
//
// Nil slices marshal to `null`, not `[]`: store.Ready/Backlog/ListItems
// return a nil slice when empty, so an empty `ready` tool result is the text
// `null` -- whereas `rounce ready --json` prints `[]`, because cmd/rounce
// normalises it. Normalising here would be a conditional in a handler.
// Accepted and recorded, not fixed.
