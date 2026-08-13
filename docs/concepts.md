# Concepts

This is the "why is it like this" page. If `docs/guide.md` is the walkthrough,
this is the reference for the decisions underneath it, with pointers to the
design docs that settled each one.

## Stage: raw vs. ticket

Every item has a `stage`, either `raw` or `ticket`. This is the single
column the whole tool pivots on.

`raw` means captured and not workable: a document dropped in verbatim, an
idea parked mid-session, a finding an agent surfaced that isn't wanted yet.
`ticket` means refined: it has a title that means something on its own,
usually acceptance criteria, and usually a score. `ready` only ever shows
`ticket`s.

The reason this matters more than it sounds like it should: `stage = 'ticket'`
is a predicate inside the `ready` SQL view, not a filter some caller applies.
A raw item can't leak into the work queue because a command forgot to check
its stage — there's no code path where that check could be skipped, because
there's no code path that queries `ready` without going through the view.
"Parking an item has to actually remove it from the work queue, or 'not yet'
means nothing" is stated as a project principle in the README, and this is
the mechanism that makes it true rather than aspirational.

There's no `refine` command that moves an item from `raw` to `ticket`.
Refinement is just an agent reading `show` on a raw item and issuing
`add --parent <id> --derived-from <id>` to create a new ticket — the raw
item itself never changes stage. See `docs/design/phase-2.md`.

## The four dependency types

`deps` is one table of typed edges: `item_id`, `depends_on_id`, `type`,
where `type` is one of `blocks`, `parent-child`, `related`, `derived-from`.

- **`blocks`** — `(A, B, blocks)` means A is blocked until B closes. This is
  the one that feeds `ready`'s filtering.
- **`parent-child`** — structure. Created only by `add --parent`, never by
  `rounce dep`; `dep` explicitly refuses to create a `parent-child` edge and
  tells you to use `add --parent` instead (`store.ErrParentChildViaDep`).
  This is what block-propagation walks.
- **`related`** — a plain cross-reference with no behavior attached.
- **`derived-from`** — provenance: which raw document or note a ticket was
  refined out of.

