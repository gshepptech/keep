// Package interlock reads what Interlock is holding, so keep's queue can
// tell the truth about what is workable right now.
//
// # Why shelling out, and not a Go import
//
// keep and Interlock answer different questions. keep says what needs
// doing; Interlock says who is doing what and where. Each is useful without
// the other, and importing Interlock's packages would break that in the
// direction that matters: keep would stop building without it.
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
// which is that keep has nothing to add to its own answer. `ready` then
// reports exactly what it would without Interlock installed.
//
// Surfacing an error instead would make `keep ready` fail in every
// repository that does not use Interlock, which is most of them. That
// is the same never-fatal rule internal/registry follows for an unreadable
// store, and the same one plugin/scripts/session-queue.py follows when
// keep itself is not installed.
package interlock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// SupportedVersion is the `status --json` envelope this package understands.
// A different version is treated as unavailable rather than parsed
// optimistically: a field that changed meaning would produce a queue that
// hides workable items, which is worse than a queue that filters nothing.
const SupportedVersion = 1

// fetchTimeout bounds the subprocess. `ready` is on the hot path, since the
// SessionStart hook and the MCP handshake both call it, so a hung Interlock
// must cost a bounded pause, never the session.
const fetchTimeout = 2 * time.Second

// Status is Interlock's `status --json` envelope, narrowed to what keep
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
// keep compares against items.paths.
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
// only actionable if you can see who to wait for. `keep blocked` names the
// blocking item rather than just omitting it for the same reason.
type Collision struct {
	Item  model.Item
	Path  string
	Lease Lease
}

// Fetch runs `interlock status --json` in dir and returns what it said.
//
// The bool reports whether the answer is usable. It is false, with no error,
// whenever Interlock is absent, failed, timed out, spoke an unsupported
// version, or produced output that will not parse. The package comment
// explains why none of those is worth surfacing.
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
//	unset   look for `interlock` on $PATH (the normal case)
//	"off"   do not consult Interlock at all
//	other   use this exact path
//
// "off" makes tests deterministic: without it, whether a test sees a
// filtered queue depends on whether the developer running it has
// Interlock installed, which is the environment leak cmd/keep's env()
// helper exists to prevent. It also serves anyone who wants the two tools
// installed but not joined.
const EnvBinary = "KEEP_INTERLOCK"

