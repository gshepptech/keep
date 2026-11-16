package main

import (
	"context"
	"flag"
	"fmt"
	"io"
)

// runWhyNot implements the `why-not` subcommand: the reason one item is not
// in `ready`.
//
// Without it the answer has to be assembled by eye from `show` (stage and
// status) and `blocked` (the propagated case), against a predicate that
// lives in a SQL view. A person can do that; an agent reliably cannot, and
// the failure is silent: it concludes the item is simply low-priority and
// moves on.
//
// Exit code is the answer, so a script can branch on it without parsing:
// 0 when the item is workable, 1 when it is not. An unknown id is exit 1
// with ErrNotFound's message, via exitFor.
func runWhyNot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("why-not", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: keep why-not [flags] <id>")
		return 2
	}

	s, err := openStoreReadOnly(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	w, err := s.WhyNot(context.Background(), fs.Arg(0))
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, w); err != nil {
			fmt.Fprintf(stderr, "keep: why-not: %v\n", err)
			return 1
		}
	} else if w.Ready {
		fmt.Fprintf(stdout, "%s is workable now\n", w.ID)
	} else {
		fmt.Fprintf(stdout, "%s %s\n", w.ID, w.Reason)
	}

	if w.Ready {
		return 0
	}
	return 1
}
