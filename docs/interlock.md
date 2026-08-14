# Running rounce and Interlock together

rounce answers **what needs doing**. Interlock answers **who is doing what, and
where**. They are separate programs, neither imports the other, and either one
works alone. This is what happens when you install both.

---

## The problem they solve together

Open Claude Code in one terminal and Codex in another. Both ask for work. Both
get the same top-ranked ticket. Both start editing the same file.

rounce alone cannot prevent that — it does not watch processes. Interlock alone
cannot prevent it either — it stops the second write, but only after both agents
have already spent a while on the same task.

Together, the second agent never gets offered the work in the first place.

```
agent A                          agent B
   |                                |
   |-- rounce ready --------------->|  both see rnc-18da17 (28)
   |                                |
   |-- edits internal/auth/token.go |
   |     Interlock takes a lease    |
   |                                |
   |                                |-- rounce ready
   |                                |     rnc-18da17 is gone
   |                                |     rnc-d15cac (23) instead
```

Nothing coordinates the two agents. rounce simply stops offering work whose
files are spoken for.

---

## Install

Both are single Go binaries with no runtime services.

```bash
go install github.com/gshepptech/rounce/cmd/rounce@latest
go install github.com/gshepptech/interlock/cmd/interlock@latest

cd /your/repo
rounce init        # creates .rounce/db.sqlite
interlock init     # wires the agent hooks, merges, never replaces
```

Order does not matter. Neither `init` touches the other's state.

---

## Give the work paths

The join runs on one field: `paths`, on a rounce item.

```bash
rounce add --score 9/9/1 \
  --paths "internal/auth/token.go" \
  --why "tokens never expire; a leaked one is permanent" \
  "Expire refresh tokens after 30 days"
```

**Comma-separated, repo-relative.** An item with no paths is never held back —
rounce does not know what that work touches, and guessing that it collides would
hide workable tickets. Interlock's own hook still blocks the write if two agents
land on the same file anyway.

That is the failure direction on purpose: rounce would rather offer work
someone has quietly started than hide work nobody is doing.

---

## What each side does with the other

| | reads | over | when it is missing |
|---|---|---|---|
| `rounce ready` | Interlock's leases | `interlock status --json` | queue is unfiltered, no error |
| `interlock next` | rounce's queue | `rounce ready --json` | one sentence saying there is no work list |

Both directions are a subprocess and a versioned CLI contract, never a Go
import. That is what keeps "either works alone" true rather than aspirational:
a Go import would mean rounce stops building without Interlock.

An unrecognised version on either side filters nothing rather than guessing.

### Turning a direction off

```bash
ROUNCE_INTERLOCK=off    # rounce stops consulting Interlock
INTERLOCK_ROUNCE=off    # Interlock stops consulting rounce
```

Two variables, one per direction. Each can also be set to an absolute path if
the binary is not on `PATH`.

---

## Seeing it

Either dashboard shows the whole picture. They share a design language on
purpose — you should not have to re-learn a page when you switch.

```bash
rounce board --web        # the queue, and who is holding it up
interlock dashboard       # the agents, and what is next
```

### rounce — `/who`

![rounce's who-is-working view](images/rounce-who.png)

Two items are held back, and the page says by whom and for how long. The queue
view omits exactly these two — the board and this page share one filter, so the
board can never omit an item this page fails to explain.

### Interlock — the panel

![Interlock's panel](images/interlock-panel.png)

The same moment from the other side. **Next** shows what is workable, already
filtered; **Files held** below shows why the two higher-scoring items are not
there.

---

## What this does not do

- **It does not assign work.** rounce holds the list and Interlock holds the
  territory. Neither starts an agent unless you ask — `interlock run` is you
  asking.
- **It does not make the queue safe to race on.** A lease appears when an agent
  writes, not when it starts thinking. Two agents that both ask within the same
  second can still both be offered the same item.
- **It does not survive a missing `paths`.** Work you want protected has to say
  what it touches.

---

## Checking it works

```bash
rounce ready                 # note the top item
# ...let an agent start editing one of its files...
rounce ready                 # the item is gone; stderr names the holder
interlock status --json      # the lease that caused it
```

`rounce ready` writes the queue to stdout and the explanation to stderr, so a
script piping the queue never has to strip prose.
