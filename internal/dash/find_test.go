package dash

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

func ptr(s string) *string { return &s }

// TestASnippetThatRepeatsTheRowIsNotShown checks that a search snippet is
// hidden when it only repeats what the row already shows.
//
// The snippet answers "why is this in my results?" and is only worth the
// line it takes when the answer is somewhere the row cannot already show.
// A match in paths produced:
//
//	Replace the hand-rolled retry loop with a backoff library
//	open · matched in paths · internal/payments/refund.go · rnc-616bf0
//	internal/payments/refund.go
//
// The template special-cased the title alone, which was the same rule
// written for one field out of the two the row displays.
func TestASnippetThatRepeatsTheRowIsNotShown(t *testing.T) {
	cases := []struct {
		name    string
		in      store.SearchResult
		wantOut string
	}{{
		name: "a paths match repeats the paths column",
		in: store.SearchResult{
			Item:      model.Item{Title: "Replace the retry loop", Paths: ptr("internal/payments/refund.go")},
			MatchedIn: "paths", Snippet: "internal/payments/refund.go",
		},
		wantOut: "",
	}, {
		name: "a title match repeats the title",
		in: store.SearchResult{
			Item:      model.Item{Title: "Refunds double-charge"},
			MatchedIn: "title", Snippet: "Refunds double-charge",
		},
		wantOut: "",
	}, {
		// The case a field-name special case cannot catch, and the reason
		// this is containment rather than a list of fields.
		name: "a body that only restates the title",
		in: store.SearchResult{
			Item:      model.Item{Title: "Rate-limit the reset endpoint", Body: "Rate-limit the reset endpoint"},
			MatchedIn: "body", Snippet: "Rate-limit the reset endpoint",
		},
		wantOut: "",
	}, {
		name: "a real reason match survives",
		in: store.SearchResult{
			Item:      model.Item{Title: "Refunds double-charge", Paths: ptr("internal/payments/refund.go")},
			MatchedIn: "why", Snippet: "…the gateway retries on a 5xx and the handler is not idempotent…",
		},
		wantOut: "…the gateway retries on a 5xx and the handler is not idempotent…",
	}, {
		// Ellipses are the cut marks, not content: a snippet that is the
		// path plus two ellipses is still just the path.
		name: "ellipses do not rescue a redundant snippet",
		in: store.SearchResult{
			Item:      model.Item{Title: "Something", Paths: ptr("src/checkout/basket.ts")},
			MatchedIn: "paths", Snippet: "…src/checkout/basket.ts…",
		},
		wantOut: "",
	}, {
		name: "an item with no paths is not confused by the nil",
		in: store.SearchResult{
			Item:      model.Item{Title: "No paths here"},
			MatchedIn: "acceptance", Snippet: "proven by a test that signs out",
		},
		wantOut: "proven by a test that signs out",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := withoutRedundantSnippets([]store.SearchResult{c.in})
			if got[0].Snippet != c.wantOut {
				t.Errorf("Snippet = %q, want %q", got[0].Snippet, c.wantOut)
			}
		})
	}
}

