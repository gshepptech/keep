# Concepts

This page covers the data model and the reasons behind it. For a hands-on
walkthrough, see [guide.md](guide.md).

## Stage: raw versus ticket

Every item has a `stage`, either `raw` or `ticket`.

- `raw` means the item is captured but not workable yet. It might be a
  document stored as written, an idea parked mid-session, or a finding nobody
  has agreed to work on.
- `ticket` means the item has been refined. Its title stands on its own, and
  it usually has acceptance criteria and a score.

`ready` only ever shows tickets. `stage = 'ticket'` is a condition inside the
`ready` SQL view, not a filter that callers apply. No code path queries the
queue without going through the view, so no command can forget the check.
Parking an item really does take it out of the work queue.

keep has no `refine` command. Refining means an agent reads a raw item with
`show` and runs `add --parent <id> --derived-from <id>` to create tickets from
it. When a parked note turns out to be the ticket itself,
`update --stage ticket` promotes it in place.

## Dependency types

`deps` is one table of typed edges (`item_id`, `depends_on_id`, `type`):

- **`blocks`**: `(A, B, blocks)` means A waits until B closes. These edges
  are what filter `ready`.
- **`parent-child`**: tree structure, which blocking walks down. Only
  `add --parent` creates these edges. `keep dep` refuses to.
- **`related`**: a cross-reference with no behaviour attached.
- **`derived-from`**: provenance, meaning the raw document or note a ticket
  was refined from.

`parent-child` and `derived-from` are separate edges because they answer
separate questions. The parent says where an item sits in the tree, so that
blocking an epic reaches it. The derived-from edge says which document the
item came from, so its origin is still known after the session that created
it has ended. A ticket can sit under one epic and be derived from a different
note.

## Hierarchical ids

A root item gets an id of `rnc-` plus six hex characters, hashed from its title
and creation time. Its children get `.1`, `.2` and so on, and a grandchild
gets `.1.1`. An agent reading an id in a log can tell where the item sits in
the tree without looking it up.

Child ordinals are allocated as `MAX(ordinal) + 1` in the same transaction as
the insert, and a `UNIQUE(parent_id, ordinal)` index backs this up. Two
concurrent `add --parent` calls under the same epic therefore cannot get the
same ordinal. This is tested across separate processes, not just goroutines,
because a race test inside one process can pass while a race between
processes still exists.

The store directory is `.rounce/` and ids use the `rnc-` prefix. Both are
fixed on-disk identifiers.

## The `ready` view

```sql
CREATE VIEW ready AS
WITH RECURSIVE
  blocked_directly AS (
    SELECT DISTINCT d.item_id FROM deps d
    WHERE d.type = 'blocks'
      AND EXISTS (SELECT 1 FROM items b
                  WHERE b.id = d.depends_on_id
                    AND b.status NOT IN ('closed', 'dropped'))
  ),
  blocked_tree AS (
    SELECT item_id, 0 AS depth FROM blocked_directly
    UNION ALL
    SELECT d.item_id, bt.depth + 1
    FROM blocked_tree bt
    JOIN deps d ON d.depends_on_id = bt.item_id
    WHERE d.type = 'parent-child' AND bt.depth < 50
  )
SELECT i.* FROM items i
LEFT JOIN blocked_tree bt ON bt.item_id = i.id
WHERE i.stage = 'ticket' AND i.status = 'open' AND bt.item_id IS NULL
ORDER BY i.score DESC, i.created_at ASC;
```

**Blocking propagates down the tree.** `blocked_directly` finds every item
with a `blocks` edge to something still open. `blocked_tree` then walks down
the `parent-child` edges from each of those items. Blocking an epic hides
everything beneath it, so a roadmap acts as a control surface. Block one
epic, and no agent asking for work will see anything under it.

**The filtering lives in the view.** The CLI, the MCP tools and the dashboard
all call the same `store.Ready`. The dashboard's tree view embeds the same
blocked-tree query. Because only one query decides what is blocked, the
surfaces cannot drift apart.

The `depth < 50` guard bounds the recursion. A `parent-child` cycle should be
impossible, because `dep` rejects self-edges and cannot create
`parent-child` edges at all. The guard stays anyway, because a mistake here
would mean an infinite loop in someone's shell.

## Scoring

```
score = value + confidence - effort + 11
```

Each input is from 0 to 10. The `+11` puts every scored item at 1 or above.
An unscored item gets exactly 0. It sorts last but stays in `ready`, because
unscored work is still work. The score is stored as a column and recomputed
on every write that touches an input, so `ready` can sort on it directly. The
store rejects inputs outside 0 to 10.

## Events

`events` is append-only, with columns `id`, `item_id`, `kind`, `actor`,
`session_id`, `at` and `payload`. Every mutation writes exactly one row.
`actor` is taken from `--actor`, then `$KEEP_ACTOR`, then the MCP client's
name, then `$USER`. `session_id` is taken from `$KEEP_SESSION`.

None of the store's logic reads `events`. No state is derived from it. It
exists because an audit trail added later can't include history that was
never recorded. `keep log`, `keep stale` and the dashboard's activity page
read from it.

A new concept gets a new table, never a new column on `items`. That rule keeps
the item row small, and it is why events, dependencies and claims each live in
their own place.

## Claims and sessions

`start` and `next` record the caller's session on the item and set its
status to `in_progress`. A second session's `start` on the same item fails,
and a repeat `start` from the same session does nothing. `next` selects the
top of `ready` and claims it in one transaction, so concurrent agents each
get a different item.

An item leaves `in_progress` in one of four ways:

- `close`: done.
- `drop`: abandoned. Never counted as done.
- `unclaim`: back to the queue, for when an agent stopped without finishing.
- `reopen`: back to `open` from `closed` or `dropped`, for when the close was
  a mistake.

All four need a reason. `update` has no `--status` flag, so a status change
always comes with an explanation in the log. `stale` reports claims with no
recent events. keep never releases a claim on its own.

## Why some things work the way they do

**`close` and `drop` are separate commands.** Abandoned work must never be
countable as finished. Two verbs make the difference impossible to blur in a
report.

**The terminal has no colour.** Agents and scripts pipe and grep terminal
output. `keep blocked` prints `← blocked by <id>`, which names the blocker
outright, something colour cannot do. The web board uses colour for three
states (workable, claimed and blocked), and never colour alone. A blocked row
also carries the word "blocked".

**Cross-repository reads open stores read-only.** `keep repos` and
`keep ready --all` open every registered store read-only. A read-write open of
a missing path would quietly create an empty database, and a deleted
repository would then look like an empty one. A read-only open fails instead,
so a missing store shows up as an error on its own row, and the other stores
still render.

**`capture` has no `--parent`, `--blocks` or `--score`.** Placement and
scoring are part of refining, and `add` has those flags.

## Out of scope

These are deliberate limits:

- **No model calls.** No embeddings or semantic search. Agents do the
  reasoning, which is why keep works with any agent.
- **No multi-machine sync.** `export` and `import` are a backup and a move
  into an empty store, not a merge.
- **No external tracker bridges.** There is no Jira, Linear or GitHub Issues
  sync. `items.external_ref` is a free-text string.
- **No HTTP API** beyond the loopback dashboard.
- **No agent orchestration.** keep says what needs doing. Deciding who works
  where belongs to Interlock.
