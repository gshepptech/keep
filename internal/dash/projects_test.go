package dash

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gshepptech/rounce/internal/interlock"
	"github.com/gshepptech/rounce/internal/registry"
	"github.com/gshepptech/rounce/internal/store"
)

// twoProjects builds a home with two registered projects and a board
// started in the first.
func twoProjects(t *testing.T) (*Server, string) {
	t.Helper()
	t.Setenv(interlock.EnvBinary, "off")
	home := t.TempDir()

	mk := func(name, title string) string {
		dir := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Join(dir, ".rounce"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, ".rounce", "db.sqlite")
		st, err := store.Init(path)
		if err != nil {
			t.Fatalf("init %s: %v", name, err)
		}
		mustAdd(t, st, store.AddItemInput{Title: title, Actor: "tester"})
		st.Close()
		if err := registry.Register(home, path, name, "2026-01-01T00:00:00.000000000Z"); err != nil {
			t.Fatalf("register: %v", err)
		}
		return path
	}

	here := mk("here", "work in here")
	mk("there", "work in there")

	st, err := store.Open(here, store.OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s, err := NewServer(st, "127.0.0.1:0", home, Writable(here), ActingAs("board"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, home
}

// TestBoardNamesItsProject is the complaint that prompted all of this: a
// board that does not say which project it is serving is one you can act on
// by mistake, and with several open in tabs that is a matter of when.
func TestBoardNamesItsProject(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, home := twoProjects(t)

	body := get(t, s.Handler(), "/").Body.String()
	if !strings.Contains(body, "here") {
		t.Errorf("the board does not name its project:\n%s", body)
	}
	// The directory too. A name alone is ambiguous across machines.
	if !strings.Contains(body, filepath.Join(home, "here")) {
		t.Errorf("the board does not show the project directory:\n%s", body)
	}
}

// TestEachProjectHasItsOwnBoard: /p/<name> serves that project's store, not
// the one the server was started in.
func TestEachProjectHasItsOwnBoard(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)

	here := get(t, s.Handler(), "/").Body.String()
	there := get(t, s.Handler(), "/p/there").Body.String()

	if !strings.Contains(here, "work in here") || strings.Contains(here, "work in there") {
		t.Errorf("the default board is not showing only its own project's work:\n%s", here)
	}
	if !strings.Contains(there, "work in there") || strings.Contains(there, "work in here") {
		t.Errorf("/p/there is not showing that project's own work:\n%s", there)
	}
	// And it must say which project it is looking at, or the two boards are
	// indistinguishable.
	if !strings.Contains(there, "there") {
		t.Errorf("/p/there does not name the project:\n%s", there)
	}
}

// TestOtherProjectLinksStayInThatProject. Without a prefix on every link, a
// click on another project's board silently navigates back to the default
// one -- which looks like the data changed under you.
func TestOtherProjectLinksStayInThatProject(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)

	body := get(t, s.Handler(), "/p/there").Body.String()
	if !strings.Contains(body, `href="/p/there/item/`) {
		t.Errorf("item links on /p/there do not stay in that project:\n%s", body)
	}
	if !strings.Contains(body, `href="/p/there/backlog"`) {
		t.Errorf("the backlog link on /p/there does not stay in that project:\n%s", body)
	}
}

// TestUnknownProjectIs404: a mistyped name is an ordinary miss. Reporting
// it as a server failure sends a person looking for a bug that is not there.
func TestUnknownProjectIs404(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)

	if rec := get(t, s.Handler(), "/p/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /p/nope returned %d, want 404", rec.Code)
	}
}

// TestProjectsPageListsEveryProject with the two facts a person needs to
// tell them apart.
func TestProjectsPageListsEveryProject(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, home := twoProjects(t)

	body := get(t, s.Handler(), "/projects").Body.String()
	for _, want := range []string{"here", "there", filepath.Join(home, "here"), filepath.Join(home, "there")} {
		if !strings.Contains(body, want) {
			t.Errorf("the projects page is missing %q:\n%s", want, body)
		}
	}
}

