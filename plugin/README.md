# keep — Claude Code plugin

Puts the work queue **in front of** your agents instead of hoping they ask for it.

## The problem this solves

Registering keep's MCP server gives an agent fourteen tools. It does not make the
agent *use* them. Say "fix the login bug" and Claude will fix the login bug,
with a `ready` tool sitting there untouched.

The usual answer is a line in `CLAUDE.md` asking it to check first. That is an
instruction to a model — it mostly works and sometimes does not.

This plugin uses a **`SessionStart` hook** instead. It runs before the agent
reads your first message, so the queue is simply *there*:

```
## keep — the work queue for this repository

**ready 2** · in progress 0 · blocked 2 · backlog 1

Top of the ranked queue:
rnc-684175      25  bug    Retry drops last error
rnc-4b0d0e      13  task   Rename the flag

2 item(s) are blocked and deliberately absent from `ready` —
`blocked` names what is holding each one.
```

That is the same distinction keep's own design turns on: the CLI was kept
complete precisely because **hooks can call a binary and cannot call an MCP
tool** (`docs/design/phase-3.md`).

## What you get

| | |
|---|---|
| **`SessionStart` hook** | the ranked queue, injected before the first message |
| **MCP server** | all fourteen tools, registered automatically |
| **`/keep:ready`** | the ranked queue on demand |
| **`/keep:board`** | counts and the top of the queue; `--web` for the dashboard |
| **`/keep:file`** | file what you noticed, choosing `add` or `capture` correctly |

## Install

Requires `keep` on your PATH:

```bash
cd ~/Projects/keep && go install ./cmd/keep
```

Then add this repository as a plugin marketplace in Claude Code and install
`keep` from it.

## It is silent where it is not used

The hook runs at the start of **every** session in **every** repository. It
produces nothing at all unless there is something worth saying:

| situation | behaviour |
|---|---|
| `keep` not installed | silent |
| no store resolves from here | silent |
| store exists but is empty | silent |
| the store has work | the block above |

It also exits 0 on every path, including failures. A hook that breaks a session
is worse than a hook that does nothing.

## Codex

**Codex gets the same hook.** Three commands:

```bash
codex plugin marketplace add <path to this repo>
codex plugin add keep@keep
keep codex trust
```

No launcher flag, no `AGENTS.md` rule, no bypass. An earlier version of this
file said Codex hooks could not fire at all; that was wrong, and the rest of
this section is what replaced the guess.

`plugin/` is both plugins in one directory. They share
`scripts/session-queue.py` — the queue an agent sees is the same text, from the
same script, on both.

| | Claude Code | Codex |
|---|---|---|
| manifest | `.claude-plugin/plugin.json` | `.codex-plugin/plugin.json` |
| hooks | `hooks/hooks.json`, `${CLAUDE_PLUGIN_ROOT}` | `codex/hooks.json`, `${PLUGIN_ROOT}` |
| marketplace | `.claude-plugin/marketplace.json` | `.agents/plugins/marketplace.json` |
| hook script | `scripts/session-queue.py` | the same file |

### Why `keep codex trust` exists

Codex will not run a hook it has not been told to trust, and installing a plugin
does not grant that — a freshly installed keep plugin reports
`trustStatus: "untrusted"` and stays silent until the trust entry exists. That
is a security control working correctly, and this command grants it deliberately
rather than routing around it. It is the same consent `claude plugin install`
takes; Codex just makes it a separate step.

**It asks Codex for the hash rather than computing one.** `hooks/list` over
`codex app-server` returns each hook's `key` and `currentHash`, and
`config/batchWrite` has Codex write them into its own `config.toml` — so keep
needs no TOML parser and cannot drift from Codex's format. Interlock computed
its own hash and it broke silently when `0.146.0-alpha.9.2` changed how trust is
recorded, while its doctor kept reporting the trust as healthy because it was
verifying its own bookkeeping.

`keep codex trust --dry-run` reports without changing anything.
`keep codex status` asks Codex what it thinks of keep's hooks.

### Proven, not assumed

The hook fires — `hook: SessionStart` in a live session on a clean install,
reproduced — and its text reaches the model. Asked "what is at the top of my
keep queue?" and told to run nothing, a fresh Codex session named the top
item, its score, and the tie below it, cold. Claude Code shows the same block in
the transcript at session start.

### What was measured, on `codex-cli 0.147.0-alpha.6.5`

- A `SessionStart` hook fires only when `config.toml` carries a matching
  `[hooks.state."…"] trusted_hash`. Remove that entry, change nothing else, and
  it stays silent — so trust is required, and it is grantable.
- A plugin-carried hook is recognised (`source: "plugin"`, `enabled: true`) and
  installs as `trustStatus: "untrusted"`. An earlier draft of this file guessed
  it would be auto-trusted; it is not.
- `${PLUGIN_ROOT}` expands to the installed plugin's cache directory, which is
  how one script serves both plugins.
- `codex mcp add keep --env KEEP_ACTOR=codex -- keep mcp` produces exactly
  the config block [`codex/AGENTS.md`](codex/AGENTS.md) documents.
- Codex's hook event set is comparable to Claude Code's — `sessionStart`,
  `sessionEnd`, `preToolUse`, `postToolUse`, `permissionRequest`,
  `userPromptSubmit`, `preCompact`, `postCompact`, `subagentStart`,
  `subagentStop`, `stop`.

One asymmetry that is **not** fixed by the plugin: MCP `instructions`. keep
returns the ranked queue in the initialize handshake, Claude Code renders it
under an "MCP Server Instructions" block, and Codex does not put it in the
model's prompt at all (`codex debug prompt-input` shows it absent). The hook is
what closes that gap, which is why it is worth the trust step.

## What it deliberately does not do

- **It does not start anything.** keep holds the list; it never calls a model
  and never assigns work. You still open the terminal and ask.
- **It does not stop two agents colliding in one checkout.** That is Interlock's
  job. keep says *what needs doing*; Interlock says *who is in which files*.
- **It does not nag.** No `Stop` hook, no unfiled-findings prompt. The queue is
  offered once, at the start, and then it is out of the way.
