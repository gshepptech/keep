package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// Options carries everything the server must not read for itself.
// internal/mcp reads no environment variable and opens no store:
// cmd/keep/mcp.go resolves actor and session through the SAME
// resolveActor/resolveSession helpers every CLI verb uses, then passes them
// in. That is what makes "the CLI and MCP write identical events rows" a
// property of shared code rather than of two implementations agreeing.
type Options struct {
	// Actor is the server's default actor, resolved once at startup
	// (KEEP_ACTOR, else $USER, via cmd/keep/flags.go's resolveActor). A
	// caller may override it per call: actor is an optional argument on
	// every mutating tool, defaulting to the server's own actor. The
	// pre-seed pattern in every handler below lets that override happen
	// with zero conditionals.
	Actor string

	// ActorFor resolves the actor from the client's own initialize
	// handshake. Every mutating handler calls it, per call, on its own
	// session's params. It MUST be non-nil.
	//
	// It is resolved per call, not captured at `initialized`, because
	// nothing orders the SDK's InitializedHandler before the first
	// tools/call, so a captured value can race and fall back to Actor.
	// Reading the session that is making the call has no ordering to get
	// wrong and needs no atomic.
	//
	// A default that has to be spelled out in each agent's manifest will
	// drift between them, so one store ends up recording one agent under
	// $USER and another under its own name. The handshake already says who
	// is calling.
	//
	// The parameter is the raw *mcp.InitializeParams, nil and all, because
	// deciding what to do with a nil ClientInfo needs conditionals, and this
	// package keeps them to NewServer and resultFor. cmd/keep/mcp.go does
	// that part, the same way it composes Instructions and resolves Session.
	ActorFor func(*mcp.InitializeParams) string

	// Session is fixed for the process lifetime, because one MCP server
	// process serves one agent session. It is never a per-call argument: every
	// mutating *Input struct's SessionID field is json:"-", unreachable over
	// the wire under any spelling, even an exact Go field name.
	Session string

	// Version is reported in the initialize handshake.
	Version string

	// Instructions is returned in the initialize handshake. It carries the
	// ranked queue as of server start, which is session start: one MCP
	// server process serves one agent session.
	//
	// # Which clients actually show it to the model
	//
	// Sending it is one thing; a client putting it in the model's prompt is
	// another, and clients differ:
	//
	//   - Claude Code renders it under an "MCP Server Instructions" block,
	//     so the model sees it.
	//   - Codex does not. `codex debug prompt-input` renders the
	//     model-visible prompt, and this text is absent from it. Codex
	//     still calls `ready` unprompted, but from the tool's name and
	//     description, not from this.
	//
	// So this field is the only delivery on Claude Code and none at all on
	// Codex, where the plugin-carried SessionStart hook delivers the queue
	// instead.
	//
	// The caller builds it, never this package. Rendering a queue needs
	// loops and branches, and this package has none outside NewServer's
	// schema check and resultFor, which keeps logic in the store.
	// cmd/keep/mcpinstructions.go composes the text the same way it resolves
	// Actor and Session.
	//
	// Empty is the correct value for an empty or unreachable store: the
	// SDK omits the field entirely (json:"instructions,omitempty"), which
	// is the same silence the SessionStart hook keeps in a repository that
	// does not use keep.
	Instructions string

	// ReadyAll answers the cross-repository queue for the `ready_all` tool.
	//
	// It is a function on Options rather than something this package
	// computes. internal/mcp holds a *store.Store, which is one store; the
	// cross-repository read needs the machine's registry, has to open every
	// other store read-only, and has to report which of them it could not
	// read. None of that is this package's business, and importing
	// internal/registry here would put a fan-out in a package that has no
	// logic of its own.
	//
	// cmd/keep supplies it, from the same code path `keep ready --all` uses,
	// so the tool and the flag cannot answer differently.
	//
	// A nil ReadyAll is legitimate: the tool is registered either way, and a
	// nil one reports that this server was started without registry access
	// rather than panicking. NewServer wraps it so the handler still makes
	// exactly one call and carries no branch of its own.
	ReadyAll func(ctx context.Context) (any, error)

	// Logger must not write to stdout. stdout carries protocol bytes only,
	// and anything else on it is fatal to an MCP stdio session.
	// A nil Logger is safe here: the SDK's own default is
	// slog.New(slog.DiscardHandler), which writes nowhere.
	Logger *slog.Logger
}

