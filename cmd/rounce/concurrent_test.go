package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// TestConcurrentInitSameDirectory is the GATE-04 proof: four real OS
// processes, all Start()-ed before any of them is Wait()-ed on, contending
// over `rounce init` against one empty shared directory.
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
	bin := buildRounce(t, "concurrent-test")
	dir := t.TempDir()
	home := t.TempDir()
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
// eight real `rounce add --parent <epic>` processes, all Start()-ed before
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
	bin := buildRounce(t, "")
	repo := t.TempDir()
	home := t.TempDir()
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
