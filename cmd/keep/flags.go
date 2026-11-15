package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gshepptech/keep/internal/store"
)

// addDBFlag registers --db, present on every subcommand. There is
// deliberately no shared options struct -- each runX registers only the
// flags it actually has. The flag/KEEP_DB/discovery
// precedence itself lives in store.Resolve; every runX just forwards the
// flag's value through openStore.
func addDBFlag(fs *flag.FlagSet) *string {
	return fs.String("db", "", "path to the store (overrides KEEP_DB and discovery)")
}

// addJSONFlag registers --json: every read command (show, list, ready,
// blocked), add, and the two mutating commands whose success output is
// genuinely useful as structured data, start and update -- all three
// return the item just created or changed,
// which a caller may want to inspect programmatically. dep, close, and drop
// deliberately do NOT register it: their only output is a one-line
// confirmation, and a --json flag that only ever echoed back an id or
// status the caller already knows would be a flag that does nothing.
func addJSONFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "emit machine-parseable JSON on stdout")
}

// addActorFlag registers --actor, present only on mutating commands
// (every events row needs an actor). resolveActor supplies the
// $USER fallback below.
func addActorFlag(fs *flag.FlagSet) *string {
	return fs.String("actor", "", "actor recorded on the events row (default: $KEEP_ACTOR, then $USER)")
}

// resolveActor applies the actor precedence: --actor, then
// $KEEP_ACTOR, then $USER. Returns a usage-shaped error (caller maps it to
// exit 2) when none of the three is set -- there is no fourth fallback,
// because a silently-empty actor would make the audit trail meaningless.
// $KEEP_ACTOR set but empty ("") behaves as unset and
// falls through to $USER -- the same ""-means-absent convention
// resolveSession and every nullable column in this codebase already use.
//
// The middle rule exists for the MCP server: its default actor is
// $KEEP_ACTOR, else $USER, resolved once at server start. runMCP resolves
// that default through this same function rather than reading the
// environment itself, which is what makes "the CLI and MCP write identical
// events rows" a property of shared code instead of two implementations
// independently agreeing. If a capability is worth having, both surfaces
// get it.
func resolveActor(flagActor string) (string, error) {
	if flagActor != "" {
		return flagActor, nil
	}
	if a := store.Env("ACTOR"); a != "" {
		return a, nil
	}
	if u := os.Getenv("USER"); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no --actor given and neither $KEEP_ACTOR nor $USER is set")
}

// resolveSession returns $KEEP_SESSION, or "" when it is unset. An empty
// session is a legitimate value for every verb that merely RECORDS one: the
// session on an `add` or a `close` event is attribution, and a missing one
// costs detail and nothing else.
//
// It is not legitimate for a CLAIM. Use resolveClaimSession there.
func resolveSession() string {
	return store.Env("SESSION")
}

