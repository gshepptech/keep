package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/gshepptech/keep/internal/store"
)

// runStale implements the `stale` subcommand: claims nothing has happened
// to in a while.
//
// The gap this closes: an agent claims an item, its session dies, and the
// item sits in_progress for ever. The queue is quietly one item shorter and
// nothing ever says why. Interlock's leases expire on their own; a keep
// claim does not, because a claim is a statement about intent rather than
// about a live process, and expiring intent automatically would mean two
// agents on one item -- the exact thing keep exists to prevent.
//
// So this reports and does not act. What it gives you is the one thing that
// was missing: the question being asked at all.
func runStale(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stale", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	afterFlag := fs.String("after", store.DefaultStaleAfter.String(),
		"how much silence counts as stale (2h, 30m, 1d)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep stale [flags]")
		return 2
	}

	after, err := parseSilence(*afterFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: stale: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	claims, err := s.StaleClaims(context.Background(), after)
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		out := claims
		if out == nil {
			out = []store.StaleClaim{}
		}
		if err := emitJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "keep: stale: %v\n", err)
			return 1
		}
		return 0
	}

	if len(claims) == 0 {
		fmt.Fprintf(stderr, "nothing has been claimed and left for more than %s\n", after)
		return 0
	}

	// Each row says what to do about it, because "this is stale" is only
	// actionable if you know the two ways out: finish it or let it go.
	idW := 0
	for _, c := range claims {
		idW = max(idW, len(c.ID))
	}
	for _, c := range claims {
		who := "nobody recorded"
		if c.Assignee != nil && *c.Assignee != "" {
			who = *c.Assignee
		}
		fmt.Fprintf(stdout, "%-*s  silent %-8s  claimed by %-16s  %s\n",
			idW, c.ID, roundSilence(c.Silent), who, c.Title)
	}
	// `drop` is not the way to "hand it back": drop says the work was not
	// worth doing, and a claim going quiet says nothing of the sort. Three
	// exits, and the middle one is the one this command is usually about.
	fmt.Fprintf(stderr, "\n%d claim%s with nothing happening. Put one back with `keep unclaim <id> --reason ...`,\nfinish it with `keep close <id> --reason ...`, or let it go with `keep drop <id> --reason ...`.\n",
		len(claims), map[bool]string{true: "", false: "s"}[len(claims) == 1])
	return 0
}

// parseSilence reads a duration, accepting the d suffix Go's own parser
// does not, for the same reason `log --since` does.
func parseSilence(spec string) (time.Duration, error) {
	if days, ok := cutDaySuffix(spec); ok {
		d, err := time.ParseDuration(days + "h")
		if err == nil {
			return d * 24, nil
		}
	}
	d, err := time.ParseDuration(spec)
	if err != nil {
		return 0, fmt.Errorf("cannot read %q as a duration: try 2h, 30m or 1d", spec)
	}
	if d <= 0 {
		return 0, fmt.Errorf("a stale threshold must be positive, got %q", spec)
	}
	return d, nil
}

func cutDaySuffix(spec string) (string, bool) {
	if len(spec) > 1 && spec[len(spec)-1] == 'd' {
		return spec[:len(spec)-1], true
	}
	return "", false
}

// roundSilence prints a duration at the precision a person cares about.
// "silent 4h" is the useful fact; "silent 4h13m52.008s" is the same fact
// with work to do before you can read it.
func roundSilence(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}
