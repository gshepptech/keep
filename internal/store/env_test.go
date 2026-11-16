package store

import (
	"os"
	"testing"
)

// setOrUnset sets name, or removes it entirely when set is false. t.Setenv
// is called either way so the testing package registers the restore; the
// Unsetenv after it is what makes "absent" really absent rather than
// present-and-empty. Env treats those two the same on purpose, and this
// helper is what lets the test prove it rather than assume it.
func setOrUnset(t *testing.T, name, value string, set bool) {
	t.Helper()
	t.Setenv(name, value)
	if !set {
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

// TestEnvPrefersKeepAndFallsBackToRounce checks the environment precedence:
// KEEP_ wins, ROUNCE_ is honoured when KEEP_ is absent, and KEEP_ set to
// the empty string counts as absent rather than as an override.
//
// Empty is treated as absent, as elsewhere in the package, so
// `KEEP_ACTOR= keep add ...` on a machine whose profile still exports
// ROUNCE_ACTOR uses the ROUNCE_ value.
func TestEnvPrefersKeepAndFallsBackToRounce(t *testing.T) {
	for _, c := range []struct {
		name     string
		keep     string
		keepSet  bool
		older    string
		olderSet bool
		want     string
	}{
		{name: "neither set", want: ""},
		{name: "only keep", keep: "new", keepSet: true, want: "new"},
		{name: "only rounce", older: "old", olderSet: true, want: "old"},
		{name: "both set, keep wins", keep: "new", keepSet: true, older: "old", olderSet: true, want: "new"},
		{name: "keep set empty falls through", keep: "", keepSet: true, older: "old", olderSet: true, want: "old"},
		{name: "keep set empty, no fallback, stays empty", keep: "", keepSet: true, want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			setOrUnset(t, "KEEP_TESTVAR", c.keep, c.keepSet)
			setOrUnset(t, "ROUNCE_TESTVAR", c.older, c.olderSet)

			if got := Env("TESTVAR"); got != c.want {
				t.Errorf("Env(TESTVAR) = %q, want %q", got, c.want)
			}
		})
	}
}
