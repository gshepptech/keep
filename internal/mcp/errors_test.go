package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gshepptech/keep/internal/store"
)

// textOf extracts the single TextContent's text from a *mcp.CallToolResult,
// failing loudly if the shape isn't what resultFor is documented to produce.
func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		t.Fatal("textOf: result is nil")
	}
	if len(res.Content) != 1 {
		t.Fatalf("textOf: len(Content) = %d, want 1", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("textOf: Content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

func TestResultFor(t *testing.T) {
	t.Run("success marshals the value verbatim, IsError false, nil Go error", func(t *testing.T) {
		item := struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}{ID: "rnc-abc123", Title: "hello"}

		res, err := resultFor(item, nil)
		if err != nil {
			t.Fatalf("resultFor: unexpected error %v", err)
		}
		if res.IsError {
			t.Fatal("resultFor: IsError = true, want false")
		}
		want, merr := json.Marshal(item)
		if merr != nil {
			t.Fatalf("json.Marshal(item): %v", merr)
		}
		if got := textOf(t, res); got != string(want) {
			t.Fatalf("resultFor: content = %q, want %q", got, string(want))
		}
	})

	t.Run("unclassified error is a protocol error, unchanged", func(t *testing.T) {
		want := errors.New("some driver failure")
		res, err := resultFor(nil, want)
		if res != nil {
			t.Fatalf("resultFor: result = %+v, want nil", res)
		}
		if !errors.Is(err, want) || err.Error() != want.Error() {
			t.Fatalf("resultFor: err = %v, want %v unchanged", err, want)
		}
	})

	t.Run("ErrInvalidDepType is a protocol error with CodeInvalidParams", func(t *testing.T) {
		wrapped := fmt.Errorf("CreateDep: %q: %w", "bogus", store.ErrInvalidDepType)
		res, err := resultFor(nil, wrapped)
		if res != nil {
			t.Fatalf("resultFor: result = %+v, want nil", res)
		}
		var jerr *jsonrpc.Error
		if !errors.As(err, &jerr) {
			t.Fatalf("resultFor: err = %v (%T), want *jsonrpc.Error", err, err)
		}
		if jerr.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("resultFor: jsonrpc.Error.Code = %d, want %d (CodeInvalidParams)", jerr.Code, jsonrpc.CodeInvalidParams)
		}
	})

	// isErrorSentinels is the single source of truth for the seven sentinels
	// resultFor classifies as isError:true tool results -- decision 4's "the
	// legitimate outcome of a well-formed call" arm. If a tenth store
	// sentinel is ever added and not added here, it is invisible to this
	// test by construction (Go has no reflection-based way to enumerate a
	// package's exported error vars) -- the same hand-maintained-table
	// tradeoff internal/store/mutating_scan_test.go's wantMutating/
	// wantReadOnly maps already accept, for the same reason.
	isErrorSentinels := map[string]error{
		"ErrNotFound":          store.ErrNotFound,
		"ErrCycle":             store.ErrCycle,
		"ErrInvalidTransition": store.ErrInvalidTransition,
		"ErrConflict":          store.ErrConflict,
		"ErrParentChildViaDep": store.ErrParentChildViaDep,
		"ErrSelfEdge":          store.ErrSelfEdge,
		"ErrNoFieldsToUpdate":  store.ErrNoFieldsToUpdate,
	}
	for name, sentinel := range isErrorSentinels {
		t.Run(name, func(t *testing.T) {
			wrapped := fmt.Errorf("ctx %q: %w", "rnc-abc123", sentinel)
			res, err := resultFor(nil, wrapped)
			if err != nil {
				t.Fatalf("resultFor(%s): err = %v, want nil", name, err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("resultFor(%s): IsError = false, want true", name)
			}
			if got := textOf(t, res); got != wrapped.Error() {
				t.Fatalf("resultFor(%s): content = %q, want wrapped message %q (wrapping context naming the id must survive)", name, got, wrapped.Error())
			}
		})
	}

	// ErrInvalidDepType (the eighth sentinel) is exercised above, separately,
	// with its jsonrpc.Error assertion -- it does not belong in
	// isErrorSentinels.
	//
	// ErrSchemaVersion (the ninth sentinel) is deliberately NOT exercised
	// here: it is produced only by store.Open, never by any of the ten tool
	// calls resultFor wraps, so it structurally never reaches this function
	// -- see resultFor's doc comment classification table.
	t.Run("all nine sentinels named", func(t *testing.T) {
		named := map[string]bool{
			"ErrInvalidDepType": true, // protocol error, tested separately above
			"ErrSchemaVersion":  true, // never reaches resultFor, tested nowhere
		}
		for name := range isErrorSentinels {
			named[name] = true
		}
		if len(named) != 9 {
			t.Fatalf("named %d sentinels, want 9: %v", len(named), named)
		}
	})
}
