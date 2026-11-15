package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// runAdd implements the `add` subcommand: create a new item and
// print its id. Mirrors runInit's shape (cmd/keep/init.go) exactly --
// flag.NewFlagSet(..., flag.ContinueOnError), fs.SetOutput(stderr), return 2
// on parse failure.
//
// Invocation shape is `keep add [flags] <title>`, and flags are also
// accepted AFTER the title -- stdlib
// flag.Parse itself still stops consuming flags at the first non-flag
// argument, unchanged, and that is still what lets a multi-word unquoted
// title work: everything left in fs.Args() after flags are consumed is
// joined with a space into the title, so a title needs no quoting even
// when it has multiple words, as long as it comes last.
// fs.Parse(permuteArgs(fs, args)) runs on a pre-pass that moves every
// well-formed flag ahead of the title first, so both orders reach
// flag.Parse identically -- see permuteArgs's doc comment
// (cmd/keep/flags.go) for the mechanism and the silent-corruption failure
// mode it avoids.
//
// cmd/keep must never import a database driver package, must never
// contain a SQL string, and must never receive a raw DB handle or a row
// -- runAdd builds exactly one store.AddItemInput and makes
// exactly one store.AddItem call.
func runAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	actorFlag := addActorFlag(fs)

	parentFlag := fs.String("parent", "", "id of the parent item -- `add --parent` is the ONLY way to place an item in the tree")
	typeFlag := fs.String("type", "", "item type, default task ("+validTypesList()+")")
	stageFlag := fs.String("stage", "", "item stage, default ticket ("+validStagesList()+")")

	// --blocks names the id that blocks the NEW item: the new item this
	// call creates is blocked UNTIL <id> closes. Verified against the
	// shipped ready view's own SQL (internal/store/items.go's
	// insertEdge doc comment): a deps row (item_id=A, depends_on_id=B,
	// type='blocks') means "A is blocked until B closes". This reading is
	// genuinely easy to invert -- this help text is where a user resolves
	// it.
	var blocksFlag stringSlice
	fs.Var(&blocksFlag, "blocks", "id that blocks this new item -- the NEW item is blocked until <id> closes; repeatable")

	var derivedFromFlag stringSlice
	fs.Var(&derivedFromFlag, "derived-from", "id this new item was derived from; repeatable")

	scoreFlag := fs.String("score", "", "value/confidence/effort, each 0-10, e.g. 8/7/3 (default: unscored, sorts last)")
	whyFlag := fs.String("why", "", "why this item matters")
	acceptanceFlag := fs.String("acceptance", "", "acceptance criteria")
	pathsFlag := fs.String("paths", "", "paths this item touches")
	bodyFlag := fs.String("body", "", "full body text, or - to read it from stdin")
	repoFlag := fs.String("repo", "", "repository this item belongs to")
	assigneeFlag := fs.String("assignee", "", "assignee")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: keep add [flags] <title>")
		return 2
	}
	title := strings.Join(fs.Args(), " ")

	if *typeFlag != "" && !model.ValidTypes[*typeFlag] {
		fmt.Fprintf(stderr, "keep: add: --type %q is not one of %s\n", *typeFlag, validTypesList())
		return 2
	}
	if *stageFlag != "" && !model.ValidStages[*stageFlag] {
		fmt.Fprintf(stderr, "keep: add: --stage %q is not one of %s\n", *stageFlag, validStagesList())
		return 2
	}

	var value, confidence, effort *int
	if *scoreFlag != "" {
		v, c, e, err := parseScore(*scoreFlag)
		if err != nil {
			fmt.Fprintf(stderr, "keep: add: %v\n", err)
			return 2
		}
		value, confidence, effort = &v, &c, &e
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: add: %v\n", err)
		return 2
	}

	// Resolved before the store is opened, so a stdin read failure exits
	// without leaving an open store behind.
	body, err := resolveBody(*bodyFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: add: %v\n", err)
		return 1
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	item, err := s.AddItem(context.Background(), store.AddItemInput{
		Title:       title,
		Actor:       actor,
		SessionID:   resolveSession(),
		ParentID:    *parentFlag,
		Type:        *typeFlag,
		Stage:       *stageFlag,
		Blocks:      blocksFlag,
		DerivedFrom: derivedFromFlag,
		Value:       value,
		Confidence:  confidence,
		Effort:      effort,
		Why:         *whyFlag,
		Acceptance:  *acceptanceFlag,
		Body:        body,
		Repo:        *repoFlag,
		Paths:       *pathsFlag,
		Assignee:    *assigneeFlag,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, item); err != nil {
			fmt.Fprintf(stderr, "keep: add: %v\n", err)
			return 1
		}
		return 0
	}

	// One bare line of id, nothing else -- what makes
	// `id=$(keep add ...)` work in the demo script and in every agent's
	// shell pipeline.
	fmt.Fprintln(stdout, item.ID)
	return 0
}

// validTypesList and validStagesList render model.TypesOrdered/StagesOrdered
// as a stable, human-readable list for usage/error text -- ranging over
// ValidTypes/ValidStages (maps) would produce a different order on every
// process, which would make a captured test assertion or a demo transcript
// flaky. model.TypesOrdered/StagesOrdered are the one place this order is
// spelled out; this function only joins it.
func validTypesList() string {
	return strings.Join(model.TypesOrdered, ", ")
}

func validStagesList() string {
	return strings.Join(model.StagesOrdered, ", ")
}

// parseScore validates and splits "v/c/e" into three ints, each in [0,10].
// strings.Split (not SplitN) is deliberate: "8//3" must
// split into exactly three parts (["8","","3"]) so the empty middle part
// fails strconv.Atoi with a clear, specific error naming "confidence",
// rather than being silently absorbed into a wider third part.
func parseScore(s string) (value, confidence, effort int, err error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("--score must be three integers 0-10 separated by '/', e.g. 8/7/3 (got %q, %d part(s))", s, len(parts))
	}
	vals := make([]int, 3)
	labels := [3]string{"value", "confidence", "effort"}
	for i, p := range parts {
		n, convErr := strconv.Atoi(p)
		if convErr != nil {
			return 0, 0, 0, fmt.Errorf("--score: %s %q is not an integer (got %q)", labels[i], p, s)
		}
		if n < 0 || n > 10 {
			return 0, 0, 0, fmt.Errorf("--score: %s %d is out of range 0-10 (got %q)", labels[i], n, s)
		}
		vals[i] = n
	}
	return vals[0], vals[1], vals[2], nil
}
