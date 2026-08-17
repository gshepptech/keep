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

// runList implements the `list` subcommand (ITEM-05): six flags mapping
// straight onto store.ListFilter, composing as AND, over items directly --
// never the ready view -- so raw items and closed/dropped items show up
// alongside open tickets, which ready/blocked deliberately exclude.
//
// --stage, --status, and --type are validated against the model package's
// closed vocabularies at the CLI boundary before the store is opened; an
// unrecognized value exits 2 naming the legal values, matching add's own
// pre-validation discipline (runAdd). --parent and --repo are free-form ids/
// strings with no closed vocabulary to check against.
func runList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	jsonFlag := addJSONFlag(fs)
	stageFlag := fs.String("stage", "", "filter by stage ("+validStagesList()+")")
	statusFlag := fs.String("status", "", "filter by item status ("+validStatusesList()+")")
	typeFlag := fs.String("type", "", "filter by item type ("+validTypesList()+")")
	parentFlag := fs.String("parent", "", "filter by parent item id")
	repoFlag := fs.String("repo", "", "filter by repository name")
	limitFlag := fs.Int("limit", 0, "maximum number of items to return (default: no limit)")

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: keep list [flags]")
		return 2
	}

	if *stageFlag != "" && !model.ValidStages[*stageFlag] {
		fmt.Fprintf(stderr, "keep: list: --stage %q is not one of %s\n", *stageFlag, validStagesList())
		return 2
	}
	if *statusFlag != "" && !model.ValidStatuses[*statusFlag] {
		fmt.Fprintf(stderr, "keep: list: --status %q is not one of %s\n", *statusFlag, validStatusesList())
		return 2
	}
	if *typeFlag != "" && !model.ValidTypes[*typeFlag] {
		fmt.Fprintf(stderr, "keep: list: --type %q is not one of %s\n", *typeFlag, validTypesList())
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	items, err := s.ListItems(context.Background(), store.ListFilter{
		Stage:    *stageFlag,
		Status:   *statusFlag,
		Type:     *typeFlag,
		ParentID: *parentFlag,
		Repo:     *repoFlag,
		Limit:    *limitFlag,
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	if *jsonFlag {
		if items == nil {
			items = []model.Item{}
		}
		if err := emitJSON(stdout, items); err != nil {
			fmt.Fprintf(stderr, "keep: list: %v\n", err)
			return 1
		}
		return 0
	}

	// list shows non-open items too, so it adds the status column
	// ready/blocked don't (02-CONTEXT.md decision 2).
	renderItems(stdout, items, renderOpts{showStatus: true})
	return 0
}

// validStatusesList renders model.StatusesOrdered as a stable, human-
// readable list -- same reasoning as add.go's validTypesList/
// validStagesList: ranging over the ValidStatuses map itself would produce a
// different order on every process.
func validStatusesList() string {
	return strings.Join(model.StatusesOrdered, ", ")
}