`parent-child` and `derived-from` are kept as two separate edges rather than
folded into one, because they answer two different questions. `--parent`
answers "where does this sit in the tree" (so blocking an epic reaches it).
`--derived-from` answers "what document or note did this come out of" (so
the ticket's origin survives past the session that created it). A ticket can
be parented under one epic while derived from an entirely different parked
note — that's not an edge case the model has to special-case, it's just two
independent facts recorded independently. See `docs/design/phase-2.md`,
"Why provenance matters here."

## Hierarchical ids

A root item gets `rnc-<6 hex chars>`, hashed from its title plus creation
timestamp. A child gets `.1`, `.2`, ...; a grandchild gets `.1.1`. This is
taken from Beads (see the README's Prior Art section) because it means an
agent reading an id in a log already knows where the item sits in the tree,
with no lookup required.

Child ordinals are allocated `MAX(ordinal) + 1` inside the same transaction
as the insert, and a `UNIQUE(parent_id, ordinal)` index backstops it, so two
concurrent `add --parent` calls under the same epic can't collide on the
same ordinal. This is tested across real separate processes, not goroutines
in one process — a purely in-process race test can pass while a real
cross-process race still exists, which is exactly the trap Interlock hit
once already.

## The `ready` view, and why blocking propagates

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

Two things carry the whole design (`docs/design/phase-1.md`):

**Blocking propagates down the parent-child tree.** `blocked_directly` finds
items with an open `blocks` edge pointing at something not yet closed.
`blocked_tree` then walks *down* the `parent-child` edges from every
directly-blocked item, at any depth. So blocking an epic doesn't just hide
the epic — it hides everything under it, recursively, because every child
(and grandchild, and so on) is reachable from the blocked epic by walking
`parent-child` downward. This is what makes a roadmap a control surface
instead of decoration: block the epic once, and an agent asking `ready`
"what's workable" simply never sees any of it, at any depth.

**The filtering lives in the view, never in a caller.** No CLI command,
no MCP tool, and no dashboard query re-implements this logic — they all
call the same `store.Ready` (or in the dashboard's case, the same
`blockedTreeCTE` constant embedded into `store.Tree`), which runs this
exact query. Four surfaces can't independently drift out of agreement about
what's blocked, because there's only one query computing it, not four.

The `depth < 50` guard bounds the recursion. A cycle in `parent-child`
should be structurally impossible — `dep` rejects self-edges and the CLI
never lets `dep` create `parent-child` edges at all — but the guard stays
anyway, because the cost of being wrong here is an infinite loop inside
someone's shell, not a bad answer.

## Events: the audit trail

`events` is append-only: `id`, `item_id`, `kind`, `actor`, `session_id`,
`at`, `payload`. Every mutation — `add`, `capture`, `dep`, `start`, `close`,
`drop`, `update` — writes exactly one row. `kind` is the command name,
`actor` comes from `--actor`, else `$ROUNCE_ACTOR`, else `$USER`, and
`session_id` comes from `$ROUNCE_SESSION` when the caller sets it
(Interlock, rounce's sibling tool, already sets that variable for hook
invocations, which is how the two line up without either side knowing about
the other).

Nothing in the store reads `events` for its own logic — no state is derived
from it, no view depends on it. It's written purely because retrofitting an
audit trail loses the history that made it worth having; by the time you
notice you want it, the events you wanted are already gone. This is also
why `items` never grows a new column for something event-shaped: rounce's
third stated principle is "a new concept gets a new table, never a new
column on `items`," specifically because Beads' `issues` table reached
roughly five dozen columns by absorbing an event bus and a scheduler
directly onto the item row. rounce's `events` table exists so that
temptation never comes up.

## The deliberate refusals

Several things rounce visibly does *not* do, and each is a decision dated
in the README rather than an oversight:

- **No model calls.** rounce is a store. It never calls an LLM, computes an
  embedding, or does semantic search. Refinement, triage, and scoring are
  things an agent does *using* rounce's commands — which is exactly what
  keeps the binary small and keeps it usable from any agent or plugin,
  since none of them need to share a model provider with rounce itself.
- **No sync.** No JSONL export, no CRDT, no merge resolution, no
  multi-machine story at all. Sharing a store today means sharing a
  repository (and therefore a filesystem). Building sync is "a decision to
  be made deliberately and dated," per the README — not a gap waiting to be
  filled by the next contributor who trips over it.
- **No orchestration.** rounce has no concept of agent state, awaits,
  timeouts, or roles. `items.claimed_by_session` and `items.paths` exist as
  columns nobody reads yet, reserved for a future join with Interlock, which
  is the tool that answers "who is doing what, and where" — rounce only ever
  answers "what needs doing." Coordinating agents belongs there, not here.
- **No external tracker bridges.** No Jira, Linear, GitHub Issues, GitLab,
  Azure DevOps, or Notion sync. `items.external_ref` is a string nobody
  reads, kept only so a bridge could be added later without a migration.
- **No HTTP API beyond the loopback dashboard.** No federation, no remote
  access, no auth model to get wrong.
- **No migration framework**, beyond a bare `schema_version` integer, until
  there's data worth not breaking. See `docs/design/phase-0.md`: no
  `is_blocked` column either, because Beads needed one only after their
  recursive view got slow at their data volume, and needed a second
  migration to keep it correct — a fix rounce isn't shaped to need yet, and
  won't add ahead of a benchmark that says it should.

## Odd-looking decisions, explained

**Why `close` and `drop` are separate commands**, rather than one command
with a status flag. Dropped work must never be countable as done — if
abandoning an item and finishing it both routed through the same verb, a
report built off `status = closed` would have to trust that nobody ever
conflated the two, instead of that distinction being structurally
impossible to blur. The store's underlying transition logic is shared
(`store.closeOrDrop`), but the CLI intentionally keeps them as two
separate commands with two separate help texts, not one command with a
mode argument.

**Why `update` can't change status.** `open → in_progress → closed/dropped`
moves only through `start`/`close`/`drop`, never through `update --status`.
`close` and `drop` both require `--reason`; if `update` could also flip
status, a caller could close out work while silently skipping that
required reason, and the event log would end up recording that something
happened without recording why. `update` doesn't register a `--status`
flag at all — a caller who tries it gets Go's own "flag provided but not
defined" and exit 2, which is a clearer signal than a custom error
pretending the flag almost existed.

**Why the terminal has no color but the web dashboard does.** Terminal
output gets piped and grepped, by scripts and by agents, and color codes
are noise in that context, not signal — plus supporting it properly would
mean TTY detection and `NO_COLOR` handling, a new mechanism introduced for
one command that every other command would then be expected to adopt. More
to the point, the terminal already has a *better* signal than color could
be: `rounce blocked` prints `← blocked by <id>`, naming the actual blocker
in text, which color can't do. The web page uses color for exactly three
states (workable/claimed/blocked) because it can't append that string to
every row without the layout collapsing — and even there, color is never
the only signal: a blocked row also carries the literal word "blocked" in
text, both for colorblind readers and because, per the design doc, "the
page says 'blocked,' not a metaphor for blocked." See
`docs/design/phase-4.md`, record 3.

**Why a deleted store errors on its row instead of showing up as empty.**
`rounce repos` and `rounce ready --all` open every registered store
read-only (`OpenOptions{ReadOnly: true}`). That's not an optimization —
it's what makes a missing store fail loudly instead of silently. A default
read-write open of a missing path would succeed and quietly create a new,
empty, 4096-byte SQLite database at that path; a deleted repository would
then be indistinguishable from a genuinely empty one. Read-only mode
creates nothing and fails instead, so a deleted or moved store shows up as
a visible error on its own row — reported and skipped, never fatal to the
rest of the board. See `docs/design/phase-5.md`, record 2.

**Why capture has no `--parent`, `--blocks`, or `--score`.** Placement and
scoring are refinement's job, not capture's — those flags exist on `add`.
Adding them to `capture` later is possible; removing them once agents
depend on them is not, so the omission is deliberate rather than an
oversight waiting to be filled in.
