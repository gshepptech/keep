package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/registry"
	"github.com/gshepptech/rounce/internal/store"
)

// makeRepo initialises a real store in <root>/<name> through the real binary
// (so init's own registration happens exactly as it does in production) and
// adds n plain tickets. It returns the absolute store path.
func makeRepo(t *testing.T, bin, root, home, name string, n int) string {
	t.Helper()

	repoDir := filepath.Join(root, name)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", repoDir, err)
	}
	e := env(home, "")

	stdout, stderr, code := run(t, bin, repoDir, e, "init")
	if code != 0 {
		t.Fatalf("init in %s: exit code = %d, want 0 (stderr=%q)", repoDir, code, stderr)
	}
	storePath := strings.TrimSpace(stdout)

	for i := 0; i < n; i++ {
		title := fmt.Sprintf("%s ticket %d", name, i)
		if _, stderr, code := run(t, bin, repoDir, e, "add", title); code != 0 {
			t.Fatalf("add in %s: exit code = %d, want 0 (stderr=%q)", repoDir, code, stderr)
		}
	}
	return storePath
}

// TestReposListsCountsAndLastActivity proves the core of REPO-02: two real
// repos with different, known item counts show up in --json with the right
// items/ready per row and a last_activity that parses with model.TimeFormat,
// and the rendered output carries a row whose exact first field is the
// repository name -- never a substring match.
func TestReposListsCountsAndLastActivity(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	makeRepo(t, bin, root, home, "alpha", 2)
	makeRepo(t, bin, root, home, "beta", 5)

	stdout, stderr, code := run(t, bin, root, e, "repos", "--json")
	if code != 0 {
		t.Fatalf("repos --json: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	var out reposJSON
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decoding repos --json output: %v\nstdout=%q", err, stdout)
	}
	if len(out.Repos) != 2 {
		t.Fatalf("repos --json: got %d rows, want 2: %+v", len(out.Repos), out.Repos)
	}

	byName := map[string]reposRow{}
	for _, r := range out.Repos {
		byName[r.Name] = r
	}
	alpha, ok := byName["alpha"]
	if !ok {
		t.Fatalf("repos --json: no row named alpha: %+v", out.Repos)
	}
	if alpha.Items != 2 || alpha.Ready != 2 {
		t.Fatalf("alpha row = %+v, want items=2 ready=2", alpha)
	}
	beta, ok := byName["beta"]
	if !ok {
		t.Fatalf("repos --json: no row named beta: %+v", out.Repos)
	}
	if beta.Items != 5 || beta.Ready != 5 {
		t.Fatalf("beta row = %+v, want items=5 ready=5", beta)
	}
	for _, r := range out.Repos {
		if r.LastActivity == "" {
			t.Fatalf("row %q: last_activity is empty, want non-empty", r.Name)
		}
		if _, err := time.Parse(model.TimeFormat, r.LastActivity); err != nil {
			t.Fatalf("row %q: last_activity = %q does not parse with model.TimeFormat: %v", r.Name, r.LastActivity, err)
		}
	}

	// Rendered output: match a row's exact first field, never a substring.
	stdoutRendered, stderrRendered, codeRendered := run(t, bin, root, e, "repos")
	if codeRendered != 0 {
		t.Fatalf("repos: exit code = %d, want 0 (stderr=%q)", codeRendered, stderrRendered)
	}
	var foundAlpha bool
	for _, line := range strings.Split(strings.TrimRight(stdoutRendered, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "alpha" {
			foundAlpha = true
		}
	}
	if !foundAlpha {
		t.Fatalf("repos: rendered output has no row whose exact first field is %q: %q", "alpha", stdoutRendered)
	}
}

// TestReposShowsErrorRowForBrokenStoreAndExitsZero proves REPO-05's visible
// half: three registered repos, one's store file deleted, still all appear,
// the deleted one carrying a non-empty error and zeroed counts, the other
// two carrying their real counts, and the command exits 0 regardless.
func TestReposShowsErrorRowForBrokenStoreAndExitsZero(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	makeRepo(t, bin, root, home, "alpha", 1)
	brokenPath := makeRepo(t, bin, root, home, "broken", 3)
	makeRepo(t, bin, root, home, "gamma", 2)

	if err := os.Remove(brokenPath); err != nil {
		t.Fatalf("removing store at %s: %v", brokenPath, err)
	}

	stdout, stderr, code := run(t, bin, root, e, "repos", "--json")
	if code != 0 {
		t.Fatalf("repos --json after deleting a store: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	var out reposJSON
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decoding repos --json output: %v\nstdout=%q", err, stdout)
	}
	if len(out.Repos) != 3 {
		t.Fatalf("repos --json: got %d rows, want 3: %+v", len(out.Repos), out.Repos)
	}

	byName := map[string]reposRow{}
	for _, r := range out.Repos {
		byName[r.Name] = r
	}
	broken, ok := byName["broken"]
	if !ok {
		t.Fatalf("repos --json: no row named broken: %+v", out.Repos)
	}
	if broken.Error == "" {
		t.Fatalf("broken row = %+v, want a non-empty error", broken)
	}
	if broken.Items != 0 || broken.Ready != 0 {
		t.Fatalf("broken row = %+v, want zeroed counts", broken)
	}
	alpha, ok := byName["alpha"]
	if !ok || alpha.Error != "" || alpha.Items != 1 || alpha.Ready != 1 {
		t.Fatalf("alpha row = %+v, want items=1 ready=1 no error", alpha)
	}
	gamma, ok := byName["gamma"]
	if !ok || gamma.Error != "" || gamma.Items != 2 || gamma.Ready != 2 {
		t.Fatalf("gamma row = %+v, want items=2 ready=2 no error", gamma)
	}
}

// TestReposAddAcceptsDirectoryOrStorePath proves `repos add <dir>` and
// `repos add <dir>/.rounce/db.sqlite` normalise to the identical path, so
// adding both leaves exactly one registry row.
func TestReposAddAcceptsDirectoryOrStorePath(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	repoDir := filepath.Join(root, "myrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, store.StoreDirName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	storePath := filepath.Join(repoDir, store.StoreDirName, store.StoreFileName)

	stdout1, stderr1, code1 := run(t, bin, root, e, "repos", "add", repoDir)
	if code1 != 0 {
		t.Fatalf("repos add <dir>: exit code = %d, want 0 (stderr=%q)", code1, stderr1)
	}
	if strings.TrimSpace(stdout1) != storePath {
		t.Fatalf("repos add <dir>: stdout = %q, want %q", stdout1, storePath)
	}

	stdout2, stderr2, code2 := run(t, bin, root, e, "repos", "add", storePath)
	if code2 != 0 {
		t.Fatalf("repos add <store file>: exit code = %d, want 0 (stderr=%q)", code2, stderr2)
	}
	if strings.TrimSpace(stdout2) != storePath {
		t.Fatalf("repos add <store file>: stdout = %q, want %q", stdout2, storePath)
	}

	envelope := readRegistry(t, home)
	if len(envelope.Repos) != 1 {
		t.Fatalf("registry has %d repos, want exactly 1 (the two forms should normalise to one row): %+v", len(envelope.Repos), envelope.Repos)
	}
	if envelope.Repos[0].Path != storePath {
		t.Fatalf("registry repo path = %q, want %q", envelope.Repos[0].Path, storePath)
	}
}

// TestReposAddDoesNotValidateTheStore proves `repos add` on a directory with
// no store at all still exits 0 and registers the row -- and that `rounce
// repos` then shows that row with an error rather than refusing to list it.
func TestReposAddDoesNotValidateTheStore(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	repoDir := filepath.Join(root, "empty-repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	stdout, stderr, code := run(t, bin, root, e, "repos", "add", repoDir)
	if code != 0 {
		t.Fatalf("repos add <dir with no store>: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	storePath := strings.TrimSpace(stdout)

	envelope := readRegistry(t, home)
	if len(envelope.Repos) != 1 || envelope.Repos[0].Path != storePath {
		t.Fatalf("registry = %+v, want exactly one row at %q", envelope.Repos, storePath)
	}

	reposOut, reposErr, reposCode := run(t, bin, root, e, "repos", "--json")
	if reposCode != 0 {
		t.Fatalf("repos --json: exit code = %d, want 0 (stderr=%q)", reposCode, reposErr)
	}
	var out reposJSON
	if err := json.Unmarshal([]byte(reposOut), &out); err != nil {
		t.Fatalf("decoding repos --json output: %v\nstdout=%q", err, reposOut)
	}
	if len(out.Repos) != 1 || out.Repos[0].Error == "" {
		t.Fatalf("repos --json = %+v, want exactly one row with a non-empty error", out.Repos)
	}
}

// TestReposForgetRemovesRowAndLeavesStoreOnDisk proves forgetting removes
// the registry row, leaves db.sqlite untouched on disk, says so in its own
// message, and exits 1 naming the path on a second, now-unregistered call.
func TestReposForgetRemovesRowAndLeavesStoreOnDisk(t *testing.T) {
	bin := buildRounce(t, "")
	root := t.TempDir()
	home := t.TempDir()
	e := env(home, "")

	storePath := makeRepo(t, bin, root, home, "solo", 1)

	stdout, stderr, code := run(t, bin, root, e, "repos", "forget", storePath)
	if code != 0 {
		t.Fatalf("repos forget: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, storePath) {
		t.Fatalf("repos forget: stdout = %q, want it to name the store path", stdout)
	}
	if !strings.Contains(strings.ToLower(stdout), "not touched") {
		t.Fatalf("repos forget: stdout = %q, want it to say the store itself was not touched", stdout)
	}

	envelope := readRegistry(t, home)
	if len(envelope.Repos) != 0 {
		t.Fatalf("registry after forget = %+v, want empty", envelope.Repos)
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("store file at %s: %v, want it to still exist", storePath, err)
	}

	_, stderr2, code2 := run(t, bin, root, e, "repos", "forget", storePath)
	if code2 != 1 {
		t.Fatalf("repos forget on an unregistered path: exit code = %d, want 1 (stderr=%q)", code2, stderr2)
	}
	if !strings.Contains(stderr2, storePath) {
		t.Fatalf("repos forget on an unregistered path: stderr = %q, want it to name the path", stderr2)
	}
}

// TestReposMalformedRegistryIsNotFatal proves 06-CONTEXT.md decision 2's
// never-fatal contract end to end: a malformed ~/.rounce/repos.json makes
// `rounce repos` exit 0 with a warning on stderr and no rows on stdout --
// and, the more important half, does not stop `rounce ready` from working
// in the repository the user is standing in.
func TestReposMalformedRegistryIsNotFatal(t *testing.T) {
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

	stdout, stderr, code := run(t, bin, root, e, "repos")
	if code != 0 {
		t.Fatalf("repos with a malformed registry: exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("repos with a malformed registry: stdout = %q, want no rows", stdout)
	}
	if !strings.Contains(strings.ToLower(stderr), "warning") {
		t.Fatalf("repos with a malformed registry: stderr = %q, want a warning", stderr)
	}

	// The second, more important half: rounce ready must still work in a
	// real repository despite the malformed HOME registry.
	repoDir := t.TempDir()
	if _, stderr2, code2 := run(t, bin, repoDir, e, "init"); code2 != 0 {
		t.Fatalf("init in a real repository despite a malformed home registry: exit code = %d, want 0 (stderr=%q)", code2, stderr2)
	}
	if _, stderr3, code3 := run(t, bin, repoDir, e, "add", "a real ticket"); code3 != 0 {
		t.Fatalf("add in a real repository despite a malformed home registry: exit code = %d, want 0 (stderr=%q)", code3, stderr3)
	}
	readyOut, readyErr, readyCode := run(t, bin, repoDir, e, "ready")
	if readyCode != 0 {
		t.Fatalf("ready in a real repository despite a malformed home registry: exit code = %d, want 0 (stdout=%q stderr=%q)", readyCode, readyOut, readyErr)
	}
	if !strings.Contains(readyOut, "a real ticket") {
		t.Fatalf("ready output = %q, want it to contain the added ticket's title", readyOut)
	}
}
