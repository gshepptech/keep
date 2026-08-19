// Command keep is a local work store for AI coding agents.
//
// `keep init` creates a versioned SQLite store in the current repository
// (or at ~/.rounce/db.sqlite with --global, or an arbitrary path with --db).
// See ROADMAP.md for the phase plan and docs/design/phase-0.md for the store
// this is built on.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

// version is set at build time via -ldflags. It is deliberately NOT the only
// source: the documented install is a plain `go install ./cmd/keep`, which
// sets no ldflags, so relying on it alone meant every binary anyone actually
// ran reported "dev" -- including at the v1.0 tag that already exists in this
// repository. A version a bug report cannot cite is not a version.
var version = ""

// resolveVersion answers what this binary is, preferring what the build was
// told over what it can work out for itself.
//
//  1. -ldflags -X main.version=… wins. Release builds and the CLI tests both
//     set it, and an explicit answer beats an inferred one.
//  2. Otherwise the module version Go stamps into every binary built from a
//     versioned module — what `go install …@v1.0` records.
//  3. Otherwise the VCS revision Go stamps into builds made inside a git
//     work tree, shortened, with a +dirty marker when the tree had uncommitted
//     changes. This is the case the documented install actually hits.
//  4. Only if the build carries no information at all does it say "dev",
//     which now means "genuinely unknown" rather than "nobody passed a flag".
func resolveVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}

	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified == "true" {
		return revision + "+dirty"
	}
	return revision
}