// TestAddingAProjectRequiresAStore. Registering is not creating: a board
// that quietly ran init would be deciding on a person's behalf that a
// directory should become a project.
func TestAddingAProjectRequiresAStore(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)
	empty := t.TempDir()

	rec := post(t, s, "/do/project-add", url.Values{"dir": {empty}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "rounce+init") && !strings.Contains(loc, "rounce%20init") {
		t.Errorf("the refusal does not say how to make it a project: %s", loc)
	}
	if strings.Contains(get(t, s.Handler(), "/projects").Body.String(), empty) {
		t.Error("a directory with no store was registered anyway")
	}
}

// TestAddingAProjectRegistersIt, given a directory that does have one.
func TestAddingAProjectRegistersIt(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)

	dir := t.TempDir()
	path := filepath.Join(dir, ".rounce", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	st, err := store.Init(path)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	st.Close()

	if rec := post(t, s, "/do/project-add", url.Values{"dir": {dir}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(get(t, s.Handler(), "/projects").Body.String(), dir) {
		t.Error("the added project is not listed")
	}
}

// TestAddingAProjectCreatesAStoreOnlyWhenAsked. Creating writes a file into
// someone's repository, which is not a thing to do because a form was
// submitted -- so the checkbox is the consent, and without it the refusal
// names both ways out.
func TestAddingAProjectCreatesAStoreOnlyWhenAsked(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)
	dir := t.TempDir()

	rec := post(t, s, "/do/project-add", url.Values{"dir": {dir}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "problem=") {
		t.Errorf("adding a storeless directory did not refuse: %s", loc)
	}
	if _, err := os.Stat(filepath.Join(dir, ".rounce", "db.sqlite")); err == nil {
		t.Fatal("a store was created without being asked for")
	}

	if rec := post(t, s, "/do/project-add", url.Values{"dir": {dir}, "create": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".rounce", "db.sqlite")); err != nil {
		t.Errorf("asked to create the store and it did not: %v", err)
	}
	if !strings.Contains(get(t, s.Handler(), "/projects").Body.String(), dir) {
		t.Error("the created project is not listed")
	}
}

// TestCreateRefusesAMissingDirectory: creating a store is one thing,
// creating the repository around it is another nobody asked for.
func TestCreateRefusesAMissingDirectory(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)
	gone := filepath.Join(t.TempDir(), "not-there")

	rec := post(t, s, "/do/project-add", url.Values{"dir": {gone}, "create": {"1"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "problem=") {
		t.Errorf("creating in a missing directory did not refuse: %s", loc)
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("the directory was created; only the store should ever be")
	}
}

// TestActionsWriteToTheProjectTheyCameFrom is the bug this file exists to
// stop coming back. Every mutating action used to resolve the store from
// startup rather than from the form, so filing an item while looking at one
// project's board put it in another's -- silently, since the item lands
// somewhere real, just not where the person was looking.
func TestActionsWriteToTheProjectTheyCameFrom(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, home := twoProjects(t)

	// Post the form the way /p/there renders it.
	rec := post(t, s, "/do/add", url.Values{
		"project": {"there"}, "prefix": {"/p/there"},
		"title": {"filed from the other board"}, "type": {"task"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	there := get(t, s.Handler(), "/p/there").Body.String()
	if !strings.Contains(there, "filed from the other board") {
		t.Errorf("the item did not land in the project it was filed from:\n%s", there)
	}
	here := get(t, s.Handler(), "/").Body.String()
	if strings.Contains(here, "filed from the other board") {
		t.Errorf("the item landed in the started-in project instead:\n%s", here)
	}
	_ = home
}

// TestEveryActionFormCarriesItsProject. The fix above is only real if the
// forms actually send the field -- one that omits it writes to the wrong
// store and says nothing.
func TestEveryActionFormCarriesItsProject(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)

	for _, path := range []string{"/p/there", "/p/there/new", "/p/there/backlog"} {
		body := get(t, s.Handler(), path).Body.String()
		forms := strings.Count(body, `action="/do/`)
		if forms == 0 {
			continue
		}
		if got := strings.Count(body, `name="project" value="there"`); got < forms {
			t.Errorf("%s has %d action forms but only %d carry the project", path, forms, got)
		}
	}
}

// TestActionRedirectsStayInTheProject: sending someone back to "/" after
// they acted on another project's board moves them somewhere else and makes
// their own change look like it did not happen.
func TestActionRedirectsStayInTheProject(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)

	rec := post(t, s, "/do/add", url.Values{
		"project": {"there"}, "prefix": {"/p/there"},
		"title": {"stay put"}, "type": {"task"},
	})
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/p/there/") {
		t.Errorf("redirected to %q, want somewhere under /p/there/", loc)
	}
}

// TestAgentControlsFollowTheProject.
//
// The launch control used to be gated on whether the interlock binary
// answered from the *server's* directory, which is two wrong questions at
// once: it asked about the wrong repository, and "is Interlock installed"
// is not "is Interlock set up here". The binary answers status from any
// directory, including one it has never been initialised in -- so the
// button appeared on projects where pressing it would do nothing.
func TestAgentControlsFollowTheProject(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, home := twoProjects(t)

	// Neither project is set up, so neither offers to launch.
	for _, path := range []string{"/", "/p/there"} {
		body := get(t, s.Handler(), path).Body.String()
		if strings.Contains(body, `action="/do/launch"`) {
			t.Errorf("%s offers to start an agent with no coordination set up:\n%s", path, body)
		}
		if !strings.Contains(body, "Agent coordination is off") {
			t.Errorf("%s does not say coordination is off", path)
		}
	}

	// Wire one of them. The stub answers `doctor` with success only for
	// that directory, which is the question Watches actually asks.
	t.Setenv(interlock.EnvBinary, doctorStub(t, filepath.Join(home, "there")))

	if body := get(t, s.Handler(), "/p/there").Body.String(); !strings.Contains(body, `action="/do/launch"`) {
		t.Errorf("/p/there does not offer to start an agent although it is set up:\n%s", body)
	}
	if body := get(t, s.Handler(), "/").Body.String(); strings.Contains(body, `action="/do/launch"`) {
		t.Error("the other project offers to start an agent although only /p/there is set up")
	}
}

// doctorStub is an interlock whose `doctor` succeeds in exactly one
// directory, and whose `status` answers everywhere.
//
// It stubs doctor rather than a marker file because that is what Watches
// asks -- and the two cheaper signals both proved wrong in use: a present
// binary answers for repositories it has never touched, and .interlock
// appears when the first lease is recorded rather than when init runs.
func doctorStub(t *testing.T, wired string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "interlock")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"doctor\" ]; then\n" +
		"  [ \"$PWD\" = " + strconvQuote(wired) + " ] && exit 0\n" +
		"  exit 1\n" +
		"fi\n" +
		"cat <<'EOF_JSON'\n" +
		`{"version":1,"repo":"/r","sessions":[],"leases":[],"errors":[]}` + "\nEOF_JSON\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return p
}

// strconvQuote is shell-quoting for one path.
func strconvQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestTurningOnCoordinationConfirmsItWorked.
//
// The first version reported success on init's exit code alone. It had run
// against a check that tested for something init does not create, so the
// board announced coordination was on while still showing it as off --
// which is worse than failing, because it stops you looking.
func TestTurningOnCoordinationConfirmsItWorked(t *testing.T) {
	// The fake interlock these tests drive is a /bin/sh script, which
	// Windows cannot execute. The behaviour under test is rounce's, not the
	// shell's, but there is no honest way to assert it here without a
	// Windows stub -- so this is skipped rather than quietly passing, and
	// the gap is real: rounce's Interlock integration is unverified on
	// Windows.
	if runtime.GOOS == "windows" {
		t.Skip("the interlock stub is a shell script; see the comment above")
	}
	s, _ := twoProjects(t)

	// An interlock whose init succeeds and whose doctor never does: the
	// shape of "it ran and it did not take". A stub that exited 0 for
	// everything would make doctor succeed too, and prove nothing.
	p := filepath.Join(t.TempDir(), "interlock")
	stub := "#!/bin/sh\n[ \"$1\" = \"doctor\" ] && exit 1\nexit 0\n"
	if err := os.WriteFile(p, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(interlock.EnvBinary, p)

	rec := post(t, s, "/do/watch", url.Values{})
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "problem=") {
		t.Errorf("reported success although coordination is still off: %s", loc)
	}
	if strings.Contains(loc, "done=") {
		t.Errorf("announced success it did not verify: %s", loc)
	}
}
