package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/gshepptech/keep/internal/store"
)

// runMigrate brings a store created by an older keep up to this binary's
// schema, or reports what it would do.
//
// Migration is a command a person runs, not something that happens quietly
// when a store is opened. The store is often the only record of why a piece
// of work exists -- a captured roadmap lives in items.body and nowhere else
// -- and rewriting that on someone's behalf because they happened to run
// `keep ready` is not a trade this tool gets to make for them. Every other
// command refuses an out-of-date store and names this one.
//
// --check reports the plan and changes nothing, exiting 0 when the store is
// current and 1 when it is not, so a script can gate on it.
func runMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := addDBFlag(fs)
	jsonOut := addJSONFlag(fs)
	check := fs.Bool("check", false, "report what migration is needed and change nothing")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "keep: migrate takes no arguments (got %q)\n", fs.Arg(0))
		return 2
	}

	path, err := store.Resolve(*db)
	if err != nil {
		return exitFor(stderr, err)
	}

	ctx := context.Background()
	if *check {
		plan, err := store.PlanMigration(ctx, path)
		if err != nil {
			return exitFor(stderr, err)
		}
		return reportMigration(stdout, path, plan, *jsonOut, false)
	}

	plan, err := store.Migrate(ctx, path)
	if err != nil {
		return exitFor(stderr, err)
	}
	return reportMigration(stdout, path, plan, *jsonOut, true)
}

// migrateReport is the --json shape. It names the backup explicitly: a
// migration whose recovery path a script cannot read is one a script cannot
// undo.
type migrateReport struct {
	Path       string   `json:"path"`
	From       int      `json:"from"`
	To         int      `json:"to"`
	Migrations []string `json:"migrations"`
	Backup     string   `json:"backup,omitempty"`
	Applied    bool     `json:"applied"`
}

func reportMigration(stdout io.Writer, path string, plan store.MigrationPlan, jsonOut, applied bool) int {
	names := plan.Names
	if names == nil {
		names = []string{}
	}
	if jsonOut {
		if err := emitJSON(stdout, migrateReport{
			Path:       path,
			From:       plan.From,
			To:         plan.To,
			Migrations: names,
			Backup:     plan.Backup,
			Applied:    applied && len(names) > 0,
		}); err != nil {
			return 1
		}
		if !applied && len(names) > 0 {
			return 1
		}
		return 0
	}

	if len(names) == 0 {
		fmt.Fprintf(stdout, "%s is at schema_version %d; nothing to do\n", path, plan.From)
		return 0
	}

	if !applied {
		fmt.Fprintf(stdout, "%s is at schema_version %d, this keep expects %d\n", path, plan.From, plan.To)
		for _, n := range names {
			fmt.Fprintf(stdout, "  would apply: %s\n", n)
		}
		fmt.Fprintln(stdout, "run `keep migrate` to apply (it copies the store first)")
		return 1
	}

	fmt.Fprintf(stdout, "backed up to %s\n", plan.Backup)
	for _, n := range names {
		fmt.Fprintf(stdout, "  applied: %s\n", n)
	}
	fmt.Fprintf(stdout, "%s is now at schema_version %d\n", path, plan.To)
	return 0
}
