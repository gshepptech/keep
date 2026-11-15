package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
)

func intp(i int) *int { return &i }

// TestScoredWorkAlwaysOutranksUnscored pins "scored work always outranks
// unscored work" as a property rather than
// as a range check, because the range check is only the mechanism.
//
// internal/rank documents the inputs as 0-10 and its scoreShift of 11 is
// chosen so the worst legal score (0 + 0 - 10 + 11) is 1, one above an
// unscored item's 0. That guarantee holds only while something enforces the
// range, and it has to be the store: if only cmd/keep's flag parser checked
// it, an MCP agent could file effort=100 for a score of -89 and bury the
// item below unscored work permanently.
func TestScoredWorkAlwaysOutranksUnscored(t *testing.T) {
	ctx := context.Background()

	t.Run("no legal input can score below an unscored item", func(t *testing.T) {
		s := openTestStore(t)
		unscored, err := s.AddItem(ctx, AddItemInput{Title: "unscored", Actor: "t"})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}
		if unscored.Score != 0 {
			t.Fatalf("an unscored item scores %d, want 0; the premise of this test is wrong", unscored.Score)
		}

		// Every corner of the legal space, not a sample. The range is small
		// enough to exhaust, so exhaust it.
		for v := model.ScoreMin; v <= model.ScoreMax; v++ {
			for c := model.ScoreMin; c <= model.ScoreMax; c++ {
				for e := model.ScoreMin; e <= model.ScoreMax; e++ {
					item, err := s.AddItem(ctx, AddItemInput{
						Title: "scored", Actor: "t",
						Value: intp(v), Confidence: intp(c), Effort: intp(e),
					})
					if err != nil {
						t.Fatalf("AddItem(%d/%d/%d): %v", v, c, e, err)
					}
					if item.Score <= unscored.Score {
						t.Fatalf("%d/%d/%d scores %d, which does not beat an unscored item's %d",
							v, c, e, item.Score, unscored.Score)
					}
				}
			}
		}
	})

	t.Run("the store refuses what would break it", func(t *testing.T) {
		s := openTestStore(t)
		cases := []struct {
			name           string
			v, c, e        *int
			wantNamedInMsg string
		}{
			// The wanted text pairs the input's NAME with its VALUE.
			// Checking for the bare word would pass on the sentinel's own
			// message, which already lists all three -- an assertion that
			// looks like it pins "say which one" and pins nothing.
			{"effort above the range", nil, nil, intp(100), "effort is 100"},
			{"value above the range", intp(1000), nil, nil, "value is 1000"},
			{"confidence below the range", nil, intp(-1), nil, "confidence is -1"},
			{"the boundary just outside", intp(11), nil, nil, "value is 11"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := s.AddItem(ctx, AddItemInput{
					Title: "x", Actor: "t", Value: tc.v, Confidence: tc.c, Effort: tc.e,
				})
				if !errors.Is(err, ErrScoreOutOfRange) {
					t.Fatalf("err = %v, want ErrScoreOutOfRange", err)
				}
				// The message has to say which input to fix, and with what
				// value. "something was out of range" sends an agent
				// guessing between three.
				if !strings.Contains(err.Error(), tc.wantNamedInMsg) {
					t.Errorf("message %q does not name %q", err, tc.wantNamedInMsg)
				}
			})
		}
	})

	t.Run("the boundary values themselves are legal", func(t *testing.T) {
		s := openTestStore(t)
		for _, n := range []int{model.ScoreMin, model.ScoreMax} {
			if _, err := s.AddItem(ctx, AddItemInput{
				Title: "boundary", Actor: "t", Value: intp(n), Confidence: intp(n), Effort: intp(n),
			}); err != nil {
				t.Errorf("AddItem with all inputs %d: %v, want it accepted", n, err)
			}
		}
	})

	// update is a write path too, and as of the MCP `update` tool it is one an
	// agent can reach. A guard on AddItem alone would leave the same hole one
	// call later.
	t.Run("update is guarded as well as add", func(t *testing.T) {
		s := openTestStore(t)
		item, err := s.AddItem(ctx, AddItemInput{Title: "fine", Actor: "t", Value: intp(5)})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}

		bad := intp(99)
		_, err = s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "t", Effort: &bad})
		if !errors.Is(err, ErrScoreOutOfRange) {
			t.Fatalf("UpdateItem with effort=99: err = %v, want ErrScoreOutOfRange", err)
		}

		// Clearing a score back to unscored is not a number and must stay
		// legal -- the outer pointer is set, the inner is nil.
		var clear *int
		if _, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "t", Value: &clear}); err != nil {
			t.Errorf("clearing a score: %v, want it accepted", err)
		}

		// And a legal change still goes through.
		ok := intp(7)
		if _, err := s.UpdateItem(ctx, UpdateItemInput{ID: item.ID, Actor: "t", Confidence: &ok}); err != nil {
			t.Errorf("a legal update: %v", err)
		}
	})
}
