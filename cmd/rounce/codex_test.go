package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCodex writes a stub `codex app-server` that speaks just enough of the
// protocol for these tests: it answers id 1 (initialize) and id 2, and
// appends every request it received to a log file so the test can assert on
// what rounce actually sent.
//
// A stub rather than the real binary because the real one is a 218MB
// dependency that is not on $PATH, may not be installed at all, and would
// make this test a check on OpenAI's release cadence. The live end-to-end
// path was verified by hand against codex-cli 0.147.0-alpha.6.5; what these
// tests lock is rounce's half of the exchange, which is the half that can
// regress here.
func fakeCodex(t *testing.T, hooksListResult string) (bin, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub uses a POSIX shebang")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "requests.jsonl")
	bin = filepath.Join(dir, "codex")

	script := `#!/usr/bin/env python3
import json, sys
LOG = ` + pyLiteral(logPath) + `
RESULT = json.loads(` + pyLiteral(hooksListResult) + `)
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    with open(LOG, "a") as f:
        f.write(line + "\n")
    try:
        msg = json.loads(line)
    except ValueError:
        continue
    mid = msg.get("id")
    if mid is None:
        continue
    if msg.get("method") == "initialize":
        out = {"jsonrpc": "2.0", "id": mid, "result": {"userAgent": "stub"}}
    elif msg.get("method") == "hooks/list":
        out = {"jsonrpc": "2.0", "id": mid, "result": RESULT}
    else:
        out = {"jsonrpc": "2.0", "id": mid, "result": {}}
    # A notification interleaved before the response is the whole reason
    # codexRPC filters by id instead of reading the next line.
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "thread/started"}) + "\n")
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return bin, logPath
}

// pyLiteral quotes s as a Python string literal. json.Marshal of a string is
// a valid Python literal too, and it escapes correctly, which hand-quoting
// a t.TempDir() path would not reliably do.
func pyLiteral(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

const oneUntrustedHook = `{"data":[{"cwd":"/repo","hooks":[
 {"key":"rounce@rounce:codex/hooks.json:session_start:0:0","command":"/x/session-queue.py",
  "eventName":"sessionStart","trustStatus":"untrusted",
  "currentHash":"sha256:deadbeef","enabled":true,"pluginId":"rounce@rounce"},
 {"key":"other@thing:hooks/hooks.json:session_start:0:0","command":"/y/other.sh",
  "eventName":"sessionStart","trustStatus":"untrusted",
  "currentHash":"sha256:cafe","enabled":true,"pluginId":"other@thing"}
],"warnings":[],"errors":[]}]}`

func readRequests(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// TestCodexTrustWritesTheHashCodexReported is the design decision this
// whole command exists for. Interlock computes the hash itself and broke
// silently when 0.146 changed the format (its commit 0e1c72a). rounce must
// write back exactly what hooks/list said and never a value of its own.
func TestCodexTrustWritesTheHashCodexReported(t *testing.T) {
	bin := buildRounce(t, "")
	repo := t.TempDir()
	home := t.TempDir()
	stub, logPath := fakeCodex(t, oneUntrustedHook)
	e := env(home, "", "ROUNCE_CODEX="+stub)
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "codex", "trust")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "trusted 1 hook(s)") {
		t.Errorf("stdout does not report one hook trusted:\n%s", stdout)
	}

	var writes []any
	for _, req := range readRequests(t, logPath) {
		if req["method"] != "config/batchWrite" {
			continue
		}
		params, _ := req["params"].(map[string]any)
		edits, _ := params["edits"].([]any)
		writes = append(writes, edits...)
	}
	if len(writes) != 1 {
		t.Fatalf("sent %d config edits, want exactly 1 (rounce's hook only): %v", len(writes), writes)
	}

	edit, _ := writes[0].(map[string]any)
	// The hash must be the one the stub reported, byte for byte.
	if got := edit["value"]; got != "sha256:deadbeef" {
		t.Errorf("wrote hash %v, want the reported sha256:deadbeef -- a computed hash is the Interlock failure", got)
	}
	// The key must be quoted: it contains dots, a colon and an @, and
	// unquoted it would be read as a nested path rather than one key.
	wantPath := `hooks.state."rounce@rounce:codex/hooks.json:session_start:0:0".trusted_hash`
	if got := edit["keyPath"]; got != wantPath {
		t.Errorf("keyPath = %v,\nwant %s", got, wantPath)
	}
}

// TestCodexTrustLeavesOtherPluginsAlone: hooks/list returns every hook on
// the machine. Trusting one rounce did not install would be rounce granting
// consent on someone else's behalf, which is the exact thing that makes
// --dangerously-bypass-hook-trust a bad answer.
func TestCodexTrustLeavesOtherPluginsAlone(t *testing.T) {
	bin := buildRounce(t, "")
	repo := t.TempDir()
	home := t.TempDir()
	stub, logPath := fakeCodex(t, oneUntrustedHook)
	e := env(home, "", "ROUNCE_CODEX="+stub)
	mustInit(t, bin, repo, e)

	if _, stderr, code := run(t, bin, repo, e, "codex", "trust"); code != 0 {
		t.Fatalf("exit %d (stderr=%q)", code, stderr)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	if strings.Contains(string(raw), "other@thing") {
		t.Errorf("rounce sent a write naming another plugin's hook:\n%s", raw)
	}
	if strings.Contains(string(raw), "sha256:cafe") {
		t.Errorf("rounce wrote another plugin's hash:\n%s", raw)
	}
}

// TestCodexTrustDryRunWritesNothing: --dry-run must reach Codex to read the
// state and then stop. A dry run that skipped the read could not report
// what it would do.
func TestCodexTrustDryRunWritesNothing(t *testing.T) {
	bin := buildRounce(t, "")
	repo := t.TempDir()
	home := t.TempDir()
	stub, logPath := fakeCodex(t, oneUntrustedHook)
	e := env(home, "", "ROUNCE_CODEX="+stub)
	mustInit(t, bin, repo, e)

	stdout, stderr, code := run(t, bin, repo, e, "codex", "trust", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "would trust 1 hook(s)") {
		t.Errorf("dry run does not say what it would do:\n%s", stdout)
	}
	sawList := false
	for _, req := range readRequests(t, logPath) {
		if req["method"] == "hooks/list" {
			sawList = true
		}
		if req["method"] == "config/batchWrite" {
			t.Errorf("--dry-run sent a config write: %v", req)
		}
	}
	if !sawList {
		t.Error("--dry-run never called hooks/list, so it cannot know what it would do")
	}
}

// TestCodexTrustFailsLoudlyWithNoPlugin: a hook rounce cannot find is a
// half-install, and the one thing this command must not do is exit 0 having
// silently done nothing -- that is the failure mode Interlock's doctor is
// on record as having (reporting trust healthy while Codex ignored it).
func TestCodexTrustFailsLoudlyWithNoPlugin(t *testing.T) {
	bin := buildRounce(t, "")
	repo := t.TempDir()
	home := t.TempDir()
	stub, _ := fakeCodex(t, `{"data":[{"cwd":"/repo","hooks":[],"warnings":[],"errors":[]}]}`)
	e := env(home, "", "ROUNCE_CODEX="+stub)
	mustInit(t, bin, repo, e)

	_, stderr, code := run(t, bin, repo, e, "codex", "trust")
	if code == 0 {
		t.Errorf("exit 0 with no rounce hooks installed, want non-zero (stderr=%q)", stderr)
	}
	if !strings.Contains(stderr, "codex plugin add") {
		t.Errorf("stderr does not tell the user how to install the plugin:\n%s", stderr)
	}
}
