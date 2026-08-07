package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gshepptech/rounce/internal/store"
)

// runInit implements the `init` subcommand: target selection, parent
// directory creation, the call into store.Init, the single success line on
// stdout, and the two informational warnings on stderr. It returns the
// process exit code rather than calling os.Exit so it is callable from a
// test in-process while still driving the real store.Init.
//
// cmd/rounce must never import a database driver package, must never
// contain a SQL string, and must never receive a raw DB handle or a row
// (STORE-10). The only internal/store symbols used below are the ones
// listed in 01-04-PLAN.md's <interfaces> section.
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbFlag := fs.String("db", "", "create the store at exactly this path")
	globalFlag := fs.Bool("global", false, "create the store at ~/.rounce/db.sqlite instead of ./.rounce/db.sqlite")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}

	// --db and --global name two different targets; silently picking one
	// would be worse than refusing (settled decision).
	if *dbFlag != "" && *globalFlag {
		fmt.Fprintln(stderr, "rounce: init: --db and --global are mutually exclusive")
		return 2
	}

	// Target selection, in this order and no other (settled decision 5):
	// init does not consult the four-rule resolver or ROUNCE_DB.
	var target string
	switch {
	case *dbFlag != "":
		abs, err := filepath.Abs(*dbFlag)
		if err != nil {
			fmt.Fprintf(stderr, "rounce: %v\n", err)
			return 1
		}
		target = abs
	case *globalFlag:
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "rounce: %v\n", err)
			return 1
		}
		target = store.StorePathIn(home)
	default:
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "rounce: %v\n", err)
			return 1
		}
		target = store.StorePathIn(cwd)
	}

	// Record the idempotent case before creating anything: after store.Init
	// the file always exists, so checking afterwards proves nothing.
	preexisting := store.OSExists(target)

	// store.Init deliberately does not invent filesystem layout; that is
	// this CLI's job.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}

	s, err := store.Init(target)
	if err != nil {
		// ErrSchemaVersion is the one sentinel that can actually fire here:
		// an init against a store written by a different binary version.
		// Every later surface reuses this errors.Is-switch shape rather than
		// re-deriving messages from err.Error().
		if errors.Is(err, store.ErrSchemaVersion) {
			fmt.Fprintf(stderr, "rounce: %s: store schema predates or postdates this binary: %v\n", target, err)
		} else {
			fmt.Fprintf(stderr, "rounce: %v\n", err)
		}
		return 1
	}
	defer s.Close()

	// Nested-store warning (settled decision 2, research Open Question 3):
	// walk up starting one directory ABOVE the directory whose .rounce/ now
	// holds the new store -- target is <dir>/.rounce/db.sqlite, so
	// Dir(Dir(Dir(target))) is the parent of <dir>. Starting at <dir> itself
	// would trivially rediscover the file init just wrote. Skipped for
	// --global: the home store is the outermost store by definition.
	if !*globalFlag {
		parent := filepath.Dir(filepath.Dir(filepath.Dir(target)))
		if outer, ok := store.FindUpward(parent, store.OSExists); ok {
			fmt.Fprintf(stderr, "rounce: warning: this store is nested inside an existing store at %s; commands run below this directory will use the new store instead\n", outer)
		}
	}

	// ROUNCE_DB shadow warning (settled decision 6): init never consults
	// ROUNCE_DB to choose its target, so if it is set to somewhere else, the
	// store just written is one no other command will read by default.
	if envDB := os.Getenv("ROUNCE_DB"); envDB != "" {
		if absEnv, err := filepath.Abs(envDB); err == nil && absEnv != target {
			fmt.Fprintf(stderr, "rounce: warning: ROUNCE_DB=%s will take precedence over the store just created at %s\n", envDB, target)
		}
	}

	// Exactly one line to stdout: the absolute target path, marked
	// "unchanged: " when it already existed before this run.
	if preexisting {
		fmt.Fprintf(stdout, "unchanged: %s\n", target)
	} else {
		fmt.Fprintln(stdout, target)
	}

	return 0
}
