package dash

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gshepptech/rounce/internal/registry"
	"github.com/gshepptech/rounce/internal/store"
)

// crossRepoFixture builds a registry home with two real, separate stores
// and returns a Server pointed at it. Two stores rather than one because
// the claim under test is a merge ACROSS them: a single store would rank
// correctly by accident, since store.Ready already does that.
//
// The scores are chosen so the correct cross-repo order interleaves the two
// repositories. A merge that concatenated per-repository lists instead of
// ranking across them would still put every item on the page, and would
// still look plausible -- interleaving is what makes it fail.
func crossRepoFixture(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()

	type ticket struct {
		title   string
		v, c, e int
	}

	mk := func(name string, tickets ...ticket) string {
		dir := filepath.Join(home, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		path := filepath.Join(dir, "db.sqlite")
		st, err := store.Init(path)
		if err != nil {
			t.Fatalf("init %s: %v", path, err)
		}
		for _, tk := range tickets {
			v, c, e := tk.v, tk.c, tk.e
			mustAdd(t, st, store.AddItemInput{
				Title: tk.title, Actor: "tester",
				Value: &v, Confidence: &c, Effort: &e,
			})
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close %s: %v", path, err)
		}
		if err := registry.Register(home, path, name, "2026-01-01T00:00:00.000000000Z"); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		return path
	}

	mk("alpha", ticket{"alpha high", 10, 10, 0}, ticket{"alpha low", 1, 1, 9})
	mk("beta", ticket{"beta middle", 6, 6, 3})

	s, err := NewServer(openTestStore(t), "127.0.0.1:0", home)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, home
}

// TestAllRanksAcrossRepositories is REPO-03 on the board: one queue over
// every registered store, ranked across all of them rather than grouped by
// repository.
func TestAllRanksAcrossRepositories(t *testing.T) {
	s, _ := crossRepoFixture(t)

	rec := get(t, s.Handler(), "/all")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /all: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Scores: alpha high = 31, beta middle = 20, alpha low = 4. Correct
	// cross-repo order interleaves the repositories.
	want := []string{"alpha high", "beta middle", "alpha low"}
	var at []int
	for _, w := range want {
		i := strings.Index(body, w)
		if i < 0 {
			t.Fatalf("%q missing from /all:\n%s", w, body)
		}
		at = append(at, i)
	}
	if !(at[0] < at[1] && at[1] < at[2]) {
		t.Errorf("rows are not in cross-repo score order %v -- offsets %v.\nA per-repository concatenation would also contain every row, which is why the order is the assertion.", want, at)
	}

	// The repository must be named per row, or a merged queue is unusable:
	// two stores can both hold an item whose id looks the same to a reader.
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(body, name) {
			t.Errorf("/all does not name repository %q:\n%s", name, body)
		}
	}
}

// TestAllReportsUnreadableRepositoryAsARowNotAPage is REPO-04/REPO-05 on
// the board. A store that has been deleted must cost you one row and not
// the page -- and must never be fabricated as an empty repository, which
// would read as "nothing to do here" rather than "this is broken".
func TestAllReportsUnreadableRepositoryAsARowNotAPage(t *testing.T) {
	s, home := crossRepoFixture(t)

	// Delete beta's store out from under the registry, leaving the
	// registration behind -- exactly the state REPO-05 describes.
	if err := os.Remove(filepath.Join(home, "beta", "db.sqlite")); err != nil {
		t.Fatalf("remove beta store: %v", err)
	}

	rec := get(t, s.Handler(), "/all")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /all with one unreadable store: status = %d, want 200 -- one broken repository must not cost the page", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "alpha high") {
		t.Errorf("alpha's rows are missing after beta broke:\n%s", body)
	}
	if !strings.Contains(body, "unreadable") {
		t.Errorf("/all does not report beta as unreadable:\n%s", body)
	}
	if strings.Contains(body, "beta middle") {
		t.Errorf("/all still shows a row from the deleted store:\n%s", body)
	}

	// The count is over registrations, not over readable stores: a heading
	// that quietly shrank would hide the failure it is meant to surface.
	if !strings.Contains(body, "2 repositories") {
		t.Errorf("/all does not still count both registered repositories:\n%s", body)
	}

	// REPO-05's own rule, restated at this boundary: reading a missing
	// store must not create one.
	if _, err := os.Stat(filepath.Join(home, "beta", "db.sqlite")); !os.IsNotExist(err) {
		t.Errorf("rendering /all re-created the deleted store (stat err = %v)", err)
	}
}

