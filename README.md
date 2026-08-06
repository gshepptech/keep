# rounce

A local work store for AI coding agents. Tickets, backlog, roadmaps, and a
dependency graph, in one SQLite file and one static binary.

You tell an agent to save a roadmap. You tell another agent to make tickets off
it. You point any plugin at rounce and it picks up workable items and gets on
with them. Work an agent surfaces that you do not want yet goes to the backlog
instead of the queue, and stays there until you ask for it to be refined.

No account, no server, no SaaS. The store is a file in your repository.

## Status

Pre-implementation. The schema and phases are settled; nothing is built. See
[ROADMAP.md](ROADMAP.md) and `docs/design/`.

## Principles

1. **rounce never calls a model.** It is a store. Refinement, triage, and
   estimation are things an agent does *using* rounce. This is why any plugin
   or any agent works with it, and why the binary stays small.
2. **One core, three surfaces.** The CLI, the MCP server, and the dashboard are
   thin wrappers over `internal/store`. No surface carries behaviour of its own.
3. **A new concept gets a new table, never a new column on `items`.** Enforced
   socially, not mechanically; an escape-hatch JSON column does not prevent
   this and has been observed not to.
4. **Raw is not workable.** Parking an item has to actually remove it from the
   work queue, or "not yet" means nothing.
5. **Local and private by default.** The dashboard binds to loopback, fetches
   no external assets, and ships no JavaScript.
6. **Correct before fast.** Materialize a computed column only after measuring
   that the query is too slow, not in anticipation.

## What rounce is not, as of 2026-08-05

This list exists because every item on it will one day arrive as a reasonable
ten-minute change. Adding one is a decision to be made deliberately and dated
here, not a patch.

- **No multi-machine sync.** No JSONL export, no Dolt, no CRDT, no merge
  resolution. Sharing a store means sharing a repository.
- **No external tracker bridges.** No Jira, Linear, GitHub Issues, GitLab,
  Azure DevOps, or Notion. `items.external_ref` is a string nobody reads.
- **No HTTP API** beyond the loopback dashboard. No federation, no remote.
- **No orchestration primitives in `items`.** No awaits, no timeouts, no
  waiters, no agent state, no roles. Coordination belongs in Interlock, which
  observes agents deterministically through hooks; rounce says what needs
  doing, Interlock says who is doing what and where.
- **No model calls, no embeddings, no semantic search.**
- **No migration framework** beyond a `schema_version` integer, until there is
  data worth not breaking.

## Prior art

The `ready` query design — dependency blocking that propagates down a
parent-child tree, so blocking an epic hides its whole subtree — is taken from
[Beads](https://github.com/steveyegge/beads) (MIT, Copyright (c) 2025 Beads
Contributors), as is the idea of typed dependency edges and recording where a
discovered item came from. No Beads code is used; the design was read and
reimplemented.

The item scoring model (`value` / `confidence` / `effort` → `score`) and the
convention that an item's rationale cites `file:line` are derived from the
Ranger plugin in `the-tavern`, which scores on four factors, not three:
rounce has no `consequence` term, and puts effort on a 0-10 scale rather than
Hollis's 0-5.

## License

All rights reserved.
