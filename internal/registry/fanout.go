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
// FetchCounts return []RepoResult and NO error, so a caller
// cannot propagate one repository's failure as a crash or a blank render.
// That is "one broken repository is a row, not a failure" expressed as a
// type rather than as a rule somebody has to remember.
//
// Deliberately untagged. Err cannot be marshalled by encoding/json, and the
// two consumers want different envelopes; each maps this into its own wire
// type. See internal/registry/merge.go for why a repository-name
// field's json tag needs care.
type RepoResult struct {
	Repo string // the registry's name field, not items.repo
	Path string

	Items []model.Item // FetchReady only

	ItemCount    int    // FetchCounts only
	ReadyCount   int    // FetchCounts only
	LastActivity string // FetchCounts only; "" when the store has no events

	Err error
}

// classify pre-classifies path with a stdlib stat/open check BEFORE
// store.Open is attempted, and returns a differently-worded error for each
// of three failure modes: deleted or moved, missing parent directory, and
// not readable.
//
// This exists because the SQL driver CANNOT distinguish them. With the
// pinned driver/libc versions, a missing file, a missing parent directory,
// and a chmod-000 file all return the identical driver error value,
// carrying failure code 14 (SQLITE_CANTOPEN). The stdlib's own fs.ErrNotExist /
// fs.ErrPermission sentinels distinguish the same three paths cleanly.
//
// Do not try to make the SQLite error carry the distinction, and do not
// parse its message text: internal/store/errors.go's standing rule is that
// driver message text is not a contract, and here message-parsing would not
// work even if it were allowed, because the three messages are the same.
//
// classify returning nil means only that the pre-check succeeded, not that
// the store is usable. store.Open (with its schema-version check) is still
// the actual failure mechanism, and its error is still surfaced on the row.
// classify governs the wording, not the outcome.
//
// Why os.Open and not os.Stat: a bare os.Stat cannot distinguish the
// chmod-000 case on POSIX platforms, because stat(2) only requires
// search/execute permission on the path's containing directories, never
// read permission on the target file itself (os.Stat on a chmod-000 file
// returns a nil error). os.Open distinguishes all three cases:
// "os.Open(missing) -> IsNotExist=true, IsPermission=false" /
// "os.Open(chmod 000) -> IsNotExist=false, IsPermission=true". classify
// therefore opens (read-only) and immediately closes rather than merely
// stat-ing, so the fs.ErrPermission branch below is actually reachable.
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
// here is read-only, and routing both fan-outs through one helper
// is what stops a third fan-out added later from quietly omitting it.
//
// mode=ro is a correctness requirement, not an optimisation: a DEFAULT
// read-write open of a missing path SUCCEEDS and creates a 4096-byte
// database file; mode=ro creates nothing and fails.
// Without ReadOnly, a deleted repository would be reported as an empty
// repository, and a stray database file would appear where the deleted one
// was. fanout_test.go asserts both halves.
//
// The store is closed before withStore returns, so no connection is held
// across the fan-out loop. Every store read method materialises its rows
// fully into Go values before returning, so this is safe.
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
// Sequential by decision: opening ~20 SQLite files per request is fine at
// this scale. If it stops being fine, the fix is a cache with an mtime
// check, not a combined database. Concurrency here would buy little and
// would complicate the one-connection-per-process model the store relies on.
//
// Each store is queried with s.Ready(ctx, 0), which is deliberately unlimited.
// Limiting per store before the merge would let each store's own tiebreak
// (created_at, which is not comparable across stores) decide which rows even
// reach the cross-store ranking. The caller limits the MERGED result. This is
// the same discipline board.go applies with its one unlimited Ready call
// feeding both the count and the rows.
//
// The ready query itself is store.Ready, never a reimplementation. A second
// "what is workable" query in Go would drift, and calling the store's own
// means `ready --all`'s per-repo view cannot disagree with that repository's
// single-store `keep ready`.
// It is a thin call on FetchEach (each.go), the generic form of the loop
// that `backlog --all`, `search --all` and `log --all` share.
func FetchReady(ctx context.Context, repos []Repo) []RepoResult {
	fanned := FetchEach(ctx, repos, func(ctx context.Context, st *store.Store) ([]model.Item, error) {
		return st.Ready(ctx, 0)
	})
	out := make([]RepoResult, 0, len(fanned))
	for _, f := range fanned {
		out = append(out, RepoResult{Repo: f.Repo, Path: f.Path, Items: f.Rows, Err: f.Err})
	}
	return out
}

// FetchCounts opens every registered store read-only and collects the row
// `keep repos` renders: total items, ready items, and last activity.
//
// The counts come from full item fetches plus len(), not from a COUNT(*)
// method. That matches cmd/keep/board.go, which calls
// Ready/ListItems/Blocked/Backlog only to take len(). keep favours correct
// over fast: a COUNT(*)-shaped method is an optimisation to add once it is
// measured, not an upfront design decision. LastActivity is
// store.LastActivity, i.e. MAX(events.at), because CreateDep touches no items
// row and MAX(items.updated_at) would therefore under-report.
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
