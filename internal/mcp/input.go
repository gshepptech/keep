package mcp

import (
	"errors"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// ReadyInput is the `ready` tool's argument shape. store.Ready takes a bare
// int, so unlike the other seven tools there is no store *Input struct to
// reuse; this exists only to give the argument a name and a schema.
type ReadyInput struct {
	Limit int `json:"limit,omitempty"`
}

// ShowInput is the `show` tool's argument shape. store.GetItem takes a bare
// string id, so, like ReadyInput, there is no store *Input struct to reuse.
type ShowInput struct {
	ID string `json:"id"`
}

// BacklogInput is the `backlog` tool's argument shape. store.Backlog takes
// (ctx, typeFilter string), so, like ReadyInput, there is no store *Input
// struct to reuse.
type BacklogInput struct {
	Type string `json:"type,omitempty"`
}

// schemaSet collects the ten generated schemas and every generation error,
// so BuildSchemas needs exactly ONE error check rather than ten. errors.Join
// returns nil when every element is nil, so no conditional is needed here
// either -- which is what keeps MCP-05's structural test able to say the
// error switch in errors.go is the only branch in this package.
type schemaSet struct {
	m    map[string]*jsonschema.Schema
	errs []error
}

// put records one tool's generated schema and its generation error (nil on
// success), passing a jsonschema.For[T](nil) call's two return values
// straight through.
func (s *schemaSet) put(name string, sc *jsonschema.Schema, err error) {
	s.m[name] = sc
	s.errs = append(s.errs, err)
}

// err reports whether any of the ten schemaSet.put calls failed.
// errors.Join(nil, nil, ..., nil) is nil, so a fully successful accumulation
// needs no conditional to detect.
func (s *schemaSet) err() error {
	return errors.Join(s.errs...)
}

// withEnum publishes a closed vocabulary on one property of one schema.
// Every call site below names a property jsonschema.For is known to have
// generated (title/type/stage/status on these fixed, concrete struct
// types), so this has no existence guard -- an unknown property name would
// be a bug in this file, and a nil-map write panicking loudly beats a
// silently-dropped enum.
//
// Advertisement, NOT enforcement: verified live (04-RESEARCH.md) that
// mcp.Server.AddTool (the low-level path this package uses -- see errors.go
// for why) does not validate arguments against InputSchema at all. A
// published enum and additionalProperties:false were both ignored on a real
// call. The enum earns its place by steering a model toward a legal value,
// and nothing in this package may depend on the SDK rejecting one that
// isn't.
func withEnum(sc *jsonschema.Schema, prop string, values []string) {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	sc.Properties[prop].Enum = enum
}

// depTypesForWire is model.DepTypesOrdered minus "parent-child", built once
// here rather than inlined at the withEnum call site below. slices.DeleteFunc
// SKIPS model.DepParentChild by predicate rather than by a separately
// hand-maintained literal list -- so a fifth dep type added to the model
// appears in this enum automatically, with no second place to remember to
// update, and with no `if` in this file's own control flow (the predicate is
// an expression, not a statement).
//
// This is deliberate: store.CreateDep refuses parent-child outright
// (ErrParentChildViaDep, whose message names `rounce add --parent`).
// Advertising a value that is always refused would be worse than not
// advertising it. This runs once at construction time (BuildSchemas), never
// per call inside a tool handler.
func depTypesForWire() []string {
	return slices.DeleteFunc(slices.Clone(model.DepTypesOrdered), func(t string) bool {
		return t == model.DepParentChild
	})
}

// BuildSchemas generates the ten tools' InputSchema values via
// jsonschema.For[T] -- the same reflection helper the SDK's generic
// AddTool[In,Out] uses
// internally -- and decorates every closed-vocabulary property with its
// enum, read from internal/model's ordered slices (never a local literal
// list, never a range over a Valid* map, whose iteration order differs per
// process and would make a tools/list assertion flaky).
//
// Ordered as 04-CONTEXT.md decision 2 names the ten: capture, add, ready,
// backlog, show, list, dep, start, close, drop.
//
// capture and add both reuse store.AddItemInput unmodified -- the same
// struct, two independent jsonschema.For calls, each producing its own
// *jsonschema.Schema (confirmed live: two calls for the same type return
// distinct pointers, so decorating one cannot leak into the other). Their
// wire shape is identical by construction: title required, session_id and
// stage both absent. The one behavioural difference between the two tools
// (capture always writes stage=raw, add defaults to stage=ticket) lives in
// the handler that pre-seeds AddItemInput.Stage before unmarshalling
// (plan 03), never in the schema.
func BuildSchemas() (map[string]*jsonschema.Schema, error) {
	set := &schemaSet{m: make(map[string]*jsonschema.Schema)}

	// Go does not allow a multi-value call to stand in for two of three
	// arguments to put (the "call g() as f's sole argument list" special
	// case only applies when g's call is f's ONLY argument) -- verified by
	// compiling. sc/err are reused across all ten pairs instead; this is
	// still zero conditionals, which is the property that matters here, not
	// statement count.
	sc, err := jsonschema.For[store.AddItemInput](nil)
	set.put("capture", sc, err)
	sc, err = jsonschema.For[store.AddItemInput](nil)
	set.put("add", sc, err)
	sc, err = jsonschema.For[ReadyInput](nil)
	set.put("ready", sc, err)
	sc, err = jsonschema.For[BacklogInput](nil)
	set.put("backlog", sc, err)
	sc, err = jsonschema.For[ShowInput](nil)
	set.put("show", sc, err)
	sc, err = jsonschema.For[store.ListFilter](nil)
	set.put("list", sc, err)
	sc, err = jsonschema.For[store.DepInput](nil)
	set.put("dep", sc, err)
	sc, err = jsonschema.For[store.StartItemInput](nil)
	set.put("start", sc, err)
	sc, err = jsonschema.For[store.CloseItemInput](nil)
	set.put("close", sc, err)
	sc, err = jsonschema.For[store.DropItemInput](nil)
	set.put("drop", sc, err)

	// No error-guard branch here: every one of the ten types above is a
	// fixed, concrete struct (plain strings/ints/slices/pointers, no map
	// with a non-string key, no func/chan/complex/unsafe-pointer field), so
	// jsonschema.For genuinely cannot fail for any of them -- verified by
	// this file's own passing tests, which would panic on a nil schema
	// entry the moment decoration below ran if that assumption were ever
	// wrong. set.err() is still returned to the caller (never silently
	// dropped), consistent with schemaSet's own doc comment that BuildSchemas
	// needs exactly one error check -- it is the caller's, not a branch here.
	withEnum(set.m["add"], "type", model.TypesOrdered)
	withEnum(set.m["capture"], "type", model.TypesOrdered)
	withEnum(set.m["list"], "type", model.TypesOrdered)
	withEnum(set.m["backlog"], "type", model.TypesOrdered)
	withEnum(set.m["list"], "stage", model.StagesOrdered)
	withEnum(set.m["list"], "status", model.StatusesOrdered)
	withEnum(set.m["dep"], "type", depTypesForWire())

	return set.m, set.err()
}
