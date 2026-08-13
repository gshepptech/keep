package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gshepptech/rounce/internal/model"
	"github.com/gshepptech/rounce/internal/store"
)

// instructionRows is how many queue rows the initialize handshake carries.
// Deliberately the same as the SessionStart hook's MAX_ROWS
// (plugin/scripts/session-queue.py): the two channels say the same thing to
// the same agent, and a reader who sees both should not have to work out
// which one is truncated differently.
const instructionRows = 5

// mcpInstructions renders the ranked queue for the MCP initialize handshake.
//
// # Why this exists at all
//
// plugin/README.md's argument for the SessionStart hook was that an
// instruction to a model can be ignored while a hook fires regardless. That
// is true, and it is also not the whole picture: the hook and this field
// both inject text, and neither compels the model to act on it.
//
// This was proposed as the cheap channel that covers every MCP client and
// so makes the per-agent hook work optional. Measurement killed half of
// that (see internal/mcp's Options.Instructions for the detail): Claude
// Code renders it under an "MCP Server Instructions" block, and Codex
// 0.147 does not put it in the model's prompt at all. It is the whole
// delivery mechanism on one agent and nothing on the other.
//
// It stays because it earns its hour on the Claude Code side and costs
// nothing elsewhere -- an MCP server that receives it but hides it is the
// client's choice, not a bug here. It does not replace the hook.
//
// # Why it returns "" rather than an error
//
// Same rule the hook follows: silence in a repository that has nothing to
// say. An empty store, or one whose counts cannot be read, must not turn
// into a handshake that announces an empty queue -- that trains a reader to
// skip the block, which defeats it. The caller passes the empty string
// straight through and the SDK omits the field.
func mcpInstructions(ctx context.Context, s *store.Store) string {
	ready, err := s.Ready(ctx, 0)
	if err != nil {
		return ""
	}
	inProgress, err := s.ListItems(ctx, store.ListFilter{Status: model.StatusInProgress})
	if err != nil {
		return ""
	}
	blocked, err := s.Blocked(ctx)
	if err != nil {
		return ""
	}
	backlog, err := s.Backlog(ctx, "")
	if err != nil {
		return ""
	}
	if len(ready)+len(inProgress)+len(blocked)+len(backlog) == 0 {
		return ""
	}

	// One Ready call, sliced in Go -- the same rule runBoard states at
	// length: the count must be exact regardless of how many rows are
	// shown, and the rows must be a prefix of the counted list. Two calls
	// with different limits is how a count silently stops matching its
	// queue.
	top := ready
	if len(top) > instructionRows {
		top = top[:instructionRows]
	}

	var b strings.Builder
	b.WriteString("rounce holds this repository's work queue. Use these tools rather than tracking it yourself.\n\n")
	fmt.Fprintf(&b, "Right now: ready %d, in progress %d, blocked %d, backlog %d.\n",
		len(ready), len(inProgress), len(blocked), len(backlog))

	b.WriteString("\nTop of the ranked queue as of this session's start:\n")
	for _, it := range top {
		fmt.Fprintf(&b, "  %-14s %3d  %-6s %s\n", it.ID, it.Score, it.Type, it.Title)
	}
	if len(ready) > len(top) {
		fmt.Fprintf(&b, "  ...and %d more -- call `ready` for the full list.\n", len(ready)-len(top))
	}

	b.WriteString("\nCall `ready` before starting work; it already excludes everything blocked, " +
		"so do not re-sort or re-filter it. Call `start` when you pick something up and " +
		"`close` with a reason when it lands -- `drop` is for abandoned work and is " +
		"deliberately not the same thing. File what you notice with `add`, or `capture` " +
		"if it is still rough, instead of mentioning it and moving on.\n")

	if len(blocked) > 0 {
		fmt.Fprintf(&b, "\n%d item(s) are blocked and deliberately absent from `ready` -- "+
			"`blocked` names what is holding each one.\n", len(blocked))
	}
	if len(backlog) > 0 {
		fmt.Fprintf(&b, "\n%d item(s) are raw in `backlog` and are not workable yet. "+
			"Promote one only after refining it into something whose title stands alone.\n", len(backlog))
	}

	// The counts above are a snapshot taken at server start. Saying so is
	// what stops an agent trusting them an hour into a session instead of
	// calling `ready` again.
	b.WriteString("\nThese counts were taken when this session started and do not update. " +
		"Call `ready` for the current queue.\n")

	return b.String()
}