// NewServer builds the keep MCP server: every tool, wired to st,
// registered through the low-level mcp.Server.AddTool and never the SDK's
// generic AddTool[In, Out].
//
// # Why not the generic AddTool[In, Out]
//
// The generic API is the SDK's ergonomic default, and reusing the store's
// *Input structs as typed tool inputs suggests it. It cannot turn a
// malformed argument into a protocol error, though. It routes both a JSON
// type mismatch and a legitimate call that omitted an optional field into
// an isError:true tool result, through a code path (toolForErr's generated
// handler) that never calls the handler at all, because it does
// `errRes.SetError(...); return &errRes, nil` on any validation or
// unmarshal failure. That nil Go error makes the dispatcher emit a normal
// successful response with no top-level "error" key.
//
// A malformed argument must produce a genuine JSON-RPC protocol error.
// Only the low-level mcp.Server.AddTool(*Tool, ToolHandler), paired with a
// manual json.Unmarshal and a plain (or *jsonrpc.Error) Go error return on
// failure, produces one. It emits exactly
// {"jsonrpc":"2.0","id":N,"error":{"code":-32602,"message":"..."}}. Every
// tool uses it, because mixing the two styles would make error behaviour
// differ per tool for no benefit.
func NewServer(st *store.Store, opts Options) (*mcp.Server, error) {
	sc, err := BuildSchemas()
	if err != nil {
		return nil, err
	}

	s := mcp.NewServer(
		&mcp.Implementation{Name: "keep", Version: opts.Version},
		&mcp.ServerOptions{Logger: opts.Logger, Instructions: opts.Instructions},
	)

	// Registration order is the order tools/list returns and the order
	// the tools/list test asserts. It opens with the core ten (capture,
	// add, ready, backlog, show, list, dep, start, close, drop), and later
	// tools are appended rather than interleaved, so a client diffing two
	// versions sees additions rather than a reshuffle.

	s.AddTool(&mcp.Tool{
		Name:        "capture",
		Description: "Store a raw note or document verbatim. Raw items never appear in `ready` until they are refined into tickets.",
		InputSchema: sc["capture"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.AddItemInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session, Stage: model.StageRaw}
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
		in := store.AddItemInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session}
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
		Name:        "search",
		Description: "Find items by text, matching title, why, acceptance, paths and body, ranked by where the match landed. Use this before filing anything: the thing you noticed may already be filed.",
		InputSchema: sc["search"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.SearchFilter{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.Search(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "log",
		Description: "What has happened in this store, newest first: every add, claim, close, drop and edit, each with its item's title. Narrow with since, actor or kind.",
		InputSchema: sc["log"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.FeedFilter{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.Feed(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "stale",
		Description: "Items claimed by someone who then went quiet -- a session that died mid-work leaves one behind. Reports only; keep never releases a claim on its own.",
		InputSchema: sc["stale"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := StaleInput{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.StaleClaims(ctx, in.After()))
	})

	s.AddTool(&mcp.Tool{
		Name:        "dep",
		Description: "Create a dependency edge between two items: blocks, related, or derived-from. Parent-child structure is created by `add`'s parent_id, not this tool.",
		InputSchema: sc["dep"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.DepInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session}
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
		in := store.StartItemInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session}
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
		in := store.CloseItemInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session}
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
		in := store.DropItemInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.DropItem(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "update",
		Description: "Change fields on an existing item -- title, body, why, acceptance, score, type, stage, repo, paths, assignee. Status is not among them: use `start`, `close` or `drop`, so the log always records why a status moved. Supply at least one field. Omitting a field leaves it alone; a score can be set here but not cleared.",
		InputSchema: sc["update"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.UpdateItemInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.UpdateItem(ctx, in))
	})

	// readyAll is opts.ReadyAll with the nil case already decided, so the
	// handler below is the same one-unmarshal-one-call shape as every other
	// handler in this file. Deciding it here rather than in the handler
	// keeps handlers free of conditionals, and NewServer is one of the two
	// places this package is allowed a branch.
	readyAll := opts.ReadyAll
	if readyAll == nil {
		readyAll = func(context.Context) (any, error) {
			return nil, errNoRegistry
		}
	}

	s.AddTool(&mcp.Tool{
		Name:        "ready_all",
		Description: "The ranked queue across every repository this machine knows about, merged and ranked by score, each row naming which repository it came from. Use it when the work spans more than the repository you are standing in. `ready` is the one you want otherwise -- it is the queue for here.",
		InputSchema: sc["ready_all"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := ReadyAllInput{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(readyAll(ctx))
	})

	s.AddTool(&mcp.Tool{
		Name:        "blocked",
		Description: "The open tickets `ready` is holding back, each naming the nearest item in its way. Read this when `ready` comes back empty -- an empty queue with a full blocked list means go unblock something, not stop.",
		InputSchema: sc["blocked"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := BlockedInput{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.Blocked(ctx))
	})

	s.AddTool(&mcp.Tool{
		Name:        "why_not",
		Description: "Why one specific item is not in `ready`: raw, not open, or blocked by something named. Ask this instead of guessing from `show`.",
		InputSchema: sc["why_not"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := WhyNotInput{}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.WhyNot(ctx, in.ID))
	})

	s.AddTool(&mcp.Tool{
		Name:        "next",
		Description: "Take the top of the queue and claim it, in one step. Prefer this over `ready` then `start`: those are two calls with a gap in between, and another agent can take the item during it. An empty queue is a normal answer here, not an error -- it comes back with how many items are blocked.",
		InputSchema: sc["next"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.NextInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.NextItem(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "unclaim",
		Description: "Put an item YOU claimed back in the queue, recording why. Use it when you started something and cannot finish it: `close` would say it was done and `drop` would say it was not worth doing, and neither is true. Refused for an item another session holds -- releasing someone else's claim is `keep unclaim`, run by a person.",
		InputSchema: sc["unclaim"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.UnclaimItemInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session, RequireOwnClaim: true}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.UnclaimItem(ctx, in))
	})

	s.AddTool(&mcp.Tool{
		Name:        "reopen",
		Description: "Bring a closed or dropped item back to open, recording why the close was wrong. Use it the moment you notice you closed the wrong id: filing the work again loses the id, the history and every dependency edge pointing at the original. The original close stays in the log.",
		InputSchema: sc["reopen"],
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := store.ReopenItemInput{Actor: opts.ActorFor(req.Session.InitializeParams()), SessionID: opts.Session}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		return resultFor(st.ReopenItem(ctx, in))
	})

	return s, nil
}

// Accepted surface differences from the CLI. They are stated as reasons
// rather than as a count of verbs, which would go stale;
// cmd/keep/mcp_parity_test.go compares the two surfaces mechanically.
//
//  1. `init` has no tool, and must not: an MCP client cannot usefully create
//     the store it was already pointed at.
//  2. `unclaim` is narrower over MCP than over the CLI. The tool sets
//     RequireOwnClaim, so an agent may release its own claim and nothing
//     else; taking work away from a live agent stays with the person at the
//     terminal. See store.UnclaimItem for the full argument.
//  3. `migrate`, `repos`, `board` and `codex` have no tool. Each is
//     operator-facing (rewriting a store's schema, editing the machine's
//     registry, binding a port, granting hook trust), and none of them is
//     something an agent should reach for while doing the work.
//  4. `add` cannot set `stage`. The CLI's `add --stage raw` has no MCP
//     equivalent; `capture` is the verb for a raw item (input.go).
//
// Empty results marshal to `[]`, not `null`, on both surfaces. That is
// guaranteed by internal/store, which returns an empty, non-nil slice from
// every list query, rather than here: normalising in this package would
// mean a conditional in a handler, and surfaces carry no behaviour of their
// own.
