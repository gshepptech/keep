package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// codexAppServerTimeout bounds the whole handshake-and-list exchange.
// `codex app-server` is a long-lived server: it never closes stdout on its
// own, so a read with no deadline is a hang, not a slow success.
const codexAppServerTimeout = 20 * time.Second

// codexBundledPath is where Codex lives once it has moved into the desktop
// app. It is not on $PATH there -- `command -v codex` finds nothing on a
// machine that has a perfectly working Codex -- which is exactly the
// failure this fallback exists to prevent. Interlock hit the same thing and
// its fix is commit cdf71b1, "Look for Codex where Codex actually is".
const codexBundledPath = "/Applications/ChatGPT.app/Contents/Resources/codex"

// codexHookMarker identifies rounce's own hooks among everything else
// hooks/list returns. Matching the plugin's hook file path rather than the
// plugin name is deliberate: the key's plugin half is
// "<plugin>@<marketplace>", and a user is free to name the marketplace
// anything at all when they add it, so "rounce@rounce" is a convention and
// not a guarantee. The path inside the plugin is ours.
const codexHookMarker = "codex/hooks.json"

// hooksListEntry and hookMetadata mirror the fields of Codex's
// HooksListResponse this command reads, and no more. The full schema is
// large and versioned; decoding only what is used means a field added
// upstream cannot break this.
type hookMetadata struct {
	Key         string `json:"key"`
	Command     string `json:"command"`
	EventName   string `json:"eventName"`
	TrustStatus string `json:"trustStatus"`
	CurrentHash string `json:"currentHash"`
	Enabled     bool   `json:"enabled"`
	PluginID    string `json:"pluginId"`
}

type hooksListEntry struct {
	Cwd      string         `json:"cwd"`
	Hooks    []hookMetadata `json:"hooks"`
	Warnings []string       `json:"warnings"`
	Errors   []struct {
		Message string `json:"message"`
		Path    string `json:"path"`
	} `json:"errors"`
}

// runCodex is `rounce codex <subcommand>`. Same dispatch shape as runRepos
// -- a literal string match, no CLI framework.
func runCodex(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "trust":
			return runCodexTrust(args[1:], stdout, stderr)
		case "status":
			return runCodexStatus(args[1:], stdout, stderr)
		}
	}
	fmt.Fprint(stderr, "usage: rounce codex <trust|status>\n"+
		"  trust   grant Codex the hook trust rounce's plugin needs\n"+
		"  status  report what Codex thinks of rounce's hooks\n")
	return 2
}

// runCodexTrust grants Codex the hook trust its plugin system requires.
//
// # Why this command exists rather than a documented config snippet
//
// Codex will not run a hook it has not been told to trust, and installing
// the plugin does not grant that -- measured 2026-08-13: a freshly
// installed rounce plugin reports trustStatus "untrusted", and the hook
// does not fire until the trust entry exists. That is a security control
// working as designed, and this command grants it on purpose rather than
// bypassing it. It is the same consent `claude plugin install` takes; Codex
// just makes it a separate step.
//
// # Why it asks Codex for the hash instead of computing one
//
// This is the whole design. Interlock computes the hash itself
// (internal/install/install.go, codexHookHash) and it broke silently when
// codex-cli 0.146.0-alpha.9.2 changed how trust is recorded -- Interlock's
// own commit 0e1c72a: "the entries Interlock writes are no longer accepted,
// so Codex skips its hooks without saying anything", while its doctor kept
// reporting the trust as healthy because it was verifying its own
// bookkeeping. A reimplementation of someone else's hash is a silent
// failure waiting for their next release.
//
// hooks/list hands over `key` and `currentHash` directly. Writing back what
// Codex just said cannot drift from what Codex expects.
//
// # Why it writes through Codex rather than editing config.toml
//
// config/value/write makes Codex edit its own config. rounce needs no TOML
// parser -- Interlock took that dependency on for exactly this job, and it
// is the one dependency its README singles out as regrettable.
func runCodexTrust(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("codex trust", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "report what would be trusted, change nothing")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}

	bin, err := findCodex()
	if err != nil {
		fmt.Fprintf(stderr, "rounce codex trust: %v\n", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "rounce codex trust: %v\n", err)
		return 1
	}

	hooks, err := codexHooks(bin, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "rounce codex trust: %v\n", err)
		return 1
	}

	ours := rounceHooks(hooks)
	if len(ours) == 0 {
		fmt.Fprintf(stderr, "rounce codex trust: Codex reports no rounce hooks for %s\n", cwd)
		fmt.Fprintf(stderr, "  install the plugin first:\n")
		fmt.Fprintf(stderr, "    codex plugin marketplace add <path to the rounce repo>\n")
		fmt.Fprintf(stderr, "    codex plugin add rounce@rounce\n")
		return 1
	}

	// Trusting a hook that is already trusted rewrites the same value, so
	// skipping is about output being readable on a second run, not about
	// correctness. Report it either way -- a command that prints nothing
	// when it did nothing is indistinguishable from one that failed.
	var pending []hookMetadata
	for _, h := range ours {
		fmt.Fprintf(stdout, "%-12s %s\n", h.TrustStatus, h.Key)
		if h.TrustStatus != "trusted" {
			pending = append(pending, h)
		}
	}
	if len(pending) == 0 {
		fmt.Fprintf(stdout, "\nnothing to do -- all %d rounce hook(s) already trusted\n", len(ours))
		return 0
	}
	if *dryRun {
		fmt.Fprintf(stdout, "\n--dry-run: would trust %d hook(s), changed nothing\n", len(pending))
		return 0
	}

	if err := codexTrustHooks(bin, pending); err != nil {
		fmt.Fprintf(stderr, "rounce codex trust: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\ntrusted %d hook(s)\n", len(pending))
	fmt.Fprintf(stdout, "start a new Codex session -- hooks are read at session start\n")
	return 0
}

