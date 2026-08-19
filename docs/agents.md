# Using keep with agents

keep is built to be driven by agents, not typed by you. This page covers
just that: registering the MCP server, what the twenty tools are, and what to
put in a project's `CLAUDE.md` so agents actually use it instead of ignoring
it.

For the mental model (why blocking propagates, why raw/ticket exist, why
there's no `refine` command) see `docs/concepts.md`. For hands-on CLI usage
see `docs/guide.md`.

## Registering the MCP server

These instructions are reproduced from `docs/design/phase-3.md`'s
Registration section — verified against `claude mcp add --help` on the
machine the phase was built on.

### Claude Code

```
claude mcp add keep -e KEEP_SESSION=<session-id> -e KEEP_ACTOR=<actor> -- keep mcp
```

Append `--db <path>` after `keep mcp` if the store isn't reachable through
the ordinary four-rule resolver (`--db` flag, `KEEP_DB` env, nearest
`.rounce/db.sqlite` walking up from the client's working directory, then
`~/.rounce/db.sqlite`) from wherever Claude Code's working directory is.

Equivalent JSON, for any client that reads one directly (Claude Code's own
`.mcp.json`, or an equivalent):

```json
{
  "mcpServers": {
    "keep": {
      "command": "keep",
      "args": ["mcp"],
      "env": {
        "KEEP_SESSION": "<session-id>",
        "KEEP_ACTOR": "<actor>",
        "KEEP_DB": "<path, optional>"
      }
    }
  }
}
```

### Codex

Codex CLI reads `~/.codex/config.toml`, a table per server:

```toml
[mcp_servers.rounce]
command = "keep"
args = ["mcp"]
env = { KEEP_SESSION = "<session-id>", KEEP_ACTOR = "<actor>" }
```

This one is written from Codex's documented config format, not verified
against a live `codex --help` — the design doc records that the `codex`
binary wasn't installed on the machine Phase 3 was built on. Treat it as
probably-right rather than confirmed.

### Notes on both

- `KEEP_SESSION` and `KEEP_ACTOR` are read **once, at server start** —
  never per call. One `keep mcp` process is scoped to one agent session; if
  you want a different session id, restart the server, not one tool call.
- `KEEP_DB` is optional and, when set, follows the same four-rule resolver
  every CLI command uses.
- There's no `--json` flag and no `--actor` flag on `keep mcp` itself — the
  protocol *is* the output format, and actor comes from the environment
  (falling back to `$KEEP_ACTOR`, then `$USER`) with a per-call override
  available on every mutating tool.
- `keep mcp` serves over stdio only. Don't run it interactively — stdin and
  stdout are the protocol channel.

## The twenty tools

The MCP server exposes twenty of the CLI's verbs, in this order (also
the order `internal/mcp.ToolNames` lists them; note that `tools/list` itself
returns them alphabetically, which is the SDK's own doing and not a choice
keep makes):

| Tool | What it does |
|---|---|
| `capture` | Store a raw note or document verbatim. Raw items never appear in `ready` until they're refined into tickets. |
| `add` | Create a workable ticket. Use `parent_id` to place it in the tree and `derived_from` to record the document it was refined out of. |
| `ready` | List the open, unblocked tickets ranked by score — what to work on right now. |
| `backlog` | List raw, unrefined items (captured documents and notes) waiting to be turned into tickets. |
| `show` | Show one item's full detail: fields, dependencies in both directions, and its event history. |
| `list` | List items with optional filters — stage, status, type, parent, repo — including raw and closed/dropped items `ready`/`backlog` omit. |
| `search` | Find items by text, matching title, why, acceptance, paths and body, ranked by where the match landed. Use this before filing anything: the thing you noticed may already be filed. |
| `log` | What has happened in this store, newest first: every add, claim, close, drop and edit, each with its item's title. Narrow with since, actor or kind. |
| `stale` | Items claimed by someone who then went quiet — a session that died mid-work leaves one behind. Reports only; keep never releases a claim on its own. |
| `dep` | Create a dependency edge between two items: blocks, related, or derived-from. Parent-child structure is created by `add`'s `parent_id`, not this tool. |
| `start` | Claim an item and mark it in progress. Refused if the item is raw, already terminal, or blocked. |
| `close` | Mark an item finished, recording why. Distinct from `drop`: close means done, drop means abandoned and must never be counted as done. |
| `drop` | Mark an item abandoned, recording why. Distinct from `close`: drop means abandoned and must never be counted as done. |
| `update` | Change fields on an existing item — title, body, why, acceptance, score, type, stage, repo, paths, assignee. Status is not among them: use `start`, `close` or `drop`, so the log always records why a status moved. |
| `ready_all` | The ranked queue across every repository this machine knows about, merged and ranked by score, each row naming which repository it came from. Use it when the work spans more than the repository you are standing in. |
| `blocked` | The open tickets `ready` is holding back, each naming the nearest item in its way. Read this when `ready` comes back empty — an empty queue with a full blocked list means go unblock something, not stop. |
| `why_not` | Why one specific item is not in `ready`: raw, not open, or blocked by something named. Ask this instead of guessing from `show`. |
| `next` | Take the top of the queue and claim it, in one step. Prefer this over `ready` then `start`: those are two calls with a gap in between, and another agent can take the item during it. |
| `unclaim` | Put an item **you** claimed back in the queue, recording why. Refused for an item another session holds. |
| `reopen` | Bring a closed or dropped item back to open, recording why the close was wrong. The original close stays in the log. |

