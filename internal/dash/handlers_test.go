package dash

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// handlerFixtureIDs names every id newHandlerFixture's fixture produces, so
// each test asserts against a real, captured id, never a hard-coded rnc-
// literal (ids are hash-derived from title plus wall-clock creation time
// and differ on every run).
type handlerFixtureIDs struct {
	epic, child, grandchild, ready, note string
}

// newHandlerFixture builds a Server backed by a real store.Store containing:
// a blocked epic with a child and a grandchild (three tree levels, root
// blocked directly, so the tree route's blocked-word behaviour is
// exercised, and the grandchild's own id carries two dots by construction),
// one separate ready ticket, and one raw note (the backlog view's one
// group).
func newHandlerFixture(t *testing.T) (*Server, handlerFixtureIDs) {
	t.Helper()
	st := openTestStore(t)
	ctx := context.Background()
	const actor = "tester"

	epic := mustAdd(t, st, store.AddItemInput{Title: "Epic root", Actor: actor})
	child := mustAdd(t, st, store.AddItemInput{Title: "Child task", Actor: actor, ParentID: epic.ID, Body: "onlyChildBodyText"})
	grandchild := mustAdd(t, st, store.AddItemInput{Title: "Grandchild task", Actor: actor, ParentID: child.ID, Body: "onlyGrandchildBodyText"})
	blocker := mustAdd(t, st, store.AddItemInput{Title: "Blocker", Actor: actor})
	if err := st.CreateDep(ctx, store.DepInput{ItemID: epic.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: actor}); err != nil {
		t.Fatalf("CreateDep: %v", err)
	}
	ready := mustAdd(t, st, store.AddItemInput{Title: "Ready ticket", Actor: actor})
	note := mustAdd(t, st, store.AddItemInput{Title: "Raw note", Actor: actor, Stage: model.StageRaw, Type: model.TypeNote})

	s, err := NewServer(st, "127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.ln.Close() })

	if strings.Count(grandchild.ID, ".") < 2 {
		t.Fatalf("fixture bug: grandchild id %q does not contain two dots", grandchild.ID)
	}

	return s, handlerFixtureIDs{
		epic: epic.ID, child: child.ID, grandchild: grandchild.ID,
		ready: ready.ID, note: note.ID,
	}
}

// get drives s's route table through httptest, with no port binding.
func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerReadyRendersQueue(t *testing.T) {
	s, ids := newHandlerFixture(t)
	rec := get(t, s.srv.Handler, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("GET /: Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	if !strings.Contains(rec.Body.String(), ids.ready) {
		t.Errorf("GET /: body does not contain ready item id %q", ids.ready)
	}
}

func TestHandlerBacklogRendersGroupedByType(t *testing.T) {
	s, ids := newHandlerFixture(t)
	rec := get(t, s.srv.Handler, "/backlog")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /backlog: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ids.note) {
		t.Errorf("GET /backlog: body does not contain raw note id %q", ids.note)
	}
	if !strings.Contains(body, ">"+model.TypeNote+"<") {
		t.Errorf("GET /backlog: body does not contain the %q group header", model.TypeNote)
	}
}

// item.html deliberately carries no title row, following `show` exactly
// (id/stage/type/status/score/parent/repo/paths/why/acceptance/created/
// updated, then deps/events, then the body). These tests therefore assert
// against the rendered id and each fixture item's own unique body-text
// marker, which the template does render, rather than a title it does not.

func TestHandlerItemRendersOneItem(t *testing.T) {
	s, ids := newHandlerFixture(t)
	rec := get(t, s.srv.Handler, "/item/"+ids.grandchild)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /item/%s: status = %d, want 200", ids.grandchild, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ids.grandchild) {
		t.Errorf("GET /item/%s: body does not contain the item's own id", ids.grandchild)
	}
	if !strings.Contains(body, "onlyGrandchildBodyText") {
		t.Errorf("GET /item/%s: body does not contain the item's own body text", ids.grandchild)
	}
}

