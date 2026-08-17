package mcp

import (
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gshepptech/keep/internal/store"
)

// resultFor is the package's one permitted conditional (04-CONTEXT.md
// decision 4, narrowing docs/design/phase-3.md line 63 from "no tool handler
// contains a conditional" to "no tool handler contains a conditional on
// BUSINESS STATE" -- formatting a known, closed sentinel set for the wire is
// not business logic). It lives in exactly one shared helper, not one switch
// per handler, which is what makes "the only branch in the package"
// mechanically checkable (internal/mcp/structure_test.go, task 3).
//
// The signature is (v any, err error) on purpose: Go lets a two-value call
// stand in for an entire argument list, so a handler's last line reads
//
//	return resultFor(st.AddItem(ctx, in))
//
// "call one store method, marshal the result" becomes one expression with
// nowhere for logic to hide.
//
// # The classification of all nine store sentinels
//
// Write this table into the file as a comment (this comment) and implement
// exactly it -- if a tenth sentinel is ever added to internal/store, add its
// row here AND to errors_test.go's isErrorSentinels table before touching
// anything else, per the same discipline internal/store/mutating_scan_test.go's
// wantMutating/wantReadOnly maps already follow.
//
//	Sentinel               | Arm                              | Why
//	------------------------|-----------------------------------|----
//	ErrNotFound             | isError: true                     | Well-formed call, legitimate failure
//	ErrCycle                | isError: true                     | Well-formed call, legitimate failure
//	ErrInvalidTransition    | isError: true                     | `start` on a blocked item names the blocker -- useful to an agent
//	ErrConflict             | isError: true                     | Names the holding session and claim time
//	ErrParentChildViaDep    | isError: true                     | Its own message already says `keep add --parent <id>`
//	ErrSelfEdge             | isError: true                     | Well-formed values, refused for a semantic reason
//	ErrNoFieldsToUpdate     | isError: true                     | Unreachable today (only UpdateItem produces it, and `update` is not one of the ten tools); classified anyway so a future `update` tool inherits the decision instead of re-litigating it
//	ErrInvalidDepType       | protocol error, CodeInvalidParams | A value outside the closed vocabulary IS a malformed argument -- MCP-04's "schema validation failure -> protocol error" row, enforced by the store because the SDK will not (see note below)
//	ErrSchemaVersion        | never reaches here                | Produced only by store.Open, so cmd/keep/mcp.go fails at startup before Run -- not a per-call failure, the store is unusable
//
// Everything unclassified falls through to `default: return nil, err` -- a
// protocol error with a generic code. The one real case today is a schema
// CHECK violation (`add`/`capture` with an invalid type/stage produces a raw
// *sqlite.Error, extended code 275, because AddItem does not pre-validate
// Type/Stage in Go the way CreateDep validates its dep type). This is a
// genuine, accepted CLI/MCP surface difference -- 04-RESEARCH.md Open
// Question 5, deliberately left out of this phase's locked scope.
//
// # ErrInvalidDepType was going to be handled by the schema enum, and cannot be
//
// 04-CONTEXT.md's decision 4 assumed schema validation would reject an
// invalid dep type before the handler ran. Verified live in plan 02
// (04-RESEARCH.md): mcp.Server.AddTool (the low-level path this package uses
// -- see server.go for why) performs NO argument validation whatsoever. A
// published `enum` and `additionalProperties:false` were both ignored on a
// real call that supplied a value outside either. The enum stays (it steers
// the model toward a legal value); the sentinel below is what actually
// enforces, and it must therefore produce the protocol error the enum was
// supposed to produce. The outcome 04-CONTEXT.md specified is preserved;
// only the mechanism moved from "schema" to "store sentinel, classified
// here".
//
// # Classification is by errors.Is, never by message text
//
// The same rule internal/store/errors.go states for SQLite codes and
// cmd/keep/flags.go's exitFor follows for CLI exit codes, for the same
// reason: messages are not a contract.
//
// A json.Marshal failure on the success path is not a business outcome
// either -- it is returned as the Go error it is (a protocol error), never
// as an isError result; see the single `if err == nil` branch below.
func resultFor(v any, err error) (*mcp.CallToolResult, error) {
	if err == nil {
		b, merr := json.Marshal(v)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, merr
	}

	switch {
	case errors.Is(err, store.ErrInvalidDepType):
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
	case errors.Is(err, store.ErrNotFound),
		errors.Is(err, store.ErrCycle),
		errors.Is(err, store.ErrInvalidTransition),
		errors.Is(err, store.ErrConflict),
		errors.Is(err, store.ErrParentChildViaDep),
		errors.Is(err, store.ErrSelfEdge),
		errors.Is(err, store.ErrNoFieldsToUpdate):
		res := &mcp.CallToolResult{}
		res.SetError(err)
		return res, nil
	default:
		return nil, err
	}
}
