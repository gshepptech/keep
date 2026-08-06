package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// runAdd implements the `add` subcommand (ITEM-01): create a new item and
// print its id. Mirrors runInit's shape (cmd/rounce/init.go) exactly --
// flag.NewFlagSet(..., flag.ContinueOnError), fs.SetOutput(stderr), return 2
// on parse failure.
//
// Invocation shape is `rounce add [flags] <title>`, flags BEFORE the title,
// not after: stdlib flag.Parse stops consuming flags at the first
// non-flag argument and treats everything from there on as fs.Args() (a
// deliberate, unavoidable stdlib flag limitation -- verified directly
// against this exact FlagSet before writing this comment, not assumed).
// runAdd exploits that same stopping point on purpose: everything left in
// fs.Args() after flags are consumed is joined with a space into the title,
// so a title needs no quoting even when it has multiple words, as long as
// it comes last.
//
// cmd/rounce must never import a database driver package, must never
// contain a SQL string, and must never receive a raw DB handle or a row
// (STORE-10) -- runAdd builds exactly one store.AddItemInput and makes
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
	// it, per 02-07-PLAN.md task 2's explicit instruction.
	var blocksFlag stringSlice
	fs.Var(&blocksFlag, "blocks", "id that blocks this new item -- the NEW item is blocked until <id> closes; repeatable")

	var derivedFromFlag stringSlice
	fs.Var(&derivedFromFlag, "derived-from", "id this new item was derived from; repeatable")

	scoreFlag := fs.String("score", "", "value/confidence/effort, each 0-10, e.g. 8/7/3 (default: unscored, sorts last)")
	whyFlag := fs.String("why", "", "why this item matters")
	acceptanceFlag := fs.String("acceptance", "", "acceptance criteria")
	pathsFlag := fs.String("paths", "", "paths this item touches")
	bodyFlag := fs.String("body", "", "full body text")
	repoFlag := fs.String("repo", "", "repository this item belongs to")
	assigneeFlag := fs.String("assignee", "", "assignee")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: rounce add [flags] <title>")
		return 2
	}
	title := strings.Join(fs.Args(), " ")

	if *typeFlag != "" && !model.ValidTypes[*typeFlag] {
		fmt.Fprintf(stderr, "rounce: add: --type %q is not one of %s\n", *typeFlag, validTypesList())
		return 2
	}
	if *stageFlag != "" && !model.ValidStages[*stageFlag] {
		fmt.Fprintf(stderr, "rounce: add: --stage %q is not one of %s\n", *stageFlag, validStagesList())
		return 2
	}

	var value, confidence, effort *int
	if *scoreFlag != "" {
		v, c, e, err := parseScore(*scoreFlag)
		if err != nil {
			fmt.Fprintf(stderr, "rounce: add: %v\n", err)
			return 2
		}
		value, confidence, effort = &v, &c, &e
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: add: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "rounce: %v\n", err)
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
		Body:        *bodyFlag,
		Repo:        *repoFlag,
		Paths:       *pathsFlag,
		Assignee:    *assigneeFlag,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if err := emitJSON(stdout, item); err != nil {
			fmt.Fprintf(stderr, "rounce: add: %v\n", err)
			return 1
		}
		return 0
	}

	// One bare line of id, nothing else -- what makes
	// `id=$(rounce add ...)` work in the demo script and in every agent's
	// shell pipeline.
	fmt.Fprintln(stdout, item.ID)
	return 0
}

// validTypesList and validStagesList render model.ValidTypes/ValidStages as
// a stable, ordered, human-readable list for usage/error text -- ranging
// over the map itself would produce a different order on every process,
// which would make a captured test assertion or a demo transcript flaky.
func validTypesList() string {
	return strings.Join([]string{model.TypeRoadmap, model.TypeEpic, model.TypeTask, model.TypeBug, model.TypeNote}, ", ")
}

func validStagesList() string {
	return strings.Join([]string{model.StageRaw, model.StageTicket}, ", ")
}

// parseScore validates and splits "v/c/e" into three ints, each in [0,10].
// strings.Split (not SplitN) is deliberate (02-RESEARCH.md "Code Examples
// -- --score v/c/e parsing and validation", used verbatim): "8//3" must
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
