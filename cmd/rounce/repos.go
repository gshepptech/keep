package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/gshepptech/rounce/internal/registry"
)

// runRepos is the project's first command with a sub-subcommand. The literal
// string match on args[0] is safe because bare `rounce repos` takes no
// positional argument at all (REPO-02's decision: it just lists), so there
// is no ambiguity between the word "add" and a path that happens to be named
// add. Keep the dispatch shape -- do not reach for a CLI framework.
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

// reposRow is one store's row in repos --json. A local wire type, not
// registry.RepoResult directly -- RepoResult.Err cannot marshal, and its
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
// always a non-nil slice, never null -- the same discipline runReady already
// applies to an empty queue.
type reposJSON struct {
	Repos []reposRow `json:"repos"`
}

// runReposList implements the bare `rounce repos`: load the registry, fan
// out read-only counts for every registered store, and render one row per
// store -- a failed row carries its error in place of the counts and is
// never dropped, and the command exits 0 regardless (06-CONTEXT.md decision
// 4). Deliberately registers no --db: repos reads the registry, not one
// store, and a --db flag here would do nothing. Enforced the way
// `update --status` and `capture --stage` already are: by NOT registering
// the flag, so flag's own "flag provided but not defined: -db" is the
// enforcement.
func runReposList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonFlag := addJSONFlag(fs)
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: rounce repos [--json]")
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}

	// A malformed registry is never fatal (06-CONTEXT.md decision 2): Load
	// hands back BOTH a usable empty envelope and the error, so this reports
	// and continues. Returning here would be the failure that decision
	// forbids.
	env, err := registry.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: warning: %v\n", err)
	}
	rows := registry.FetchCounts(context.Background(), env.Repos)

	// Sorted by name then path -- a total order, so sort.Slice's instability
	// is irrelevant (03-05 hit that trap once already).
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
			fmt.Fprintf(stderr, "rounce: repos: %v\n", err)
			return 1
		}
		return 0
	}

	renderRepos(stdout, rows)
	return 0
}

// renderRepos writes one line per store -- name, items, ready, last
// activity, path -- with items/ready right-aligned and "-" standing in for
// an absent last activity, columns aligned to the widest value in THIS
// result set, never a fixed width. No colour, no box drawing, space padding
// only, so awk/cut keep working.
//
// A failed row is never dropped and never collapses the whole command: it
// prints "<name>  error: <message>" in place of the counts, since the error
// text already names the path (classify builds it that way) and repeating
// the path column would be redundant. This is the visible half of REPO-05.
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

// runReposAdd is implemented in task 2 of this plan.
func runReposAdd(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "rounce: repos add: not yet implemented")
	return 2
}

// runReposForget is implemented in task 2 of this plan.
func runReposForget(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "rounce: repos forget: not yet implemented")
	return 2
}
