// Package interlock reads what Interlock is holding, so rounce's queue can
// tell the truth about what is workable right now.
//
// # Why shelling out, and not a Go import
//
// rounce and Interlock answer different questions -- rounce says what needs
// doing, Interlock says who is doing what and where -- and both roadmaps
// state that neither depends on the other to be useful. Importing
// Interlock's packages would make that false in the one direction that
// matters: rounce would stop building without it.
//
// So this package shells out to the `interlock` binary and parses its
// documented `status --json` envelope. The coupling is a CLI contract with
// a version integer, which either side can evolve without recompiling the
// other, rather than a Go API that pins both to one module version.
//
// # Why every failure is silence
//
// Interlock missing, Interlock failing, Interlock slow, Interlock speaking
// a version this does not understand: all of them mean the same thing here,
// which is that rounce has nothing to add to its own answer. `ready` then
// reports exactly what it reported before this package existed.
//
// The alternative -- surfacing an error -- would make `rounce ready` fail in
// every repository that does not use Interlock, which is most of them. That
// is the same never-fatal rule internal/registry follows for an unreadable
// store, and the same one plugin/scripts/session-queue.py follows when
// rounce itself is not installed.
package interlock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gshepptech/rounce/internal/model"
)

// SupportedVersion is the `status --json` envelope this package understands.
// A different version is treated as unavailable rather than parsed
// optimistically: a field that changed meaning would produce a queue that
// hides workable items, which is worse than a queue that filters nothing.
const SupportedVersion = 1

// fetchTimeout bounds the subprocess. `ready` is on the hot path -- the
// SessionStart hook and the MCP handshake both call it -- so a hung
// Interlock must cost a bounded pause, never the session.
const fetchTimeout = 2 * time.Second

// Status is Interlock's `status --json` envelope, narrowed to what rounce
// reads. Fields it does not use are deliberately absent rather than carried
// along: a struct that mirrors someone else's output invites code that
// depends on parts of it nobody meant to promise.
type Status struct {
	Version  int       `json:"version"`
	Repo     string    `json:"repo"`
	Sessions []Session `json:"sessions"`
	Leases   []Lease   `json:"leases"`
	Errors   []string  `json:"errors"`
}

// Session is one agent Interlock has observed.
type Session struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Team      string `json:"team"`
	CWD       string `json:"cwd"`
	RunID     string `json:"run_id"`
	LastSeen  string `json:"last_seen"`
	ExpiresAt string `json:"expires_at"`
	Expired   bool   `json:"expired"`
}

// Lease is one file held right now. Path is repo-relative, which is what
// rounce compares against items.paths.
type Lease struct {
	Path      string `json:"path"`
	Agent     string `json:"agent"`
	Team      string `json:"team"`
	SessionID string `json:"session_id"`
	Implicit  bool   `json:"implicit"`
	Acquired  string `json:"acquired"`
	ExpiresAt string `json:"expires_at"`
}

// Collision is one item excluded from `ready` because a path it declares is
// held. It names the path and the holder, because "this is not workable" is
// only actionable if you can see who to wait for -- the same reason
// `rounce blocked` names the blocking item rather than just omitting it.
type Collision struct {
	Item  model.Item
	Path  string
	Lease Lease
}

// Fetch runs `interlock status --json` in dir and returns what it said.
//
// The bool reports whether the answer is usable. It is false, with no error,
// whenever Interlock is absent, failed, timed out, spoke an unsupported
// version, or produced output that will not parse -- see the package comment
// for why none of those is worth surfacing.
func Fetch(ctx context.Context, dir string) (Status, bool) {
	bin, ok := binary()
	if !ok {
		return Status{}, false
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "status", "--json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return Status{}, false
	}

	var st Status
	if err := json.Unmarshal(out, &st); err != nil {
		return Status{}, false
	}
	if st.Version != SupportedVersion {
		return Status{}, false
	}
	return st, true
}

// EnvBinary names the environment variable that overrides how the Interlock
// binary is found.
//
//	unset  -- look for `interlock` on $PATH (the normal case)
//	"off"  -- do not consult Interlock at all
//	other  -- use this exact path
//
// The "off" value is not only for tests, though it is what makes them
// deterministic: without it, whether a test sees a filtered queue depends on
// whether the developer running it happens to have Interlock installed,
// which is precisely the environment leak cmd/rounce's env() helper exists
// to prevent. It is also the answer for anyone who wants the two tools
// installed but not joined.
const EnvBinary = "ROUNCE_INTERLOCK"

