package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gshepptech/rounce/internal/store"
)

// runLog implements the `log` subcommand: what happened, newest first.
//
// rounce's whole premise is agents working while nobody is watching, and
// until this the only way to find out what they did was to open items one
// at a time. The events table has always held the answer -- EVENT-01 makes
// it the complete append-only record of every mutation -- so this reads it
// rather than recording anything new. There is nothing here that can drift
// from what actually happened.
func runLog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	limitFlag := fs.Int("limit", 30, "at most n entries (0 for all)")
	sinceFlag := fs.String("since", "", "only what happened since: 24h, 7d, or an RFC3339 timestamp")
	actorFlag := fs.String("actor", "", "only what this person or agent did")
	kindFlag := fs.String("kind", "", "only this kind of event (add, start, close, drop, update, dep)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: rounce log [flags]")
		return 2
	}

	since, err := resolveSince(*sinceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: log: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
	defer s.Close()

	entries, err := s.Feed(context.Background(), store.FeedFilter{
		Since: since,
		Actor: *actorFlag,
		Kind:  *kindFlag,
		Limit: *limitFlag,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		out := entries
		if out == nil {
			out = []store.FeedEntry{}
		}
		if err := emitJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "rounce: log: %v\n", err)
			return 1
		}
		return 0
	}

	if len(entries) == 0 {
		fmt.Fprintln(stderr, "nothing has happened yet")
		return 0
	}

	renderFeed(stdout, entries)
	return 0
}

// resolveSince turns "24h", "7d" or an RFC3339 timestamp into a timestamp
// the store can compare against.
//
// The short forms exist because "what happened today" is the question, and
// nobody wants to type a timestamp to ask it. Go's ParseDuration handles
// h/m/s but not days, so d is expanded first rather than left as the one
// unit that mysteriously does not work.
func resolveSince(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	if days, ok := strings.CutSuffix(spec, "d"); ok {
		if n, err := time.ParseDuration(days + "h"); err == nil {
			return time.Now().Add(-n * 24).UTC().Format(time.RFC3339Nano), nil
		}
	}
	if d, err := time.ParseDuration(spec); err == nil {
		return time.Now().Add(-d).UTC().Format(time.RFC3339Nano), nil
	}
	if t, err := time.Parse(time.RFC3339, spec); err == nil {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	return "", fmt.Errorf("cannot read %q as a time: try 24h, 7d, or 2026-01-31T00:00:00Z", spec)
}

// renderFeed prints the feed as aligned columns, same rules as every other
// table this CLI prints: no colour, widest-value alignment, and the thing
// you scan for on the left.
func renderFeed(w io.Writer, entries []store.FeedEntry) {
	whenW, kindW, actorW := 0, 0, 0
	whens := make([]string, len(entries))
	for i, e := range entries {
		whens[i] = agoOrRaw(e.At)
		whenW = max(whenW, len(whens[i]))
		kindW = max(kindW, len(e.Kind))
		actorW = max(actorW, len(e.Actor))
	}

	// Titles and reasons are both truncated for the same reason the rest of
	// this CLI truncates titles: a close reason in a real store is often
	// several paragraphs -- agents write down what they did -- and printing
	// one in full turns a twenty-line log into a wall nobody reads. The
	// whole text is one `rounce show <id>` away, and --json never truncates.
	// The title is what you scan for, so it keeps the larger share; the
	// reason is context you read only on the row you stopped at.
	budget := titleWidth(w)
	titleBudget := max(budget/2, minTitleBudget)
	reasonBudget := max(budget/2, minTitleBudget)

	for i, e := range entries {
		// The reason is the useful half of a close or a drop; without it
		// the log says work ended but never why.
		suffix := ""
		if e.Reason != "" {
			suffix = "  -- " + truncateTitle(oneLine(e.Reason), reasonBudget)
		}
		fmt.Fprintf(w, "%-*s  %-*s  %-*s  %s  %s%s\n",
			whenW, whens[i], kindW, e.Kind, actorW, e.Actor,
			e.ItemID, truncateTitle(e.Title, titleBudget), suffix)
	}
}

// oneLine flattens a multi-line reason onto one line, so a paragraph does
// not break the column alignment of every row after it.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// agoOrRaw prints a timestamp as an age, falling back to the raw string if
// it cannot be read. An unparseable timestamp is worth showing verbatim
// rather than hiding behind a wrong "0s ago".
func agoOrRaw(at string) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return at
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
