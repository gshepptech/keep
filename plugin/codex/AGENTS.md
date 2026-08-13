# rounce — snippet for Codex

**Codex gets the hook now. This snippet is the fallback, not the plan.**

An earlier version of this file said Codex hooks cannot fire. That was wrong.
`plugin/` ships a Codex plugin carrying a `SessionStart` hook, and three
commands install it:

```bash
codex plugin marketplace add <path to the rounce repo>
codex plugin add rounce@rounce
rounce codex trust
```

See [`../README.md`](../README.md) for what that does and why the trust step
exists. What follows is still worth having: section 1 registers the tools (the
plugin does not do that for you), and section 2 is the instruction to paste when
you would rather not install a plugin at all.

## 1. Register the tools

`~/.codex/config.toml`:

```toml
[mcp_servers.rounce]
command = "rounce"
args = ["mcp"]
env = { ROUNCE_ACTOR = "codex" }
```

> **Verified 2026-08-13** against `codex-cli 0.147.0-alpha.6.5`. Running
> `codex mcp add rounce --env ROUNCE_ACTOR=codex -- rounce mcp` in an isolated
> `CODEX_HOME` produces exactly this, with `env` as a `[mcp_servers.rounce.env]`
> sub-table — equivalent TOML to the inline form above.
>
> Still unverified: that the ten tools load and answer inside a real turn. The
> test session failed authentication before any turn ran, so the server was
> never exercised by a model (`rnc-967360`).

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
