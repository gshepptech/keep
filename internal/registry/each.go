package registry

import (
	"context"
	"sort"

	"github.com/gshepptech/keep/internal/store"
)

// Fanout is one registered repository's contribution to a cross-store read,
// for whatever row type the read produces.
//
// It is RepoResult's shape with the row type made a parameter. RepoResult
// predates it and stays for FetchCounts, whose three scalar fields are not
// rows at all; everything that fans out a LIST of something goes through
// this.
//
// The same property RepoResult was built for holds here and is the reason
// there is no error return: FetchEach returns []Fanout[T] and NO error, so a
// caller structurally cannot propagate one repository's failure as a crash
// or a blank render. A store that will not open reports on its own row and
// the others are read anyway.
type Fanout[T any] struct {
	Repo string // the registry's name field -- NOT items.repo
	Path string
	Rows []T
	Err  error
}

// FetchEach opens every registered store read-only, in sequence, and runs
// query against each.
//
// This is the one place a cross-repository read happens. Without it, every
// command with an `--all` flag would write its own
// open-classify-collect-close loop, and sooner or later one would get the
// read-only open wrong: without mode=ro a deleted store is silently
// recreated as an empty one, which reads as a repository with no work
// rather than as a repository that is gone.
//
// Sequential by decision: opening ~20 SQLite files per request is fine at
// this scale. If it stops being fine, the fix is a cache with an mtime
// check, not a combined database.
//
// query is a store method, never a reimplementation of one. That is what
// makes a cross-repository view structurally unable to disagree with the
// single-store command it aggregates.
func FetchEach[T any](ctx context.Context, repos []Repo, query func(context.Context, *store.Store) ([]T, error)) []Fanout[T] {
	out := make([]Fanout[T], 0, len(repos))
	for _, r := range repos {
		res := Fanout[T]{Repo: r.Name, Path: r.Path}
		res.Err = withStore(r.Path, func(st *store.Store) error {
			rows, err := query(ctx, st)
			if err != nil {
				return err
			}
			res.Rows = rows
			return nil
		})
		out = append(out, res)
	}
	return out
}

// Tagged is one row of a cross-store read, carrying the repository it came
// from.
//
// The json tag is "repository", NOT "repo", for the reason MergedItem's own
// comment sets out at length: a row type that embeds model.Item already has
// a promoted field tagged "repo", and encoding/json resolves that collision
// silently by depth -- the shallower field wins and the item's own repo
// value vanishes from the output with no error. That is a real bug this
// codebase already has a permanent test against; do not rename this for
// symmetry.
type Tagged[T any] struct {
	Row        T      `json:"row"`
	Repository string `json:"repository"`
}

// Flatten collects every successful fan-out's rows into one slice, tagging
// each with its repository, and orders them with less.
//
// Errored results contribute nothing -- they have no rows to contribute.
// The caller surfaces them separately, because a silent partial result is
// the failure mode the whole cross-repository surface is shaped against.
//
// less must impose a TOTAL order. sort.Slice is documented as unstable, so
// a comparison that returns false in both directions for two distinct rows
// produces output that varies between runs -- a trap this codebase hit once
// already (see Merge).
func Flatten[T any](results []Fanout[T], less func(a, b Tagged[T]) bool) []Tagged[T] {
	out := make([]Tagged[T], 0)
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		for _, row := range r.Rows {
			out = append(out, Tagged[T]{Row: row, Repository: r.Repo})
		}
	}
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

// Failures returns the repositories a fan-out could not read, so a caller
// can report which view is incomplete. An empty result is a positive
// statement that every registered store was read.
func Failures[T any](results []Fanout[T]) []Fanout[T] {
	var out []Fanout[T]
	for _, r := range results {
		if r.Err != nil {
			out = append(out, r)
		}
	}
	return out
}
