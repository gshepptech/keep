package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestTimeFormat_FixedWidthAndOrdered proves the property TimeFormat exists
// for: formatting two instants one nanosecond apart always produces two
// strings of equal length, and the earlier one sorts lexically before the
// later one -- the property the ready view's created_at ASC tiebreak relies
// on (see TimeFormat's doc comment).
func TestTimeFormat_FixedWidthAndOrdered(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	earlier := FormatTime(base)
	later := FormatTime(base.Add(time.Nanosecond))

	if len(earlier) != len(later) {
		t.Fatalf("expected equal-length strings, got %d (%q) and %d (%q)",
			len(earlier), earlier, len(later), later)
	}
	if !(earlier < later) {
		t.Fatalf("expected earlier %q to sort before later %q", earlier, later)
	}
}

// TestTimeFormat_WholeSecondKeepsFractionalDigits is the property that makes
// TimeFormat NOT time.RFC3339Nano: a whole-second time must still emit nine
// fractional digits, not none.
func TestTimeFormat_WholeSecondKeepsFractionalDigits(t *testing.T) {
	wholeSecond := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := FormatTime(wholeSecond)
	if !strings.HasSuffix(got, ".000000000Z") {
		t.Fatalf("whole-second time formatted as %q, want suffix .000000000Z", got)
	}
	const wantLen = len("2026-01-01T00:00:00.000000000Z")
	if len(got) != wantLen {
		t.Fatalf("formatted whole-second time has length %d (%q), want %d", len(got), got, wantLen)
	}
}

// TestNow_UsesTimeFormat is a light smoke test that Now() produces a string
// of the same fixed width FormatTime does, without pinning a specific value
// (Now() is inherently non-deterministic).
func TestNow_UsesTimeFormat(t *testing.T) {
	got := Now()
	want := len(FormatTime(time.Now()))
	if len(got) != want {
		t.Fatalf("Now() produced length %d (%q), want %d", len(got), got, want)
	}
}

// TestItem_JSONSnakeCaseAndNull proves that marshalling an Item produces
// snake_case keys matching the items table's column names, and that a null
// column marshals as JSON null, not "" or 0.
func TestItem_JSONSnakeCaseAndNull(t *testing.T) {
	item := Item{
		ID:         "rnc-abc123",
		ParentID:   nil,
		Ordinal:    nil,
		Stage:      StageTicket,
		Type:       TypeTask,
		Title:      "do the thing",
		Body:       "",
		Acceptance: "",
		Status:     StatusOpen,
		Value:      nil,
		Confidence: nil,
		Effort:     nil,
		Score:      0,
		Why:        "",
		CreatedAt:  Now(),
		UpdatedAt:  Now(),
		ClosedAt:   nil,
	}

	b, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}

	// snake_case keys matching the column names.
	for _, key := range []string{
		"id", "parent_id", "ordinal", "stage", "type", "title", "body",
		"acceptance", "status", "value", "confidence", "effort", "score",
		"why", "repo", "paths", "claimed_by_session", "claimed_at",
		"assignee", "external_ref", "created_at", "updated_at", "closed_at",
	} {
		v, ok := raw[key]
		if !ok {
			t.Fatalf("marshalled JSON missing key %q; got keys %v", key, keysOf(raw))
		}
		_ = v
	}

	// nullable columns left unset marshal as JSON null, not "" or 0.
	for _, key := range []string{"parent_id", "ordinal", "value", "confidence", "effort", "closed_at"} {
		if got := string(raw[key]); got != "null" {
			t.Fatalf("key %q marshalled as %s, want null", key, got)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestOrderedVocabulariesMatchTheirMaps is the guard that matters: a new
// value added to a Valid* map but not its matching
// *Ordered slice would otherwise silently vanish from every enum and every
// ordering built on it. For each of the four pairs, this asserts set
// equality in both directions plus equal length, AND the literal expected
// order -- so a reordering is a test failure, not a silent behaviour change.
func TestOrderedVocabulariesMatchTheirMaps(t *testing.T) {
	cases := []struct {
		name      string
		ordered   []string
		valid     map[string]bool
		wantOrder []string
	}{
		{
			name:      "types",
			ordered:   TypesOrdered,
			valid:     ValidTypes,
			wantOrder: []string{"roadmap", "epic", "task", "bug", "note"},
		},
		{
			name:      "stages",
			ordered:   StagesOrdered,
			valid:     ValidStages,
			wantOrder: []string{"raw", "ticket"},
		},
		{
			name:      "statuses",
			ordered:   StatusesOrdered,
			valid:     ValidStatuses,
			wantOrder: []string{"open", "in_progress", "closed", "dropped"},
		},
		{
			name:      "dep types",
			ordered:   DepTypesOrdered,
			valid:     ValidDepTypes,
			wantOrder: []string{"blocks", "parent-child", "related", "derived-from"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !reflect.DeepEqual(c.ordered, c.wantOrder) {
				t.Fatalf("%s: ordered = %v, want literal order %v", c.name, c.ordered, c.wantOrder)
			}
			if len(c.ordered) != len(c.valid) {
				t.Fatalf("%s: len(ordered) = %d, len(valid map) = %d, want equal", c.name, len(c.ordered), len(c.valid))
			}
			seen := make(map[string]bool, len(c.ordered))
			for _, v := range c.ordered {
				seen[v] = true
				if !c.valid[v] {
					t.Errorf("%s: ordered slice has %q, which is not a key of the Valid* map", c.name, v)
				}
			}
			for k := range c.valid {
				if !seen[k] {
					t.Errorf("%s: Valid* map has key %q, which is not in the ordered slice", c.name, k)
				}
			}
		})
	}
}