// binary resolves the Interlock binary, honouring EnvBinary.
//
// Read through store.Env rather than os.Getenv so that ROUNCE_INTERLOCK
// still works: this is the variable a person sets to "off" to keep the two
// tools unjoined, and having it quietly stop meaning that after a rename
// would silently rejoin them.
func binary() (string, bool) {
	switch v := store.Env("INTERLOCK"); {
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
// defined HERE, at the only thing that reads the column. Comma-separated
// because the CLI takes `--paths` as one string, and splitting on
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
// It is one function with two callers, `keep ready` and the dashboard's
// queue view, for the same reason internal/store's blockedTreeCTE is one
// constant with five: two implementations of "is this held" would drift,
// and the drift would stay invisible until the board and the CLI disagreed
// about what is workable. The dashboard's agreement test guards that.
//
// An item with no paths is never excluded. Declaring no paths means keep
// does not know what the work touches, and guessing that it collides would
// hide workable items, which is the worse way to be wrong.
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
// keep does not reach into Interlock's lease files to do this. Releasing
// someone else's hold is a deliberate guard rail in Interlock, which takes
// the session id twice on purpose, and a second implementation here would
// make that guard rail optional.
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
// keep does not start agents itself. Interlock owns process lifecycle, so
// the board asks Interlock rather than growing a supervisor of its own.
//
// --no-follow because the caller is an HTTP handler: the run outlives the
// request, and Interlock is the thing watching it.
func Run(ctx context.Context, agent, item, dir string) error {
	return run(ctx, agent, dir, []string{"--item", item}, "")
}

// Prompt starts an agent on a free-text instruction rather than an item.
//
// The queue is the normal way in: an item carries a brief, acceptance
// criteria and the files it touches, and an agent started from one begins
// with all of that. A prompt has none of it, so this is for the thing you
// want done now and would not have written a ticket for. Interlock still
// supervises it, and Interlock's own guard still applies: a prompt cannot
// talk its way past a lease.
func Prompt(ctx context.Context, agent, text, dir string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("no prompt given")
	}
	return run(ctx, agent, dir, nil, text)
}

// run is the shared launcher.
//
// flags are trusted, built here. prompt is not, because it is whatever a
// person typed into a web form, so it goes last, behind a "--" sentinel,
// and every flag goes in front of it.
//
// Without the sentinel a prompt beginning with a dash is parsed as flags:
// --env and --model become injectable, and the prompt itself is discarded,
// so the run does something other than what was asked. Quoting cannot fix
// that from this side, because argv carries no quoting.
func run(ctx context.Context, agent, dir string, flags []string, prompt string) error {
	bin, ok := binary()
	if !ok {
		return errors.New("interlock is not installed here, so there is nothing to start an agent with")
	}
	switch agent {
	case "claude", "codex":
	default:
		return fmt.Errorf("unknown agent %q", agent)
	}

	// Longer than fetchTimeout: launching supervises a process rather than
	// reading a file, and two seconds is not a fair budget for that.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	full := append([]string{"run", agent}, flags...)
	full = append(full, "--no-follow")
	if prompt != "" {
		full = append(full, "--", prompt)
	}
	cmd := exec.CommandContext(ctx, bin, full...)

	// The working directory is set on the process, not passed as --cwd:
	// Interlock's --cwd is relative to the repository it resolves, so an
	// absolute path is rejected outright. Running IN the directory lets it
	// resolve the repository the same way every other command does.
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("interlock run: %s", strings.TrimSpace(string(out)))
	}

	// --no-follow returns as soon as the supervisor is up, so a zero exit
	// means "launched", not "working". A run that is going to fail on
	// configuration (not logged in, no such model, a bad hook) does so
	// within a second or two, and reporting those as started would announce
	// an outcome nobody checked.
	//
	// So give it a moment, then ask. A run still alive after that is
	// running, and its outcome belongs in Interlock, not in a redirect.
	id := runIDFrom(string(out))
	if id == "" {
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(settleWindow):
	}
	if state, why := runState(ctx, bin, dir, id); state == "failed" {
		return fmt.Errorf("the agent started and stopped straight away: %s", why)
	}
	return nil
}

// settleWindow is how long to wait before believing a run is under way.
// Long enough to catch a configuration failure, short enough that a person
// who pressed a button is not left watching a spinner.
const settleWindow = 2500 * time.Millisecond

// runIDFrom finds the run id `interlock run` prints.
func runIDFrom(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if _, after, ok := strings.Cut(line, "run: "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// runState asks Interlock what became of a run.
func runState(ctx context.Context, bin, dir, id string) (state, why string) {
	cmd := exec.CommandContext(ctx, bin, "runs", "show", id)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "state:"); ok {
			state = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "outcome:"); ok {
			why = strings.TrimSpace(v)
		}
	}
	if why == "" {
		why = "see interlock runs show " + id
	}
	return state, why
}

// Watches reports whether Interlock is wired into this repository.
//
// It asks Interlock, via `doctor`, which exits 0 when the hooks are in
// place and 1 when they are not. Two cheaper-looking checks both give the
// wrong answer:
//
//   - "is the binary installed" answers about the machine, not the
//     repository. `status` succeeds from a directory Interlock has never
//     touched, so this offers controls that quietly do nothing.
//   - "does .interlock exist" answers about history, not configuration.
//     That directory appears when the first session or lease is recorded,
//     not when init runs, so a freshly wired project reads as unwired
//     until an agent happens to work in it.
//
// doctor is the only one of the three that answers the question actually
// being asked. It costs a subprocess, which is why the result is not
// consulted in a loop.
func Watches(dir string) bool {
	if dir == "" {
		return false
	}
	bin, ok := binary()
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "doctor")
	cmd.Dir = dir
	return cmd.Run() == nil
}

// Init runs `interlock init` in dir.
//
// Offered rather than done silently. It writes hook registrations into each
// agent's own settings and trust hashes into ~/.codex/config.toml. That
// configuration makes agents execute commands on this machine, so it takes
// an explicit click and never happens just because someone opened a board.
func Init(ctx context.Context, dir string) error {
	bin, ok := binary()
	if !ok {
		return errors.New("interlock is not installed, so there is nothing to set up")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "init")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("interlock init: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