Descriptions above are the literal tool descriptions registered in
`internal/mcp/server.go` — what a model actually sees.

**What has no MCP tool, and why.** `init`, `migrate`, `repos`, `board`,
`export`, `import` and `codex` are absent, and that is deliberate rather than
an oversight (`docs/design/phase-3.md`):

- `init` isn't exposed because an MCP client is, by definition, already
  pointed at a store — there's nothing useful "create the store I'm already
  talking to" would do over the protocol.
- `migrate`, `export` and `import` rewrite or replace the whole store. A
  migration copies the file first and then changes its schema; an import only
  works into an empty store. Neither is something to do while a model is
  reasoning about the work inside it.
- `repos`, `board` and `codex` are operator-facing — editing the machine's
  registry, binding a port, granting hook trust.

**Three that used to be absent and no longer are.** Worth recording, because
each was withheld for a stated reason and each reason turned out to be wrong
in a specific way:

- `update` left the agent-facing surface write-once, so an agent that filed an
  item with a wrong title or score could only go back through `add`, producing
  a duplicate with a new id while the original stood.
- `blocked` was left off the original locked list and never argued for. An
  agent that gets an empty `ready` needs to tell "there is no work" from
  "there is work and something is in its way", and those call for opposite
  responses. `why_not` answers the same question about one item.
- `reopen` was withheld because "a mis-close is a thing a person notices and
  corrects". The person is not the one who makes this mistake. An agent that
  closes the wrong id notices within the same turn, and had no way to undo it
  — it had to file the work again, losing the id, the history and every
  dependency edge pointing at the original, which is the exact loss `reopen`
  was added to prevent. It is also the safest verb to expose: it destroys
  nothing and the original close stays in the events table.

**`unclaim` is narrower here than on the CLI, and that is the whole reason it
exists as a tool at all.** Unrestricted, it can take work away from an agent
that is still running, and that call belongs with whoever can look at the
terminal and see whether the session is actually dead. The tool sets
`RequireOwnClaim`, unreachable over the wire, so an agent may release **its
own** claim and nothing else. An agent that started something and then found
it could not do the work previously had only the two exits `unclaim` was
written to avoid forcing on anyone: `close`, which says it was done, and
`drop`, which says it was not worth doing. `stale` still only reports.

**On `reason`, and on `type`/`stage`.** Both were enforced in `cmd/keep` and
nowhere else, which meant the rule held only for whoever went through the
command line. `close` over MCP accepted an omitted `reason` and recorded a
close with no why at all; `add` with an invalid `type` came back as SQLite's
own text — `constraint failed: CHECK constraint failed: type IN ('roadmap',
'epic', 'task', 'bug', 'note') (275)` — which is indistinguishable from a
broken store. Both now live in `internal/store` and both surfaces get them:
an empty reason is a tool error you can retry from, and a value outside a
closed vocabulary is a protocol error that names the value and the legal
ones.

**On `value`, `confidence` and `effort`.** Each is 0-10, and the schema now
says so with `minimum` and `maximum`. Take that as steering, not as a gate:
the SDK does not validate arguments against `InputSchema` at all — a
published enum was confirmed ignored on a real call — so the store is what
refuses an out-of-range number, and it returns a tool error naming the input
and its value. Before it did, an agent could file `effort: 100` and score the
item **-89**, below an unscored item's 0, burying it beneath work nobody had
even estimated.

**Why exactly these, not fewer, and not more.** The tool count tracks the CLI,
minus the operator-facing verbs above — it grows only when the CLI does, never
as a convenience layer on top of it, and **every tool is one call to one store
method with no logic in between**, which `internal/mcp/structure_test.go`
proves by walking the AST rather than by review.

