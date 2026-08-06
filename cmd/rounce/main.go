// Command rounce is a local work store for AI coding agents.
//
// `rounce init` creates a versioned SQLite store in the current repository
// (or at ~/.rounce/db.sqlite with --global, or an arbitrary path with --db).
// See ROADMAP.md for the phase plan and docs/design/phase-0.md for the store
// this is built on.
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags.
var version = "dev"

const usage = `rounce - a local work store for AI coding agents

usage: rounce <command> [flags]

commands:
  init      create a store in this repository
              --db <path>   create the store at exactly this path
              --global      create the store at ~/.rounce/db.sqlite
  add       create an item: rounce add [flags] <title>  (flags before the title)
  dep       create a dependency edge: rounce dep <item-id> <type> <depends-on-id>
  ready     list workable items, ranked
              --limit <n>   at most n items
              --json        machine-parseable output
  blocked   list blocked items, each naming the item blocking it
              --json        machine-parseable output
  show      show one item's full detail: rounce show [flags] <id>
              --json        machine-parseable output
  list      list items with filters, composed as AND
              --stage/--status/--type/--parent/--repo/--limit
              --json        machine-parseable output
  version   print the version

See ROADMAP.md.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "init":
		os.Exit(runInit(os.Args[2:], os.Stdout, os.Stderr))
	case "add":
		os.Exit(runAdd(os.Args[2:], os.Stdout, os.Stderr))
	case "dep":
		os.Exit(runDep(os.Args[2:], os.Stdout, os.Stderr))
	case "ready":
		os.Exit(runReady(os.Args[2:], os.Stdout, os.Stderr))
	case "blocked":
		os.Exit(runBlocked(os.Args[2:], os.Stdout, os.Stderr))
	case "show":
		os.Exit(runShow(os.Args[2:], os.Stdout, os.Stderr))
	case "list":
		os.Exit(runList(os.Args[2:], os.Stdout, os.Stderr))
	case "version", "--version":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "rounce: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
