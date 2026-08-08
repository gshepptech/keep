package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/registry"
)

// initRepo creates dir and runs a real `rounce init` in it, registering it
// in home's registry exactly the way production does (REPO-01) -- unlike
// makeRepo (repos_test.go), it adds no tickets, so callers can add scored
// items themselves with mustAdd.
func initRepo(t *testing.T, bin, dir string, e []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if _, stderr, code := run(t, bin, dir, e, "init"); code != 0 {
		t.Fatalf("init in %s: exit %d, stderr=%q", dir, code, stderr)
	}
}

// TestReadyAllRanksAcrossStores proves REPO-03's core claim: three real
// repos, each holding items with explicitly-set --score values chosen so
// the correct global order INTERLEAVES the repositories (alpha holds ranks
// 1 and 4, beta ranks 2 and 5, gamma rank 3). An order that merely
// concatenated per-repo results would fail this; a fixture that did not
// interleave would let it pass.
//
// internal/rank.Score is value + confidence - effort + 11, a pure function
// with no store-specific state, so the same triple yields the same score in
// every repository -- this is what makes cross-store score comparison
// meaningful at all.
func TestReadyAllRanksAcrossStores(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	aDir := filepath.Join(root, "alpha")
	bDir := filepath.Join(root, "beta")
	cDir := filepath.Join(root, "gamma")
	initRepo(t, bin, aDir, e)
	initRepo(t, bin, bDir, e)
	initRepo(t, bin, cDir, e)

	aRank1 := mustAdd(t, bin, aDir, e, "--score", "10/10/1", "alpha rank1") // 30
	bRank2 := mustAdd(t, bin, bDir, e, "--score", "10/5/1", "beta rank2")   // 25
	cRank3 := mustAdd(t, bin, cDir, e, "--score", "5/5/1", "gamma rank3")   // 20
	aRank4 := mustAdd(t, bin, aDir, e, "--score", "0/5/1", "alpha rank4")   // 15
	bRank5 := mustAdd(t, bin, bDir, e, "--score", "0/0/1", "beta rank5")    // 10

	stdout, stderr, code := run(t, bin, root, e, "ready", "--all", "--json")
	if code != 0 {
		t.Fatalf("ready --all --json: exit %d, stderr=%q", code, stderr)
	}
	var out readyAllJSON
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decoding ready --all --json: %v\nstdout=%q", err, stdout)
	}
	if len(out.Errors) != 0 {
		t.Fatalf("ready --all --json errors = %+v, want none", out.Errors)
	}

	wantIDs := []string{aRank1, bRank2, cRank3, aRank4, bRank5}
	wantRepos := []string{"alpha", "beta", "gamma", "alpha", "beta"}
	if len(out.Items) != len(wantIDs) {
		t.Fatalf("ready --all --json items = %+v, want %d items", out.Items, len(wantIDs))
	}
	for i := range wantIDs {
		if out.Items[i].ID != wantIDs[i] {
			t.Fatalf("ready --all --json item %d id = %q, want %q (full order: %v)", i, out.Items[i].ID, wantIDs[i], idsOfMerged(out.Items))
		}
		if out.Items[i].Repository != wantRepos[i] {
			t.Fatalf("ready --all --json item %d repository = %q, want %q", i, out.Items[i].Repository, wantRepos[i])
		}
	}
}

// TestReadyAllCarriesPerRepoErrors proves --json's errors array names a
// deleted store's repository and path with a non-empty message, the items
// array still holds the other two repos' items, and the exit code is 0.
func TestReadyAllCarriesPerRepoErrors(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	makeRepo(t, bin, root, home, "alpha", 1)
	brokenPath := makeRepo(t, bin, root, home, "broken", 1)
	makeRepo(t, bin, root, home, "gamma", 1)

	if err := os.Remove(brokenPath); err != nil {
		t.Fatalf("removing store at %s: %v", brokenPath, err)
	}

	stdout, stderr, code := run(t, bin, root, e, "ready", "--all", "--json")
	if code != 0 {
		t.Fatalf("ready --all --json after deleting a store: exit %d, stderr=%q", code, stderr)
	}
	var out readyAllJSON
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decoding ready --all --json: %v\nstdout=%q", err, stdout)
	}
	if len(out.Errors) != 1 {
		t.Fatalf("ready --all --json errors = %+v, want exactly 1", out.Errors)
	}
	if out.Errors[0].Repository != "broken" || out.Errors[0].Error == "" {
		t.Fatalf("ready --all --json errors[0] = %+v, want repository=broken with a non-empty message", out.Errors[0])
	}
	if len(out.Items) != 2 {
		t.Fatalf("ready --all --json items = %+v, want 2 (alpha and gamma survive)", out.Items)
	}
	for _, it := range out.Items {
		if it.Repository == "broken" {
			t.Fatalf("ready --all --json items contain a row from the broken repo: %+v", out.Items)
		}
	}
}

