package mcp

import (
	"errors"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
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

// BlockedInput is the `blocked` tool's argument shape. store.Blocked takes
// no arguments at all, so this is deliberately empty: it exists so the
// handler has something to unmarshal into and therefore has the same shape
// as every other handler in this package: one unmarshal, one store call,
// one resultFor. A handler that skipped the unmarshal would be the only one
// without it, and internal/mcp/structure_test.go asserts the shape rather
// than trusting it.
type BlockedInput struct{}

// ReadyAllInput is the `ready_all` tool's argument shape. It is empty for
// the same reason BlockedInput is: the underlying read takes no arguments,
// and a handler with nothing to unmarshal would be the only one in this
// package without the shape structure_test.go asserts.
type ReadyAllInput struct{}

// WhyNotInput is the `why_not` tool's argument shape. store.WhyNot takes a
// bare string id, like ShowInput.
type WhyNotInput struct {
	ID string `json:"id"`
}

// BacklogInput is the `backlog` tool's argument shape. store.Backlog takes
// (ctx, typeFilter string), so, like ReadyInput, there is no store *Input
// struct to reuse.
type BacklogInput struct {
	Type string `json:"type,omitempty"`
}

// schemaSet collects the generated schemas and every generation error, so
// BuildSchemas needs one error check rather than ten. errors.Join returns
// nil when every element is nil, so no conditional is needed here either,
// and the structural test can still say the error switch in errors.go is
// the only branch in this package.
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

// withRange publishes an integer property's bounds, the numeric parallel of
// withEnum and subject to the same caveat: the SDK does not validate
// arguments against InputSchema, so this steers a model toward a legal
// number and nothing may depend on it refusing an illegal one.
// store.ErrScoreOutOfRange is what actually refuses.
//
// jsonschema's Minimum/Maximum are *float64, so the bounds are taken by
// value and addressed here, in one place, rather than through a
// package-level var per bound whose address could be shared by accident.
func withRange(sc *jsonschema.Schema, prop string, min, max float64) {
	sc.Properties[prop].Minimum = &min
	sc.Properties[prop].Maximum = &max
}

// withScoreRange applies model's bounds to all three scoring inputs of one
// tool, so a fourth caller cannot advertise two of the three.
func withScoreRange(sc *jsonschema.Schema) {
	for _, prop := range []string{"value", "confidence", "effort"} {
		withRange(sc, prop, float64(model.ScoreMin), float64(model.ScoreMax))
	}
}

// withEnum publishes a closed vocabulary on one property of one schema.
// Every call site below names a property jsonschema.For is known to have
// generated (title/type/stage/status on these fixed, concrete struct
// types), so this has no existence guard. An unknown property name would
// be a bug in this file, and a nil-map write panicking loudly beats a
// silently dropped enum.
//
// The enum is advertisement, not enforcement. mcp.Server.AddTool, the
// low-level path this package uses (server.go explains why), does not
// validate arguments against InputSchema, and a published enum and
// additionalProperties:false are both ignored on a call. The enum steers a
// model toward a legal value, and nothing in this package may depend on the
// SDK rejecting one that isn't.
func withEnum(sc *jsonschema.Schema, prop string, values []string) {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	sc.Properties[prop].Enum = enum
}

// depTypesForWire is model.DepTypesOrdered minus "parent-child", built once
// here rather than inlined at the withEnum call site below. slices.DeleteFunc
// skips model.DepParentChild by predicate rather than by a separately
// hand-maintained literal list, so a fifth dep type added to the model
// appears in this enum automatically, with no second place to update and no
// `if` in this file's own control flow (the predicate is an expression, not
// a statement).
//
// parent-child is left out because store.CreateDep refuses it outright
// (ErrParentChildViaDep, whose message names `keep add --parent`).
// Advertising a value that is always refused would be worse than not
// advertising it. This runs once at construction time (BuildSchemas), never
// per call inside a tool handler.
func depTypesForWire() []string {
	return slices.DeleteFunc(slices.Clone(model.DepTypesOrdered), func(t string) bool {
		return t == model.DepParentChild
	})
}

// ToolNames is every tool this server registers, in registration order,
// which is the order tools/list returns.
//
// It is the one place the tool set is written down. The schema test, the AST
// handler count and the wire-level tools/list assertion all derive from it,
// and each still checks something independent: BuildSchemas is checked against
// it by map key, the AST walk counts real function literals, and cmd/keep's
// MCP test reads the list back over a real stdio session. Agreeing with this
// slice is a claim about three different mechanisms, not one.
var ToolNames = []string{
	"capture", "add", "ready", "backlog", "show", "list", "search", "log",
	"stale", "dep", "start", "close", "drop", "update",
	"blocked", "ready_all", "why_not", "next", "unclaim", "reopen",
}

// BuildSchemas generates every tool's InputSchema value via
// jsonschema.For[T], the reflection helper the SDK's generic
// AddTool[In,Out] uses internally. It decorates every closed-vocabulary
// property with its enum, read from internal/model's ordered slices (never
// a local literal list, never a range over a Valid* map, whose iteration
// order differs per process and would make a tools/list assertion flaky).
//
// Ordered as ToolNames lists them.
//
// capture and add both reuse store.AddItemInput unmodified: the same
// struct, two independent jsonschema.For calls, each producing its own
// *jsonschema.Schema (two calls for the same type return distinct pointers,
// so decorating one cannot leak into the other). Their wire shape is
// identical by construction: title required, session_id and stage both
// absent. The one behavioural difference between the two tools
// (capture always writes stage=raw, add defaults to stage=ticket) lives in
// the handler that pre-seeds AddItemInput.Stage before unmarshalling,
// never in the schema.
func BuildSchemas() (map[string]*jsonschema.Schema, error) {
	set := &schemaSet{m: make(map[string]*jsonschema.Schema)}

	// Go does not allow a multi-value call to stand in for two of three
	// arguments to put: the "call g() as f's sole argument list" special
	// case only applies when g's call is f's only argument. sc/err are
	// reused across all ten pairs instead, which still adds no conditional.
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
	sc, err = jsonschema.For[store.SearchFilter](nil)
	set.put("search", sc, err)
	sc, err = jsonschema.For[store.FeedFilter](nil)
	set.put("log", sc, err)
	sc, err = jsonschema.For[StaleInput](nil)
	set.put("stale", sc, err)
	sc, err = jsonschema.For[store.UpdateItemInput](nil)
	set.put("update", sc, err)
	sc, err = jsonschema.For[BlockedInput](nil)
	set.put("blocked", sc, err)
	sc, err = jsonschema.For[ReadyAllInput](nil)
	set.put("ready_all", sc, err)
	sc, err = jsonschema.For[WhyNotInput](nil)
	set.put("why_not", sc, err)
	sc, err = jsonschema.For[store.NextInput](nil)
	set.put("next", sc, err)
	sc, err = jsonschema.For[store.UnclaimItemInput](nil)
	set.put("unclaim", sc, err)
	sc, err = jsonschema.For[store.ReopenItemInput](nil)
	set.put("reopen", sc, err)

	// No error-guard branch here: every one of the nineteen types above is a
	// fixed, concrete struct (plain strings/ints/slices/pointers, no map
	// with a non-string key, no func/chan/complex/unsafe-pointer field), so
	// jsonschema.For cannot fail for any of them. If it did, the decoration
	// below would panic on a nil schema entry and this package's tests would
	// fail. set.err() is still returned to the caller, so the one error
	// check schemaSet promises belongs to the caller, not to a branch here.
	withEnum(set.m["add"], "type", model.TypesOrdered)
	withEnum(set.m["capture"], "type", model.TypesOrdered)
	withEnum(set.m["list"], "type", model.TypesOrdered)
	withEnum(set.m["backlog"], "type", model.TypesOrdered)
	withEnum(set.m["list"], "stage", model.StagesOrdered)
	withEnum(set.m["list"], "status", model.StatusesOrdered)
	withEnum(set.m["dep"], "type", depTypesForWire())
	withEnum(set.m["update"], "type", model.TypesOrdered)
	withEnum(set.m["update"], "stage", model.StagesOrdered)
	withScoreRange(set.m["add"])
	withScoreRange(set.m["capture"])
	withScoreRange(set.m["update"])

	return set.m, set.err()
}

// StaleInput is `stale`'s one knob, expressed in hours because JSON has no
// duration type and a bare number of nanoseconds is not something anyone
// would type correctly.
type StaleInput struct {
	// SilentHours is how long an item must have had nothing happen to it.
	// Zero means the store's own default.
	SilentHours float64 `json:"silent_hours,omitempty"`
}

// After turns the wire value into a duration. It does not apply a default:
// StaleClaims already treats a non-positive duration as its own default, so
// an omitted field arrives as zero and means exactly that. Defaulting here
// too would be a second copy of one policy, and it would need a branch the
// structure test forbids outside the two places this package allows them.
func (in StaleInput) After() time.Duration {
	return time.Duration(in.SilentHours * float64(time.Hour))
}
