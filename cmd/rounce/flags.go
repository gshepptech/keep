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

// addJSONFlag registers --json, present only on read commands (CLI-01:
// show, list, ready, blocked -- plans 08/09). Mutating commands (add, dep,
// start, close, drop, update) do not register it -- an advertised flag that
// does nothing is worse than an absent one.
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
