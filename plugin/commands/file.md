---
description: File something you noticed into keep instead of dropping it
---

File `$ARGUMENTS` into keep.

Decide which of the two is right, and say which you chose:

- **`add`**: you understand it well enough to state it as a workable ticket.
  Give it a title that means something on its own, and score it
  `--score value/confidence/effort`, each 0-10. Add `--why` with a `file:line`
  citation where you can; six weeks from now that is what makes the ticket
  legible.
- **`capture`**: it is a rough finding, a parked idea, or a document. It lands
  as `raw`, which means it will **not** appear in `ready` until someone refines
  it. Parking keeps it out of the queue until it is refined.

Two rules worth holding to:

- Do not file something already in the queue. Check `ready` and `backlog` first.
- Do not inflate scores to get attention. `confidence` in particular means how
  sure you are, and a speculative item scored 9 is worse than no item.