// TestAllWithNoRegistryIsAnEmptyPageNotAnError: a Server built without a
// registry home still answers /all, because the nav link is unconditional.
// A 404 on a link the page itself renders is worse than an empty table.
func TestAllWithNoRegistryIsAnEmptyPageNotAnError(t *testing.T) {
	s, _ := newHandlerFixture(t) // built with home ""

	rec := get(t, s.Handler(), "/all")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /all with no registry: status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "nothing is workable") {
		t.Errorf("/all with no registry does not render its empty state:\n%s", body)
	}
}

// TestAllNavLinkIsOnEveryView: the cross-repo queue is only reachable if
// every page offers it. Asserting it on each view rather than on the layout
// template is what catches a view that stops going through the shared head
// block.
func TestAllNavLinkIsOnEveryView(t *testing.T) {
	s, ids := newHandlerFixture(t)

	for _, path := range []string{"/", "/backlog", "/item/" + ids.ready, "/tree/" + ids.epic} {
		rec := get(t, s.Handler(), path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `href="/all"`) {
			t.Errorf("%s does not link to the cross-repo queue", path)
		}
	}
}

// TestNoExternalReferencesInCrossRepoView extends DASH-05's exit-gate proof
// to /all, which the four-view scan in offhost_test.go predates.
//
// This view renders two things none of the other four do, and both are
// attacker-reachable in their own way:
//
//   - a title from a store this repository does not own. An agent working
//     in one repository can file an item that renders on a board someone
//     opens in another.
//   - an error string built from a filesystem path, when a registered store
//     cannot be read. That text comes from os and from internal/registry
//     rather than from an item, so a scan that only ever considered item
//     fields would not have been looking at it.
//
// Both cases are forced here: alpha's title carries hostile text, and beta
// is deleted so its error row renders in the same response.
func TestNoExternalReferencesInCrossRepoView(t *testing.T) {
	home := t.TempDir()

	mk := func(name, title string) string {
		dir := filepath.Join(home, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, "db.sqlite")
		st, err := store.Init(path)
		if err != nil {
			t.Fatalf("init: %v", err)
		}
		mustAdd(t, st, store.AddItemInput{Title: title, Actor: "tester"})
		if err := st.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := registry.Register(home, path, name, "2026-01-01T00:00:00.000000000Z"); err != nil {
			t.Fatalf("register: %v", err)
		}
		return path
	}

	// Two shapes of hostile title, because they fail differently.
	//
	// A sentence containing a URL is what an agent actually writes, and it
	// is the weaker probe: dropped into a URL attribute, html/template
	// percent-encodes the spaces, so "queue http://evil..." becomes the
	// relative path "queue%20http://evil..." and is correctly not off-host.
	// A test built only from that shape passes even if a title is rendered
	// into src= -- verified by breaking it exactly that way.
	//
	// The bare URL is the probe with teeth: nothing about it stops
	// resolving off-host in any attribute context.
	mk("alpha", "queue "+hostileHTTP+" and "+hostileProtoRelative)
	mk("bare", hostileHTTP)
	betaPath := mk("beta", "beta item")

	// Break beta so the error table renders in the same response as the
	// hostile title -- one scan over both.
	if err := os.Remove(betaPath); err != nil {
		t.Fatalf("remove beta store: %v", err)
	}

	s, err := NewServer(openTestStore(t), "127.0.0.1:0", home)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	rec := get(t, s.Handler(), "/all")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /all: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Confirm the hostile text is actually present, escaped and inert,
	// BEFORE trusting a clean scan -- a view that dropped the title would
	// pass vacuously, which is the trap offhost_test.go names.
	for _, want := range []string{hostileHTTP, hostileProtoRelative} {
		if !strings.Contains(body, want) {
			t.Errorf("hostile text %q not present in /all -- scan would pass vacuously:\n%s", want, body)
		}
	}
	if strings.Count(body, hostileHTTP) < 2 {
		t.Errorf("the bare-URL title is not rendered, so the probe with teeth is vacuous:\n%s", body)
	}
	if !strings.Contains(body, "unreadable") {
		t.Fatal("/all did not render the error table, so the error-string half of this scan is vacuous")
	}

	if v := scanRenderedHTML(body); len(v) != 0 {
		t.Errorf("off-host references in /all:\n%s", strings.Join(v, "\n"))
	}
}
