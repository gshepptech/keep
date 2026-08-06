package rank

import "testing"

// TestScore_UnscoredAlwaysSortsLast enumerates the FULL input space rather
// than sampling it: each of value/confidence/effort is either nil or one of
// 0..10 -- 12 possibilities per input, 12^3 = 1,728 combinations total. This
// completes in well under a second, so there is no reason to reach for
// property-based/fuzz testing here; exhaustive enumeration is strictly
// stronger and just as fast.
func TestScore_UnscoredAlwaysSortsLast(t *testing.T) {
	opts := func() []*int {
		vals := []*int{nil}
		for i := 0; i <= 10; i++ {
			i := i
			vals = append(vals, &i)
		}
		return vals
	}
	var cases int
	for _, v := range opts() {
		for _, c := range opts() {
			for _, e := range opts() {
				cases++
				got := Score(v, c, e)
				allNil := v == nil && c == nil && e == nil
				if allNil {
					if got != 0 {
						t.Fatalf("all-nil should score 0, got %d", got)
					}
					continue
				}
				if got < 1 {
					t.Fatalf("scored combo (v=%v c=%v e=%v) produced %d, want >=1 so it outranks the unscored 0", v, c, e, got)
				}
			}
		}
	}
	const wantCases = 12 * 12 * 12 // 1,728 -- nil plus 0..10 for each of value/confidence/effort
	if cases != wantCases {
		t.Fatalf("enumerated %d combinations, want %d -- the full space did not run", cases, wantCases)
	}
}

// TestScore_ConcreteCases pins the arithmetic itself, not just the
// >=1-vs-0 inequality the exhaustive test proves.
func TestScore_ConcreteCases(t *testing.T) {
	ptr := func(i int) *int { return &i }

	cases := []struct {
		name                      string
		value, confidence, effort *int
		want                      int
	}{
		{"all nil scores 0", nil, nil, nil, 0},
		{"8/7/3 scores 8+7-3+11=23", ptr(8), ptr(7), ptr(3), 23},
		{"nil members of a scored triple count as 0: 5/nil/nil scores 5+0-0+11=16", ptr(5), nil, nil, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Score(tc.value, tc.confidence, tc.effort)
			if got != tc.want {
				t.Fatalf("Score(%v, %v, %v) = %d, want %d", tc.value, tc.confidence, tc.effort, got, tc.want)
			}
		})
	}
}