// binary resolves the Interlock binary, honouring EnvBinary.
func binary() (string, bool) {
	switch v := os.Getenv(EnvBinary); {
	case v == "off":
		return "", false
	case v != "":
		return v, true
	}
	bin, err := exec.LookPath("interlock")
	if err != nil {
		return "", false
	}
	return bin, true
}

// SplitPaths reads an item's paths field.
//
// The format is comma-separated, repo-relative, whitespace trimmed. It is
// defined HERE, at the first thing that ever reads the column: v1 shipped
// items.paths deliberately unread ("rounce carries two unread columns for
// the eventual join"), so nothing until now had an opinion. Comma-separated
// because the CLI already takes `--paths` as one string, and splitting on
// commas is the reading a person writing that flag would expect.
func SplitPaths(paths *string) []string {
	if paths == nil {
		return nil
	}
	var out []string
	for _, p := range strings.Split(*paths, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Exclude partitions items into those still workable and those held.
//
// It is one function with two callers -- `rounce ready` and the dashboard's
// queue view -- for the same reason internal/store's blockedTreeCTE is one
// constant with five: two implementations of "is this held" would drift, and
// the drift would be invisible until the board and the CLI disagreed about
// what is workable. DASH-07's agreement test exists because that already
// almost happened once.
//
// An item with no paths is never excluded. Declaring no paths means rounce
// does not know what the work touches, and guessing that it collides would
// hide workable items -- the failure direction this must not take.
func Exclude(items []model.Item, st Status) ([]model.Item, []Collision) {
	held := make(map[string]Lease, len(st.Leases))
	for _, l := range st.Leases {
		held[l.Path] = l
	}

	kept := make([]model.Item, 0, len(items))
	var out []Collision
	for _, it := range items {
		hit, lease, found := firstHeld(SplitPaths(it.Paths), held)
		if !found {
			kept = append(kept, it)
			continue
		}
		out = append(out, Collision{Item: it, Path: hit, Lease: lease})
	}
	return kept, out
}

// firstHeld returns the first of paths that is held, if any. First rather
// than all: an item is equally unworkable whichever of its files is taken,
// and naming one holder is what a reader acts on.
func firstHeld(paths []string, held map[string]Lease) (string, Lease, bool) {
	for _, p := range paths {
		if l, ok := held[p]; ok {
			return p, l, true
		}
	}
	return "", Lease{}, false
}

// Release releases one session's file holds by running Interlock's own
// command, confirmation argument included.
//
// rounce does not reach into Interlock's lease files to do this. Releasing
// someone else's hold is a guard rail Interlock built deliberately -- it
// takes the session id twice on purpose -- and a second implementation here
// would be rounce deciding that guard rail is optional.
func Release(ctx context.Context, session string) error {
	bin, ok := binary()
	if !ok {
		return errors.New("interlock is not installed here, so there are no holds to release")
	}
	if strings.TrimSpace(session) == "" {
		return errors.New("no session named")
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "lease", "release", session, "--confirm", session)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("interlock lease release: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Run starts an agent on one item, through Interlock's own launcher.
//
// rounce does not start agents -- that is a stated limit, not an oversight,
// and this does not change it. Interlock owns process lifecycle, so the
// board asks Interlock rather than growing a supervisor of its own.
//
// --no-follow because the caller is an HTTP handler: the run outlives the
// request, and Interlock is the thing watching it.
func Run(ctx context.Context, agent, item, dir string) error {
	bin, ok := binary()
	if !ok {
		return errors.New("interlock is not installed here, so there is nothing to start an agent with")
	}
	switch agent {
	case "claude", "codex":
	default:
		return fmt.Errorf("unknown agent %q", agent)
	}
	if strings.TrimSpace(item) == "" {
		return errors.New("no item named")
	}

	// Longer than fetchTimeout: launching supervises a process rather than
	// reading a file, and two seconds is not a fair budget for that.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "run", agent, "--item", item, "--cwd", dir, "--no-follow")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("interlock run: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
