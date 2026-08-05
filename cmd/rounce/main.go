// Command rounce is a local work store for AI coding agents.
//
// Nothing is implemented yet. See ROADMAP.md for the phase plan and
// docs/design/phase-0.md for the store this will be built on.
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
  version   print the version

No other command is implemented. See ROADMAP.md.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "rounce: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
