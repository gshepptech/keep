# keep with Codex, without the plugin

The Codex plugin is the recommended setup. See [../README.md](../README.md).
This page is for people who would rather register the MCP server by hand and
add an instruction to the repository's `AGENTS.md`.

## 1. Register the tools

```bash
codex mcp add keep -- keep mcp
```

This writes the following to `~/.codex/config.toml`:

```toml
[mcp_servers.keep]
command = "keep"
args = ["mcp"]
```

`KEEP_ACTOR` is optional. If it is unset, keep records events under the
client name from the MCP handshake, so Codex's work is attributed to Codex.
Set it (`--env KEEP_ACTOR=...`) to record a named person or a CI identity
instead. `KEEP_SESSION` is also optional. Both are read once, when the server
starts.

## 2. Add this to the repository's `AGENTS.md`

```markdown
## Work tracking

This repository uses keep, a local work store. The tools are available to you.

Before starting work, call `next` (or `ready` to look first). It returns the
ranked list of what is workable and already excludes anything blocked; do not
re-sort or re-filter it.

When you notice something worth doing that is not what you were asked to do,
`search` for it, then file it with `add` (or `capture` if it is still rough)
rather than mentioning it and moving on. Score with value/confidence/effort,
each 0-10, and put a `file:line` citation in `why` where you can.

When work lands, call `close` with a reason. `drop` is for abandoned work and
is deliberately distinct from `close`, so do not use one for the other. If you
stop without finishing, `unclaim` returns the item to the queue.

Items in `backlog` are raw and deliberately not workable yet. Promote one only
when you have refined it into something with a title that stands alone.
```

## Sharing a repository with Claude Code

Both agents read and write the same `.rounce/db.sqlite`, and neither needs to
know about the other. If they run in the same checkout at the same time, use
[Interlock](https://github.com/gshepptech/interlock) to keep them out of each
other's files. See [../../docs/interlock.md](../../docs/interlock.md).
