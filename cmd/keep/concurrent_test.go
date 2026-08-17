package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/registry"
	"github.com/gshepptech/keep/internal/store"
)

// TestConcurrentInitSameDirectory is the GATE-04 proof: four real OS
// processes, all Start()-ed before any of them is Wait()-ed on, contending
// over `keep init` against one empty shared directory.
//
// This is deliberately not a goroutine-based simulation. SQLite's own
// locking (which busy_timeout=3000 and _txlock=immediate arbitrate) is
// enforced at the OS level via fcntl/flock on the file descriptor -- that
// mechanism is structurally invisible to goroutines sharing a process,
// even four separate *sql.DB handles inside one process never exercise a
// real fcntl lock conflict the way four distinct OS processes do. If this
// test is ever "simplified" into a sync.WaitGroup over in-process calls, it
// stops proving anything about cross-process concurrency at all.
func TestConcurrentInitSameDirectory(t *testing.T) {
	bin := buildKeep(t, "concurrent-test")
	dir := tempDir(t)
	home := tempDir(t)
	e := env(home, "")

	const n = 4

	type proc struct {
		cmd    *exec.Cmd
		stdout bytes.Buffer
		stderr bytes.Buffer
	}

	procs := make([]*proc, n)

	// Start all four before waiting on any of them. Starting and waiting
	// one at a time would serialize them and make the rest of this test
	// vacuous -- there would be no real contention to prove anything about.
	for i := 0; i < n; i++ {
		p := &proc{}
		cmd := exec.Command(bin, "init")
		cmd.Dir = dir
		cmd.Env = e
		cmd.Stdout = &p.stdout
		cmd.Stderr = &p.stderr
		p.cmd = cmd
		if err := cmd.Start(); err != nil {
			t.Fatalf("process %d failed to start: %v", i, err)
		}
		procs[i] = p
	}

	for i, p := range procs {
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("process %d exited non-zero: %v\nstdout=%q\nstderr=%q",
				i, err, p.stdout.String(), p.stderr.String())
		}
	}

	// busy_timeout=3000 plus a single IMMEDIATE DDL transaction should make
	// contention wait, not fail -- so no process's stderr should mention
	// lock contention at all.
	for i, p := range procs {
		stderr := p.stderr.String()
		if strings.Contains(stderr, "SQLITE_BUSY") || strings.Contains(stderr, "database is locked") {
			t.Fatalf("process %d stderr mentions lock contention (want busy_timeout+IMMEDIATE to make contention wait, not fail): %q", i, stderr)
		}
	}

	target := store.StorePathIn(dir)
	if !store.OSExists(target) {
		t.Fatalf("%s does not exist after four concurrent inits", target)
	}

	// The real integrity assertion: Open reads schema_version through the
	// guard added in plan 02. If any of the four concurrent inits left a
	// partial or corrupt schema, this is what would catch it.
	s, err := store.Open(target, store.OpenOptions{})
	if err != nil {
		t.Fatalf("store.Open(%s) = %v, want success", target, err)
	}
	s.Close()

	// Every process closed cleanly and checkpointed -- no WAL/SHM residue.
	if store.OSExists(target + "-wal") {
		t.Fatalf("%s-wal still exists after all four processes exited", target)
	}
	if store.OSExists(target + "-shm") {
		t.Fatalf("%s-shm still exists after all four processes exited", target)
	}

	// Exactly one of the four stdout lines should report a newly created
	// store and the other three unchanged -- or, if they genuinely raced
	// before any file existed, all four could report created. Assert the
	// weaker, true property only: every stdout is exactly one line and
	// every line names the same absolute path. Asserting a specific
	// created/unchanged split would be asserting a race outcome by design,
	// which would make this test flaky.
	for i, p := range procs {
		out := strings.TrimRight(p.stdout.String(), "\n")
		lines := strings.Split(out, "\n")
		if len(lines) != 1 {
			t.Fatalf("process %d stdout = %q, want exactly one line", i, p.stdout.String())
		}
		line := strings.TrimPrefix(lines[0], "unchanged: ")
		if line != target {
			t.Fatalf("process %d stdout names %q, want %q", i, line, target)
		}
	}

	// A fifth, sequential init afterwards must exit 0 and report the store
	// as unchanged, proving none of the four concurrent runs left a partial
	// schema behind for it to trip over.
	stdout, stderr, code := run(t, bin, dir, e, "init")
	if code != 0 {
		t.Fatalf("fifth (sequential) init: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	wantLine := "unchanged: " + target
	if got := strings.TrimSpace(stdout); got != wantLine {
		t.Fatalf("fifth init stdout = %q, want %q", got, wantLine)
	}
}

// TestConcurrentAddChildrenNoOrdinalCollision is GATE-04's ITEM-03 proof:
// eight real `keep add --parent <epic>` processes, all Start()-ed before
// any of them is Wait()-ed on, contending over one parent's ordinal
// allocation.
//
// Deliberately not a goroutine simulation, for the same structural reason
// TestConcurrentInitSameDirectory above is not one: SQLite's write lock is
// arbitrated by the OS via fcntl/flock on the file descriptor, a mechanism
// eight goroutines sharing one process -- and therefore one *sql.DB, one
// connection pool, one address space -- can never actually exercise, no
// matter how a sync.WaitGroup is arranged around them. Interlock learned
// this the hard way with an in-process race test that stayed green for the
// entire time a real cross-process lease bug was live; Phase 1's own
// stress-testing found a genuine 1-in-8 WAL-bootstrap race no goroutine test
// could have seen (01-05-SUMMARY.md). If this test is ever "simplified" into
// a WaitGroup over in-process calls, it stops proving anything about the
// ordinal allocator at all.
//
// 02-RESEARCH.md Pattern 2 measured this exact scenario at up to 8 real
// processes x 40 inserts against one parent (320 total) with zero
// SQLITE_BUSY and zero SQLITE_CONSTRAINT_UNIQUE hits: BEGIN IMMEDIATE blocks
// a second process's own BeginTx call until the first commits, so the two
// transactions are never concurrently in flight from the application's
// point of view. This test is a regression lock on that finding, not an
// investigation -- it is expected to pass cleanly, with zero retries
// observed, on every run; its job is to keep that true as AddItem evolves,
// not to manufacture contention this DSN structurally prevents. No
// time.Sleep anywhere: contention comes from starting all eight processes
// together, nothing else.
func TestConcurrentAddChildrenNoOrdinalCollision(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")

	if _, stderr, code := run(t, bin, repo, e, "init"); code != 0 {
		t.Fatalf("init: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}

	// `add [flags] <title>`, flags before the title -- the same stdlib
	// flag.Parse stopping-point constraint plan 07 documented and every
	// later CLI test in this package follows.
	epicOut, epicErr, epicCode := run(t, bin, repo, e, "add", "--type", "epic", "epic")
	if epicCode != 0 {
		t.Fatalf("add epic: exit code = %d, want 0 (stderr=%q)", epicCode, epicErr)
	}
	epicID := strings.TrimSpace(epicOut)
	if epicID == "" {
		t.Fatalf("add epic: stdout was empty, want a bare id")
	}

	const n = 8

	type proc struct {
		cmd    *exec.Cmd
		stdout bytes.Buffer
		stderr bytes.Buffer
	}
	procs := make([]*proc, n)

	// Start() all eight before Wait()-ing on any of them. Starting and
	// waiting one at a time would serialize them and make the rest of this
	// test vacuous -- there would be no real contention for the ordinal
	// allocator to survive.
	for i := 0; i < n; i++ {
		p := &proc{}
		cmd := exec.Command(bin, "add", "--parent", epicID, fmt.Sprintf("child-%d", i))
		cmd.Dir = repo
		cmd.Env = e
		cmd.Stdout = &p.stdout
		cmd.Stderr = &p.stderr
		p.cmd = cmd
		if err := cmd.Start(); err != nil {
			t.Fatalf("process %d failed to start: %v", i, err)
		}
		procs[i] = p
	}

	for i, p := range procs {
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("process %d (child-%d) exited non-zero: %v\nstdout=%q\nstderr=%q",
				i, i, err, p.stdout.String(), p.stderr.String())
		}
	}

	// busy_timeout=3000 plus BEGIN IMMEDIATE should make contention wait,
	// not fail -- so no process's stderr should mention lock contention at
	// all, matching TestConcurrentInitSameDirectory's identical assertion
	// above.
	for i, p := range procs {
		stderr := p.stderr.String()
		if strings.Contains(stderr, "SQLITE_BUSY") || strings.Contains(stderr, "database is locked") {
			t.Fatalf("process %d (child-%d) stderr mentions lock contention (want BEGIN IMMEDIATE + busy_timeout to make contention wait, not fail): %q", i, i, stderr)
		}
	}

	listOut, listErr, listCode := run(t, bin, repo, e, "list", "--parent", epicID, "--json")
	if listCode != 0 {
		t.Fatalf("list --parent %s --json: exit code = %d, want 0 (stderr=%q)", epicID, listCode, listErr)
	}

	var items []model.Item
	if err := json.Unmarshal([]byte(listOut), &items); err != nil {
		t.Fatalf("list --parent --json: json.Unmarshal(%q): %v", listOut, err)
	}

	if len(items) != n {
		t.Fatalf("list --parent %s --json returned %d items, want exactly %d", epicID, len(items), n)
	}

	// Set equality, not a count and not a maximum: two processes both
	// landing on ordinal 3 while nobody ever gets 5 would still pass a
	// "count is 8" or "max ordinal is 8" assertion -- exactly the collision
	// this test exists to catch.
	gotOrdinals := make(map[int]bool, n)
	gotIDs := make(map[string]bool, n)
	for _, it := range items {
		if it.Ordinal == nil {
			t.Fatalf("item %s has a nil ordinal, want one of 1..%d", it.ID, n)
		}
		gotOrdinals[*it.Ordinal] = true
		gotIDs[it.ID] = true
	}
	if len(gotOrdinals) != n {
		t.Fatalf("got %d distinct ordinals %v from %d items, want %d distinct values -- no gaps, no duplicates", len(gotOrdinals), gotOrdinals, len(items), n)
	}
	for i := 1; i <= n; i++ {
		if !gotOrdinals[i] {
			t.Fatalf("ordinal %d missing from the set %v -- want exactly {1..%d}", i, gotOrdinals, n)
		}
	}
	if len(gotIDs) != n {
		t.Fatalf("got %d distinct ids %v, want %d", len(gotIDs), gotIDs, n)
	}
	for i := 1; i <= n; i++ {
		wantID := fmt.Sprintf("%s.%d", epicID, i)
		if !gotIDs[wantID] {
			t.Fatalf("id %q missing from the set %v -- want exactly {%s.1 .. %s.%d}", wantID, gotIDs, epicID, epicID, n)
		}
	}
}

// TestConcurrentInitDifferentRepositoriesLeavesRegistryValid is GATE-04 for
// Phase 6: six real `keep init` processes, in six different directories,
// sharing one HOME, all Start()-ed before any is Wait()-ed on, contending
// over one ~/.rounce/repos.json.
//
// WHAT THIS ASSERTS, AND WHY IT STOPS THERE. registry.Save writes a temp
// file in the destination directory and renames it over repos.json. POSIX
// rename(2) guarantees any concurrent reader sees either the old file or the
// new file, never a partial write -- so the registry is never torn, never
// half-written, and never left in a state that breaks every future `keep
// repos` call. That is the guarantee this test checks.
//
// IT NOW ALSO ASSERTS THAT ALL SIX SURVIVE. That is a change of contract,
// recorded here because the old comment argued at length that it must not.
//
// The old reasoning was right about its premise and right to stop where it
// did: atomicity of the WRITE does not imply isolation of the
// read-modify-write sequence around it, so two processes registering two
// DIFFERENT repositories -- each having read before the other's rename
// landed -- had one rename discard the other's addition. Measured at
// 1-of-6 survivors, 10 runs out of 10 (06-03-SUMMARY.md). Asserting set
// equality against that design would have been testing a guarantee it did
// not make.
//
// internal/registry/lock.go now makes it. Register and Forget hold an
// exclusive lock file across Load AND Save, so the window that lost updates
// is closed rather than narrowed. Re-measured after the change: 6-of-6, 10
// runs out of 10, with no lock file left behind. rnc-245572.
//
// This now sits alongside TestConcurrentInitSameDirectory and
// TestConcurrentAddChildrenNoOrdinalCollision, which assert set equality
// from store-level constraints (UNIQUE(parent_id, ordinal),
// _txlock=immediate) rather than from a lock.
func TestConcurrentInitDifferentRepositoriesLeavesRegistryValid(t *testing.T) {
	bin := buildKeep(t, "")
	home := tempDir(t)
	e := env(home, "")

	const n = 6

	type proc struct {
		dir    string
		want   string
		cmd    *exec.Cmd
		stdout bytes.Buffer
		stderr bytes.Buffer
	}

	base := tempDir(t)
	procs := make([]*proc, n)
	wantPaths := make(map[string]bool, n)

	// Start() all six before Wait()-ing on any of them -- the same discipline
	// as TestConcurrentInitSameDirectory and
	// TestConcurrentAddChildrenNoOrdinalCollision above. Goroutine simulation
	// does not satisfy GATE-04.
	for i := 0; i < n; i++ {
		dir := filepath.Join(base, fmt.Sprintf("repo-%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		p := &proc{dir: dir, want: store.StorePathIn(dir)}
		cmd := exec.Command(bin, "init")
		cmd.Dir = dir
		cmd.Env = e
		cmd.Stdout = &p.stdout
		cmd.Stderr = &p.stderr
		p.cmd = cmd
		if err := cmd.Start(); err != nil {
			t.Fatalf("process %d failed to start: %v", i, err)
		}
		procs[i] = p
		wantPaths[p.want] = true
	}

	for i, p := range procs {
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("process %d exited non-zero: %v\nstdout=%q\nstderr=%q",
				i, err, p.stdout.String(), p.stderr.String())
		}
	}

	envelope := readRegistry(t, home)
	if envelope.Version != registry.Version {
		t.Fatalf("registry version = %d, want %d", envelope.Version, registry.Version)
	}

	seenPaths := make(map[string]bool, len(envelope.Repos))
	for _, r := range envelope.Repos {
		if r.Path == "" {
			t.Fatalf("registry row has empty path: %+v", r)
		}
		if r.Name == "" {
			t.Fatalf("registry row has empty name: %+v", r)
		}
		if _, err := time.Parse(model.TimeFormat, r.LastSeen); err != nil {
			t.Fatalf("registry row last_seen = %q does not parse with model.TimeFormat: %v", r.LastSeen, err)
		}
		if seenPaths[r.Path] {
			t.Fatalf("registry has a duplicate path: %q", r.Path)
		}
		seenPaths[r.Path] = true
		if !wantPaths[r.Path] {
			t.Fatalf("registry row path %q is not one of the six expected store paths %v", r.Path, wantPaths)
		}
	}

	// Report the observed survivor count on every run, pass or fail -- it is
	// the only visibility into how wide the read-modify-write window
	// actually is on this machine.
	t.Logf("TestConcurrentInitDifferentRepositoriesLeavesRegistryValid: %d/%d registrations survived", len(envelope.Repos), n)

	// Every one of the six must survive. Before the lock this was 1-of-6,
	// reproducibly, so a regression here is not a flake to retry -- it is
	// the read-modify-write window reopening.
	if len(envelope.Repos) != n {
		var got []string
		for _, r := range envelope.Repos {
			got = append(got, r.Path)
		}
		t.Fatalf("%d/%d registrations survived, want all %d -- the Load/Save window has reopened (internal/registry/lock.go).\nsurvivors: %v", len(envelope.Repos), n, n, got)
	}
	for path := range wantPaths {
		if !seenPaths[path] {
			t.Errorf("registration for %s was lost", path)
		}
	}

	// The lock must not outlive the processes that took it.
	if _, err := os.Stat(filepath.Join(filepath.Dir(registry.PathIn(home)), "repos.json.lock")); !os.IsNotExist(err) {
		t.Errorf("a lock file survived all six processes (stat err = %v)", err)
	}

	registryDir := filepath.Dir(registry.PathIn(home))
	entries, err := os.ReadDir(registryDir)
	if err != nil {
		t.Fatalf("reading %s: %v", registryDir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+registry.FileName+".tmp-") {
			t.Fatalf("%s contains a leftover temp file: %s", registryDir, entry.Name())
		}
	}
}
