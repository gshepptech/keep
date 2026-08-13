---
description: Show the rounce board — counts plus the top of the queue
---

Run `rounce board`. If `$ARGUMENTS` contains `--web`, run `rounce board --web`
instead and report the URL it prints on its first line of stdout.

`board` is a glance, not a query: a counts line (ready / in progress / blocked /
backlog) followed by the top of the ranked queue. For the full list use
`rounce ready`.

The web view is read-only, binds `127.0.0.1` only, ships no JavaScript, and
fetches nothing off-host. It will not open a browser for you — print the URL and
let the user click it.
