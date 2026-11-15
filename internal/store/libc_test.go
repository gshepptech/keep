package store

import (
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
)

// expectedLibcVersion is the exact modernc.org/libc version
// modernc.org/sqlite v1.56.0's own go.mod requires (see the comment above
// the modernc.org/libc line in this project's go.mod). Named
// here, next to the assertion, so the failure message and the assertion
// itself cannot drift apart.
const expectedLibcVersion = "v1.74.4"

// TestLibcVersionPinned is a regression test: it fails if the
// module graph's resolved modernc.org/libc version is not exactly
// expectedLibcVersion. A mismatch would mean some future `go get` resolved a
// different libc version independently of modernc.org/sqlite's own
// requirement, which corrupts data silently
// (gitlab.com/cznic/sqlite/-/issues/177).
//
// Why `go list -m` and not debug.ReadBuildInfo(): on the Go 1.26
// toolchain, debug.ReadBuildInfo() returns ok=true but an always-
// empty Deps slice for a test binary built from a non-main package (this
// package, "store", is never package main); the same check inside a
// package-main test's own _test.go correctly lists every dependency. Because
// Deps is unconditionally empty here regardless of the actual resolved
// version, the ReadBuildInfo() form of this test would fail every run --
// including when libc has not drifted at all -- which is a false positive,
// not a real regression signal, and would train contributors to ignore this
// test's output. A test that FAILS unconditionally is just as
// untrustworthy as one that passes unconditionally.
//
// `go list -m` reads the same resolved module graph (go.mod + go.sum) that
// produces the binary in the first place. It requires the go
// toolchain to be on PATH at test time, which holds anywhere this module
// can be built or tested at all.
func TestLibcVersionPinned(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "modernc.org/libc").Output()
	if err != nil {
		// `go` not on PATH, or run in some environment where `go list`
		// cannot execute -- a tooling limitation, not evidence of drift.
		t.Skipf("`go list -m modernc.org/libc` unavailable: %v", err)
	}

	got := strings.TrimSpace(string(out))
	if got == "" {
		// modernc.org/libc not present in the module graph at all means
		// modernc.org/sqlite stopped depending on it -- itself a change
		// worth failing on, not silently skipping.
		t.Fatal("modernc.org/libc not found in module graph")
	}
	if got != expectedLibcVersion {
		t.Fatalf("modernc.org/libc drifted to %s, expected %s (required by modernc.org/sqlite v1.56.0, see gitlab.com/cznic/sqlite/-/issues/177)", got, expectedLibcVersion)
	}

	// Secondary, best-effort corroboration via debug.ReadBuildInfo(), kept
	// because it is cheap and catches a different failure mode (this
	// binary's OWN embedded module info disagreeing with `go list`'s view,
	// e.g. a stale cached test binary). Never the sole check -- see the
	// deviation note above for why Deps is expected to be empty here on
	// this toolchain, so an empty/absent Deps entry is not itself a
	// failure, only a disagreement with a *present* entry is.
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "modernc.org/libc" && dep.Version != expectedLibcVersion {
				t.Fatalf("build info reports modernc.org/libc %s, expected %s", dep.Version, expectedLibcVersion)
			}
		}
	}
}
