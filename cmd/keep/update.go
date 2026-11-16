package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// runUpdate implements the `update` subcommand: change any of add's fields
// except status on an existing item. Score is recomputed on any write that
// changes one of its inputs, and this is the write that can.
//
// --status is deliberately not a registered flag. open -> in_progress ->
// closed/dropped moves only through start/close/drop, so the event log
// always records why a status changed; an --status flag here would let a
// caller close work while bypassing close's own --reason, leaving a gap in
// the audit trail (the same reasoning that keeps close and drop separate).
// A caller who types --status gets flag's own "flag provided but not
// defined: -status" on stderr and exit 2 from fs.Parse's ContinueOnError
// failure, which says plainly that the flag does not exist.
//
// cmd/keep contains no SQL: every flag's help text below says "change
// fields on an existing item", never a fragment resembling a SQL statement,
// so TestNoSQLAboveStore's literal scan does not trip on it.
func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: keep update [flags] <id>")
		fmt.Fprintln(stderr, "  at least one field flag is required; status cannot be changed here -- see keep start / close / drop")
		fs.PrintDefaults()
	}

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	actorFlag := addActorFlag(fs)

	titleFlag := fs.String("title", "", "change fields on an existing item: new title")
	bodyFlag := fs.String("body", "", "change fields on an existing item: new body")
	acceptanceFlag := fs.String("acceptance", "", "change fields on an existing item: new acceptance criteria")
	whyFlag := fs.String("why", "", "change fields on an existing item: new rationale")
	scoreFlag := fs.String("score", "", "change fields on an existing item: value/confidence/effort, each 0-10, e.g. 8/7/3 (\"none\" clears the score back to unscored)")
	typeFlag := fs.String("type", "", "change fields on an existing item: new type ("+validTypesList()+")")
	stageFlag := fs.String("stage", "", "change fields on an existing item: new stage ("+validStagesList()+")")
	repoFlag := fs.String("repo", "", "change fields on an existing item: new repo")
	pathsFlag := fs.String("paths", "", "change fields on an existing item: new paths")
	assigneeFlag := fs.String("assignee", "", "change fields on an existing item: new assignee")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)

	// fs.Visit distinguishes "supplied" (even --title "", which clears the
	// title) from "left at its zero value". Reading *titleFlag directly
	// could not tell the two apart, which is why UpdateItemInput has pointer
	// fields (internal/store/transitions.go).
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	if visited["type"] && *typeFlag != "" && !model.ValidTypes[*typeFlag] {
		fmt.Fprintf(stderr, "keep: update: --type %q is not one of %s\n", *typeFlag, validTypesList())
		return 2
	}
	if visited["stage"] && *stageFlag != "" && !model.ValidStages[*stageFlag] {
		fmt.Fprintf(stderr, "keep: update: --stage %q is not one of %s\n", *stageFlag, validStagesList())
		return 2
	}

	strPtr := func(name string, val *string) *string {
		if !visited[name] {
			return nil
		}
		v := *val
		return &v
	}

	// --score's double-pointer encoding (internal/store/transitions.go's
	// UpdateItemInput doc comment): the outer pointer means "touched"; the
	// inner pointer is nil to CLEAR the score back to unscored, or points
	// at the parsed value to SET it. "none" is the spelling that wires the
	// clearing path that encoding supports.
	var value, confidence, effort **int
	if visited["score"] {
		if *scoreFlag == "none" {
			var nilInner *int
			value, confidence, effort = &nilInner, &nilInner, &nilInner
		} else {
			v, c, e, err := parseScore(*scoreFlag)
			if err != nil {
				fmt.Fprintf(stderr, "keep: update: %v\n", err)
				return 2
			}
			vp, cp, ep := &v, &c, &e
			value, confidence, effort = &vp, &cp, &ep
		}
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: update: %v\n", err)
		return 2
	}

	in := store.UpdateItemInput{
		ID:         id,
		Actor:      actor,
		SessionID:  resolveSession(),
		Title:      strPtr("title", titleFlag),
		Body:       strPtr("body", bodyFlag),
		Acceptance: strPtr("acceptance", acceptanceFlag),
		Why:        strPtr("why", whyFlag),
		Type:       strPtr("type", typeFlag),
		Stage:      strPtr("stage", stageFlag),
		Repo:       strPtr("repo", repoFlag),
		Paths:      strPtr("paths", pathsFlag),
		Assignee:   strPtr("assignee", assigneeFlag),
		Value:      value,
		Confidence: confidence,
		Effort:     effort,
	}

	// No fields supplied is a usage error (exit 2), not a silent no-op, and
	// it is returned before opening the store: opening a database to
	// discover the caller asked for nothing is wasted work and a worse error
	// message. It checks the built UpdateItemInput's own fields rather than
	// counting fs.NFlag(), because --db/--actor/--json also count toward
	// NFlag but are not field flags.
	anyFieldSet := in.Title != nil || in.Body != nil || in.Acceptance != nil || in.Why != nil ||
		in.Type != nil || in.Stage != nil || in.Repo != nil || in.Paths != nil || in.Assignee != nil ||
		in.Value != nil || in.Confidence != nil || in.Effort != nil
	if !anyFieldSet {
		fmt.Fprintln(stderr, "keep: update: at least one field flag is required (see keep update -h)")
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	item, err := s.UpdateItem(context.Background(), in)
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, item); err != nil {
			fmt.Fprintf(stderr, "keep: update: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "%s updated\n", item.ID)
	return 0
}