// resolveClaimSession returns the session id `start` and `next` claim under.
//
// $KEEP_SESSION wins: Interlock sets it for hook invocations, which is how
// the two programs line up without either knowing about the other.
//
// Absent it, this derives one rather than passing "" down to a store that
// refuses anonymous claims (store.ErrNoSession). Refusing here instead
// would break `keep start` for the ordinary case of a person at a terminal
// who has never heard of KEEP_SESSION, or an MCP server registered without
// setting it.
//
// The derived id is STABLE for a person on a machine, deliberately, and not
// per-process. A per-invocation id would make `keep start` on an item you
// already hold return ErrConflict against your own earlier claim, because
// each invocation would be a different session -- turning the idempotent
// retry that `start` guarantees into a failure that looks like a collision.
//
// It is prefixed so it is obvious in `keep stale` and in the event trail
// that this is a terminal rather than an agent session.
func resolveClaimSession() string {
	if s := store.Env("SESSION"); s != "" {
		return s
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("cli:%s@%s", user, host)
}

// openStore resolves flagDB the way every subcommand needs (store.Resolve's
// four-rule precedence: --db > KEEP_DB > walk-up > home) and opens it --
// the store.Resolve/store.Open pairing lives in exactly this one place so it
// cannot drift between commands.
func openStore(flagDB string) (*store.Store, error) {
	return openStoreWith(flagDB, store.OpenOptions{})
}

// openStoreReadOnly is the dashboard's opener for reads. mode=ro makes
// "a GET never writes" true by construction rather than by review. A
// read-only open coexists with the DSN's _txlock=immediate: a bare BEGIN
// succeeds and only an actual write fails, with SQLITE_READONLY.
func openStoreReadOnly(flagDB string) (*store.Store, error) {
	return openStoreWith(flagDB, store.OpenOptions{ReadOnly: true})
}

// openStoreWith is the one place store.Resolve and store.Open are paired,
// for every caller regardless of OpenOptions -- openStore and
// openStoreReadOnly are its only two callers.
func openStoreWith(flagDB string, opts store.OpenOptions) (*store.Store, error) {
	path, err := store.Resolve(flagDB)
	if err != nil {
		return nil, err
	}
	return store.Open(path, opts)
}

// exitFor prints a diagnostic for err on stderr and returns the process exit
// code. It classifies by sentinel via errors.Is -- never by message text,
// the same rule internal/store/errors.go states for SQLite codes and for the
// same reason: messages are not a contract.
//
// store.ErrParentChildViaDep, store.ErrSelfEdge, store.ErrInvalidDepType,
// store.ErrInvalidType, store.ErrInvalidStage and store.ErrEmptyReason are
// the sentinels mapped to exit 2, not 1: each is a usage error --
// the caller asked for something the CLI does not do (reparent through dep,
// depend on itself, or an unrecognized dep type slipping past the CLI's own
// pre-validation) -- not an operational failure of an otherwise well-formed
// request. store.ErrParentChildViaDep's own message already names
// `keep add --parent`, so it is passed through verbatim rather than
// restated.
//
// Every other sentinel here is an operational failure (exit 1): the request
// was well-formed but the store refused it for a reason inherent to the
// data -- not found, a conflicting claim (the store's message already names
// the holding session for ErrConflict from `start`, so it too is passed
// through rather than restated), a would-be cycle, an illegal state
// transition, or an on-disk schema mismatch.
func exitFor(stderr io.Writer, err error) int {
	switch {
	case errors.Is(err, store.ErrParentChildViaDep),
		errors.Is(err, store.ErrSelfEdge),
		errors.Is(err, store.ErrInvalidDepType),
		errors.Is(err, store.ErrInvalidType),
		errors.Is(err, store.ErrInvalidStage),
		errors.Is(err, store.ErrEmptyReason),
		errors.Is(err, store.ErrEmptyTitle):
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 2
	default:
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
}

// permuteArgs reorders args so every well-formed flag (and its value, when
// it takes one) moves ahead of the first positional argument, before
// fs.Parse ever runs -- Go's flag.Parse stops consuming flags at the first
// non-flag argument, so without this pre-pass every command taking a
// positional id or title would reject flags placed after it, including the
// canonical `keep capture "Q3 roadmap" --type roadmap --body -`.
//
// fs MUST already have every flag registered on it before this is called:
// permuteArgs asks fs (via fs.VisitAll + each flag's IsBoolFlag() bool)
// which registered flags take a value, NEVER a hard-coded list -- a name
// alone cannot say whether a flag takes a value, because the same name can
// mean different things on different commands in this exact binary: --body
// is a string flag on add/capture and a bool flag on show. Called
// before flags are registered, fs.VisitAll sees an empty set and
// mis-permutes everything.
//
// If a value-taking flag has no following token to serve as its value,
// permutation stops there and the remainder (including that flag) is left
// in original order as trailing "positionals" -- this is load-bearing, not
// defensive padding. The naive alternative (unconditionally inserting a
// synthetic "--" ahead of positionals regardless of whether every
// value-taking flag actually got a value) fails silently:
// `keep add "title" --body` (--body missing its value)
// permutes to ["--body", "--", "title"], and flag.Parse's own unconditional
// "next token is the value" behavior sets body = "--" with exit 0 and no
// error -- silent corruption, not a bug in flag.Parse. Leaving the malformed
// tail un-permuted instead makes fs.Parse's native behavior handle it
// exactly as it does today (folded into runAdd's title join, or a native
// "flag needs an argument" error).
//
// "--" still terminates flag parsing: once seen, everything after it is a
// positional and is never re-examined as a flag, so
// `keep add -- --weird-title` keeps working.
func permuteArgs(fs *flag.FlagSet, args []string) []string {
	// -h and --help are the two names the flag package handles itself, on
	// every FlagSet, without anyone registering them. They are not in
	// fs.VisitAll for exactly that reason, so the loop below would classify
	// them as value-taking and -- being last on the line, with no value
	// after them -- send them down the "dump the rest verbatim" branch into
	// the positionals. `keep add --help` then reached flag.Parse as a
	// positional argument after a `--`, which is to say: as a TITLE. It
	// filed an item called "--help", printed its id, and exited 0.
	//
	// Seeding them as bool flags is what puts them back in front of the
	// `--`, so flag.Parse sees them, prints the flag list, and returns
	// ErrHelp -- which every runX in this package already turns into exit 2
	// without touching the store.
	//
	// This is the one hard-coded pair in a function whose doc comment says
	// it never hard-codes flag names. The reason that rule exists is that
	// the same name can take a value on one command and not on another; -h
	// and --help cannot, on any FlagSet, ever.
	boolFlags := map[string]bool{"h": true, "help": true}
	fs.VisitAll(func(f *flag.Flag) {
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			boolFlags[f.Name] = true
		}
	})

	var flags, positionals []string
	i, sawDashDash := 0, false
tokenLoop:
	for i < len(args) {
		a := args[i]
		switch {
		case sawDashDash:
			positionals = append(positionals, a)
			i++
		case a == "--":
			sawDashDash = true
			i++
		case len(a) > 1 && a[0] == '-':
			name := strings.TrimLeft(a, "-")
			base, hasEq := name, strings.Contains(name, "=")
			if hasEq {
				base = name[:strings.IndexByte(name, '=')]
			}
			if !hasEq && !boolFlags[base] {
				if i+1 >= len(args) {
					// No value available -- stop permuting; dump the rest
					// verbatim so fs.Parse's native behaviour takes over.
					// This is the silent-corruption regression lock: without
					// this branch, the naive "-- always follows" alternative
					// sets a missing value to the literal string "--".
					positionals = append(positionals, args[i:]...)
					break tokenLoop
				}
				flags = append(flags, a, args[i+1])
				i += 2
				continue
			}
			flags = append(flags, a)
			i++
		default:
			positionals = append(positionals, a)
			i++
		}
	}
	out := append([]string{}, flags...)
	if len(positionals) > 0 {
		out = append(out, "--")
		out = append(out, positionals...)
	}
	return out
}

// resolveBody implements --body -'s "read from stdin" contract, shared by
// add and capture. Having --body - mean two different things in two
// commands would be worse than either meaning alone, so there is exactly
// one definition of this function, and add.go/capture.go are its only two
// call sites.
//
// It reads os.Stdin directly rather than taking an io.Reader parameter,
// because every runX in this package has the fixed signature
// (args []string, stdout, stderr io.Writer) int, and threading a reader
// through all twelve of them to serve two would be a large, unrelated
// change. Every test in this package drives the real binary through
// os/exec (cli_test.go), so a real pipe is what gets exercised either way
// regardless of which shape resolveBody's own signature takes -- there is
// no test-only reason to prefer an injected reader here.
//
// The read is gated STRICTLY on bodyFlag being the literal string "-",
// never on "is stdin non-empty" or a TTY check:
// an unconditional read would make a plain, interactive `keep add
// "title"` (no --body at all) block forever waiting for EOF a real
// terminal never sends. Any other value, including "", is returned
// unchanged and never touches stdin. Empty piped stdin is success, not an
// error -- io.ReadAll on an immediately-EOF reader returns ("", nil).
func resolveBody(bodyFlag string) (string, error) {
	if bodyFlag != "-" {
		return bodyFlag, nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stringSlice implements flag.Value for a repeatable string flag: each
// -flag <value> occurrence appends, rather than the flag package's default
// last-one-wins behaviour for a *string. Used by add's --blocks/
// --derived-from: a comma-joined string was deliberately rejected
// -- a comma-separated list is a parsing rule nobody documents, and an id
// containing a comma would be unrepresentable.
type stringSlice []string

func (s *stringSlice) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}