const usage = `keep - a local work store for AI coding agents

usage: keep <command> [flags]

commands:
  init      create a store in this repository
              --db <path>   create the store at exactly this path
              --global      create the store at ~/.rounce/db.sqlite
  add       create an item: keep add [flags] <title>
  capture   capture a raw note or document: keep capture [flags] <title>
              --type <t>    item type, default note (roadmap/epic/task/bug/note)
              --body -      read the body from stdin
              stage is always raw -- there is no --stage flag
  backlog   list every raw item still parked, grouped by type, newest first
              --type <t>    show only this type (roadmap/epic/task/bug/note)
              --json        machine-parseable output (flat array, not grouped)
              --all         merge every registered store's pen, newest first,
                            repository shown per row (grouping is dropped)
  dep       create a dependency edge: keep dep <item-id> <type> <depends-on-id>
  ready     list workable items, ranked
              --limit <n>   at most n items
              --json        machine-parseable output
              --all         merge every registered store's ready queue,
                            ranked across all of them (see keep repos);
                            cannot combine with --db
  blocked   list blocked items, each naming the item blocking it
              --json        machine-parseable output
  next      claim the top of the queue in one step: ready and start, with
              no window between them for another agent to take it
              --json        machine-parseable output
              an empty queue exits 0 and says how much is blocked
  why-not   why one item is not in ready: raw, not open, or blocked by
              something named. exit 0 if it is workable, 1 if it is not
              --json        machine-parseable output
  show      show one item's full detail: keep show [flags] <id>
              --json        machine-parseable output
              --body        print only the stored body, verbatim, and
                            nothing else (cannot combine with --json)
  search    find items by text: keep search [flags] <text>
              matches title, why, acceptance, paths and body, ranked by
              where the match landed
              --closed      search closed and dropped items too
              --limit <n>   at most n results
              --json        machine-parseable output
              --all         search every registered store, re-ranked
                            together, repository shown per row
  log       what happened, newest first
              --since <s>   24h, 7d, or an RFC3339 timestamp
              --actor <a>   only what this person or agent did
              --kind <k>    add/start/unclaim/close/drop/reopen/update/dep
              --limit <n>   at most n entries (default 30, 0 = all)
              --json        machine-parseable output
              --all         merge every registered store's activity into one
                            feed, newest first, repository shown per row
  stale     claims nothing has happened to in a while
              --after <d>   how much silence counts (default 2h)
              --json        machine-parseable output
              reports only -- keep never releases a claim on its own
  list      list items with filters, composed as AND
              --stage/--status/--type/--parent/--repo/--limit
              --json        machine-parseable output
  start     claim an item: keep start [flags] <id>
              --force       claim even when another agent holds a file the
                            item declares (refused without it, naming who)
              session comes from $KEEP_SESSION, not a flag; same-session
              repeat is a silent no-op, a different session's claim exits 1
              --json        machine-parseable output
  close     close an item: keep close [flags] <id> --reason <text>
              --reason      required; records why (exit 2 if empty/absent)
  drop      drop an item: keep drop [flags] <id> --reason <text>
              --reason      required; records why (exit 2 if empty/absent)
  unclaim   return a claimed item to the queue: keep unclaim [flags] <id> --reason <text>
              the third exit from in progress, for when the agent that
              claimed it is not coming back -- close says it was done and
              drop says it was not worth doing; this says only that the
              claim ended. Any session may release any claim; the event
              records who did
              --reason      required; records why (exit 2 if empty/absent)
  reopen    bring a closed or dropped item back: keep reopen [flags] <id> --reason <text>
              the way out of the two terminal statuses, for when the close
              was wrong -- the item returns to open, never to in progress,
              and the original close stays in the log
              --reason      required; records why (exit 2 if empty/absent)
  update    change fields on an existing item: keep update [flags] <id>
              --title/--body/--acceptance/--why/--score/--type/--stage
              --repo/--paths/--assignee (at least one required)
              status cannot be changed here -- see start/close/drop
  mcp       serve the MCP protocol over stdio (stdin/stdout are the
              protocol channel -- do not run this interactively)
              --db <path>   the store to serve
              actor defaults to $KEEP_ACTOR then $USER; session comes
              from $KEEP_SESSION, both read once at start
  board     the status glance, or the loopback dashboard
              --limit <n>   queue rows after the counts (default 10, 0 = all)
              --json        one object: the counts and the items
              --web         serve the dashboard on 127.0.0.1 and print the URL
              --port <n>    a fixed port instead of an OS-assigned one
              --all         counts and queue across every registered store
                            (cannot combine with --web -- the dashboard has
                            its own projects page)
              the page writes: file, edit, claim, close, promote,
              assign, and start an agent where Interlock is installed
  repos     the registry of known stores across repositories
              --json        machine-parseable output
              repos add <path>     register a repository directory or store
              repos forget <path>  remove it; the store itself is untouched
              repos forget --gone  remove every registration whose store
                                   file is gone; one whose store merely
                                   will not open is kept
              a store that cannot be opened is reported on its row, not
              fatal -- repos exits 0 even when some rows failed
  codex     the Codex side of the plugin
              codex trust    grant Codex the hook trust its plugin needs,
                             using the hash Codex itself reports
                             --dry-run  report, change nothing
              codex status   what Codex says about keep's hooks
                             --all      every hook, not just keep's
  export    write the whole store to stdout as JSONL, one record per line
              --out <path>  write to this file instead (never overwrites)
              a round-trip, not sync: for backups, moving a store between
              machines, and reading one without this binary
  import    read an export back into an EMPTY store: keep import [file]
              reads stdin when no file is given
              refused if the store already holds items -- merging two
              stores means resolving collisions, which keep does not do
  migrate   bring a store created by an older keep up to this one's schema
              --check       report what is needed and change nothing;
                            exit 0 if current, 1 if a migration is due
              --json        machine-parseable output
              the store is copied to <path>.v<n>.bak before anything is
              changed, and the whole ladder runs in one transaction --
              a failure part-way leaves the store exactly as it was
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
	case "capture":
		os.Exit(runCapture(os.Args[2:], os.Stdout, os.Stderr))
	case "backlog":
		os.Exit(runBacklog(os.Args[2:], os.Stdout, os.Stderr))
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
	case "search":
		os.Exit(runSearch(os.Args[2:], os.Stdout, os.Stderr))
	case "log":
		os.Exit(runLog(os.Args[2:], os.Stdout, os.Stderr))
	case "stale":
		os.Exit(runStale(os.Args[2:], os.Stdout, os.Stderr))
	case "start":
		os.Exit(runStart(os.Args[2:], os.Stdout, os.Stderr))
	case "close":
		os.Exit(runClose(os.Args[2:], os.Stdout, os.Stderr))
	case "drop":
		os.Exit(runDrop(os.Args[2:], os.Stdout, os.Stderr))
	case "unclaim":
		os.Exit(runUnclaim(os.Args[2:], os.Stdout, os.Stderr))
	case "reopen":
		os.Exit(runReopen(os.Args[2:], os.Stdout, os.Stderr))
	case "update":
		os.Exit(runUpdate(os.Args[2:], os.Stdout, os.Stderr))
	case "mcp":
		os.Exit(runMCP(os.Args[2:], os.Stdout, os.Stderr))
	case "board":
		os.Exit(runBoard(os.Args[2:], os.Stdout, os.Stderr))
	case "repos":
		os.Exit(runRepos(os.Args[2:], os.Stdout, os.Stderr))
	case "codex":
		os.Exit(runCodex(os.Args[2:], os.Stdout, os.Stderr))
	case "migrate":
		os.Exit(runMigrate(os.Args[2:], os.Stdout, os.Stderr))
	case "next":
		os.Exit(runNext(os.Args[2:], os.Stdout, os.Stderr))
	case "why-not":
		os.Exit(runWhyNot(os.Args[2:], os.Stdout, os.Stderr))
	case "export":
		os.Exit(runExport(os.Args[2:], os.Stdout, os.Stderr))
	case "import":
		os.Exit(runImport(os.Args[2:], os.Stdout, os.Stderr))
	case "version", "--version":
		fmt.Println(resolveVersion())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "keep: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
