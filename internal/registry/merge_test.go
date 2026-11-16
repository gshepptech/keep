package registry

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

// item is a small synthetic-fixture helper: every field Merge's comparator
// or a JSON assertion cares about, nothing else. It uses no store and no
// temp directory, so ids and names can be chosen to assert tie-breaking
// deterministically; real rnc- ids are hash-derived and cannot be arranged.
func item(id string, score int) model.Item {
	return model.Item{ID: id, Score: score, Title: "t-" + id}
}

// TestMergeOrdersByScoreDescending proves items from three repos with
// distinct scores come back ordered by score descending, regardless of the
// input RepoResult order.
func TestMergeOrdersByScoreDescending(t *testing.T) {
	results := []RepoResult{
		{Repo: "low", Items: []model.Item{item("z1", 3)}},
		{Repo: "high", Items: []model.Item{item("z2", 30)}},
		{Repo: "mid", Items: []model.Item{item("z3", 16)}},
	}
	got := Merge(results)
	wantIDs := []string{"z2", "z3", "z1"}
	if len(got) != len(wantIDs) {
		t.Fatalf("Merge: got %d items, want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("Merge order = %v, want %v", idsOf(got), wantIDs)
		}
	}
}

// TestMergeTiebreaksByRepositoryNameThenID checks that two items with the
// same score, from repos named beta and alpha, come back alpha first. The
// fixture gives beta's item the lexically smaller id, so a comparator that
// dropped the repository-name key and fell through to id would produce the
// opposite order. The test depends on id order and repository order
// disagreeing.
func TestMergeTiebreaksByRepositoryNameThenID(t *testing.T) {
	results := []RepoResult{
		{Repo: "beta", Items: []model.Item{item("aaa-beta", 20)}},
		{Repo: "alpha", Items: []model.Item{item("zzz-alpha", 20)}},
	}
	got := Merge(results)
	if len(got) != 2 {
		t.Fatalf("Merge: got %d items, want 2: %+v", len(got), got)
	}
	if got[0].Repository != "alpha" || got[1].Repository != "beta" {
		t.Fatalf("Merge tie order = [%s %s], want [alpha beta] (repository name breaks the tie, not id)",
			got[0].Repository, got[1].Repository)
	}
}

// TestMergeTiebreaksByIDWithinSameRepo proves two items with the same score
// from the SAME repo come back ordered by id ascending.
func TestMergeTiebreaksByIDWithinSameRepo(t *testing.T) {
	results := []RepoResult{
		{Repo: "solo", Items: []model.Item{item("bbb", 5), item("aaa", 5)}},
	}
	got := Merge(results)
	wantIDs := []string{"aaa", "bbb"}
	if len(got) != len(wantIDs) {
		t.Fatalf("Merge: got %d items, want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("Merge same-repo tie order = %v, want %v", idsOf(got), wantIDs)
		}
	}
}

// TestMergeDropsErroredResultsWithoutAffectingOrder proves a RepoResult
// carrying a non-nil Err contributes no rows, and does not affect the
// ordering of the rest.
func TestMergeDropsErroredResultsWithoutAffectingOrder(t *testing.T) {
	results := []RepoResult{
		{Repo: "ok1", Items: []model.Item{item("x1", 10)}},
		{Repo: "broken", Err: errors.New("store not found"), Items: []model.Item{item("would-not-appear", 99)}},
		{Repo: "ok2", Items: []model.Item{item("x2", 5)}},
	}
	got := Merge(results)
	wantIDs := []string{"x1", "x2"}
	if len(got) != len(wantIDs) {
		t.Fatalf("Merge: got %d items, want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("Merge with an errored result = %v, want %v", idsOf(got), wantIDs)
		}
	}
	for _, mi := range got {
		if mi.ID == "would-not-appear" {
			t.Fatalf("Merge included an item from an errored RepoResult: %+v", got)
		}
	}
}

// TestMergeEmptyReturnsNonNilEmptySlice checks that an empty input returns
// a non-nil, zero-length slice, never nil. runReady does the same for an
// empty queue, because a consumer doing `for x in $(jq '.[]')` breaks on
// null.
func TestMergeEmptyReturnsNonNilEmptySlice(t *testing.T) {
	got := Merge(nil)
	if got == nil {
		t.Fatalf("Merge(nil) = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("Merge(nil) = %+v, want empty", got)
	}

	got2 := Merge([]RepoResult{{Repo: "empty", Items: nil}})
	if got2 == nil {
		t.Fatalf("Merge with no items = nil, want a non-nil empty slice")
	}
	if len(got2) != 0 {
		t.Fatalf("Merge with no items = %+v, want empty", got2)
	}
}

// TestMergedItemTagDoesNotCollideWithItemRepo is the tag-collision proof.
// Marshal one MergedItem whose embedded Item.Repo points at
// "item-level-value" and whose Repository is "registry-level-name".
// Unmarshal into map[string]any and assert BOTH keys are present with their
// distinct values. Asserted on the decoded map, not a substring of the JSON
// text, so this cannot be fooled by an incidental substring match.
func TestMergedItemTagDoesNotCollideWithItemRepo(t *testing.T) {
	itemRepo := "item-level-value"
	mi := MergedItem{
		Item:       model.Item{ID: "x1", Repo: &itemRepo},
		Repository: "registry-level-name",
	}

	b, err := json.Marshal(mi)
	if err != nil {
		t.Fatalf("json.Marshal(MergedItem): %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", b, err)
	}

	gotRepo, ok := decoded["repo"]
	if !ok {
		t.Fatalf("decoded MergedItem JSON %s has no \"repo\" key -- model.Item.Repo was dropped by the tag collision", b)
	}
	if gotRepo != "item-level-value" {
		t.Fatalf("decoded MergedItem JSON \"repo\" = %v, want %q", gotRepo, "item-level-value")
	}

	gotRepository, ok := decoded["repository"]
	if !ok {
		t.Fatalf("decoded MergedItem JSON %s has no \"repository\" key", b)
	}
	if gotRepository != "registry-level-name" {
		t.Fatalf("decoded MergedItem JSON \"repository\" = %v, want %q", gotRepository, "registry-level-name")
	}
}

func idsOf(items []MergedItem) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}
