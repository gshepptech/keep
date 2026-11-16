package interlock

import (
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

func ptr(s string) *string { return &s }

func item(id, paths string) model.Item {
	it := model.Item{ID: id, Title: id}
	if paths != "" {
		it.Paths = ptr(paths)
	}
	return it
}

func held(paths ...string) Status {
	st := Status{Version: SupportedVersion}
	for _, p := range paths {
		st.Leases = append(st.Leases, Lease{
			Path: p, Agent: "claude", SessionID: "sess-1",
		})
	}
	return st
}

func ids(items []model.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestSplitPaths(t *testing.T) {
	cases := []struct {
		name string
		in   *string
		want int
	}{
		{"nil is no paths, not one empty path", nil, 0},
		{"empty string is no paths", ptr(""), 0},
		{"one path", ptr("a.go"), 1},
		{"comma separated", ptr("a.go,b.go"), 2},
		{"whitespace is trimmed", ptr(" a.go , b.go "), 2},
		{"empty fields are dropped", ptr("a.go,,b.go,"), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SplitPaths(tc.in); len(got) != tc.want {
				t.Errorf("SplitPaths(%v) = %v, want %d entries", tc.in, got, tc.want)
			}
		})
	}
}

// TestExcludeDropsOnlyItemsWhosePathIsHeld is the join itself. The fixture
// deliberately mixes all three cases, because getting one right while
// getting another wrong is the realistic failure: an over-eager filter hides
// workable tickets, which is worse than no filter at all.
func TestExcludeDropsOnlyItemsWhosePathIsHeld(t *testing.T) {
	items := []model.Item{
		item("held", "internal/shared.go"),
		item("free", "cmd/other.go"),
		item("nopaths", ""),
		item("multi", "cmd/a.go,internal/shared.go"),
	}

	kept, collisions := Exclude(items, held("internal/shared.go"))

	wantKept := []string{"free", "nopaths"}
	if got := ids(kept); len(got) != len(wantKept) || got[0] != wantKept[0] || got[1] != wantKept[1] {
		t.Errorf("kept = %v, want %v", got, wantKept)
	}
	if len(collisions) != 2 {
		t.Fatalf("got %d collisions, want 2 (held and multi): %+v", len(collisions), collisions)
	}
	for _, c := range collisions {
		if c.Path != "internal/shared.go" {
			t.Errorf("%s collided on %q, want the held path", c.Item.ID, c.Path)
		}
		if c.Lease.Agent != "claude" || c.Lease.SessionID != "sess-1" {
			t.Errorf("%s does not name the holder: %+v", c.Item.ID, c.Lease)
		}
	}
}

// TestExcludeKeepsItemsWithNoPaths states the failure direction this must
// take. An item that declares no paths is one keep knows nothing about:
// treating unknown as collided would hide workable work, and a queue that
// hides work is worse than a queue that occasionally offers work someone
// else is already doing. Interlock's own hook blocks that write anyway.
func TestExcludeKeepsItemsWithNoPaths(t *testing.T) {
	kept, collisions := Exclude([]model.Item{item("nopaths", "")}, held("anything.go"))
	if len(kept) != 1 {
		t.Errorf("an item with no paths was excluded; unknown must never mean collided")
	}
	if len(collisions) != 0 {
		t.Errorf("got collisions for an item with no paths: %+v", collisions)
	}
}

// TestExcludeWithNoLeasesChangesNothing checks that with no leases Exclude
// leaves the queue exactly as the store returned it. That is the common
// path, since most repositories do not use Interlock.
func TestExcludeWithNoLeasesChangesNothing(t *testing.T) {
	items := []model.Item{item("a", "x.go"), item("b", "y.go")}
	kept, collisions := Exclude(items, Status{Version: SupportedVersion})
	if len(kept) != 2 || len(collisions) != 0 {
		t.Errorf("kept %d of 2 with no leases held, %d collisions", len(kept), len(collisions))
	}
}

// TestExcludeReturnsEmptyNotNil checks that Exclude returns an empty slice
// rather than nil. `ready --json` substitutes an empty slice for nil so an
// agent doing `jq '.[]'` does not break on null. That substitution happens in
// cmd/keep, but a filter that turned a non-nil slice into nil would
// reintroduce the case just upstream of it.
func TestExcludeReturnsEmptyNotNil(t *testing.T) {
	kept, _ := Exclude([]model.Item{item("only", "x.go")}, held("x.go"))
	if kept == nil {
		t.Error("Exclude returned nil after filtering everything out, want an empty slice")
	}
}
