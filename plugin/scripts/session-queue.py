#!/usr/bin/env python3
"""SessionStart hook: put rounce's ready queue in front of the agent.

This is the whole reason the plugin exists. A `CLAUDE.md` line asking an agent
to check the queue is an instruction it can ignore; a hook fires whether or not
the model cooperates, which is why rounce's CLI was kept complete in the first
place (docs/design/phase-3.md: "hooks can call a binary; hooks cannot call an
MCP tool").

Silence is a feature. This runs at the start of every session in every
repository, most of which do not use rounce, so it must produce nothing at all
unless there is something worth saying:

  - rounce not installed        -> silent
  - no store resolvable here    -> silent
  - store exists but is empty   -> silent

Never fails a session. Every error path exits 0 with no output.
"""

import json
import os
import shutil
import subprocess
import sys

TIMEOUT_SECONDS = 5
MAX_ROWS = 5


def emit(context: str) -> None:
    """Emit additionalContext in the shape SessionStart expects, then stop."""
    json.dump(
        {
            "hookSpecificOutput": {
                "hookEventName": "SessionStart",
                "additionalContext": context,
            }
        },
        sys.stdout,
    )
    sys.stdout.write("\n")


def main() -> int:
    binary = shutil.which("rounce")
    if not binary:
        return 0

    # `board --json` is one process for counts *and* the top of the queue --
    # deliberately, because this runs on every session start and three separate
    # invocations would be three times the cost for the same answer.
    try:
        proc = subprocess.run(
            [binary, "board", "--json", "--limit", str(MAX_ROWS)],
            capture_output=True,
            text=True,
            timeout=TIMEOUT_SECONDS,
        )
    except (subprocess.TimeoutExpired, OSError):
        return 0

    # A non-zero exit here is the ordinary case, not an error worth reporting:
    # it means no store resolves from this directory, i.e. this repository does
    # not use rounce.
    if proc.returncode != 0:
        return 0

    try:
        board = json.loads(proc.stdout)
        counts = board["counts"]
        items = board["items"]
    except (ValueError, KeyError, TypeError):
        return 0

    ready = counts.get("ready", 0)
    in_progress = counts.get("in_progress", 0)
    blocked = counts.get("blocked", 0)
    backlog = counts.get("backlog", 0)

    # An initialised but empty store has nothing to say. Saying it anyway would
    # train the reader to skip this block, which defeats the point.
    if not any((ready, in_progress, blocked, backlog)):
        return 0

    lines = [
        "## rounce — the work queue for this repository",
        "",
        f"**ready {ready}** · in progress {in_progress} · "
        f"blocked {blocked} · backlog {backlog}",
    ]

    if items:
        lines += ["", "Top of the ranked queue:", "```"]
        for it in items[:MAX_ROWS]:
            lines.append(
                "%-14s %3s  %-6s %s"
                % (
                    it.get("id", ""),
                    it.get("score", ""),
                    it.get("type", ""),
                    it.get("title", ""),
                )
            )
        lines.append("```")
        if ready > len(items):
            lines.append(f"…and {ready - len(items)} more. `ready` shows all of them.")

    lines += [
        "",
        "Use the rounce tools rather than tracking this yourself:",
        "`ready` before starting work — it already excludes anything blocked. "
        "`start` when you pick something up, `close` with a reason when it lands. "
        "File what you notice with `add`, or `capture` if it is still rough, "
        "instead of mentioning it and moving on.",
    ]

    if blocked:
        lines.append(
            f"{blocked} item(s) are blocked and deliberately absent from `ready` — "
            "`blocked` names what is holding each one."
        )

    emit("\n".join(lines))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        # A hook that breaks a session is worse than a hook that does nothing.
        sys.exit(0)
