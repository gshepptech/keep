package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gshepptech/rounce/internal/model"
)

// runShow implements the `show` subcommand (ITEM-04): the labelled block an
// agent reads before starting an item -- id, stage/type/status, score with
// its raw value/confidence/effort, dependencies in both directions, event
// history oldest first, then the body verbatim last (02-CONTEXT.md
// decision 2).
//
// One store.GetItem call. An unknown id returns store.ErrNotFound, already
// wrapped with the id by GetItem, mapped to exit 1 with nothing on stdout by
// exitFor.
func runShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: rounce show <id>")
		return 2
	}
	id := fs.Arg(0)

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
		return 1
	}
	defer s.Close()

	detail, err := s.GetItem(context.Background(), id)
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, detail); err != nil {
			fmt.Fprintf(stderr, "rounce: show: %v\n", err)
			return 1
		}
		return 0
	}

	writeShowHeader(stdout, detail.Item)
	writeDepSection(stdout, "depends on", detail.DependsOn, func(d model.Dep) string { return d.DependsOnID })
	writeDepSection(stdout, "depended on by", detail.DependedOnBy, func(d model.Dep) string { return d.ItemID })
	writeEventsSection(stdout, detail.Events)

	// The body goes last and verbatim: one blank line, the "body" header,
	// then the body's own bytes with nothing appended -- no trailing
	// decoration, so `rounce show <id> | tail -n +N` gets the document
	// back intact. Phase 3's CAP-02 requires this exact discipline.
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "body")
	io.WriteString(stdout, detail.Body)

	return 0
}

// showLabelWidth is the left column's fixed width in the header block: the
// widest label in the fixed set below is "acceptance" (10 chars) plus one
// separating space. Fixed rather than computed per call, since the label set
// itself never changes.
const showLabelWidth = 11

// writeShowHeader renders the fixed-order labelled block: id, stage, type,
// status, score, parent, repo, paths, why, acceptance, created, updated
// (02-CONTEXT.md decision 2 / 02-08-PLAN.md task 2's exact worked example).
// A field's line is omitted entirely when the underlying column is null or
// empty, rather than printed with an empty value -- a show full of blank
// labels is harder to read than a short one.
func writeShowHeader(w io.Writer, it model.Item) {
	line := func(label, value string) {
		fmt.Fprintf(w, "%-*s%s\n", showLabelWidth, label, value)
	}

	line("id", it.ID)
	line("stage", it.Stage)
	line("type", it.Type)
	line("status", it.Status)
	line("score", fmt.Sprintf("%d  %s", it.Score, scoreDetail(it)))
	if it.ParentID != nil {
		line("parent", *it.ParentID)
	}
	if it.Repo != nil {
		line("repo", *it.Repo)
	}
	if it.Paths != nil {
		line("paths", *it.Paths)
	}
	if it.Why != "" {
		line("why", it.Why)
	}
	if it.Acceptance != "" {
		line("acceptance", it.Acceptance)
	}
	line("created", it.CreatedAt)
	line("updated", it.UpdatedAt)
}

// scoreDetail renders the raw value/confidence/effort inputs alongside the
// stored score. Per RANK-02/02-CONTEXT.md decision 1, "unscored" is only the
// all-NULL case; any other combination shows the raw inputs, missing ones
// counted as 0 -- the same substitution rank.Score itself already applied to
// produce the stored score.
func scoreDetail(it model.Item) string {
	if it.Value == nil && it.Confidence == nil && it.Effort == nil {
		return "(unscored)"
	}
	v, c, e := 0, 0, 0
	if it.Value != nil {
		v = *it.Value
	}
	if it.Confidence != nil {
		c = *it.Confidence
	}
	if it.Effort != nil {
		e = *it.Effort
	}
	return fmt.Sprintf("(value %d, confidence %d, effort %d)", v, c, e)
}

// depTypeWidth is the fixed column width for a dep's type label in the
// "depends on"/"depended on by" sections: the closed vocabulary's two
// longest members, "parent-child" and "derived-from", are both 12
// characters, plus a 2-space separator.
const depTypeWidth = 14

// writeDepSection renders one dependency-direction block: a blank line, the
// header, then one indented "<type>  <id>" line per dep, via idFor (which
// side of the edge -- DependsOnID or ItemID -- to print depends on the
// section's direction). The whole section, including its header, is omitted
// when there are no deps in that direction: a header over nothing is
// clutter, not information.
func writeDepSection(w io.Writer, header string, deps []model.Dep, idFor func(model.Dep) string) {
	if len(deps) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, header)
	for _, d := range deps {
		fmt.Fprintf(w, "  %-*s%s\n", depTypeWidth, d.Type, idFor(d))
	}
}

// writeEventsSection renders the event history, oldest first (Events is
// already ordered id ASC by GetItem). Kind/actor columns are aligned to the
// widest value in THIS item's own event set, matching renderItems' own
// per-result-set alignment convention rather than a fixed width. The section
// is omitted entirely when there are no events (never true in practice --
// every item has at least an `add` event -- but treated the same as the dep
// sections for consistency).
func writeEventsSection(w io.Writer, events []model.Event) {
	if len(events) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "events")

	kindW, actorW := 0, 0
	for _, e := range events {
		if l := len(e.Kind); l > kindW {
			kindW = l
		}
		if l := len(e.Actor); l > actorW {
			actorW = l
		}
	}
	for _, e := range events {
		var b strings.Builder
		fmt.Fprintf(&b, "  %s  %-*s  %-*s", e.At, kindW, e.Kind, actorW, e.Actor)
		if e.SessionID != nil && *e.SessionID != "" {
			fmt.Fprintf(&b, "  session=%s", *e.SessionID)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}
