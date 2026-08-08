package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gshepptech/rounce/internal/store"
)

// Version is the registry envelope's schema version. It exists so a future
// field does not require guessing at an unversioned array -- Load refuses a
// version it does not understand rather than silently misreading it.
const Version = 1

// FileName is the registry's basename inside the home store directory
// (store.StoreDirName). The registry lives beside ~/.rounce/db.sqlite, which
// `rounce init --global` already writes.
const FileName = "repos.json"

// Repo is one registered store.
type Repo struct {
	Path     string `json:"path"`      // absolute path to db.sqlite
	Name     string `json:"name"`      // the repository directory's base name
	LastSeen string `json:"last_seen"` // model.TimeFormat
}

// Envelope is the whole file.
type Envelope struct {
	Version int    `json:"version"`
	Repos   []Repo `json:"repos"`
}

// empty returns the canonical empty envelope every Load failure path
// returns, so no failure path can accidentally diverge from another.
func empty() Envelope {
	return Envelope{Version: Version}
}

// PathIn returns home's registry file path, spelled through
// store.StoreDirName (never a literal ".rounce") so the registry and the
// store cannot drift onto two different directory names.
func PathIn(home string) string {
	return filepath.Join(home, store.StoreDirName, FileName)
}

// NameFor derives a repository's display name from its store path: the base
// name of the directory containing .rounce/, or of the containing directory
// when the store is not in a .rounce/ layout at all.
//
// 06-CONTEXT.md decision 2 phrases this as "the directory two levels above
// db.sqlite", which is exactly right for the standard <repo>/.rounce/
// db.sqlite layout and wrong for `rounce init --db /tmp/scratch/x.sqlite`,
// where two levels up is "/tmp". Checking for the .rounce/ component first
// honours the decision's intent (the repository directory's base name) in
// both layouts. The store file's own name is never used: it is always
// "db.sqlite" and useless as a label.
func NameFor(storePath string) string {
	dir := filepath.Dir(storePath)
	if filepath.Base(dir) == store.StoreDirName {
		dir = filepath.Dir(dir)
	}
	return filepath.Base(dir)
}

// Load reads home's registry.
//
// It returns BOTH an empty, usable envelope AND a non-nil error whenever the
// file is present but unusable -- malformed JSON, an unreadable file, an
// unrecognised version. That double return is deliberate, not sloppy:
// 06-CONTEXT.md decision 2 makes a broken registry never fatal, so every
// caller reports err on stderr and proceeds with the envelope it was also
// handed. A caller that returned early on err would stop `rounce ready` from
// working in the repository the user is standing in, which is the exact
// failure that decision forbids.
//
// A MISSING file is not an error at all: (Envelope{Version: Version}, nil).
// Nothing has been registered yet is a normal state, not a fault.
func Load(home string) (Envelope, error) {
	b, err := os.ReadFile(PathIn(home))
	if err != nil {
		if os.IsNotExist(err) {
			return empty(), nil
		}
		return empty(), fmt.Errorf("registry: read %s: %w", PathIn(home), err)
	}

	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return empty(), fmt.Errorf("registry: parse %s: %w", PathIn(home), err)
	}

	if env.Version != Version {
		return empty(), fmt.Errorf("registry: %s has version %d, want %d", PathIn(home), env.Version, Version)
	}

	return env, nil
}

// Save writes env to home's registry atomically: a temp file created IN THE
// DESTINATION DIRECTORY, synced, closed, then renamed over repos.json.
//
// The temp file must be in the destination directory, never os.TempDir():
// os.Rename fails with EXDEV across filesystems, and ~/.rounce and /tmp are
// not guaranteed to be the same mount. POSIX rename(2) guarantees a
// concurrent reader observes either the old file or the new file, never a
// partial write -- which is what makes two racing `rounce init` runs unable
// to leave behind a registry that breaks every future `repos` call.
//
// What this does NOT guarantee, and must not be claimed: isolation of the
// read-modify-write sequence AROUND the rename. Two processes registering
// two DIFFERENT new repos concurrently, each having read the registry before
// the other's rename landed, will have one rename completely replace the
// other's addition -- a lost update, not a corrupt file. 06-CONTEXT.md
// decision 2 accepts that cost explicitly ("a half-written registry that
// breaks every future `repos` call is a much worse failure than a lost
// registration"). Plan 03's GATE-04 test asserts the guarantee this actually
// makes, and deliberately not the stronger one.
func Save(home string, env Envelope) error {
	env.Version = Version

	sort.Slice(env.Repos, func(i, j int) bool { return env.Repos[i].Path < env.Repos[j].Path })

	dir := filepath.Dir(PathIn(home))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("registry: mkdir %s: %w", dir, err)
	}

	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: marshal: %w", err)
	}
	b = append(b, '\n')

	f, err := os.CreateTemp(dir, "."+FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("registry: create temp file in %s: %w", dir, err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("registry: write %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("registry: sync %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("registry: close %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, PathIn(home)); err != nil {
		return fmt.Errorf("registry: rename %s to %s: %w", tmpPath, PathIn(home), err)
	}
	return nil
}

// Register adds storePath to home's registry, or updates its last_seen if it
// is already there. It is idempotent by requirement (06-CONTEXT.md decision
// 3): re-registering never appends a second row, so `rounce init` run twice
// in one repository leaves one entry, not two.
//
// storePath is stored exactly as given. Register does not absolutize it --
// filepath.Abs consults the process's working directory, which is a property
// of the caller, not of the registry. Every caller in cmd/rounce passes an
// already-absolute path; matching here is plain string equality on that
// value.
func Register(home, storePath, name, lastSeen string) error {
	// Load's error is deliberately ignored here, not propagated: the
	// never-fatal contract means a corrupted registry must not make every
	// future `rounce init` fail to register. Register proceeds with the
	// empty envelope Load hands back on any failure, effectively rebuilding
	// the registry from this one entry rather than refusing to write.
	env, _ := Load(home)

	found := false
	for i := range env.Repos {
		if env.Repos[i].Path == storePath {
			env.Repos[i].Name = name
			env.Repos[i].LastSeen = lastSeen
			found = true
			break
		}
	}
	if !found {
		env.Repos = append(env.Repos, Repo{Path: storePath, Name: name, LastSeen: lastSeen})
	}

	return Save(home, env)
}

// Forget removes storePath from home's registry and reports whether a row
// was actually there. It never touches the store itself -- forgetting is not
// deleting, and the CLI's message says so.
func Forget(home, storePath string) (bool, error) {
	// Same never-fatal reasoning as Register: proceed with the empty
	// envelope on a Load failure rather than propagating it.
	env, _ := Load(home)

	idx := -1
	for i := range env.Repos {
		if env.Repos[i].Path == storePath {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false, nil
	}
	env.Repos = append(env.Repos[:idx], env.Repos[idx+1:]...)

	if err := Save(home, env); err != nil {
		return true, err
	}
	return true, nil
}
