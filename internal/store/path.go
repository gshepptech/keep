package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// StoreDirName and StoreFileName are the on-disk layout of a keep store.
// Defined once here so no caller anywhere in the codebase re-spells the
// ".rounce/db.sqlite" layout.
const StoreDirName = ".rounce"
const StoreFileName = "db.sqlite"

// StorePathIn returns the store path for a directory: <dir>/.rounce/db.sqlite
func StorePathIn(dir string) string {
	return filepath.Join(dir, StoreDirName, StoreFileName)
}

// ExistsFunc reports whether the file at path exists. It is injected into
// FindUpward and ResolvePath so both can be tested hermetically, with a
// synthetic map of "these paths exist", rather than by creating real
// directory trees or calling os.Chdir (process-global, a known source of
// flaky parallel tests).
type ExistsFunc func(path string) bool

// OSExists is the production ExistsFunc, backed by os.Stat. It distinguishes
// "does not exist" from every other stat failure via
// errors.Is(err, fs.ErrNotExist). Neither a bare os.Stat(...) == nil check
// nor os.IsNotExist(err) on an unexamined nil check is used here, because
// both would treat a permission error (the file is there, but
// this process cannot see it) as "absent", which is the wrong answer for a
// resolver deciding where to open a store.
func OSExists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	// Any other error (permission denied, I/O error, ...) means we cannot
	// conclude the path is absent, so treat it as present rather than
	// silently treating an inaccessible file as "no store here".
	return true
}

// FindUpward searches for a store starting at startDir and ascending toward
// the filesystem root, testing StorePathIn(dir) at each level via exists. It
// returns the first hit and true, or "" and false if no directory from
// startDir up to and including the root contains a store.
//
// FindUpward is exported separately from ResolvePath (rather than folded
// into it) because init's nested-store warning reuses this
// exact walk, started one directory above a store it just wrote, instead of
// duplicating the walk-up logic.
//
// The walk goes all the way to the filesystem root. There is no .git
// ceiling. The rule is "nearest .rounce/db.sqlite, walking up from the
// working directory", with no boundary; comparable tools (git, direnv) stop
// at a marker directory or an explicit ceiling env var, but keep has no
// need for one, and a .git ceiling would hide a store kept above the
// repository root.
//
// Termination is structural, not counter-based: filepath.Dir(dir) == dir is
// a fixed point reached only at the root, and that fixed point, not a depth
// limit, stops the loop.
func FindUpward(startDir string, exists ExistsFunc) (string, bool) {
	dir := startDir
	for {
		candidate := StorePathIn(dir)
		if exists(candidate) {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// ResolvePath applies the four-rule store resolution order, first hit wins:
//
//  1. flagDB, if non-empty.
//  2. envDB, if non-empty.
//  3. FindUpward(cwd, exists), if it hits.
//  4. StorePathIn(home).
//
// ResolvePath returns whatever its inputs imply; it does not run the result
// through filepath.Abs. Resolve (below) is the process-facing wrapper
// responsible for absolutizing, which is why the CLI's init prints an
// absolute path.
func ResolvePath(flagDB, envDB, cwd, home string, exists ExistsFunc) (string, error) {
	// Rule 1: --db beats everything else, including KEEP_DB and any
	// nearby or home store, and is returned without an existence check.
	// An explicit --db names where the user wants the store, whether or
	// not it is there yet: init creates it, and every other command
	// fails on open. Silently falling through to rule 3 because the
	// named file is missing would write to a store nobody asked for.
	if flagDB != "" {
		return flagDB, nil
	}

	// Rule 2: KEEP_DB beats a nearby store and the home store, for the
	// same reasoning as rule 1.
	if envDB != "" {
		return envDB, nil
	}

	// Rule 3: the nearest .rounce/db.sqlite walking up from cwd. The
	// per-repo store wins over the home store, so work done inside a
	// repository lands in that repository, the less surprising outcome.
	if path, ok := FindUpward(cwd, exists); ok {
		return path, nil
	}

	// Rule 4: the home store. This is the only rule that can fail to
	// produce a path: when rules 1-3 all miss and home is empty (e.g.
	// os.UserHomeDir() errored), there is nothing left to return.
	if home == "" {
		return "", fmt.Errorf("cannot resolve store path: no --db, no KEEP_DB, no store found walking up from %s, and no home directory available", cwd)
	}
	return StorePathIn(home), nil
}

// Resolve is the only function in this file that touches process state. It
// reads KEEP_DB, the working directory, and the user's home directory,
// then delegates to ResolvePath with OSExists as the existence predicate.
// Resolve is kept free of any branching beyond error propagation so that
// everything worth testing lives in the pure ResolvePath/FindUpward
// functions above.
func Resolve(flagDB string) (string, error) {
	envDB := Env("DB")

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve store path: %w", err)
	}

	// A home-directory lookup failure is not fatal here: ResolvePath only
	// needs home if rules 1-3 all miss, and it produces a clear error in
	// that case on its own.
	home, _ := os.UserHomeDir()

	path, err := ResolvePath(flagDB, envDB, cwd, home, OSExists)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve store path %s: %w", path, err)
	}
	return abs, nil
}
