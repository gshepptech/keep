package main

import (
	"encoding/json"
	"slices"
	"testing"
)

// idsOfJSONItems extracts each item map's "id" field, in order, from a
// board/ready --json items array unmarshalled generically (never into a
// shared Go struct -- this proves the WIRE shape agrees, not that two call
// sites happen to share a type).
func idsOfJSONItems(t *testing.T, items []map[string]any) []string {
	t.Helper()
	ids := make([]string, len(items))
	for i, it := range items {
		id, ok := it["id"].(string)
		if !ok {
			t.Fatalf("item %d has no string \"id\" field: %#v", i, it)
		}
		ids[i] = id
	}
	return ids
}

// TestBoardAgreesWithReady is DASH-07's cross-process same-call proof:
// `board --json --limit 0`'s items and `ready --json`'s items must be the
// exact same ids, in the exact same order, against the same store.
// board.go's own comment names why this holds: one unlimited
// store.Ready(ctx, 0) call feeds both the printed count and the (Go-sliced)
// queue rows -- never two independently-limited calls that could "agree"
// only because one happens to be a superset of the other today.
func TestBoardAgreesWithReady(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	buildBoardFixture(t, bin, repo, e)

	boardStdout, boardStderr, boardCode := run(t, bin, repo, e, "board", "--json", "--limit", "0")
	if boardCode != 0 {
		t.Fatalf("board --json --limit 0: exit %d, stderr=%q", boardCode, boardStderr)
	}
	var boardOut boardJSONOut
	if err := json.Unmarshal([]byte(boardStdout), &boardOut); err != nil {
		t.Fatalf("board --json --limit 0: json.Unmarshal(entire stdout %q): %v", boardStdout, err)
	}

	readyStdout, readyStderr, readyCode := run(t, bin, repo, e, "ready", "--json")
	if readyCode != 0 {
		t.Fatalf("ready --json: exit %d, stderr=%q", readyCode, readyStderr)
	}
	var readyItems []map[string]any
	if err := json.Unmarshal([]byte(readyStdout), &readyItems); err != nil {
		t.Fatalf("ready --json: json.Unmarshal(entire stdout %q): %v", readyStdout, err)
	}

	boardIDs := idsOfJSONItems(t, boardOut.Items)
	readyIDs := idsOfJSONItems(t, readyItems)

	if len(readyIDs) <= defaultBoardLimit {
		t.Fatalf("fixture's ready count = %d, want it above defaultBoardLimit (%d) so this test cannot pass coincidentally", len(readyIDs), defaultBoardLimit)
	}
	if !slices.Equal(boardIDs, readyIDs) {
		t.Fatalf("board and ready disagree:\nboard --json --limit 0 ids (%d): %v\nready --json ids (%d): %v", len(boardIDs), boardIDs, len(readyIDs), readyIDs)
	}
}

// TestBoardCountMatchesReady proves counts.ready equals len(ready --json)
// while the DEFAULT --limit (10) is in effect -- the only arrangement that
// distinguishes "the count came from the unlimited call" from "the count
// came from len(rows)". The fixture (buildBoardFixture, 12 ready tickets)
// stays above defaultBoardLimit so a limit leaking into the count is caught
// immediately rather than only once the queue grows past 10 in production.
func TestBoardCountMatchesReady(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	buildBoardFixture(t, bin, repo, e)

	boardStdout, boardStderr, boardCode := run(t, bin, repo, e, "board", "--json")
	if boardCode != 0 {
		t.Fatalf("board --json: exit %d, stderr=%q", boardCode, boardStderr)
	}
	var boardOut boardJSONOut
	if err := json.Unmarshal([]byte(boardStdout), &boardOut); err != nil {
		t.Fatalf("board --json: json.Unmarshal(entire stdout %q): %v", boardStdout, err)
	}

	readyStdout, readyStderr, readyCode := run(t, bin, repo, e, "ready", "--json")
	if readyCode != 0 {
		t.Fatalf("ready --json: exit %d, stderr=%q", readyCode, readyStderr)
	}
	var readyItems []map[string]any
	if err := json.Unmarshal([]byte(readyStdout), &readyItems); err != nil {
		t.Fatalf("ready --json: json.Unmarshal(entire stdout %q): %v", readyStdout, err)
	}

	if len(readyItems) <= defaultBoardLimit {
		t.Fatalf("fixture's ready count = %d, want it above defaultBoardLimit (%d)", len(readyItems), defaultBoardLimit)
	}
	if boardOut.Counts.Ready != len(readyItems) {
		t.Fatalf("board --json counts.ready = %d, want %d (len(ready --json)) -- the default --limit is in effect, so this only holds if the count came from the unlimited call, not len(rows)", boardOut.Counts.Ready, len(readyItems))
	}
}

// TestBoardDefaultLimitTruncatesRowsOnly proves --limit's default (10)
// truncates board --json's items to length 10 while counts.ready stays the
// full number -- the companion assertion to TestBoardCountMatchesReady,
// naming the specific shape a limit-leaks-into-the-count bug produces
// (items and counts.ready both truncated to 10, instead of only items).
func TestBoardDefaultLimitTruncatesRowsOnly(t *testing.T) {
	bin := buildKeep(t, "")
	repo := tempDir(t)
	home := tempDir(t)
	e := env(home, "")
	mustInit(t, bin, repo, e)
	buildBoardFixture(t, bin, repo, e)

	boardStdout, boardStderr, boardCode := run(t, bin, repo, e, "board", "--json")
	if boardCode != 0 {
		t.Fatalf("board --json: exit %d, stderr=%q", boardCode, boardStderr)
	}
	var boardOut boardJSONOut
	if err := json.Unmarshal([]byte(boardStdout), &boardOut); err != nil {
		t.Fatalf("board --json: json.Unmarshal(entire stdout %q): %v", boardStdout, err)
	}

	if len(boardOut.Items) != defaultBoardLimit {
		t.Fatalf("board --json items length = %d, want %d (defaultBoardLimit) -- --limit must truncate rows, never the count", len(boardOut.Items), defaultBoardLimit)
	}
	if boardOut.Counts.Ready <= defaultBoardLimit {
		t.Fatalf("board --json counts.ready = %d, want it above defaultBoardLimit (%d) -- fixture must exceed the default limit for this assertion to be meaningful", boardOut.Counts.Ready, defaultBoardLimit)
	}
}
