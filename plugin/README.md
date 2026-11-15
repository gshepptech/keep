# keep plugins for Claude Code and Codex

These plugins put keep's work queue in front of the agent at the start of
every session.

Registering an MCP server gives an agent tools, but it doesn't make the agent
use them. If you say "fix the login bug", the agent fixes the login bug and
never looks at `ready`. A line in `CLAUDE.md` asking it to check first
usually works, but not always.

These plugins use a `SessionStart` hook instead. The hook runs before the
agent reads your first message, so the queue is already in its context:

```
## keep — the work queue for this repository

**ready 2** · in progress 0 · blocked 2 · backlog 1

Top of the ranked queue:
rnc-684175      25  bug    Retry drops last error
rnc-4b0d0e      13  task   Rename the flag

2 item(s) are blocked and deliberately absent from `ready` —
`blocked` names what is holding each one.
```

The hook calls the `keep` binary, not an MCP tool, because hooks can run
commands but cannot call MCP tools. This is why the CLI covers everything the
MCP server does.

## What you get

| | |
|---|---|
| `SessionStart` hook | The ranked queue, added to context before the first message |
| MCP server | All twenty keep tools, registered automatically (Claude Code) |
| `/keep:ready` | The ranked queue on demand |
| `/keep:board` | Counts and the top of the queue; `--web` opens the dashboard |
| `/keep:file` | Files something you noticed, choosing between `add` and `capture` |

## Install

Both plugins need `keep` on your `PATH`:

```bash
go install github.com/gshepptech/keep/cmd/keep@latest
```

### Claude Code

```bash
claude plugin marketplace add gshepptech/keep
claude plugin install keep@keep
```

### Codex

```bash
codex plugin marketplace add <path to a keep checkout>
codex plugin add keep@keep
keep codex trust
codex mcp add keep -- keep mcp
```

The Codex plugin carries the hook. Register the MCP tools separately with
`codex mcp add`, as shown above. The slash commands are Claude Code only.

Codex does not run a plugin's hook until the hook is trusted, and installing
a plugin does not grant that trust. `keep codex trust` grants it. It asks
Codex for each hook's key and current hash (`hooks/list` over
`codex app-server`), and then has Codex write them to its own `config.toml`
(`config/batchWrite`). keep never parses or computes Codex's trust format
itself. `keep codex trust --dry-run` reports what it would do without making
changes. `keep codex status` shows what Codex reports about keep's hooks.

## Silent where it is not used

The hook runs at the start of every session in every repository. It prints
nothing unless it has something to say:

| Situation | Output |
|---|---|
| `keep` not installed | nothing |
| No store found from this directory | nothing |
| Store exists but is empty | nothing |
| Store has work | the block above |

It exits 0 on every path, including failures. A broken hook should never
break a session.

## Layout

`plugin/` holds both plugins. They share one hook script, so both agents see
the same text.

| | Claude Code | Codex |
|---|---|---|
| Manifest | `.claude-plugin/plugin.json` | `.codex-plugin/plugin.json` |
| Hooks | `hooks/hooks.json` (`${CLAUDE_PLUGIN_ROOT}`) | `codex/hooks.json` (`${PLUGIN_ROOT}`) |
| Marketplace | `../.claude-plugin/marketplace.json` | `../.agents/plugins/marketplace.json` |
| Hook script | `scripts/session-queue.py` | the same file |

The MCP server also sends the ranked queue as its `instructions` when a
client connects. Claude Code shows these to the model. Codex does not, so on
Codex the hook is the only way the queue reaches the model at session start.

If you would rather not install a plugin on Codex, see
[codex/AGENTS.md](codex/AGENTS.md) for manual MCP registration and an
instruction snippet.

## What it does not do

- **It does not start work.** keep holds the list. It never calls a model or
  assigns work.
- **It does not stop two agents editing the same file.** Interlock does that.
  See [../docs/interlock.md](../docs/interlock.md).
- **It does not nag.** There is no `Stop` hook and no prompt about unfiled
  findings. The queue is offered once, at the start of the session.
