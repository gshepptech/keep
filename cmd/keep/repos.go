package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/registry"
	"github.com/gshepptech/keep/internal/store"
)

// runRepos dispatches `keep repos` and its sub-subcommands. The literal
// string match on args[0] is safe because bare `keep repos` takes no
// positional argument at all (it just lists), so there is no ambiguity
// between the word "add" and a path that happens to be named add. A literal
// match is all this needs, so there is no CLI framework.
func runRepos(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return runReposAdd(args[1:], stdout, stderr)
		case "forget":
			return runReposForget(args[1:], stdout, stderr)
		}
	}
	return runReposList(args, stdout, stderr)
}

// reposRow is one store's row in repos --json. It is a local wire type, not
// registry.RepoResult, because RepoResult.Err cannot marshal, and its
// ItemCount/ReadyCount field names don't match this command's public
// vocabulary (items/ready).
type reposRow struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Items        int    `json:"items"`
	Ready        int    `json:"ready"`
	LastActivity string `json:"last_activity,omitempty"`
	Error        string `json:"error,omitempty"`
}

// reposJSON is repos --json's whole (and only) stdout payload. Repos is
// always a non-nil slice, never null, as runReady does for an empty queue.
type reposJSON struct {
	Repos []reposRow `json:"repos"`
}

// runReposList implements the bare `keep repos`: load the registry, fan
// out read-only counts for every registered store, and render one row per
// store. A failed row carries its error in place of the counts and is never
// dropped, and the command exits 0 regardless. It registers no --db: repos
// reads the registry, not one store, and a --db flag here would do nothing.
// As with `update --status` and `capture --stage`, the flag is simply not
// registered, so flag's own "flag provided but not defined: -db" refuses it.
func runReposList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep repos [--json]")
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}

	// A malformed registry is never fatal: Load hands back BOTH a usable
	// empty envelope and the error, so this reports and continues.
	env, err := registry.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "keep: warning: %v\n", err)
	}
	rows := registry.FetchCounts(context.Background(), env.Repos)

	// Sorted by name then path, a total order, so sort.Slice's instability
	// does not matter.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		return rows[i].Path < rows[j].Path
	})

	if *jsonFlag {
		out := reposJSON{Repos: make([]reposRow, 0, len(rows))}
		for _, r := range rows {
			row := reposRow{
				Name:         r.Repo,
				Path:         r.Path,
				Items:        r.ItemCount,
				Ready:        r.ReadyCount,
				LastActivity: r.LastActivity,
			}
			if r.Err != nil {
				row.Error = r.Err.Error()
			}
			out.Repos = append(out.Repos, row)
		}
		if err := emitJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "keep: repos: %v\n", err)
			return 1
		}
		return 0
	}

	renderRepos(stdout, rows)
	return 0
}

// renderRepos writes one line per store (name, items, ready, last activity,
// path) with items/ready right-aligned and "-" standing in for an absent
// last activity, columns aligned to the widest value in this result set,
// never a fixed width. No colour, no box drawing, space padding only, so
// awk/cut keep working.
//
// A failed row is never dropped and never collapses the whole command: it
// prints "<name>  error: <message>" in place of the counts, since the error
// text already names the path (classify builds it that way) and repeating
// the path column would be redundant.
func renderRepos(w io.Writer, rows []registry.RepoResult) {
	if len(rows) == 0 {
		return
	}

	nameW, itemsW, readyW, activityW := 0, 0, 0, 0
	itemsS := make([]string, len(rows))
	readyS := make([]string, len(rows))
	activityS := make([]string, len(rows))
	for i, r := range rows {
		if l := len(r.Repo); l > nameW {
			nameW = l
		}
		if r.Err != nil {
			continue
		}
		itemsS[i] = strconv.Itoa(r.ItemCount)
		readyS[i] = strconv.Itoa(r.ReadyCount)
		activityS[i] = r.LastActivity
		if activityS[i] == "" {
			activityS[i] = "-"
		}
		if l := len(itemsS[i]); l > itemsW {
			itemsW = l
		}
		if l := len(readyS[i]); l > readyW {
			readyW = l
		}
		if l := len(activityS[i]); l > activityW {
			activityW = l
		}
	}

	for i, r := range rows {
		if r.Err != nil {
			fmt.Fprintf(w, "%-*s  error: %v\n", nameW, r.Repo, r.Err)
			continue
		}
		fmt.Fprintf(w, "%-*s  %*s  %*s  %-*s  %s\n",
			nameW, r.Repo, itemsW, itemsS[i], readyW, readyS[i], activityW, activityS[i], r.Path)
	}
}