// runCodexStatus reports what Codex itself says about rounce's hooks, which
// is a different question from what rounce wrote. Interlock's doctor
// answers the second and says so in its own output ("whether Codex accepts
// them is only visible in the 'actually running' check"); this answers the
// first.
func runCodexStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("codex status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "show every hook Codex reports, not just rounce's")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return 2
	}

	bin, err := findCodex()
	if err != nil {
		fmt.Fprintf(stderr, "rounce codex status: %v\n", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "rounce codex status: %v\n", err)
		return 1
	}
	hooks, err := codexHooks(bin, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "rounce codex status: %v\n", err)
		return 1
	}

	shown := hooks
	if !*all {
		shown = rounceHooks(hooks)
	}
	if len(shown) == 0 {
		fmt.Fprintf(stdout, "no rounce hooks reported for %s\n", cwd)
		return 0
	}
	for _, h := range shown {
		fmt.Fprintf(stdout, "%-12s %-14s enabled=%-5t %s\n",
			h.TrustStatus, h.EventName, h.Enabled, h.Key)
	}
	return 0
}

// findCodex resolves the Codex binary: $PATH first, then the desktop app
// bundle. $ROUNCE_CODEX overrides both, which is what makes this testable
// against a stub without a 218MB binary in the test fixtures.
func findCodex() (string, error) {
	if p := os.Getenv("ROUNCE_CODEX"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("codex"); err == nil {
		return p, nil
	}
	if _, err := os.Stat(codexBundledPath); err == nil {
		return codexBundledPath, nil
	}
	return "", fmt.Errorf("cannot find codex on $PATH or at %s (set $ROUNCE_CODEX to override)", codexBundledPath)
}

// rounceHooks filters hooks/list's output down to the ones this plugin
// installed.
func rounceHooks(hooks []hookMetadata) []hookMetadata {
	var out []hookMetadata
	for _, h := range hooks {
		if strings.Contains(h.Key, codexHookMarker) {
			out = append(out, h)
		}
	}
	return out
}

// codexHooks runs one `codex app-server`, performs the handshake, and
// returns every hook Codex reports for cwd.
func codexHooks(bin, cwd string) ([]hookMetadata, error) {
	res, err := codexRPC(bin, []any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{
				"name": "rounce", "title": "rounce", "version": version}}},
		map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "hooks/list",
			"params": map[string]any{"cwds": []string{cwd}}},
	}, 2)
	if err != nil {
		return nil, err
	}

	var listed struct {
		Data []hooksListEntry `json:"data"`
	}
	if err := json.Unmarshal(res, &listed); err != nil {
		return nil, fmt.Errorf("decode hooks/list: %w", err)
	}
	var out []hookMetadata
	for _, e := range listed.Data {
		out = append(out, e.Hooks...)
	}
	return out, nil
}

// codexTrustHooks writes each hook's own reported hash back through
// config/batchWrite, so Codex edits its own config.toml and rounce never
// parses TOML.
//
// The keyPath quotes the hook key because it contains dots, colons and an
// @ -- unquoted it would be read as a nested path rather than one key.
func codexTrustHooks(bin string, hooks []hookMetadata) error {
	edits := make([]map[string]any, 0, len(hooks))
	for _, h := range hooks {
		edits = append(edits, map[string]any{
			"keyPath":       fmt.Sprintf("hooks.state.%q.trusted_hash", h.Key),
			"mergeStrategy": "upsert",
			"value":         h.CurrentHash,
		})
	}

	_, err := codexRPC(bin, []any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{
				"name": "rounce", "title": "rounce", "version": version}}},
		map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "config/batchWrite",
			"params": map[string]any{"edits": edits}},
	}, 2)
	return err
}

// codexRPC sends msgs to one `codex app-server` over stdio and returns the
// result of the response whose id is wantID.
//
// Reading is line-by-line and filtered by id because the server interleaves
// notifications with responses -- taking the next line after a request is a
// race with whatever it decided to announce.
func codexRPC(bin string, msgs []any, wantID float64) (json.RawMessage, error) {
	cmd := exec.Command(bin, "app-server")
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s app-server: %w", filepath.Base(bin), err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			return nil, fmt.Errorf("write to app-server: %w", err)
		}
	}

	type rpcResp struct {
		ID     float64         `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	lines := make(chan string, 32)
	errs := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		errs <- sc.Err()
		close(lines)
	}()

	deadline := time.After(codexAppServerTimeout)
	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("app-server did not answer within %s", codexAppServerTimeout)
		case err := <-errs:
			return nil, fmt.Errorf("app-server closed without answering (%v)", err)
		case line, ok := <-lines:
			if !ok {
				return nil, fmt.Errorf("app-server closed without answering")
			}
			var r rpcResp
			if json.Unmarshal([]byte(line), &r) != nil || r.ID != wantID {
				continue
			}
			if r.Error != nil {
				return nil, fmt.Errorf("app-server error %d: %s", r.Error.Code, r.Error.Message)
			}
			return r.Result, nil
		}
	}
}
