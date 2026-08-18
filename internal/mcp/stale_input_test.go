package mcp

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// TestStaleInputAfter covers the one conversion between what an agent sends
// and what the store is asked. `stale` is the tool that answers "which
// claims has nothing happened to", so getting this wrong does not fail
// loudly -- it silently reports the wrong set of abandoned work.
//
// It was the only function in this package with no coverage at all, which
// is how a one-line conversion ends up unexamined: it looks too small to be
// worth a test right up until the units are wrong.
func TestStaleInputAfter(t *testing.T) {
	cases := []struct {
		name  string
		hours float64
		want  time.Duration
	}{
		// Zero is not "no wait", it is "the store decides". StaleClaims
		// treats any non-positive duration as its own default, and After
		// deliberately does not apply one -- two copies of one policy is
		// how the two drift apart.
		{"omitted or zero defers to the store", 0, 0},

		{"whole hours", 2, 2 * time.Hour},
		{"a full day", 24, 24 * time.Hour},
		{"a fraction of an hour survives", 0.5, 30 * time.Minute},
		{"a small fraction is not rounded away", 0.25, 15 * time.Minute},

		// Negative arrives as negative rather than being clamped here, for
		// the same reason zero is passed through: the store's non-positive
		// rule already covers it, and clamping would be the second copy.
		{"negative is passed through, not clamped", -1, -time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (StaleInput{SilentHours: tc.hours}).After(); got != tc.want {
				t.Errorf("StaleInput{%v}.After() = %v, want %v", tc.hours, got, tc.want)
			}
		})
	}

	// A float64 large enough to exceed int64 saturates rather than wrapping.
	// The exact result of an out-of-range float-to-int conversion is not
	// specified by the Go language, so this pins the property that matters
	// instead of the value: an absurd number of hours must never come back
	// NEGATIVE, because the store reads non-positive as "use the default"
	// and would then quietly report every claim as stale.
	t.Run("an absurd value never turns negative", func(t *testing.T) {
		for _, hours := range []float64{1e18, 1e30, math.MaxFloat64} {
			if got := (StaleInput{SilentHours: hours}).After(); got < 0 {
				t.Errorf("StaleInput{%v}.After() = %v, which is negative -- the store would read that as its default and report everything stale", hours, got)
			}
		}
	})
}

// TestStaleInputWireShape pins what actually crosses the protocol, because
// After is only correct if the field it reads was populated the way the
// schema says.
func TestStaleInputWireShape(t *testing.T) {
	t.Run("an omitted field is zero, which means the store's default", func(t *testing.T) {
		var in StaleInput
		if err := json.Unmarshal([]byte(`{}`), &in); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if in.After() != 0 {
			t.Errorf("After() = %v, want 0", in.After())
		}
	})

	t.Run("the wire name is silent_hours", func(t *testing.T) {
		var in StaleInput
		if err := json.Unmarshal([]byte(`{"silent_hours":2}`), &in); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if in.After() != 2*time.Hour {
			t.Errorf("After() = %v, want 2h -- the json tag is not silent_hours", in.After())
		}
	})

	// A string is refused rather than coerced. An agent that sends "2h",
	// which is what the CLI's --after takes, gets an error naming the field
	// instead of a silent zero that would mean "use the default".
	t.Run("a string is refused, not coerced", func(t *testing.T) {
		var in StaleInput
		err := json.Unmarshal([]byte(`{"silent_hours":"2"}`), &in)
		if err == nil {
			t.Fatal("a string was accepted; an agent sending \"2h\" would silently get the store default")
		}
	})
}
