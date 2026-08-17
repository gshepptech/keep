package registry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// RepoResult is one registered repository's contribution to a fan-out.
//
// Its shape makes "the whole fan-out failed" unrepresentable: FetchReady and
// FetchCounts return []RepoResult and NO error, so a caller structurally
// cannot propagate one repository's failure as a crash or a blank render.
// That is REPO-05's requirement expressed as a type rather than as a rule
// somebody has to remember (.planning/research/ARCHITECTURE.md: "Structure
// the fan-out so the function signature makes 'the whole thing failed'
// unrepresentable").
//
// Deliberately untagged. Err cannot be marshalled by encoding/json, and the
// two consumers want different envelopes; each maps this into its own wire
// type. See internal/registry/merge.go (plan 05) for why a repository-name
// field's json tag needs care.
type RepoResult struct {
	Repo string // the registry's name field -- NOT items.repo
	Path string

	Items []model.Item // FetchReady only

	ItemCount    int    // FetchCounts only
	ReadyCount   int    // FetchCounts only
	LastActivity string // FetchCounts only; "" when the store has no events

	Err error
}

// classify pre-classifies path with a stdlib stat/open check BEFORE
// store.Open is attempted, and returns a differently-worded error for each
// of REPO-05's three named failure modes.
//
// This exists because the SQL driver CANNOT distinguish them. Verified live
// during 06 research against the pinned driver/libc versions this module is
// pinned to: a missing file, a missing parent directory, and a chmod-000
// file all return the identical driver error value, carrying failure code 14
// (SQLITE_CANTOPEN) -- a scratch switch over the three cases produced
// literal duplicate-case compiler errors. The stdlib's own fs.ErrNotExist /
// fs.ErrPermission sentinels distinguish the same three paths cleanly.
//
// Do not try to make the SQLite error carry the distinction, and do not
// parse its message text: internal/store/errors.go's standing rule is that
// driver message text is not a contract, and here message-parsing would not
// work even if it were allowed, because the three messages are the same.
//
// classify returning nil does NOT mean the store is usable -- only that the
// pre-check succeeded. store.Open (with its schema-version check) is still
// the actual failure mechanism, and its error is still surfaced on the row.
// classify governs the wording, not the outcome.
//
// Deviation from this plan's literal "Body: os.Stat(path)" instruction,
// recorded here rather than silently followed: a bare os.Stat cannot
// distinguish the chmod-000 case on this (POSIX) platform, because stat(2)
// only requires search/execute permission on the path's containing
// directories, never read permission on the target file itself -- verified
// live on this machine (os.Stat on a chmod-000 file returns nil, err). This
// matches the 06-RESEARCH.md finding's own quoted proof, which measured
// os.Open (not os.Stat) distinguishing all three cases:
// "os.Open(missing) -> IsNotExist=true, IsPermission=false" /
// "os.Open(chmod 000) -> IsNotExist=false, IsPermission=true". classify
// therefore opens (read-only) and immediately closes rather than merely
// stat-ing, so the fs.ErrPermission branch below is actually reachable. This
// changes nothing about the "os.Stat is the same problem OSExists already
// solved one level up" framing -- os.Open with an fs.ErrNotExist/
// fs.ErrPermission classification is the same idiom, just the specific stdlib
// call that can see a permission failure at all.
func classify(path string) error {
	f, err := os.Open(path)
	switch {
	case err == nil:
		f.Close()
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("store not found at %s (deleted or moved): %w", path, err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("store at %s is not readable: %w", path, err)
	default:
		return fmt.Errorf("store at %s: %w", path, err)
	}
}

// withStore is the ONLY place internal/registry opens a store. Every open
// here is read-only (REPO-04), and routing both fan-outs through one helper
// is what stops a third fan-out added later from quietly omitting it.
//
// mode=ro is a correctness requirement, not an optimisation. Verified live
// during 06 research: a DEFAULT read-write open of a missing path SUCCEEDS
// and creates a 4096-byte database file; mode=ro creates nothing and fails.
// Without ReadOnly, a deleted repository would be reported as an EMPTY
// repository -- and a stray database file would appear where the deleted one
// was, which is the more alarming half. fanout_test.go asserts both halves.
//
// The store is closed before withStore returns, so no connection is held
// across the fan-out loop (.planning/research/PITFALLS.md names this for
// this exact phase). Every store read method materialises its rows fully
// into Go values before returning, so this is safe.
func withStore(path string, fn func(*store.Store) error) error {
	if err := classify(path); err != nil {
		return err
	}
	st, err := store.Open(path, store.OpenOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(st)
}

// FetchReady opens every registered store read-only, in sequence, and
// collects each one's ready queue.
//
// Sequential by decision (06-CONTEXT.md decision 4, quoting phase-5.md):
// "Opening ~20 SQLite files per request is fine at this scale. If it stops
// being fine, the fix is a cache with an mtime check, not a combined
// database." Concurrency here would buy little and would reintroduce exactly
// the one-connection-per-process reasoning Phase 1 settled.
//
// Each store is queried with s.Ready(ctx, 0) -- UNLIMITED, deliberately.
// Limiting per store before the merge would let each store's own tiebreak
// (created_at, which is not comparable across stores) decide which rows even
// reach the cross-store ranking. The caller limits the MERGED result. This
// is the same discipline board.go already applies with its one unlimited
// Ready call feeding both the count and the rows.
//
// The ready query itself is store.Ready, never a reimplementation: a second
// "what is workable" query in Go is Anti-Pattern 1 in
// .planning/research/ARCHITECTURE.md, and calling the shipped one is what
// makes `ready --all`'s per-repo view structurally unable to disagree with
// that repository's own single-store `keep ready`.
func FetchReady(ctx context.Context, repos []Repo) []RepoResult {
	out := make([]RepoResult, 0, len(repos))
	for _, r := range repos {
		res := RepoResult{Repo: r.Name, Path: r.Path}
		res.Err = withStore(r.Path, func(st *store.Store) error {
			items, err := st.Ready(ctx, 0)
			if err != nil {
				return err
			}
			res.Items = items
			return nil
		})
		out = append(out, res)
	}
	return out
}

// FetchCounts opens every registered store read-only and collects the row
// `keep repos` renders: total items, ready items, and last activity.
//
// The counts come from full item fetches plus len(), not from a COUNT(*)
// method (06-RESEARCH.md Open Question 1). That matches cmd/keep/board.go's
// established precedent exactly -- it already calls Ready/ListItems/Blocked/
// Backlog purely to take len() -- and README principle 6 ("correct before
// fast") says a COUNT(*)-shaped method is a measured optimisation, not an
// upfront design decision. LastActivity is store.LastActivity, i.e.
// MAX(events.at), because CreateDep touches no items row and
// MAX(items.updated_at) would therefore under-report.
func FetchCounts(ctx context.Context, repos []Repo) []RepoResult {
	out := make([]RepoResult, 0, len(repos))
	for _, r := range repos {
		res := RepoResult{Repo: r.Name, Path: r.Path}
		res.Err = withStore(r.Path, func(st *store.Store) error {
			items, err := st.ListItems(ctx, store.ListFilter{})
			if err != nil {
				return err
			}
			res.ItemCount = len(items)

			ready, err := st.Ready(ctx, 0)
			if err != nil {
				return err
			}
			res.ReadyCount = len(ready)

			last, _, err := st.LastActivity(ctx)
			if err != nil {
				return err
			}
			res.LastActivity = last
			return nil
		})
		out = append(out, res)
	}
	return out
}
