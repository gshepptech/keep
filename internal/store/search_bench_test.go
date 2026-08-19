package store

// Benchmarks for Search, kept in the tree because they are the evidence for
// a decision README principle 6 requires evidence for: "Materialize a
// computed column only after measuring that the query is too slow, not in
// anticipation."
//
// Search was five LIKE '%x%' scans for most of this project's life, on the
// stated reasoning that the corpus is small and a scan is under a
// millisecond. These benchmarks are what ended that reasoning, and they are
// what the next person should re-run before changing the shape again.
//
// Run: go test ./internal/store/ -run XXX -bench BenchmarkSearch -benchtime 5x
//
// Measured on an Apple M2 Max, items with 120-word bodies, 5 iterations:
//
//	                       LIKE only    with items_fts
//	  1,000 items, common      14 ms            19 ms
//	 10,000 items, common     178 ms           175 ms
//	 50,000 items, common   1,008 ms         1,579 ms
//	 50,000 items, rare     ~1,000 ms          0.21 ms
//	 50,000 items, one hit  ~1,000 ms          0.74 ms
//
// Read the last two rows first: a term that narrows the corpus -- which is
// what searching for a ticket actually is -- went from a full second to a
// fifth of a millisecond.
//
// The third row is the honest cost. A term that appears in nearly every
// item makes the index return nearly every row, so the trigram lookup is
// paid for and narrows nothing, and the LIKE conditions then run anyway.
// That case is 55% slower than it was. It is kept rather than optimised
// away: dropping the LIKE conditions when the prefilter ran would recover
// it, and would make correctness depend entirely on trigram MATCH being
// exactly equivalent to LIKE '%x%' forever. TestSearchFTSAgreesWithAFullScan
// says they agree today, on every case anyone thought to write down, which
// is not the same as a guarantee -- and a query matching 90% of a fifty
// thousand item store is not a search anyone is waiting on the result of.

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// words is the vocabulary the synthetic corpus is built from. It is small
// on purpose: a 25-word vocabulary in a 120-word body means almost every
// item contains almost every word, which is what makes the "common" case
// genuinely unselective rather than accidentally so.
var words = []string{"refresh", "token", "expire", "queue", "agent", "store", "sqlite", "dashboard", "lease", "interlock", "migration", "schema", "blocked", "roadmap", "capture", "refine", "score", "effort", "retry", "handler", "payment", "refund", "auth", "session", "backlog"}

func seedStore(tb testing.TB, n int) *Store {
	tb.Helper()
	s, err := Init(filepath.Join(tb.TempDir(), "bench.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < n; i++ {
		body := ""
		for j := 0; j < 120; j++ {
			body += words[rng.Intn(len(words))] + " "
		}
		if i == n/2 {
			body += "pangolin "
		}
		if _, err := s.AddItem(ctx, AddItemInput{
			Title: fmt.Sprintf("%s %s %d", words[rng.Intn(len(words))], words[rng.Intn(len(words))], i),
			Actor: "bench",
			Body:  body,
			Why:   words[rng.Intn(len(words))] + ".go:42",
		}); err != nil {
			tb.Fatal(err)
		}
	}
	return s
}

func benchN(b *testing.B, n int, term string) {
	s := seedStore(b, n)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Search(ctx, SearchFilter{Query: term}); err != nil {
			b.Fatal(err)
		}
	}
}

// "refund" is in the generator's word list, so it appears in nearly every
// 120-word body: the term matches almost the whole corpus.
func BenchmarkSearchCommon10k(b *testing.B) { benchN(b, 10000, "refund") }
func BenchmarkSearchCommon50k(b *testing.B) { benchN(b, 50000, "refund") }

// "zzq" appears nowhere. This is the selective case -- what an actual
// search for a ticket looks like.
func BenchmarkSearchRare10k(b *testing.B) { benchN(b, 10000, "zzq") }
func BenchmarkSearchRare50k(b *testing.B) { benchN(b, 50000, "zzq") }

// A term seeded into exactly one item out of n.
func BenchmarkSearchNeedle50k(b *testing.B) { benchN(b, 50000, "pangolin") }
