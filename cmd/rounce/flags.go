package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gshepptech/rounce/internal/store"
)

// addDBFlag registers --db, present on every subcommand (plan 07's
// "Deliberately do not build one shared options struct" -- each runX
// registers only the flags it actually has). The flag/ROUNCE_DB/discovery
// precedence itself lives in store.Resolve; every runX just forwards the
// flag's value through openStore.
func addDBFlag(fs *flag.FlagSet) *string {
	return fs.String("db", "", "path to the store (overrides ROUNCE_DB and discovery)")
}

// addJSONFlag registers --json (CLI-01): every read command (show, list,
// ready, blocked -- plan 08), add (plan 07), and the two mutating commands
// whose success output is genuinely useful as structured data, start and
// update (plan 09) -- all three return the item just created or changed,
// which a caller may want to inspect programmatically. dep, close, and drop
// deliberately do NOT register it: their only output is a one-line
// confirmation, and a --json flag that only ever echoed back an id or
// status the caller already knows would be a flag that does nothing.
func addJSONFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "emit machine-parseable JSON on stdout")
}

// addActorFlag registers --actor, present only on mutating commands
// (EVENT-01 needs an actor on every events row). resolveActor supplies the
// $USER fallback below.
func addActorFlag(fs *flag.FlagSet) *string {
	return fs.String("actor", "", "actor recorded on the events row (default: $USER)")
}

// resolveActor applies CLI-01/EVENT-01's precedence: --actor, then $USER.
// Returns a usage-shaped error (caller maps it to exit 2) when neither is
// set -- there is no third fallback, because a silently-empty actor would
// make EVENT-01's audit trail unfalsifiable.
func resolveActor(flagActor string) (string, error) {
	if flagActor != "" {
		return flagActor, nil
	}
	if u := os.Getenv("USER"); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no --actor given and $USER is not set")
}

// resolveSession returns $ROUNCE_SESSION, or "" when it is unset. There is
// no requirement that an agent set one (StartItemInput's doc comment,
// internal/store/transitions.go) -- an empty SessionID is a legitimate
// value throughout this package, not an error.
func resolveSession() string {
	return os.Getenv("ROUNCE_SESSION")
}

// openStore resolves flagDB the way every subcommand needs (store.Resolve's
// four-rule precedence: --db > ROUNCE_DB > walk-up > home) and opens it --
// the store.Resolve/store.Open pairing lives in exactly this one place so it
// cannot drift between commands.
func openStore(flagDB string) (*store.Store, error) {
	path, err := store.Resolve(flagDB)
	if err != nil {
		return nil, err
	}
	return store.Open(path, store.OpenOptions{})
}

// exitFor prints a diagnostic for err on stderr and returns the process exit
// code. It classifies by sentinel via errors.Is -- never by message text,
// the same rule internal/store/errors.go states for SQLite codes and for the
// same reason: messages are not a contract.
//
// store.ErrParentChildViaDep, store.ErrSelfEdge, and store.ErrInvalidDepType
// are the three sentinels mapped to exit 2, not 1: each is a usage error --
// the caller asked for something the CLI does not do (reparent through dep,
// depend on itself, or an unrecognized dep type slipping past the CLI's own
// pre-validation) -- not an operational failure of an otherwise well-formed
// request. store.ErrParentChildViaDep's own message already names
// `rounce add --parent`, so it is passed through verbatim rather than
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
		errors.Is(err, store.ErrInvalidDepType):
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 2
	default:
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
}

// permuteArgs reorders args so every well-formed flag (and its value, when
// it takes one) moves ahead of the first positional argument, before
// fs.Parse ever runs (03-CONTEXT.md decision 1a) -- Go's flag.Parse stops
// consuming flags at the first non-flag argument, so without this pre-pass
// every command taking a positional id or title would reject flags placed
// after it, including the canonical `rounce capture "Q3 roadmap" --type
// roadmap --body -` docs/design/phase-2.md documents.
//
// fs MUST already have every flag registered on it before this is called:
// permuteArgs asks fs (via fs.VisitAll + each flag's IsBoolFlag() bool)
// which registered flags take a value, NEVER a hard-coded list -- a name
// alone cannot say whether a flag takes a value, because the same name can
// mean different things on different commands in this exact binary: --body
// is a string flag on add/capture and a bool flag on show (plan 04). Called
// before flags are registered, fs.VisitAll sees an empty set and
// mis-permutes everything.
//
// If a value-taking flag has no following token to serve as its value,
// permutation stops there and the remainder (including that flag) is left
// in original order as trailing "positionals" -- this is load-bearing, not
// defensive padding. The naive alternative (unconditionally inserting a
// synthetic "--" ahead of positionals regardless of whether every
// value-taking flag actually got a value) was reproduced live during
// 03-RESEARCH.md: `rounce add "title" --body` (--body missing its value)
// permutes to ["--body", "--", "title"], and flag.Parse's own unconditional
// "next token is the value" behavior sets body = "--" with exit 0 and no
// error -- silent corruption, not a bug in flag.Parse. Leaving the malformed
// tail un-permuted instead makes fs.Parse's native behavior handle it
// exactly as it does today (folded into runAdd's title join, or a native
// "flag needs an argument" error).
//
// "--" still terminates flag parsing: once seen, everything after it is a
// positional and is never re-examined as a flag, so
// `rounce add -- --weird-title` keeps working.
func permuteArgs(fs *flag.FlagSet, args []string) []string {
	boolFlags := map[string]bool{}
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

// stringSlice implements flag.Value for a repeatable string flag: each
// -flag <value> occurrence appends, rather than the flag package's default
// last-one-wins behaviour for a *string. Used by add's --blocks/
// --derived-from (plan 07): a comma-joined string was deliberately rejected
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
