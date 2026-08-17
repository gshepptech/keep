package mcp

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// wantRequired is the exact `required` set BuildSchemas must produce for
// each of the thirteen tools, per the plan's own <behavior> block. ready/backlog/
// list have no required fields -- []string{} rather than nil so the
// comparison below is order-independent and nil-tolerant either way.
var wantRequired = map[string][]string{
	"capture": {"title"},
	"add":     {"title"},
	"ready":   {},
	"backlog": {},
	"show":    {"id"},
	"list":    {},
	"dep":     {"item_id", "depends_on_id", "type"},
	"start":   {"id"},
	"close":   {"id", "reason"},
	"drop":    {"id", "reason"},
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// TestBuildSchemasReturnsThirteenWithNoError proves BuildSchemas produces exactly
// the ten tool schemas the plan names, and that generation itself returns no
// error -- the schemaSet accumulator's single error check covers all ten.
func TestBuildSchemasReturnsThirteenWithNoError(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v, want nil", err)
	}
	if len(schemas) != 13 {
		t.Fatalf("BuildSchemas() returned %d schemas, want 13: %v", len(schemas), schemas)
	}
	for name := range wantRequired {
		if _, ok := schemas[name]; !ok {
			t.Errorf("BuildSchemas() missing tool %q", name)
		}
	}
}

// TestSchemaTypeIsObject proves every one of the ten schemas is a JSON
// Schema object type.
func TestSchemaTypeIsObject(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v", err)
	}
	for name, sc := range schemas {
		if sc.Type != "object" {
			t.Errorf("schema %q: Type = %q, want %q", name, sc.Type, "object")
		}
	}
}

// TestSchemaPropertiesAreSnakeCase catches the PascalCase regression
// (Pitfall 2 in 04-RESEARCH.md) in one assertion across all ten schemas: no
// property name may contain an uppercase letter.
func TestSchemaPropertiesAreSnakeCase(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v", err)
	}
	for name, sc := range schemas {
		for prop := range sc.Properties {
			if strings.ToLower(prop) != prop {
				t.Errorf("schema %q: property %q is not snake_case (contains an uppercase letter)", name, prop)
			}
		}
	}
}

// TestSchemaSessionIDNeverAppears proves session_id is absent from every one
// of the ten schemas, under any spelling -- decision 3: session is a
// server-lifetime property, never a per-call argument.
func TestSchemaSessionIDNeverAppears(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v", err)
	}
	for name, sc := range schemas {
		for prop := range sc.Properties {
			if strings.Contains(strings.ToLower(prop), "session") {
				t.Errorf("schema %q: property %q exposes session over the wire, want absent", name, prop)
			}
		}
	}
}

// TestSchemaStageAbsentOnAddAndCapture proves the verb chooses the stage,
// not an argument -- capture's whole contract is "this is raw" and add's is
// "this is a ticket"; a wire-settable stage would let capture be told to
// make a ticket.
func TestSchemaStageAbsentOnAddAndCapture(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v", err)
	}
	for _, name := range []string{"add", "capture"} {
		sc, ok := schemas[name]
		if !ok {
			t.Fatalf("schema %q missing", name)
		}
		if _, ok := sc.Properties["stage"]; ok {
			t.Errorf("schema %q: has a stage property, want absent (the verb chooses the stage)", name)
		}
	}
}

// TestSchemaRequiredFields proves each tool's required set is exactly what
// the plan specifies -- in particular, that parent_id is NOT required on
// add, the live-reproduced Pitfall-2 defect this whole plan exists to fix.
func TestSchemaRequiredFields(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v", err)
	}
	for name, want := range wantRequired {
		sc, ok := schemas[name]
		if !ok {
			t.Fatalf("schema %q missing", name)
		}
		got := sortedCopy(sc.Required)
		wantSorted := sortedCopy(want)
		if !reflect.DeepEqual(got, wantSorted) {
			t.Errorf("schema %q: Required = %v, want %v", name, got, wantSorted)
		}
	}
}

// TestSchemaAddParentIDNotRequired is the plan's own named case, kept
// separate from the table-driven check above so a regression here fails
// with a message naming the exact defect (Pitfall 2), not just a set diff.
func TestSchemaAddParentIDNotRequired(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v", err)
	}
	sc := schemas["add"]
	for _, r := range sc.Required {
		if r == "parent_id" {
			t.Fatalf("schema %q: parent_id is required -- a legitimate root-item add omitting it would be rejected (Pitfall 2)", "add")
		}
	}
}

// stringsToAny converts an ordered []string vocabulary into []any, the type
// jsonschema.Schema.Enum holds, for a reflect.DeepEqual comparison that
// checks both membership and order.
func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// TestSchemaEnums proves every closed-vocabulary property carries a JSON
// Schema enum sourced from internal/model's ordered slices -- by value AND
// by order, never a local literal list and never a range over a Valid* map
// (whose iteration order is per-process and would make this flaky).
func TestSchemaEnums(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v", err)
	}

	depTypesMinusParentChild := make([]string, 0, len(model.DepTypesOrdered)-1)
	for _, dt := range model.DepTypesOrdered {
		if dt == model.DepParentChild {
			continue
		}
		depTypesMinusParentChild = append(depTypesMinusParentChild, dt)
	}

	cases := []struct {
		tool, prop string
		want       []string
	}{
		{"add", "type", model.TypesOrdered},
		{"capture", "type", model.TypesOrdered},
		{"list", "type", model.TypesOrdered},
		{"backlog", "type", model.TypesOrdered},
		{"list", "stage", model.StagesOrdered},
		{"list", "status", model.StatusesOrdered},
		{"dep", "type", depTypesMinusParentChild},
	}

	for _, c := range cases {
		sc, ok := schemas[c.tool]
		if !ok {
			t.Fatalf("schema %q missing", c.tool)
		}
		prop, ok := sc.Properties[c.prop]
		if !ok {
			t.Fatalf("schema %q: property %q missing", c.tool, c.prop)
		}
		want := stringsToAny(c.want)
		if !reflect.DeepEqual(prop.Enum, want) {
			t.Errorf("schema %q property %q: Enum = %v, want %v", c.tool, c.prop, prop.Enum, want)
		}
	}
}

// TestDepEnumExcludesParentChild is the plan's own named case for the dep
// type enum, kept separate so a regression fails naming the exact reason:
// store.CreateDep refuses parent-child outright (ErrParentChildViaDep), so
// advertising it as a legal dep type would be worse than not advertising it.
func TestDepEnumExcludesParentChild(t *testing.T) {
	schemas, err := BuildSchemas()
	if err != nil {
		t.Fatalf("BuildSchemas() error = %v", err)
	}
	prop := schemas["dep"].Properties["type"]
	for _, v := range prop.Enum {
		if v == model.DepParentChild {
			t.Fatalf("dep.type enum contains %q, which CreateDep always refuses (ErrParentChildViaDep)", model.DepParentChild)
		}
	}
}