// TestHandlerItemDotContainingIDRoutesToTheRightChild proves a
// dot-containing id is captured whole by the {id} wildcard, routing to that
// child, not its ancestor.
func TestHandlerItemDotContainingIDRoutesToTheRightChild(t *testing.T) {
	s, ids := newHandlerFixture(t)
	rec := get(t, s.srv.Handler, "/item/"+ids.grandchild)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /item/%s (two-dot id): status = %d, want 200", ids.grandchild, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "onlyGrandchildBodyText") {
		t.Errorf("GET /item/%s: rendered the wrong item, not its own body text", ids.grandchild)
	}
	if strings.Contains(body, "onlyChildBodyText") {
		t.Errorf("GET /item/%s: rendered its ancestor's body text too -- id was not captured whole", ids.grandchild)
	}
}

func TestHandlerTreeRendersSubtreeWithBlockedWord(t *testing.T) {
	s, ids := newHandlerFixture(t)
	rec := get(t, s.srv.Handler, "/tree/"+ids.epic)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tree/%s: status = %d, want 200", ids.epic, rec.Code)
	}
	body := rec.Body.String()
	for _, id := range []string{ids.epic, ids.child, ids.grandchild} {
		if !strings.Contains(body, id) {
			t.Errorf("GET /tree/%s: body does not contain %q", ids.epic, id)
		}
	}
	if !strings.Contains(body, "blocked") {
		t.Errorf("GET /tree/%s: body does not contain the word \"blocked\"", ids.epic)
	}
}

func TestHandlerItemUnknownIDIs404(t *testing.T) {
	s, _ := newHandlerFixture(t)
	rec := get(t, s.srv.Handler, "/item/rnc-nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /item/rnc-nope: status = %d, want 404", rec.Code)
	}
}

func TestHandlerTreeUnknownIDIs404(t *testing.T) {
	s, _ := newHandlerFixture(t)
	rec := get(t, s.srv.Handler, "/tree/rnc-nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /tree/rnc-nope: status = %d, want 404", rec.Code)
	}
}

func TestHandlerRootRefusesPOST(t *testing.T) {
	s, _ := newHandlerFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: status = %d, want 405", rec.Code)
	}
}

// TestAMissingIdGetsAPageNotBareText checks that a request for an unknown id
// renders a full 404 page with navigation.
//
// http.Error writes an unstyled sentence with no nav and no way back. A
// board whose URLs carry ids is a board people edit URLs on, so this is
// reached by ordinary use and must not be a dead end.
//
// A page nested inside another page's Data makes the template fall back to
// bare text. The body assertion catches that; the status code alone would
// not.
func TestAMissingIdGetsAPageNotBareText(t *testing.T) {
	s, _ := writableFixture(t)

	rec := get(t, s.srv.Handler, "/item/rnc-nothing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Nothing here", "Back to the board", "<nav", "class=\"skip\""} {
		if !strings.Contains(body, want) {
			t.Errorf("the 404 page is missing %q -- it fell back to bare text", want)
		}
	}
}

// TestTheActiveNavSaysSoOutLoud checks that the current page's nav link
// carries aria-current. That is what tells a screen reader which page it is
// on; the highlight alone says it only to people who can see it.
func TestTheActiveNavSaysSoOutLoud(t *testing.T) {
	s, _ := writableFixture(t)

	// The stylesheet is inlined into every page and contains the selector
	// `.nav-item[aria-current="page"]`, so a naive Contains matches the CSS
	// and passes even when no element carries the attribute. Strip the style
	// block before looking.
	body := get(t, s.srv.Handler, "/").Body.String()
	if i := strings.Index(body, "<style>"); i >= 0 {
		if j := strings.Index(body[i:], "</style>"); j >= 0 {
			body = body[:i] + body[i+j:]
		}
	}
	if !strings.Contains(body, `aria-current="page"`) {
		t.Error("no element carries aria-current -- the active nav item is styled but silent")
	}
}
