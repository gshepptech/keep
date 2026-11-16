#!/usr/bin/env python3
"""Verify keep's MCP tools load inside Codex and answer, without a turn.

This checks the Codex MCP registration against a live binary. The obvious
way -- run `codex exec` and watch it call a tool -- needs authentication and
spends a model turn, and fails before any tool is exercised if the session
is not authenticated.

codex app-server sidesteps that entirely: mcpServerStatus/list reports what
Codex thinks of each configured server, and mcpServer/tool/call invokes a
tool directly. No model, no quota, no auth.
"""

import json
import os
import shutil
import subprocess
import sys

# Codex is not on $PATH once it has moved into the desktop app; $KEEP_CODEX
# overrides, matching cmd/keep/codex.go's own findCodex order.
CODEX = os.environ.get("KEEP_CODEX") or shutil.which("codex") \
    or "/Applications/ChatGPT.app/Contents/Resources/codex"
REPO = os.environ.get("KEEP_REPO") or os.getcwd()


class AppServer:
    def __init__(self, codex):
        self.p = subprocess.Popen(
            [codex, "app-server"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, text=True, bufsize=1,
        )
        self.next_id = 1

    def send(self, obj):
        self.p.stdin.write(json.dumps(obj) + "\n")
        self.p.stdin.flush()

    def call(self, method, params=None, timeout_lines=4000):
        rid = self.next_id
        self.next_id += 1
        self.send({"jsonrpc": "2.0", "id": rid, "method": method,
                   "params": params if params is not None else {}})
        for _ in range(timeout_lines):
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError(f"{method}: app-server closed the stream")
            try:
                msg = json.loads(line)
            except ValueError:
                continue
            if msg.get("id") != rid:
                continue
            if "error" in msg:
                raise RuntimeError(f"{method}: {msg['error']}")
            return msg.get("result")
        raise RuntimeError(f"{method}: no response")

    def close(self):
        self.p.kill()


def main() -> int:
    failures = []

    def check(label, ok, detail=""):
        print(f"{'PASS' if ok else 'FAIL'}  {label}")
        if detail:
            print(f"      {detail}")
        if not ok:
            failures.append(label)

    # What the CLI says, read first and independently. The MCP answer is
    # compared against this, never the other way round.
    cli = json.loads(subprocess.run(
        ["keep", "ready", "--json"], cwd=REPO,
        capture_output=True, text=True, check=True).stdout)
    cli_ids = [it["id"] for it in cli]
    print(f"CLI `keep ready --json` -> {len(cli_ids)} items: {cli_ids}\n")

    s = AppServer(CODEX)
    try:
        s.call("initialize", {"clientInfo": {
            "name": "keep-verify", "title": "keep-verify", "version": "1"}})
        s.send({"jsonrpc": "2.0", "method": "initialized", "params": {}})

        status = s.call("mcpServerStatus/list")
        entries = status.get("data") or status.get("servers") or []
        keep = next((e for e in entries if "keep" in json.dumps(e)), None)
        check("Codex reports a server named keep", keep is not None)
        if keep is None:
            return 1

        # Every tool, by name. A count alone would pass if the server
        # advertised the right number of something else after a refactor.
        # Mirrors internal/mcp.ToolNames.
        want = sorted(["capture", "add", "ready", "backlog", "show", "list",
                       "search", "log", "stale", "dep", "start", "close",
                       "drop", "update", "blocked", "ready_all", "why_not",
                       "next", "unclaim", "reopen"])
        got = sorted(t if isinstance(t, str) else t.get("name")
                     for t in (keep.get("tools") or []))
        check("all tools are loaded, by name", got == want, f"{got}")

        thread = s.call("thread/start", {"cwd": REPO})
        tid = thread.get("threadId") or thread.get("thread", {}).get("id")
        check("thread/start returned a thread id", bool(tid), f"threadId={tid}")
        if not tid:
            return 1

        listed = s.call("mcpServer/tool/call", {
            "server": "keep", "threadId": tid,
            "tool": "ready", "arguments": {}})

        text = json.dumps(listed)
        check("mcpServer/tool/call ready answered", bool(text and text != "null"))

        # Pull the ids out of whatever envelope the result came back in.
        found = [i for i in cli_ids if i in text]
        check("every id the CLI reports is present in the MCP answer",
              len(found) == len(cli_ids),
              f"{len(found)}/{len(cli_ids)} matched")

        print("\n--- raw tool result (truncated) ---")
        print(text[:600])
    finally:
        s.close()

    print()
    if failures:
        print(f"{len(failures)} check(s) failed: {failures}")
        return 1
    print("all checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
