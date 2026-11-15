package registry

import (
	"sort"

	"github.com/gshepptech/keep/internal/model"
)

// MergedItem is one row of a cross-store merge: an item, plus the name of
// the registered repository it came from.
//
// The json tag is "repository", NOT "repo", and that is load-bearing rather
// than a style choice. model.Item already carries `Repo *string
// json:"repo"` (the items.repo column). A field tagged "repo" here would
// collide with the promoted embedded field, and encoding/json resolves that
// collision SILENTLY BY DEPTH: the shallower field wins and the embedded
// item's own repo value is dropped from the output entirely -- no error, no
// warning, no duplicate-key diagnostic. merge_test.go's tag-collision test
// is the permanent lock; do not rename this tag to "repo" for symmetry.
type MergedItem struct {
	model.Item
	Repository string `json:"repository"`
}

// Merge flattens every successful RepoResult into one slice ranked across
// all of the stores.
//
// Order: score DESC, then repository name ASC, then item id ASC.
//
// The tiebreak is deliberately NOT created_at, which single-store `ready`
// uses. Two stores' created_at values are wall-clock strings from two
// unrelated histories; ordering by them across stores would produce an order
// that is meaningless and, worse, unstable across machines. Repository name
// then id is a total order over the merged set, which is what makes the
// output identical on every run -- and, being total, makes sort.Slice's
// documented instability irrelevant here.
//
// Errored results contribute nothing: they have no items to contribute. The
// caller surfaces them separately, because a silent partial result is the
// failure this design avoids.
func Merge(results []RepoResult) []MergedItem {
	out := make([]MergedItem, 0)
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		for _, it := range r.Items {
			out = append(out, MergedItem{Item: it, Repository: r.Repo})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Repository != out[j].Repository {
			return out[i].Repository < out[j].Repository
		}
		return out[i].ID < out[j].ID
	})
	return out
}
