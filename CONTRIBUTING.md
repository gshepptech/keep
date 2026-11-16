# Contributing

Issues and pull requests are welcome.

## Setup

You need Go 1.25 or newer. Python 3 is used only by the plugin hook script.

```bash
git clone https://github.com/gshepptech/keep.git
cd keep
go build ./cmd/keep
```

## Checks

Run these before opening a pull request:

```bash
gofmt -l .        # should print nothing
go vet ./...
go test ./...
```

Most CLI tests build the real binary and run it against a temporary store.
The Interlock integration test (`cmd/keep/join_test.go`) builds Interlock from
`$INTERLOCK_SRC` or `~/Projects/interlock`, and skips when neither exists.

## Ground rules

- All SQL lives in `internal/store`. The CLI, the MCP server and the
  dashboard call store methods and carry no behaviour of their own. Tests
  enforce both rules.
- A new concept gets a new table, not a new column on `items`.
- A new MCP tool needs a matching CLI command, and goes in
  `internal/mcp.ToolNames`.
- A schema change needs a migration step in `internal/store/migrations.go`.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/).

By contributing, you agree that your contributions are licensed under the MIT
License.