// TestTheRenderedSearchPageDoesNotRepeatThePath drives the real handler,
// because the unit test above proves a function and the bug was visible on
// a page. It catches the handler not calling the filter at all.
//
// What it does NOT pin, checked rather than assumed: removing the
// template's `{{if .Snippet}}` guard leaves this passing, because a blanked
// snippet renders as an empty <span> either way. That guard is cosmetic
// once the handler has done its work, and nothing here should be read as
// holding it up.
func TestTheRenderedSearchPageDoesNotRepeatThePath(t *testing.T) {
	s, _ := writableFixture(t)

	// Seeded through the board's own form rather than a direct store call,
	// because the fixture keeps its store to itself and because filing this
	// way is the path a person actually takes to create the row under test.
	for _, form := range []url.Values{
		{"title": {"Replace the hand-rolled retry loop"}, "type": {"task"},
			"paths": {"internal/payments/refund.go"}},
		// The title deliberately does NOT contain the query. Title outranks
		// why in store.Search, so a title that also said "refund" would
		// make this a title match, its snippet would be the title, and
		// blanking it would be correct -- the test would then be asserting
		// the opposite of what it claims to. First draft got this wrong.
		{"title": {"Double-charging on gateway retry"}, "type": {"bug"},
			"why": {"the refund handler is not idempotent under retry"}},
	} {
		if rec := post(t, s, "/do/add", form); rec.Code != http.StatusSeeOther {
			t.Fatalf("filing %q: status = %d, want 303", form.Get("title"), rec.Code)
		}
	}

	rec := get(t, s.Handler(), "/search?q=refund")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Strip the inlined stylesheet before counting: an earlier test in this
	// package passed by matching its own search string inside <style>.
	if i, j := strings.Index(body, "<style>"), strings.Index(body, "</style>"); i >= 0 && j > i {
		body = body[:i] + body[j:]
	}

	if n := strings.Count(body, "internal/payments/refund.go"); n != 1 {
		t.Errorf("the path appears %d times in the rendered page, want 1 (the paths column)", n)
	}
	// And the snippet that earns its place is still there.
	if !strings.Contains(body, "idempotent") {
		t.Error("the reason match lost its snippet; the rule is too greedy")
	}
}

// TestBeingWorkedOnOffersAllThreeExits checks that an in-progress item offers
// close, drop and unclaim.
//
// Close and Drop both record something about the work (done, or not worth
// doing), and an agent that simply stopped had neither happen to it. With
// only those two, whoever was looking at this board could only rescue the
// item by picking one and writing something false into the event trail the
// rest of the design works to keep honest. Unclaim is the third exit.
//
// Asserted on the rendered page rather than on the handler, because the
// defect was that a control did not exist where a person was looking.
func TestBeingWorkedOnOffersAllThreeExits(t *testing.T) {
	s, _ := writableFixture(t)

	if rec := post(t, s, "/do/add", url.Values{
		"title": {"Expire refresh tokens"}, "type": {"bug"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("filing: status = %d", rec.Code)
	}

	body := get(t, s.Handler(), "/").Body.String()
	id := regexp.MustCompile(`rnc-[0-9a-f]{6}`).FindString(body)
	if id == "" {
		t.Fatal("no item id on the board")
	}
	if rec := post(t, s, "/do/start", url.Values{"id": {id}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("claiming: status = %d", rec.Code)
	}

	page := get(t, s.Handler(), "/").Body.String()
	if i, j := strings.Index(page, "<style>"), strings.Index(page, "</style>"); i >= 0 && j > i {
		page = page[:i] + page[j:]
	}
	if !strings.Contains(page, "Being worked on") {
		t.Fatal("the claimed item is not in the Being worked on panel")
	}
	for _, want := range []string{
		"verb=unclaim&amp;id=" + id,
		"verb=drop&amp;id=" + id,
		"Put back",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the panel does not offer %q", want)
		}
	}
}

// TestPuttingWorkBackNeedsAReason. Same line drop learned to hold: the board
// must not supply a reason on the user's behalf, because a rescue with no
// reason records that a claim vanished and nothing about why.
func TestPuttingWorkBackNeedsAReason(t *testing.T) {
	s, _ := writableFixture(t)
	post(t, s, "/do/add", url.Values{"title": {"A thing"}, "type": {"task"}})
	body := get(t, s.Handler(), "/").Body.String()
	id := regexp.MustCompile(`rnc-[0-9a-f]{6}`).FindString(body)
	post(t, s, "/do/start", url.Values{"id": {id}})

	rec := post(t, s, "/do/unclaim", url.Values{"id": {id}, "reason": {"   "}})
	if rec.Code == http.StatusSeeOther {
		if page := get(t, s.Handler(), "/").Body.String(); !strings.Contains(page, "Being worked on") {
			t.Fatal("a blank reason released the item anyway")
		}
	}

	if rec := post(t, s, "/do/unclaim", url.Values{
		"id": {id}, "reason": {"the agent crashed"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("a real reason was refused: status = %d", rec.Code)
	}
	if page := get(t, s.Handler(), "/").Body.String(); strings.Contains(page, "Being worked on") {
		t.Error("the item is still being worked on after a successful unclaim")
	}
}
