package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

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
