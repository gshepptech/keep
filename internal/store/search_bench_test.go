package store

// Benchmarks for Search over stores of 10,000 and 50,000 items with
// 120-word bodies. The common case searches for a term in nearly every body,
// the rare case for a term in none, and the needle case for a term in exactly
// one item.
//
// Run: go test ./internal/store/ -run XXX -bench BenchmarkSearch -benchtime 5x

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
// truly unselective rather than accidentally so.
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

// BenchmarkSearchCommon10k and BenchmarkSearchCommon50k search for "refund",
// which is in the generator's word list, so it appears in nearly every
// 120-word body: the term matches almost the whole corpus.
func BenchmarkSearchCommon10k(b *testing.B) { benchN(b, 10000, "refund") }
func BenchmarkSearchCommon50k(b *testing.B) { benchN(b, 50000, "refund") }

// BenchmarkSearchRare10k and BenchmarkSearchRare50k search for "zzq", which
// appears nowhere. This is the selective case, which is what a real search
// for a ticket looks like.
func BenchmarkSearchRare10k(b *testing.B) { benchN(b, 10000, "zzq") }
func BenchmarkSearchRare50k(b *testing.B) { benchN(b, 50000, "zzq") }

// BenchmarkSearchNeedle50k searches for a term seeded into exactly one item
// out of n.
func BenchmarkSearchNeedle50k(b *testing.B) { benchN(b, 50000, "pangolin") }