Ten became twenty in three moves, and only the first was the rule working as
originally imagined. `search`, `log` and `stale` were built as CLI verbs first
and mirrored here afterwards. `update`, `blocked` and `reopen` were corrections
to an artifact: the CLI verbs already existed and the original locked list had
simply not included them. `next` and `why_not` are the only two that were built
for the agent-facing surface first — and they went into `internal/store`, so
the CLI has them too, which is what stops "built for agents" turning into "an
MCP-only shortcut".

`ready_all` is the one tool whose answer does not come from the store this
server is pointed at. It reads the machine's registry, opens every other
store read-only and reports which of them it could not read — none of which
belongs in `internal/mcp`, whose entire claim is that it holds no logic. So
`cmd/keep` injects it as one function, from the same code path `keep ready
--all` renders from, and the structural test was widened by NAME rather than
by loosening its count: a handler may make one data call, to the store or to
one named injected reader, and a second entry on that list is a decision
someone has to write down.

The count itself now lives in one place, `internal/mcp.ToolNames`. It used to
be typed out in four — a schema test, an AST constant, a wire-level assertion
and a sentence in a doc comment — and it had already drifted in two of them,
which is how this document came to say "thirteen verbs" while the binary
served fourteen tools.

> Block rebuilt their Linear MCP server three times and ended at 2 tools
> instead of 30+ — the standing justification for **no MCP-only convenience
> tools**: the count grows only when the CLI does, never otherwise... keep's
> are flat, disjoint verbs with no pair a model would hesitate between.
> — `docs/design/phase-3.md`

A few real surface differences from the CLI, all recorded as deliberate
tradeoffs rather than bugs (`docs/design/phase-3.md`):

- `add`'s MCP tool **cannot** set `stage` — the CLI's `add --stage raw` has no
  MCP equivalent, because `capture` is the verb for a raw item.
- An empty `ready`, `backlog`, or `list` result marshals to JSON `null` over
  MCP, not `[]` — unlike `keep ready --json`, which normalizes an empty
  result to `[]`. If you're parsing tool output programmatically, check for
  `null` as well as an empty array.
- Passing keep's `--json`-shaped output through as the tool result is less
  token-efficient than a custom Markdown or XML format would be. That's the
  accepted price of "every surface is a thin wrapper with no formatting logic
  of its own" — MCP doesn't get a formatter the CLI doesn't have.
- Malformed arguments (a bad type, a missing required field) come back as a
  genuine JSON-RPC protocol error, not a silently-successful `isError: true`
  result — this was specifically verified live during Phase 3, because the
  SDK's more ergonomic generic tool-registration API routes exactly that case
  into a fake-success response instead.

## What to tell an agent

Talking to an agent about keep works the same way as any other tool: say
what you want, let it pick the calls. In practice, three sentences cover
almost everything:

> *"Scout this repo and file what's worth fixing in keep."* → the agent
> calls `capture` for things it isn't sure about yet, `add` for things it's
> ready to commit to as tickets.

> *"What should I work on?"* → `ready`.

> *"Take the top one, and when you're done, close it with why."* → `start`,
> then `close --reason ...` (or `drop --reason ...` if it turns out not to be
> worth doing).

The one habit worth calling out explicitly, because agents don't do it by
default: **check `ready` before starting work, and file findings instead of
just doing them inline.** An agent that notices something worth fixing while
working on something else should `capture` it rather than silently expanding
scope — that's what keeps the backlog from being either empty or a landfill.

### A `CLAUDE.md` snippet

Drop something like this into a project's `CLAUDE.md` if you want agents to
actually use keep as their task list rather than working off ad hoc
todo-list tool calls:

```markdown
## Task tracking (keep)

This repo uses keep for tickets and backlog. Before starting work:

- Run/call `ready` to see what's workable right now, ranked by score. Don't
  pick up anything not in `ready` — if it's not there, it's either done,
  blocked, or not refined into a ticket yet.
- `start` the item you're taking, `close --reason "..."` when it's done, or
  `drop --reason "..."` if it turns out not to be worth doing. Always give a
  real reason — it's the only record of why later.
- If you find something worth fixing that isn't what you were asked to do,
  `capture` it as a raw note rather than doing it inline or dropping it in
  chat. Don't expand scope silently.
- If you're turning a document or a parked note into real tickets, use
  `add --parent <id> --derived-from <id>` so the ticket remembers where it
  came from. There's no `refine` command — this is what refinement is.
```

Adjust the specifics (which repo, what score threshold counts as worth
picking up) to the project. The core instruction — consult `ready`, file
findings instead of dropping them, always give `close`/`drop` a real reason —
is what makes keep worth having instead of an agent just keeping its own
mental list for the session.