// normaliseStorePath turns a user-supplied argument into an absolute store
// path. A path already ending in .rounce/db.sqlite is taken as-is; anything
// else is treated as a repository directory and passed through
// store.StorePathIn. Both `repos add` and `repos forget` call it, so the two
// cannot disagree about what a path means.
//
// It does not check that the path exists or that it holds a real keep
// store. A registered-but-currently-broken store is a normal case, not an
// error at registration time; validating here would make `repos add` refuse
// the situation the registry exists to handle, and would make
// re-registering a store on a disconnected volume impossible.
func normaliseStorePath(arg string) (string, error) {
	clean := filepath.Clean(arg)
	suffix := filepath.Join(store.StoreDirName, store.StoreFileName)

	var path string
	if strings.HasSuffix(clean, suffix) {
		path = clean
	} else {
		path = store.StorePathIn(clean)
	}
	return filepath.Abs(path)
}

// runReposAdd implements `repos add <path>`.
func runReposAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repos add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// No flags today beyond the implicit -h/--help; permuteArgs still needed
	// because fs must be fully registered before it is called, per its own
	// doc comment, even with zero registered flags.
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: keep repos add <path>")
		return 2
	}

	path, err := normaliseStorePath(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}

	// Unlike `init`, a registry write failure here is fatal (exit 1). init
	// warns and exits 0 because the store it was asked to create was
	// created, so there is a core operation behind the bookkeeping.
	// `repos add` has none: the registry write is the entire command, and
	// reporting success after failing to do the only thing asked would be
	// false.
	if err := registry.Register(home, path, registry.NameFor(path), model.Now()); err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, path)
	return 0
}

// runReposForget implements `repos forget <path>` and `repos forget --gone`.
func runReposForget(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repos forget", flag.ContinueOnError)
	fs.SetOutput(stderr)
	goneFlag := fs.Bool("gone", false, "forget every registration whose store file no longer exists")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if *goneFlag {
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: keep repos forget --gone   (no path; it finds them)")
			return 2
		}
		return runReposForgetGone(stdout, stderr)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: keep repos forget <path>   or   keep repos forget --gone")
		return 2
	}

	path, err := normaliseStorePath(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}

	removed, err := registry.Forget(home, path)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	if !removed {
		// Matches every other command's treatment of a named-but-absent
		// thing (store.ErrNotFound -> exit 1 via exitFor); silently
		// succeeding would hide a typo.
		fmt.Fprintf(stderr, "keep: not registered: %s\n", path)
		return 1
	}
	// Forgetting is not deleting, and the message says so.
	fmt.Fprintf(stdout, "forgotten: %s (the store itself was not touched)\n", path)
	return 0
}

// runReposForgetGone drops every registration whose store file is gone.
//
// `init` registers automatically, so without this the registry only grows.
// A few days of scratch repositories under /tmp is enough to bury the real
// rows, and every dead one prints a full error line to stderr on each
// `ready --all`, the command someone working across repositories runs most.
//
// The test is os.Stat reporting IsNotExist, and nothing else. A store that
// exists but will not open (a permission, a half-written file, an unmounted
// disk that is about to come back) is left registered and keeps
// reporting its error. Forgetting a registration because a disk was
// unplugged would lose the entry for good, which is a far worse outcome
// than the noise this removes: the registry is the only record that the
// repository was ever known.
func runReposForgetGone(stdout, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	env, err := registry.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}

	var gone []string
	for _, repo := range env.Repos {
		if _, err := os.Stat(repo.Path); os.IsNotExist(err) {
			gone = append(gone, repo.Path)
		}
	}
	if len(gone) == 0 {
		fmt.Fprintln(stdout, "nothing to forget: every registered store is still on disk")
		return 0
	}

	for _, path := range gone {
		if _, err := registry.Forget(home, path); err != nil {
			fmt.Fprintf(stderr, "keep: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "forgotten: %s\n", path)
	}
	fmt.Fprintf(stdout, "%s forgotten. No store file was touched.\n", plural(len(gone), "registration"))
	return 0
}

// plural renders a count with its noun, so the summary line reads as a
// sentence for one as well as for many.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
