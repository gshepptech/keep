# Using keep with agents

keep is meant to be driven by agents. This page covers registering the MCP
server, the twenty tools it exposes, and what to put in a project's
`CLAUDE.md` or `AGENTS.md` so that agents use it.

For the data model, see [concepts.md](concepts.md). For the CLI, see
[guide.md](guide.md). For the Claude Code and Codex plugins, which add a
session hook and slash commands on top of the MCP server, see
[../plugin/README.md](../plugin/README.md).

## Registering the MCP server

### Claude Code

```bash
claude mcp add keep -- keep mcp
```

Add environment variables with `-e`, for example
`-e KEEP_ACTOR=alice -e KEEP_SESSION=agent-a`. If Claude Code's working
directory doesn't lead to the store, add `--db <path>` after `keep mcp`.

The equivalent `.mcp.json`:

```json
{
  "mcpServers": {
    "keep": {
      "command": "keep",
      "args": ["mcp"],
      "env": {}
    }
  }
}
```

### Codex

```bash
codex mcp add keep -- keep mcp
```

This writes the following to `~/.codex/config.toml`:

```toml
[mcp_servers.keep]
command = "keep"
args = ["mcp"]
```

### Environment

| Variable | Meaning |
|---|---|
| `KEEP_DB` | The store to serve. If unset, keep looks for the nearest `.rounce/db.sqlite` above the working directory, then `~/.rounce/db.sqlite` |
| `KEEP_ACTOR` | The name recorded on every event. If unset, keep uses the client name from the MCP handshake, then `$USER` |
| `KEEP_SESSION` | The session id recorded on claims. Optional |

keep reads all three once, when the server starts, and never per call. One
`keep mcp` process serves one agent session. The server speaks over stdio, so
don't run it interactively.

When a client connects, the server sends the current ranked queue as its MCP
`instructions`. Claude Code shows these to the model. Codex does not, and the
plugin's session hook fills that gap.

## The twenty tools

| Tool | What it does |
|---|---|
| `capture` | Store a raw note or document exactly as given. Raw items stay out of `ready` until refined. |
| `add` | Create a workable ticket. `parent_id` places it in the tree, and `derived_from` records the document it came from. |
| `ready` | The open, unblocked tickets, ranked by score. |
| `next` | Take the top of the queue and claim it in one step. Use this instead of `ready` followed by `start`. |
| `backlog` | Raw items waiting to be refined. |
| `show` | One item in full: its fields, dependencies in both directions, and event history. |
| `list` | Items filtered by stage, status, type, parent or repo, including raw and closed items. |
| `search` | Text search across title, why, acceptance, paths and body. Search before filing, in case the item already exists. |
| `log` | Every add, claim, close, drop and edit, newest first. |
| `stale` | Claims whose sessions have gone quiet. It only reports. |
| `blocked` | The tickets `ready` is holding back, each naming the nearest blocker. |
| `why_not` | Why one item is not in `ready`: it is raw, not open, or blocked by a named item. |
| `ready_all` | The ranked queue across every repository registered on this machine. |
| `dep` | Add a `blocks`, `related` or `derived-from` edge. Tree structure comes from `add`. |
| `start` | Claim an item and mark it in progress. Refused if the item is raw, finished or blocked. |
| `close` | Mark an item done, with a reason. |
| `drop` | Mark an item abandoned, with a reason. It is never counted as done. |
| `unclaim` | Return an item this session claimed to the queue, with a reason. |
| `reopen` | Bring a closed or dropped item back to open, with a reason. The original close stays in the log. |
| `update` | Change an item's title, body, why, acceptance, score, type, stage, repo, paths or assignee. Status changes go through `start`, `close`, `drop`, `unclaim` and `reopen`. |

The canonical list is `internal/mcp.ToolNames`. Each tool is one call to one
store method with no logic in between, and `internal/mcp/structure_test.go`
enforces this by walking the AST. Validation happens in `internal/store`:
required reasons, closed vocabularies such as `type` and `stage`, and the
0-10 range for scores. As a result, the MCP server and the CLI accept and
reject the same inputs.

### Differences from the CLI

- `init`, `migrate`, `export`, `import`, `repos`, `board` and `codex` have no
  tool. They either replace the whole store or are operator tasks, such as
  binding a port or granting hook trust.
- Over MCP, `unclaim` only releases a claim held by the calling session. On
  the CLI, any session can release any claim, because there a person can see
  whether the original session is gone.
- `add` cannot set `stage`. Use `capture` for raw items.
- An empty result from `ready`, `backlog` or `list` comes back as JSON `null`
  over MCP, while the CLI's `--json` output gives `[]`.
- Malformed arguments, and values outside a closed vocabulary, return a
  JSON-RPC protocol error that names the value and the legal ones. Failures
  an agent can retry, such as an empty reason or an unknown id, return a
  tool result with `isError: true`.

## What to tell an agent

Plain requests work:

- "Scout this repo and file what's worth fixing." The agent uses `capture`
  for uncertain findings and `add` for clear tickets.
- "What should I work on?" The agent calls `ready`.
- "Take the top one, and close it with a reason when you're done." The agent
  calls `next`, then `close`, or `drop` if the work turns out not to be worth
  doing.

Agents don't do one thing by default, so it is worth asking for: file
findings instead of fixing them inline. An agent that notices something
outside its task should `capture` it rather than quietly growing its scope.

### Snippet for `CLAUDE.md` or `AGENTS.md`

```markdown
## Work tracking (keep)

This repository uses keep for tickets and backlog. The tools are available to you.

- Before starting work, call `next` (or `ready` to look first). It returns
  only workable items, already ranked; do not re-sort or re-filter it.
- When work lands, call `close` with a real reason. Use `drop` for work that
  is abandoned; never use one for the other.
- If you notice something worth doing outside the current task, `search` for
  it, then file it with `add` (or `capture` if it is still rough). Score with
  value/confidence/effort, each 0-10, and cite `file:line` in `why`.
- To turn a parked document into tickets, use `add` with `parent_id` and
  `derived_from` pointing at it.
```
