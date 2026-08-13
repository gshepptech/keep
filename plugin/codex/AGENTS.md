# rounce — snippet for Codex

Codex gets the same ten tools, and for now an instruction rather than a hook.

**An earlier version of this file said Codex hooks cannot fire. That was wrong.**
Measured on `codex-cli 0.147.0-alpha.6.5`: a `.codex/hooks.json` `SessionStart`
hook runs whenever `~/.codex/config.toml` carries the matching
`[hooks.state."<abs path>:session_start:0:0"] trusted_hash`, and does not run
when that entry is removed. Trust is required and grantable — it is not a wall.

So the gap below is rounce's, not Codex's. The fix is a Codex plugin carrying
the hook. Installing it does not trust the hook by itself — `hooks/list` over
`codex app-server` reports the hook's `key` and `currentHash`, and writing that
pair into `config.toml` grants the trust. Reading the hash beats computing it:
Interlock computed its own and broke silently when `0.146.0-alpha.9.2` changed
the format. Tracked as `rnc-069e8a`.

Until then: Claude Code cannot *not* see the queue; Codex is being asked to
look. Do not assume parity **yet**.

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
