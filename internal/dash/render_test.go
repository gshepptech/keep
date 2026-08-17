package dash

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// update rewrites testdata/tree.golden.html from renderTree's current
// output. Run once (`go test ./internal/dash/ -run TestRenderTreeGolden
// -update`) to (re)generate the golden file, then READ IT and confirm by
// eye that it contains the three depth levels, the word "blocked" on each
// blocked row, the "dimmed" class on the dropped row, and no rnc- id other
// than the five literals below -- a golden file is only as good as the one
// human read of it that happens at creation time.
var update = flag.Bool("update", false, "rewrite golden files")

// fixedTime is the one timestamp string every fixture in this file uses.
// Never model.Now()/time.Now() here: rnc- ids are
// sha256(title, createdAt, attempt)-derived from wall-clock time, so a
// fixture built through a live store produces a different id on every run.
// Every id below (rnc-epic, rnc-epic.1, rnc-epic.1.1, rnc-epic.2,
// rnc-blocker) is a hand-picked literal, not a real store id, and is not
// supposed to be -- that is what makes the golden comparison stable on every
// machine, forever.
const fixedTime = "2026-01-01T00:00:00.000000000Z"

// treeFixture is the three-level blocked subtree DASH-06 requires: the root
// epic is blocked directly by rnc-blocker; every descendant is blocked
// because the epic is, naming the epic (not rnc-blocker) as the nearest
// blocker, matching store.Tree's own nearest-blocked-ancestor semantics.
// rnc-epic.2 is additionally dropped, exercising the "dimmed" class
// alongside "blocked" on the same row. Order matches store.Tree's own
// depth-first ORDER BY path: epic, epic.1, epic.1.1, epic.2.
func treeFixture() []model.TreeNode {
	item := func(id, typ, title, status string) model.Item {
		return model.Item{
			ID:        id,
			Stage:     model.StageTicket,
			Type:      typ,
			Title:     title,
			Status:    status,
			CreatedAt: fixedTime,
			UpdatedAt: fixedTime,
		}
	}
	return []model.TreeNode{
		{Item: item("rnc-epic", model.TypeEpic, "Epic root", model.StatusOpen), Depth: 0, Blocked: true, BlockedBy: "rnc-blocker"},
		{Item: item("rnc-epic.1", model.TypeTask, "Task A", model.StatusOpen), Depth: 1, Blocked: true, BlockedBy: "rnc-epic"},
		{Item: item("rnc-epic.1.1", model.TypeTask, "Subtask under A", model.StatusOpen), Depth: 2, Blocked: true, BlockedBy: "rnc-epic"},
		{Item: item("rnc-epic.2", model.TypeTask, "Task B (dropped)", model.StatusDropped), Depth: 1, Blocked: true, BlockedBy: "rnc-epic"},
	}
}

const treeGoldenPath = "testdata/tree.golden.html"

// TestRenderTreeGolden is DASH-06's byte-for-byte proof: renderTree on the
// fixture above must equal testdata/tree.golden.html exactly. See -update's
// doc comment above for how to (re)generate the golden file.
func TestRenderTreeGolden(t *testing.T) {
	got, err := renderTree(treeFixture())
	if err != nil {
		t.Fatalf("renderTree: %v", err)
	}

	if *update {
		if err := os.WriteFile(treeGoldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d bytes) -- read it and confirm by eye before committing", treeGoldenPath, len(got))
		return
	}

	want, err := os.ReadFile(treeGoldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to generate it)", treeGoldenPath, err)
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("renderTree output does not match %s:\n%s", treeGoldenPath, diffSummary(got, want))
	}
}

// diffSummary reports len(got)/len(want) and the first differing byte
// offset plus a short window around it -- never the whole document --
// matching cmd/keep/roundtrip_test.go's existing discipline for a golden
// comparison failure.
func diffSummary(got, want []byte) string {
	n := min(len(got), len(want))
	off := -1
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			off = i
			break
		}
	}
	if off == -1 {
		if len(got) == len(want) {
			return fmt.Sprintf("len(got)=%d len(want)=%d, byte-equal check failed unexpectedly", len(got), len(want))
		}
		off = n
	}

	window := func(b []byte) string {
		lo := max(0, off-20)
		hi := min(len(b), off+20)
		if lo >= len(b) {
			return "<past end>"
		}
		return fmt.Sprintf("%q", b[lo:hi])
	}

	return fmt.Sprintf("len(got)=%d len(want)=%d firstDiffOffset=%d\n  got:  %s\n  want: %s",
		len(got), len(want), off, window(got), window(want))
}

