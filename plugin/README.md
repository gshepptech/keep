# rounce — Claude Code plugin

Puts the work queue **in front of** your agents instead of hoping they ask for it.

## The problem this solves

Registering rounce's MCP server gives an agent ten tools. It does not make the
agent *use* them. Say "fix the login bug" and Claude will fix the login bug,
with a `ready` tool sitting there untouched.

The usual answer is a line in `CLAUDE.md` asking it to check first. That is an
instruction to a model — it mostly works and sometimes does not.

This plugin uses a **`SessionStart` hook** instead. It runs before the agent
reads your first message, so the queue is simply *there*:

```
## rounce — the work queue for this repository

**ready 2** · in progress 0 · blocked 2 · backlog 1

Top of the ranked queue:
rnc-684175      25  bug    Retry drops last error
rnc-4b0d0e      13  task   Rename the flag

2 item(s) are blocked and deliberately absent from `ready` —
`blocked` names what is holding each one.
```

That is the same distinction rounce's own design turns on: the CLI was kept
complete precisely because **hooks can call a binary and cannot call an MCP
tool** (`docs/design/phase-3.md`).

## What you get

| | |
|---|---|
| **`SessionStart` hook** | the ranked queue, injected before the first message |
| **MCP server** | all ten tools, registered automatically |
| **`/rounce:ready`** | the ranked queue on demand |
| **`/rounce:board`** | counts and the top of the queue; `--web` for the dashboard |
| **`/rounce:file`** | file what you noticed, choosing `add` or `capture` correctly |

## Install

Requires `rounce` on your PATH:

```bash
cd ~/Projects/rounce && go install ./cmd/rounce
```

Then add this repository as a plugin marketplace in Claude Code and install
`rounce` from it.

## It is silent where it is not used

The hook runs at the start of **every** session in **every** repository. It
produces nothing at all unless there is something worth saying:

| situation | behaviour |
|---|---|
| `rounce` not installed | silent |
| no store resolves from here | silent |
| store exists but is empty | silent |
| the store has work | the block above |

It also exits 0 on every path, including failures. A hook that breaks a session
is worse than a hook that does nothing.

## Codex

Codex gets the same ten tools, and today an `AGENTS.md` instruction rather than
a hook. That is a gap in this plugin, **not a limit of Codex** — an earlier
version of this file claimed otherwise and was wrong.

**What was actually measured**, on `codex-cli 0.147.0-alpha.6.5`:

- A `.codex/hooks.json` `SessionStart` hook **fires**, provided
  `~/.codex/config.toml` carries a matching `[hooks.state."…"] trusted_hash`.
  Codex prints `hook: SessionStart` and the command runs.
- With the identical `hooks.json` and the trust entry removed, it does **not**
  fire. So trust is required, and it is grantable.
- `codex mcp add rounce --env ROUNCE_ACTOR=codex -- rounce mcp` produces exactly
  the config block [`codex/AGENTS.md`](codex/AGENTS.md) documents.

Codex's own protocol schema (`codex app-server generate-json-schema`) also shows
a hook event set at parity with Claude Code's — `sessionStart`, `sessionEnd`,
`preToolUse`, `postToolUse`, `permissionRequest`, `userPromptSubmit`,
`preCompact`, `postCompact`, `subagentStart`, `subagentStop`, `stop` — and a
plugin system that can carry hooks (`PluginDetail.hooks`, `HookSource: plugin`,
`HookTrustStatus: managed`).

**The intended fix is a Codex plugin**, mirroring this one, so trust follows from
having installed it rather than from writing a hash into someone's config. That
a plugin-sourced hook is auto-trusted is read from the schema and **has not been
run** — see `rnc-069e8a`.

**Parity is not claimed yet.** Claude Code cannot *not* see the queue. Codex is
still being asked to look. The difference is now a to-do, not a ceiling.

## What it deliberately does not do

- **It does not start anything.** rounce holds the list; it never calls a model
  and never assigns work. You still open the terminal and ask.
- **It does not stop two agents colliding in one checkout.** That is Interlock's
  job. rounce says *what needs doing*; Interlock says *who is in which files*.
- **It does not nag.** No `Stop` hook, no unfiled-findings prompt. The queue is
  offered once, at the start, and then it is out of the way.
