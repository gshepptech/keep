package mcp

import (
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gshepptech/keep/internal/store"
)

// errNoRegistry is what the `ready_all` tool reports when the server was
// started without a cross-repository reader (Options.ReadyAll nil). It is a
// plain error, so it falls to resultFor's default arm and becomes a
// protocol error: the call was well-formed, and the answer is that this
// server cannot serve it at all, which is not a fact about the work.
var errNoRegistry = errors.New("this keep mcp server was started without registry access, so it cannot answer across repositories; use `ready` for this repository's queue")

// resultFor is the package's one permitted conditional. The rule is that no
// tool handler branches on business state, and formatting a known, closed
// sentinel set for the wire is not business logic. It lives in one shared
// helper, not one switch per handler, so the package has exactly one
// branch to review.
//
// The signature is (v any, err error) on purpose: Go lets a two-value call
// stand in for an entire argument list, so a handler's last line reads
//
//	return resultFor(st.AddItem(ctx, in))
//
// "call one store method, marshal the result" becomes one expression with
// nowhere for logic to hide.
//
// # The classification of every store sentinel
//
// The switch below implements exactly this table. A new sentinel in
// internal/store needs a row here and in errors_test.go's isErrorSentinels
// table.
//
//	Sentinel             | Arm                     | Why
//	---------------------|-------------------------|----
//	ErrNotFound          | isError                 | well-formed call, legitimate failure
//	ErrCycle             | isError                 | well-formed call, legitimate failure
//	ErrInvalidTransition | isError                 | `start` on a blocked item names the blocker
//	ErrConflict          | isError                 | names the holding session and claim time
//	ErrParentChildViaDep | isError                 | the message says `keep add --parent <id>`
//	ErrSelfEdge          | isError                 | well-formed values refused on semantics
//	ErrScoreOutOfRange   | isError                 | see (a) below
//	ErrEmptyTitle        | isError                 | see (b) below
//	ErrNoFieldsToUpdate  | isError                 | an `update` call that asks for nothing
//	ErrEmptyReason       | isError                 | see (c) below
//	ErrInvalidType       | protocol, InvalidParams | outside the closed vocabulary: malformed
//	ErrInvalidStage      | protocol, InvalidParams | as ErrInvalidType
//	ErrTooDeep           | isError                 | see (d) below
//	ErrInvalidDepType    | protocol, InvalidParams | as ErrInvalidType; see the note below
//	ErrSchemaVersion     | never reaches here      | see (e) below
//	ErrNeedsMigration    | never reaches here      | see (e) below
//
// (a) A well-formed integer refused for a semantic reason. The schema
// advertises the bounds but the SDK does not enforce them, so this is where
// an agent finds out, and it needs the message to correct the number.
//
// (b) A well-formed string, refused because a titleless item is unreadable
// in every list. It is reachable from `add`, `capture` and `update`, and an
// agent that gets it as a tool error can retry with a title.
//
// (c) A close, drop, unclaim or reopen with no stated reason is the record
// the rule exists to prevent. As a tool error, the agent can retry with a
// reason.
//
// (d) Reached by `add` with a parent_id at the bottom of a MaxDepth chain.
// The message names the depth and the bound so an agent can pick a
// shallower parent.
//
// (e) Produced only by store.Open/Migrate, so cmd/keep/mcp.go fails at
// startup before Run. It is not a per-call outcome, and an agent cannot fix
// it: `keep migrate` is a person's command.
//
// Everything unclassified falls through to `default: return nil, err`, a
// protocol error with a generic code. Nothing a well-formed call can reach
// should land there: closed vocabularies are validated in Go
// (internal/store/validate.go) so a model never receives SQLite's own
// "CHECK constraint failed" text and is left to work out what to send.
//
// # Why ErrInvalidDepType is enforced here and not by the schema enum
//
// mcp.Server.AddTool, the low-level path this package uses (server.go
// explains why), performs no argument validation. A published `enum` and
// `additionalProperties:false` are both ignored on a call that supplies a
// value outside either. The enum stays because it steers the model toward a
// legal value; the store sentinel is what enforces, so it produces the
// protocol error a schema check would have.
//
// # Classification is by errors.Is, never by message text
//
// internal/store/errors.go states the same rule for SQLite codes, and
// cmd/keep/flags.go's exitFor follows it for CLI exit codes: messages are
// not a contract.
//
// A json.Marshal failure on the success path is not a business outcome
// either. It is returned as the Go error it is (a protocol error), never as
// an isError result; see the single `if err == nil` branch below.
func resultFor(v any, err error) (*mcp.CallToolResult, error) {
	if err == nil {
		b, merr := json.Marshal(v)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, merr
	}

	switch {
	case errors.Is(err, store.ErrInvalidDepType),
		errors.Is(err, store.ErrInvalidType),
		errors.Is(err, store.ErrInvalidStage):
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
	case errors.Is(err, store.ErrNotFound),
		errors.Is(err, store.ErrCycle),
		errors.Is(err, store.ErrInvalidTransition),
		errors.Is(err, store.ErrConflict),
		errors.Is(err, store.ErrParentChildViaDep),
		errors.Is(err, store.ErrSelfEdge),
		errors.Is(err, store.ErrEmptyTitle),
		errors.Is(err, store.ErrScoreOutOfRange),
		errors.Is(err, store.ErrNoFieldsToUpdate),
		errors.Is(err, store.ErrTooDeep),
		errors.Is(err, store.ErrEmptyReason):
		res := &mcp.CallToolResult{}
		res.SetError(err)
		return res, nil
	default:
		return nil, err
	}
}
