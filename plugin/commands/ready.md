---
description: Show keep's ranked queue of workable items
---

Run `keep ready --json` and present the result as a compact table: id, score,
type, title.

If `$ARGUMENTS` contains a number, pass it as `--limit`.

Notes for reading the output:

- The list is already ranked by score and already excludes anything blocked;
  do not filter it further or re-sort it.
- An unscored ticket scores 0 and sorts last. It is *not* excluded, and it is
  not a mistake; it means nobody has scored it yet.
- If the list is empty, say so plainly and check `keep blocked` before
  concluding there is no work, since everything may be waiting on something.
