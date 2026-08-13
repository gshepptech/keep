# rounce — snippet for Codex

Codex cannot be given the SessionStart hook this plugin uses for Claude Code.
As of `codex-cli 0.146` it requires hook trust and offers no way to grant it, so
its hooks do not fire. Interlock hit the same wall and named it the honest
ceiling for an agent that cannot be intercepted.

So Codex gets the same ten tools, and an instruction instead of enforcement.
That difference is real: Claude Code cannot *not* see the queue, whereas Codex
is being asked to look. Do not assume parity.

## 1. Register the tools

`~/.codex/config.toml`:

```toml
[mcp_servers.rounce]
command = "rounce"
args = ["mcp"]
env = { ROUNCE_ACTOR = "codex" }
```

> Written from Codex CLI's documented config format. It has **not** been
> verified against a live `codex --help` — the binary was not installed on the
> machine this was built on. Confirm it before relying on it.

`ROUNCE_ACTOR` is stamped on every event so the log records who did what.
`ROUNCE_SESSION` is optional and read once at server start, never per call.

## 2. Paste this into the repository's `AGENTS.md`

```markdown
## Work tracking

This repository uses rounce, a local work store. The tools are available to you.

Before starting work, call `ready`. It returns the ranked list of what is
workable and already excludes anything blocked — do not re-sort or re-filter it.

When you notice something worth doing that is not what you were asked to do,
file it with `add` (or `capture` if it is still rough) rather than mentioning it
and moving on. Score with value/confidence/effort, each 0-10, and put a
`file:line` citation in `--why` where you can.

When you pick something up call `start`; when it lands call `close` with a
reason. `drop` is for abandoned work and is deliberately distinct from `close`,
so do not use one for the other.

Items in `backlog` are `raw` and deliberately not workable yet. Promote one only
when you have refined it into something with a title that stands alone.
```

## 3. Sharing a repository with Claude Code

Both read the same `.rounce/db.sqlite`. Neither needs to know the other exists,
and the events they write are byte-identical for the same operation — Phase 4
proves that by comparing actual database rows rather than output.

If you are running both agents in one checkout at the same time, rounce will not
stop them editing the same file. That is Interlock's job, not this one.
