package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// runCapture implements the `capture` subcommand (CAP-01) -- the raw-path
// front door -- as a thin wrapper over the existing store.AddItem, exactly
// as 03-CONTEXT.md decision 3 and 03-RESEARCH.md Pattern 1 settle it: no new
// store method, no new EVENT-01 table row.
//
// `capture` earns being its own command rather than an alias for
// `add --stage raw` in three ways:
//  1. `--body -` reads stdin (via the shared resolveBody, cmd/keep/
//     flags.go), so an agent can pipe a document straight in --
//     `keep capture "Q3 roadmap" --type roadmap --body - < ROADMAP.md`
//     is the canonical invocation docs/design/phase-2.md documents.
//  2. Its stage is pinned to model.StageRaw and cannot be set to "ticket" --
//     capture means capture; something that could produce a ticket is
//     `add`. `--stage` is deliberately NOT a registered flag on this
//     FlagSet, mirroring update.go's identical, deliberate omission of
//     `--status`: a caller who types `keep capture --stage ticket ...`
//     gets flag's own native "flag provided but not defined: -stage" at
//     exit 2, not a hand-written refusal -- the same structural-enforcement
//     idiom already shipped in Phase 2 (verified: `grep -n stage
//     cmd/keep/capture.go` shows no registered flag).
//  3. `--type` defaults to model.TypeNote (not add's "task", and not
//     AddItem's own empty-Type default) -- the common case: a finding
//     parked mid-session, with `--type roadmap` for documents.
//
// `--parent`, `--blocks`, and `--score` are omitted deliberately, not by
// oversight (03-CONTEXT.md decision 3, 03-RESEARCH.md Open Question 1):
// placement and scoring are refinement's job (`add --parent
// --derived-from`), not capture's. It is purely additive to expand this
// flag set later; it is not additive to remove from it.
//
// cmd/keep must never import a database driver package, must never
// contain a SQL string, and must never receive a raw DB handle or a row
// (STORE-10) -- runCapture builds exactly one store.AddItemInput and makes
// exactly one store.AddItem call, mirroring runAdd's shape.
func runCapture(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: keep capture [flags] <title>")
		fmt.Fprintln(stderr, "  stage is always raw; there is no --stage flag")
		fs.PrintDefaults()
	}

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	actorFlag := addActorFlag(fs)

	typeFlag := fs.String("type", model.TypeNote, "item type, default note ("+validTypesList()+")")
	bodyFlag := fs.String("body", "", "body text, or - to read it from stdin")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	title := strings.Join(fs.Args(), " ")

	if !model.ValidTypes[*typeFlag] {
		fmt.Fprintf(stderr, "keep: capture: --type %q is not one of %s\n", *typeFlag, validTypesList())
		return 2
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: capture: %v\n", err)
		return 2
	}

	// Resolved before the store is opened, so a stdin read failure exits
	// without leaving an open store behind, exactly as runAdd does.
	body, err := resolveBody(*bodyFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: capture: %v\n", err)
		return 1
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	item, err := s.AddItem(context.Background(), store.AddItemInput{
		Title:     title,
		Actor:     actor,
		SessionID: resolveSession(),
		Type:      *typeFlag,
		Stage:     model.StageRaw,
		Body:      body,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, item); err != nil {
			fmt.Fprintf(stderr, "keep: capture: %v\n", err)
			return 1
		}
		return 0
	}

	// One bare line of id, nothing else -- what makes
	// `id=$(keep capture ...)` work in an agent's shell pipeline, exactly
	// as `add` already does.
	fmt.Fprintln(stdout, item.ID)
	return 0
}
