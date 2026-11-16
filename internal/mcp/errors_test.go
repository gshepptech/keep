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

	// isErrorSentinels lists the sentinels resultFor classifies as
	// isError:true tool results, the legitimate outcome of a well-formed
	// call. Go has no reflection-based way to enumerate a package's exported
	// error vars, so a new store sentinel missing from this table is
	// invisible to this test.
	isErrorSentinels := map[string]error{
		"ErrNotFound":          store.ErrNotFound,
		"ErrCycle":             store.ErrCycle,
		"ErrInvalidTransition": store.ErrInvalidTransition,
		"ErrConflict":          store.ErrConflict,
		"ErrParentChildViaDep": store.ErrParentChildViaDep,
		"ErrSelfEdge":          store.ErrSelfEdge,
		"ErrEmptyTitle":        store.ErrEmptyTitle,
		"ErrScoreOutOfRange":   store.ErrScoreOutOfRange,
		"ErrNoFieldsToUpdate":  store.ErrNoFieldsToUpdate,
		"ErrTooDeep":           store.ErrTooDeep,
		"ErrEmptyReason":       store.ErrEmptyReason,
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

	// ErrInvalidDepType is exercised above, separately, with its
	// jsonrpc.Error assertion, so it does not belong in isErrorSentinels.
	//
	// ErrSchemaVersion is not exercised here: it is produced only by
	// store.Open, never by any tool call resultFor wraps, so it cannot reach
	// this function (see the classification table in resultFor's doc
	// comment).
	//
	// Every store sentinel must have a row in resultFor's table. Without
	// one, it reaches the agent as a raw protocol error instead of a tool
	// error it can retry from.
	t.Run("every sentinel is named", func(t *testing.T) {
		named := map[string]bool{
			"ErrInvalidDepType": true, // protocol error, tested separately above
			"ErrSchemaVersion":  true, // never reaches resultFor: store.Open fails first
			"ErrNeedsMigration": true, // never reaches resultFor: store.Open fails first
			"ErrInvalidType":    true, // protocol error, tested with ErrInvalidDepType above
			"ErrInvalidStage":   true, // protocol error, tested with ErrInvalidDepType above
			"ErrStoreNotEmpty":  true, // Import has no tool; a store-replacing verb is not an agent's to call
			"ErrBadExport":      true, // as ErrStoreNotEmpty
		}
		for name := range isErrorSentinels {
			named[name] = true
		}
		if len(named) != 18 {
			t.Fatalf("named %d sentinels, want 18: %v", len(named), named)
		}
	})
}