// TestReadyAllLimitAppliesToTheMergedRanking proves --limit 2 over an
// interleaved fixture returns exactly the global top two -- which come from
// two DIFFERENT repositories, so a per-store limit could not produce the
// same answer by accident.
func TestReadyAllLimitAppliesToTheMergedRanking(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	aDir := filepath.Join(root, "alpha")
	bDir := filepath.Join(root, "beta")
	initRepo(t, bin, aDir, e)
	initRepo(t, bin, bDir, e)

	aRank1 := mustAdd(t, bin, aDir, e, "--score", "10/10/1", "alpha rank1") // 30
	bRank2 := mustAdd(t, bin, bDir, e, "--score", "10/5/1", "beta rank2")   // 25
	mustAdd(t, bin, aDir, e, "--score", "0/5/1", "alpha rank3")             // 15
	mustAdd(t, bin, bDir, e, "--score", "0/0/1", "beta rank4")              // 10

	stdout, stderr, code := run(t, bin, root, e, "ready", "--all", "--json", "--limit", "2")
	if code != 0 {
		t.Fatalf("ready --all --json --limit 2: exit %d, stderr=%q", code, stderr)
	}
	var out readyAllJSON
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decoding ready --all --json --limit 2: %v\nstdout=%q", err, stdout)
	}
	if len(out.Items) != 2 {
		t.Fatalf("ready --all --limit 2 items = %+v, want exactly 2", out.Items)
	}
	wantIDs := []string{aRank1, bRank2}
	for i := range wantIDs {
		if out.Items[i].ID != wantIDs[i] {
			t.Fatalf("ready --all --limit 2 item %d id = %q, want %q", i, out.Items[i].ID, wantIDs[i])
		}
	}
}

// TestReadyAllRejectsDBFlag proves ready --all --db <path> exits 2.
func TestReadyAllRejectsDBFlag(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	dbPath := filepath.Join(root, "somewhere", "db.sqlite")
	stdout, stderr, code := run(t, bin, root, e, "ready", "--all", "--db", dbPath)
	if code != 2 {
		t.Fatalf("ready --all --db: exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--all") || !strings.Contains(stderr, "--db") {
		t.Fatalf("ready --all --db: stderr = %q, want it to name both flags", stderr)
	}
}

// TestReadyWithoutAllResolvesExactlyOneStore is REPO-06's positive proof:
// with three repos registered, rounce ready --json run inside repo A
// returns exactly repo A's ids and none of B's or C's, and its output is a
// flat ARRAY (not the --all envelope object) -- decoded as []model.Item to
// prove the shape is unchanged.
func TestReadyWithoutAllResolvesExactlyOneStore(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	makeRepo(t, bin, root, home, "alpha", 2)
	makeRepo(t, bin, root, home, "beta", 2)
	makeRepo(t, bin, root, home, "gamma", 2)

	aDir := filepath.Join(root, "alpha")
	stdout, stderr, code := run(t, bin, aDir, e, "ready", "--json")
	if code != 0 {
		t.Fatalf("ready --json in alpha: exit %d, stderr=%q", code, stderr)
	}

	var items []model.Item
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatalf("ready --json in alpha did not decode as a flat []model.Item array: %v\nstdout=%q", err, stdout)
	}
	if len(items) != 2 {
		t.Fatalf("ready --json in alpha: got %d items, want exactly 2 (alpha's own)", len(items))
	}
	for _, it := range items {
		if !strings.Contains(it.Title, "alpha ticket") {
			t.Fatalf("ready --json in alpha returned a non-alpha item: %+v", it)
		}
	}
}

// TestReadyWithoutAllSurvivesAMalformedRegistry is REPO-06's negative proof:
// with $HOME/.rounce/repos.json set to `{ not json`, plain rounce ready
// inside a real repo exits 0 with its items and writes nothing to stderr --
// ready without --all never reads the registry at all -- while ready --all
// warns and returns an empty items array, also at exit 0.
func TestReadyWithoutAllSurvivesAMalformedRegistry(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	if err := os.MkdirAll(filepath.Dir(registry.PathIn(home)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(registry.PathIn(home), []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("writing malformed registry: %v", err)
	}

	// ready --all reads the registry directly: assert its behaviour against
	// the still-malformed file BEFORE anything else in this test has a
	// chance to overwrite it (init's own best-effort registration, below,
	// would repair it as a side effect).
	allStdout, allStderr, allCode := run(t, bin, root, e, "ready", "--all", "--json")
	if allCode != 0 {
		t.Fatalf("ready --all --json with a malformed home registry: exit %d, stderr=%q", allCode, allStderr)
	}
	if !strings.Contains(strings.ToLower(allStderr), "warning") {
		t.Fatalf("ready --all --json with a malformed home registry: stderr = %q, want a warning", allStderr)
	}
	var out readyAllJSON
	if err := json.Unmarshal([]byte(allStdout), &out); err != nil {
		t.Fatalf("decoding ready --all --json: %v\nstdout=%q", err, allStdout)
	}
	if len(out.Items) != 0 {
		t.Fatalf("ready --all --json items = %+v, want empty (nothing is registered under a malformed registry)", out.Items)
	}

	// Plain ready must be entirely unaffected: init/add/ready in a real
	// repository still work, and ready itself -- unlike init's own
	// best-effort registration -- never touches the registry, so its own
	// invocation writes nothing to stderr regardless of the registry's state.
	repoDir := t.TempDir()
	if _, stderr, code := run(t, bin, repoDir, e, "init"); code != 0 {
		t.Fatalf("init despite a malformed home registry: exit %d, stderr=%q", code, stderr)
	}
	if _, stderr, code := run(t, bin, repoDir, e, "add", "a real ticket"); code != 0 {
		t.Fatalf("add despite a malformed home registry: exit %d, stderr=%q", code, stderr)
	}
	stdout, stderr, code := run(t, bin, repoDir, e, "ready")
	if code != 0 {
		t.Fatalf("plain ready despite a malformed home registry: exit %d, stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("plain ready: stderr = %q, want empty -- ready without --all never reads the registry", stderr)
	}
	if !strings.Contains(stdout, "a real ticket") {
		t.Fatalf("plain ready stdout = %q, want it to contain the added ticket's title", stdout)
	}
}

func idsOfMerged(items []registry.MergedItem) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}
