#!/usr/bin/env bash
#
# What the tests actually cover, including the parts that run in a subprocess.
#
# `go test ./... -cover` reports cmd/keep at a few percent. That number is not
# a gap, it is a blind spot: most of this package's behaviour is tested in a
# real built binary driven through os/exec, and the in-process coverage
# instrument cannot see into another process. A figure that reads as "barely
# tested" where the truth is "tested somewhere the meter cannot reach" is worse
# than no figure -- it hides any real gap underneath a number nobody trusts.
#
# So this builds the binary with -cover (Go 1.20+), points the cross-process
# tests at a GOCOVERDIR, and reports both halves:
#
#   in-process   what `go test -cover` sees, per package
#   subprocess   what the built binary executed, merged across every test run
#
# The two are reported separately rather than summed. They measure overlapping
# statements through different instruments, and adding them would invent a
# precision neither has.
#
# Usage: scripts/coverage.sh
set -euo pipefail

cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
covdata="$work/covdata"
mkdir -p "$covdata"

echo "== in-process, what go test -cover sees =="
go test ./... -count=1 -coverprofile="$work/in-process.out" >"$work/in-process.log" 2>&1 || true
grep -E 'coverage:|FAIL' "$work/in-process.log" || true

echo
echo "== subprocess, what the built binary executed =="
# KEEP_COVERDIR makes cmd/keep's buildKeep helper add -cover, and makes its
# run helper hand each subprocess a GOCOVERDIR to write counters into.
KEEP_COVERDIR="$covdata" go test ./cmd/keep/ -count=1 >/dev/null 2>&1 ||
	{ echo "the instrumented run failed; coverage below would be a lie" >&2; exit 1; }

# A measurement that measured nothing must fail rather than print 0%. This is
# the same rule the repository's scanning tests follow: a walk that finds
# nothing is a broken walk, not a clean result.
count=$(find "$covdata" -name 'covcounters.*' | wc -l | tr -d ' ')
if [ "$count" -eq 0 ]; then
	echo "no coverage counters were written to $covdata." >&2
	echo "the binary was not built with -cover, or GOCOVERDIR never reached it." >&2
	echo "treat this as a failure, not as 0% coverage." >&2
	exit 1
fi
echo "($count subprocess runs contributed counters)"
echo

go tool covdata percent -i="$covdata"

echo
echo "== what this changes =="
go tool covdata textfmt -i="$covdata" -o "$work/subprocess.out"

# Reuse the first run's output rather than paying for the suite twice.
in_process=$(awk '/cmd\/keep\t/ {for (i=1; i<=NF; i++) if ($i ~ /%$/) print $i}' "$work/in-process.log")
through_binary=$(go tool covdata percent -i="$covdata" |
	awk '/cmd\/keep/ {for (i=1; i<=NF; i++) if ($i ~ /%$/) print $i}')

printf '%-34s %s\n' "cmd/keep as go test reports it:" "${in_process:-unknown}"
printf '%-34s %s\n' "cmd/keep as actually executed:" "${through_binary:-unknown}"
echo
echo "The first number is the blind spot, not the coverage. Cite the second."
