package dash

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/store"
)

// writableFixture builds a board that can act on its store.
func writableFixture(t *testing.T) (*Server, string) {
	t.Helper()
	t.Setenv(interlock.EnvBinary, "off")
	dir := t.TempDir() + "/db.sqlite"
	st, err := store.Init(dir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s, err := NewServer(st, "127.0.0.1:0", "", Writable(dir), ActingAs("board"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

// post sends a form to the board, carrying the token unless told otherwise.
func post(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if !form.Has(csrfField) {
		form.Set(csrfField, s.csrf)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestBoardFilesAnItem is the whole point of making the board writable: a
// person watching a queue can add to it without leaving the page.
func TestBoardFilesAnItem(t *testing.T) {
	s, _ := writableFixture(t)

	rec := post(t, s, "/do/add", url.Values{
		"title": {"Filed from the board"}, "type": {"task"},
		"value": {"9"}, "confidence": {"8"}, "effort": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add: status %d, want 303: %s", rec.Code, rec.Body.String())
	}

	body := get(t, s.Handler(), "/").Body.String()
	if !strings.Contains(body, "Filed from the board") {
		t.Errorf("the new item is not on the board:\n%s", body)
	}
	// 9 + 8 - 1 + 11 = 27. Asserted because a form that dropped the score
	// fields would still file the item, and it would sort last for ever.
	if !strings.Contains(body, "27") {
		t.Errorf("the item was filed unscored; the score fields did not reach the store:\n%s", body)
	}
}

// TestBoardRefusesAForeignToken. The server is loopback-only, but loopback
// includes every other program on the machine and every page the browser
// has open. Without this, any site could POST here.
func TestBoardRefusesAForeignToken(t *testing.T) {
	s, _ := writableFixture(t)

	rec := post(t, s, "/do/add", url.Values{
		csrfField: {"not-the-token"}, "title": {"Should not exist"},
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a wrong token got %d, want 403", rec.Code)
	}
	if body := get(t, s.Handler(), "/").Body.String(); strings.Contains(body, "Should not exist") {
		t.Error("an item was filed with a wrong token")
	}
}

// TestBoardRefusesAForeignOrigin is the second, independent check. A
// browser that sends neither header is still covered by the token; one that
// sends a foreign Origin is refused before the token is even read.
func TestBoardRefusesAForeignOrigin(t *testing.T) {
	s, _ := writableFixture(t)

	form := url.Values{csrfField: {s.csrf}, "title": {"Should not exist"}}
	req := httptest.NewRequest(http.MethodPost, "/do/add", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("a foreign Origin got %d, want 403", rec.Code)
	}
}

// TestBoardGetCannotMutate: every mutation is POST, so no link, prefetch,
// or reload can change the store.
func TestBoardGetCannotMutate(t *testing.T) {
	s, _ := writableFixture(t)

	for _, verb := range []string{"add", "start", "close", "drop", "update", "promote"} {
		if rec := get(t, s.Handler(), "/do/"+verb); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET /do/%s returned %d, want 405", verb, rec.Code)
		}
	}
}

// TestBoardWithoutWritableRendersNoActions. A button that is going to say
// no is worse than no button, so a read-only board does not render them.
func TestBoardWithoutWritableRendersNoActions(t *testing.T) {
	t.Setenv(interlock.EnvBinary, "off")
	s, _ := newHandlerFixture(t) // built without Writable

	body := get(t, s.Handler(), "/").Body.String()
	if strings.Contains(body, "/do/start") {
		t.Errorf("a read-only board rendered an action form:\n%s", body)
	}
	if !strings.Contains(body, "Read only") {
		t.Errorf("a read-only board does not say so:\n%s", body)
	}
}

// TestBoardStartsAndClosesAnItem walks the transition the queue offers, and
// reads the result back off the page rather than from the action's own
// response.
func TestBoardStartsAndClosesAnItem(t *testing.T) {
	s, _ := writableFixture(t)

	post(t, s, "/do/add", url.Values{"title": {"Walk the transitions"}, "type": {"task"}})
	id := firstIDOnBoard(t, s)

	if rec := post(t, s, "/do/start", url.Values{"id": {id}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if body := get(t, s.Handler(), "/").Body.String(); !strings.Contains(body, "Being worked on") {
		t.Errorf("a started item is not in the being-worked-on section:\n%s", body)
	}

	if rec := post(t, s, "/do/close", url.Values{"id": {id}, "reason": {"done"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("close: %d", rec.Code)
	}
	if body := get(t, s.Handler(), "/").Body.String(); strings.Contains(body, "Walk the transitions") {
		t.Errorf("a closed item is still on the board:\n%s", body)
	}
}

// TestBoardPromotesAParkedItem: parking and promoting is the one decision
// the backlog exists for, so it has to be doable from the page.
func TestBoardPromotesAParkedItem(t *testing.T) {
	s, _ := writableFixture(t)

	post(t, s, "/do/add", url.Values{
		"title": {"Noticed in passing"}, "type": {"note"}, "park": {"1"},
	})
	if body := get(t, s.Handler(), "/").Body.String(); strings.Contains(body, "Noticed in passing") {
		t.Errorf("a parked item went straight onto the board:\n%s", body)
	}

	backlog := get(t, s.Handler(), "/backlog").Body.String()
	if !strings.Contains(backlog, "Noticed in passing") {
		t.Fatalf("the parked item is not in the backlog:\n%s", backlog)
	}

	id := idFromBody(t, backlog)
	if rec := post(t, s, "/do/promote", url.Values{"id": {id}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body.String())
	}
	if body := get(t, s.Handler(), "/").Body.String(); !strings.Contains(body, "Noticed in passing") {
		t.Errorf("a promoted item did not reach the queue:\n%s", body)
	}
}

// TestBoardEditsAnItem covers the update path, including that a field the
// form did not send is left alone rather than blanked.
func TestBoardEditsAnItem(t *testing.T) {
	s, _ := writableFixture(t)

	post(t, s, "/do/add", url.Values{
		"title": {"Before"}, "type": {"task"}, "why": {"the original reason"},
	})
	id := firstIDOnBoard(t, s)

	if rec := post(t, s, "/do/update", url.Values{"id": {id}, "title": {"After"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}

	item := get(t, s.Handler(), "/item/"+id).Body.String()
	if !strings.Contains(item, "After") {
		t.Errorf("the title did not change:\n%s", item)
	}
	if !strings.Contains(item, "the original reason") {
		t.Errorf("a field the form never sent was blanked -- update must touch only what it was given:\n%s", item)
	}
}

func firstIDOnBoard(t *testing.T, s *Server) string {
	t.Helper()
	return idFromBody(t, get(t, s.Handler(), "/").Body.String())
}

func idFromBody(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "/item/rnc-")
	if i < 0 {
		t.Fatalf("no item link in body:\n%s", body)
	}
	rest := body[i+len("/item/"):]
	return rest[:strings.IndexAny(rest, `"`)]
}

// TestRedirectPrefixCannotLeaveTheHost.
//
// If the prefix came straight off the form and were concatenated into
// the redirect, a posted "//evil.example" would make a protocol-relative
// URL, which browsers follow off-host -- an open redirect, reachable by
// anyone who can get a token-bearing form submitted. The prefix is derived
// from the project name, which is checked against the registry, so the only
// value it can ever take is "/p/<a project that exists>".
//
// Two paths, because they fail differently. A hostile prefix alongside a
// project this board knows must be ignored; a hostile project name must be
// refused before it can become a prefix at all.
func TestRedirectPrefixCannotLeaveTheHost(t *testing.T) {
	s, _ := writableFixture(t)

	hostile := []string{"//evil.example", "/\\evil.example", "https://evil.example", "/p/../../evil"}

	t.Run("hostile prefix is ignored", func(t *testing.T) {
		for _, h := range hostile {
			rec := post(t, s, "/do/add", url.Values{
				"title": {"redirect probe"}, "type": {"task"}, "prefix": {h},
			})
			loc := rec.Header().Get("Location")
			if loc == "" {
				t.Fatalf("prefix %q: no redirect at all (%d)", h, rec.Code)
			}
			if !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") {
				t.Errorf("prefix %q produced an off-host redirect: %s", h, loc)
			}
			if strings.Contains(loc, "evil.example") {
				t.Errorf("prefix %q reached the redirect target: %s", h, loc)
			}
		}
	})

	t.Run("hostile project is refused", func(t *testing.T) {
		for _, h := range hostile {
			rec := post(t, s, "/do/add", url.Values{
				"title": {"redirect probe"}, "type": {"task"},
				"project": {h}, "prefix": {h},
			})
			if loc := rec.Header().Get("Location"); strings.Contains(loc, "evil.example") {
				t.Errorf("project %q reached the redirect target: %s", h, loc)
			}
			if rec.Code == http.StatusSeeOther {
				t.Errorf("project %q was accepted; an unregistered project must be refused", h)
			}
		}
	})
}

// TestDropRefusesWithoutAReason.
//
// `keep drop` requires --reason and exits 2 without one. The board must not
// hold a lower standard than the command line for the same operation, on
// the record they both write. The confirm page asks for a reason; this is
// what makes that more than a suggestion.
func TestDropRefusesWithoutAReason(t *testing.T) {
	s, _ := writableFixture(t)
	post(t, s, "/do/add", url.Values{"title": {"a thing to drop"}, "type": {"task"}})
	id := firstIDOnBoard(t, s)

	rec := post(t, s, "/do/drop", url.Values{"id": {id}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "needs+a+reason") {
		t.Errorf("drop with no reason redirected to %q, want a problem saying a reason is needed", loc)
	}

	rec = post(t, s, "/do/drop", url.Values{"id": {id}, "reason": {"superseded"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "done=") {
		t.Errorf("drop with a reason redirected to %q, want success", loc)
	}
}

// TestConfirmPageNamesWhatItWillDestroyAndActsOnNothing.
//
// A confirmation that does not show its subject is one you answer from
// memory of what you clicked. And it is a GET: viewing it must change
// nothing, which is the property that makes it safe to link to.
func TestConfirmPageNamesWhatItWillDestroyAndActsOnNothing(t *testing.T) {
	s, _ := writableFixture(t)
	post(t, s, "/do/add", url.Values{"title": {"the item being confirmed"}, "type": {"task"}})
	id := firstIDOnBoard(t, s)

	req := httptest.NewRequest(http.MethodGet, "/confirm?verb=drop&id="+id, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	// The TITLE is the property here: an id is what you clicked, a title is
	// what you meant. Asserting only the id would let a page that drops the
	// title pass.
	for _, want := range []string{"Drop this item?", "the item being confirmed", id, "Cancel", "name=\"reason\""} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page does not contain %q", want)
		}
	}

	// Viewing it must not have dropped anything.
	after := httptest.NewRequest(http.MethodGet, "/", nil)
	arec := httptest.NewRecorder()
	s.Handler().ServeHTTP(arec, after)
	if !strings.Contains(arec.Body.String(), id) {
		t.Error("the item left the board after merely viewing the confirm page")
	}
}