// TestRenderReadySmoke proves renderReady returns non-empty output and a nil
// error both on a populated fixture and on an empty queue -- an empty queue
// is a page that says so, not a 500.
func TestRenderReadySmoke(t *testing.T) {
	items := []model.Item{
		{ID: "rnc-a", Stage: model.StageTicket, Type: model.TypeTask, Title: "A", Status: model.StatusOpen, Score: 7, CreatedAt: fixedTime, UpdatedAt: fixedTime},
	}
	if got, err := renderReady(items); err != nil || len(got) == 0 {
		t.Fatalf("renderReady(populated): got %d bytes, err %v", len(got), err)
	}
	if got, err := renderReady(nil); err != nil || len(got) == 0 {
		t.Fatalf("renderReady(nil): got %d bytes, err %v", len(got), err)
	}
}

// TestRenderBacklogSmoke proves renderBacklog returns non-empty output and a
// nil error both on a populated, multi-type fixture and on an empty backlog.
func TestRenderBacklogSmoke(t *testing.T) {
	items := []model.Item{
		{ID: "rnc-a", Stage: model.StageRaw, Type: model.TypeRoadmap, Title: "Q3 plan", Status: model.StatusOpen, CreatedAt: fixedTime, UpdatedAt: fixedTime},
		{ID: "rnc-b", Stage: model.StageRaw, Type: model.TypeNote, Title: "A stray thought", Status: model.StatusOpen, CreatedAt: fixedTime, UpdatedAt: fixedTime},
	}
	if got, err := renderBacklog(items); err != nil || len(got) == 0 {
		t.Fatalf("renderBacklog(populated): got %d bytes, err %v", len(got), err)
	}
	if got, err := renderBacklog(nil); err != nil || len(got) == 0 {
		t.Fatalf("renderBacklog(nil): got %d bytes, err %v", len(got), err)
	}
}

// TestRenderItemSmoke proves renderItem returns non-empty output and a nil
// error both on a populated ItemDetail (parent, repo, deps both directions,
// events, a multi-line body) and on one with no deps/events -- the common
// case for a freshly captured item, not an error state.
func TestRenderItemSmoke(t *testing.T) {
	repo := "example/repo"
	parent := "rnc-parent"
	populated := model.ItemDetail{
		Item: model.Item{
			ID: "rnc-a", ParentID: &parent, Repo: &repo,
			Stage: model.StageTicket, Type: model.TypeTask, Title: "Do the thing",
			Body: "the body\nacross two lines", Why: "because", Status: model.StatusOpen,
			CreatedAt: fixedTime, UpdatedAt: fixedTime,
		},
		DependsOn:    []model.Dep{{ItemID: "rnc-a", DependsOnID: "rnc-x", Type: model.DepBlocks, CreatedAt: fixedTime}},
		DependedOnBy: []model.Dep{{ItemID: "rnc-y", DependsOnID: "rnc-a", Type: model.DepParentChild, CreatedAt: fixedTime}},
		Events:       []model.Event{{ID: 1, ItemID: "rnc-a", Kind: "add", Actor: "alice", At: fixedTime}},
	}
	if got, err := renderItem(populated); err != nil || len(got) == 0 {
		t.Fatalf("renderItem(populated): got %d bytes, err %v", len(got), err)
	}

	bare := model.ItemDetail{
		Item: model.Item{ID: "rnc-b", Stage: model.StageTicket, Type: model.TypeTask, Title: "Bare", Status: model.StatusOpen, CreatedAt: fixedTime, UpdatedAt: fixedTime},
	}
	if got, err := renderItem(bare); err != nil || len(got) == 0 {
		t.Fatalf("renderItem(no deps/events): got %d bytes, err %v", len(got), err)
	}
}
