package dash

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// agreementFixtureReadyCount is the number of plain ready tickets
// newAgreementFixture creates. This size is LOAD-BEARING, not a detail: a
// smaller fixture hides a trap -- a
// queue handler hard-coding `Ready(ctx, 10)` would pass on any fixture of 10
// or fewer items and only start failing later, at which point the failure
// looks like a regression in unrelated work. 16 stays above both
// cmd/keep's defaultBoardLimit (10) and any other default limit in play,
// with margin. Do not shrink this constant "to make the test faster."
const agreementFixtureReadyCount = 16

// newAgreementFixture builds a Server backed by a real store.Store
// containing agreementFixtureReadyCount plain ready tickets, a blocked
// subtree (an epic blocked by a separate blocker, with one child), and one
// raw backlog item -- so the queue view has real content to disagree about,
// not just an easy, tiny fixture. Returns the Server (for driving real HTTP
// requests) and the underlying Store (for calling st.Ready directly, the
// exact call the queue handler itself makes).
func newAgreementFixture(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := openTestStore(t)
	ctx := context.Background()
	const actor = "agreement-tester"

	for i := 0; i < agreementFixtureReadyCount; i++ {
		mustAdd(t, st, store.AddItemInput{Title: fmt.Sprintf("ready item %d", i), Actor: actor})
	}

	blocker := mustAdd(t, st, store.AddItemInput{Title: "blocker", Actor: actor})
	epic := mustAdd(t, st, store.AddItemInput{Title: "epic", Actor: actor})
	mustAdd(t, st, store.AddItemInput{Title: "child", Actor: actor, ParentID: epic.ID})
	if err := st.CreateDep(ctx, store.DepInput{ItemID: epic.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: actor}); err != nil {
		t.Fatalf("CreateDep: %v", err)
	}

	mustAdd(t, st, store.AddItemInput{Title: "raw note", Actor: actor, Stage: model.StageRaw, Type: model.TypeNote})

	s, err := NewServer(st, "127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return s, st
}

// idsFromQueueTable extracts every "/item/{id}" href's id, in document
// order, but ONLY from inside the element carrying class="queue" -- the
// board's own stable hook, so this test does not need to know whether the
// queue is a table, a list, or whatever it becomes next. Scoping to the
// hook (rather than collecting every /item/
// href on the page) is deliberate: collecting every such href would work
// today too, since the nav uses only fixed literal paths, but that is an
// accident of the current nav, not a guarantee -- a future "see also" link
// elsewhere on the page must not silently join this comparison.
//
// This walks a parsed DOM (golang.org/x/net/html), never a regex over the
// page's raw bytes: a raw-byte scan cannot distinguish "inside the queue
// table" from "inside a future footer" without becoming a second, worse
// HTML parser.
func idsFromQueueTable(t *testing.T, page string) []string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatalf("idsFromQueueTable: html.Parse: %v", err)
	}

	var table *html.Node
	var findTable func(*html.Node)
	findTable = func(n *html.Node) {
		if table != nil {
			return
		}
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "class" && a.Val == "queue" {
					table = n
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findTable(c)
		}
	}
	findTable(doc)
	if table == nil {
		t.Fatalf("idsFromQueueTable: no <table class=%q> found in rendered page:\n%s", "queue", page)
	}

	const itemPrefix = "/item/"
	var ids []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key == "href" && strings.HasPrefix(a.Val, itemPrefix) {
					ids = append(ids, strings.TrimPrefix(a.Val, itemPrefix))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)
	return ids
}

// idsOf returns items' ids, in order.
func idsOf(items []model.Item) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

// TestQueueViewAgreesWithReady is the web-side same-call proof: the
// ids rendered inside the queue table's <a href="/item/{id}"> links, in
// document order, must equal st.Ready(ctx, 0)'s ids exactly -- same length,
// same order, same values. This is stronger than "the page shows the ready
// items" -- it is the literal call handleReady itself makes
// (internal/dash/handlers.go's handleReady: `s.st.Ready(r.Context(), 0)`),
// proven against a fixture deliberately sized above every default limit in
// play (agreementFixtureReadyCount, 16), so a hard-coded limit cannot pass
// coincidentally the way it would against a smaller fixture.
func TestQueueViewAgreesWithReady(t *testing.T) {
	s, st := newAgreementFixture(t)

	rec := get(t, s.Handler(), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	got := idsFromQueueTable(t, rec.Body.String())

	want, err := st.Ready(context.Background(), 0)
	if err != nil {
		t.Fatalf("st.Ready(ctx, 0): %v", err)
	}
	wantIDs := idsOf(want)

	if len(wantIDs) <= 10 {
		t.Fatalf("fixture's ready count = %d, want it above 10 (and above agreementFixtureReadyCount's own margin) so this test cannot pass coincidentally against a hard-coded limit", len(wantIDs))
	}

	// On failure, print both slices in full -- the diff is the whole
	// diagnostic (this task's own <action> instruction).
	if !slices.Equal(got, wantIDs) {
		t.Fatalf("queue view and store.Ready(ctx, 0) disagree:\nqueue view ids (%d): %v\nReady(ctx, 0) ids (%d): %v", len(got), got, len(wantIDs), wantIDs)
	}
}
