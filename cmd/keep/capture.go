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

// runCapture implements the `capture` subcommand, the front door for raw
// items, as a thin wrapper over store.AddItem: no new store method, no new
// kind of event.
//
// `capture` earns being its own command rather than an alias for
// `add --stage raw` in three ways:
//  1. `--body -` reads stdin (via the shared resolveBody, cmd/keep/
//     flags.go), so an agent can pipe a document straight in:
//     `keep capture "Q3 roadmap" --type roadmap --body - < q3-plan.md`
//     is the canonical invocation.
//  2. Its stage is pinned to model.StageRaw and cannot be set to "ticket";
//     something that could produce a ticket is `add`. `--stage` is not a
//     registered flag on this FlagSet, as update.go omits `--status`: a
//     caller who types `keep capture --stage ticket ...` gets flag's own
//     "flag provided but not defined: -stage" at exit 2, not a
//     hand-written refusal.
//  3. `--type` defaults to model.TypeNote (not add's "task", and not
//     AddItem's own empty-Type default), the common case being a finding
//     parked mid-session, with `--type roadmap` for documents.
//
// `--parent`, `--blocks`, and `--score` are omitted on purpose: placement
// and scoring are refinement's job (`add --parent --derived-from`), not
// capture's. Adding a flag later is compatible; removing one is not.
//
// cmd/keep never imports a database driver package, never contains a SQL
// string, and never receives a raw DB handle or a row: runCapture builds
// exactly one store.AddItemInput and makes exactly one store.AddItem call,
// as runAdd does.
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

	// One bare line of id, nothing else, so `id=$(keep capture ...)` works
	// in an agent's shell pipeline, as it does for `add`.
	fmt.Fprintln(stdout, item.ID)
	return 0
}
