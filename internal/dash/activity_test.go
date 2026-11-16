package dash

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/registry"
	"github.com/gshepptech/keep/internal/store"
)

// activityFixture builds a home with two registered projects, each carrying
// its own add-then-close history, and a board started in the first.
//
// It is deliberately not twoProjects: the activity page's whole content is
// the event log, and twoProjects' projects have exactly one event each,
// which cannot tell "this project's history" apart from "the first event of
// every project", nor rendering newest-first apart from rendering one row.
// Every marker below is unique to its project and shares no substring with
// the other's, because the negative assertions are the point: both projects
// are named in the sidebar of both pages, so a marker that collided with a
// project name would pass whatever the handler did.
func activityFixture(t *testing.T) (*Server, map[string]*store.Store) {
	t.Helper()
	// No subprocess is wanted from this page, and without this whether one
	// runs depends on whether the developer has Interlock installed.
	t.Setenv(interlock.EnvBinary, "off")
	home := t.TempDir()
	ctx := context.Background()
	stores := map[string]*store.Store{}

	mk := func(name, title, actor, reason string) string {
		dir := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Join(dir, ".rounce"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		path := filepath.Join(dir, ".rounce", "db.sqlite")
		st, err := store.Init(path)
		if err != nil {
			t.Fatalf("init %s: %v", name, err)
		}
		t.Cleanup(func() { st.Close() })

		it := mustAdd(t, st, store.AddItemInput{Title: title, Actor: actor})
		if _, err := st.CloseItem(ctx, store.CloseItemInput{ID: it.ID, Reason: reason, Actor: actor}); err != nil {
			t.Fatalf("CloseItem %s: %v", name, err)
		}
		if err := registry.Register(home, path, name, "2026-01-01T00:00:00.000000000Z"); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		stores[name] = st
		return path
	}

	here := mk("here", "AlphaMarkerTitle", "alpha-agent", "AlphaCloseReason")
	mk("there", "BetaMarkerTitle", "beta-agent", "BetaCloseReason")

	s, err := NewServer(stores["here"], "127.0.0.1:0", home, Writable(here), ActingAs("board"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, stores
}

// renderedEvent is one row of the activity page as the browser would read
// it: the kind in bold and the item it happened to.
type renderedEvent struct{ kind, itemID string }

// activityRows reads the page back out of the HTML, in render order.
//
// It parses rather than searching for substrings because both events in the
// fixture belong to the same item, so an assertion that looks the item id up
// with strings.Index finds the same first occurrence for every event and
// cannot see a reordered feed at all. That was this test's first shape, and
// it passed against a handler that rendered the log backwards.
func activityRows(t *testing.T, body string) []renderedEvent {
	t.Helper()
	start := strings.Index(body, `<ol class="queue">`)
	if start < 0 {
		return nil
	}
	var out []renderedEvent
	for _, m := range qrow.FindAllStringSubmatch(body[start:], -1) {
		out = append(out, renderedEvent{kind: m[1], itemID: m[2]})
	}
	return out
}

// qrow matches one activity row: the kind, then the id that closes it. The
// two are in this order in activity.html and nothing else on the page pairs
// them, so a row cannot be matched against another row's id.
var qrow = regexp.MustCompile(`(?s)<b class="qkind">(.*?)</b>.*?<span class="id">(.*?)</span>`)

// assertRendersFeedOf checks that body is the activity page for st: the same
// events the store holds, all of them, in the store's own order.
//
// The expectation comes from st.Feed rather than from a literal, because a
// literal would have to be rewritten every time a verb starts writing an
// extra event, and would assert a fixed number rather than the number of
// things that happened.
func assertRendersFeedOf(t *testing.T, body string, st *store.Store, path string) {
	t.Helper()
	entries, err := st.Feed(context.Background(), store.FeedFilter{Limit: 200})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("fixture bug: %s's store holds %d events, want at least the add and the close", path, len(entries))
	}

	// Feed is newest first and the page ranges over it as given, so this one
	// comparison carries three claims at once: every event is shown, nothing
	// else is, and the order is the log's.
	var want, got []string
	for _, e := range entries {
		want = append(want, e.Kind+" "+e.ItemID)
	}
	for _, r := range activityRows(t, body) {
		got = append(got, r.kind+" "+r.itemID)
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("GET %s: rows = [%s], want [%s]", path, strings.Join(got, ", "), strings.Join(want, ", "))
	}

	if want := fmt.Sprintf("%d events", len(entries)); !strings.Contains(body, want) {
		t.Errorf("GET %s: page does not report %q", path, want)
	}
	for _, e := range entries {
		if !strings.Contains(body, e.Actor) {
			t.Errorf("GET %s: the %s event does not name its actor %q", path, e.Kind, e.Actor)
		}
		if e.Reason != "" && !strings.Contains(body, e.Reason) {
			t.Errorf("GET %s: the %s event does not carry its reason %q", path, e.Kind, e.Reason)
		}
	}
}

// TestActivityRendersWhatHappened checks that /activity renders every event
// in the store's own order. It is the page you open after leaving agents
// running, and its entire content is the event log, so this asserts
// against the log itself rather than against a fixture literal.
func TestActivityRendersWhatHappened(t *testing.T) {
	s, stores := activityFixture(t)

	rec := get(t, s.Handler(), "/activity")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /activity: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("GET /activity: Content-Type = %q, want text/html; charset=utf-8", ct)
	}

	body := rec.Body.String()
	assertRendersFeedOf(t, body, stores["here"], "/activity")

	for _, marker := range []string{"BetaMarkerTitle", "beta-agent", "BetaCloseReason"} {
		if at := strings.Index(body, marker); at >= 0 {
			t.Errorf("GET /activity: the default board's history includes %q, which happened in another project: %s", marker, excerpt(body, at))
		}
	}
}

// TestProjectScopedActivityServesThatProjectsHistory checks that
// /p/{project}/activity renders that project's events. /activity reads the
// store the server was started in, which is also the store a broken
// /p/{project}/activity would fall back to, so only a second project with
// its own history can tell them apart.
func TestProjectScopedActivityServesThatProjectsHistory(t *testing.T) {
	s, stores := activityFixture(t)

	rec := get(t, s.Handler(), "/p/there/activity")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /p/there/activity: status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	assertRendersFeedOf(t, body, stores["there"], "/p/there/activity")

	for _, marker := range []string{"AlphaMarkerTitle", "alpha-agent", "AlphaCloseReason"} {
		if at := strings.Index(body, marker); at >= 0 {
			t.Errorf("GET /p/there/activity: shows %q, which happened in the project the board was started in: %s", marker, excerpt(body, at))
		}
	}
	// Every link off the page has to stay inside the project being read, or
	// a row leads to the wrong board's item.
	if !strings.Contains(body, `href="/p/there/item/`) {
		t.Errorf("GET /p/there/activity: rows do not link into /p/there")
	}
}

// TestActivityForAnUnknownProjectIs404 checks that an unknown project gets a
// 404 rather than the board's own history under another project's name,
// which is what a fallback in storeFor would produce.
func TestActivityForAnUnknownProjectIs404(t *testing.T) {
	s, _ := activityFixture(t)

	if rec := get(t, s.Handler(), "/p/nope/activity"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /p/nope/activity: status = %d, want 404", rec.Code)
	}
}

// excerpt is the window around one offset. The page carries its stylesheet
// inline, so printing the whole body on failure buries the one line that
// explains it under 60KB of CSS.
func excerpt(body string, at int) string {
	lo, hi := max(at-120, 0), min(at+120, len(body))
	return "..." + body[lo:hi] + "..."
}
