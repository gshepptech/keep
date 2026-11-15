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

// runDep implements the `dep` subcommand: `keep dep <a>
// <type> <b>` creates one edge. Three positional arguments after flag
// parsing (fs.Arg(0..2)); anything else is a usage error.
//
// The dep type is validated against model.ValidDepTypes before the store is
// even opened -- an unrecognized type exits 2 naming the four legal values.
// `parent-child` IS in that closed vocabulary, so it
// passes this check and reaches store.CreateDep, which is the single place
// that refuses it (store.ErrParentChildViaDep, whose own message names
// `keep add --parent`) -- exitFor maps that sentinel to exit 2, not 1,
// because it is a usage error, not an operational one.
func runDep(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dep", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbFlag := addDBFlag(fs)
	actorFlag := addActorFlag(fs)

	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}

	if fs.NArg() != 3 {
		fmt.Fprintf(stderr, "usage: keep dep <item-id> <type> <depends-on-id>  (type is one of %s)\n", validDepTypesList())
		return 2
	}
	itemID, depType, dependsOnID := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	if !model.ValidDepTypes[depType] {
		fmt.Fprintf(stderr, "keep: dep: %q is not one of %s\n", depType, validDepTypesList())
		return 2
	}

	actor, err := resolveActor(*actorFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: dep: %v\n", err)
		return 2
	}

	s, err := openStore(*dbFlag)
	if err != nil {
		fmt.Fprintf(stderr, "keep: %v\n", err)
		return 1
	}
	defer s.Close()

	err = s.CreateDep(context.Background(), store.DepInput{
		ItemID:      itemID,
		DependsOnID: dependsOnID,
		Type:        depType,
		Actor:       actor,
		SessionID:   resolveSession(),
	})
	if err != nil {
		return exitFor(stderr, err)
	}

	fmt.Fprintf(stdout, "%s %s %s\n", itemID, depType, dependsOnID)
	return 0
}

// validDepTypesList renders model.ValidDepTypes as a stable, ordered,
// human-readable list -- ranging over the map itself would produce a
// different order on every process (same reasoning as add.go's
// validTypesList/validStagesList).
func validDepTypesList() string {
	return strings.Join([]string{model.DepBlocks, model.DepParentChild, model.DepRelated, model.DepDerivedFrom}, ", ")
}
