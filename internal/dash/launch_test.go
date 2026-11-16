package dash

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/store"
)

// These tests cover the four board actions that reach Interlock: doRelease,
// doAssign, doLaunch and doPrompt. /do/launch and /do/prompt start a real
// agent process in a repository directory from an HTTP form, which is the
// highest-consequence thing the board does.
//
// They drive the real path down to exec, against a fake interlock on
// KEEP_INTERLOCK, so what the board would actually run is asserted rather
// than assumed.

// fakeInterlock writes a stub binary that records its argv and its working
// directory, and returns the path of the log to read back.
func fakeInterlock(t *testing.T, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a /bin/sh script; Windows is a separate, deferred question")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "interlock")

	// One line per argument, so an argument containing spaces cannot be
	// mistaken for two. pwd is recorded because doLaunch must make sure
	// that the agent starts in the project's directory, not the board's.
	script := "#!/bin/sh\n" +
		"echo \"pwd=$(pwd)\" >> " + log + "\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + log + "; done\n" +
		"echo '--' >> " + log + "\n" +
		"exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}
	t.Setenv(interlock.EnvBinary, bin)
	return log
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return string(rune('0' + i))
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// seedItem puts one workable item on a board, so a handler that acts on an
// item has something to act on.
func seedItem(t *testing.T, s *Server) string {
	t.Helper()
	v, c, e := 9, 8, 2
	item, err := s.st.AddItem(t.Context(), store.AddItemInput{
		Title: "Expire refresh tokens", Actor: "tester",
		Value: &v, Confidence: &c, Effort: &e,
	})
	if err != nil {
		t.Fatalf("seeding an item: %v", err)
	}
	return item.ID
}

// interlockFixture is writableFixture with a stub Interlock instead of "off".
func interlockFixture(t *testing.T, exitCode int) (*Server, string, string) {
	t.Helper()
	log := fakeInterlock(t, exitCode)

	dbPath := filepath.Join(t.TempDir(), "db.sqlite")
	st, err := store.Init(dbPath)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s, err := NewServer(st, "127.0.0.1:0", "", Writable(dbPath), ActingAs("board"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dbPath, log
}

func TestBoardReleasesHolds(t *testing.T) {
	s, _, log := interlockFixture(t, 0)

	rec := post(t, s, "/do/release", url.Values{"session": {"agent-a"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}

	// Interlock's own command, confirmation argument and all. The board does
	// not get to invent a gentler version of another tool's guard rail.
	got := readLog(t, log)
	for _, want := range []string{"lease", "release", "agent-a", "--confirm"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("interlock was not asked for %q; argv was:\n%s", want, got)
		}
	}
	if !strings.Contains(rec.Header().Get("Location"), "done") {
		t.Errorf("redirect %q does not carry a done flash", rec.Header().Get("Location"))
	}
}

// TestBoardReportsAFailedReleaseWithoutBreaking checks that a failing
// Interlock lands the person back on the board with the reason, not on a
// 500. The board is the thing they are watching; losing it because a
// peer tool exited non-zero is the wrong trade.
func TestBoardReportsAFailedReleaseWithoutBreaking(t *testing.T) {
	s, _, _ := interlockFixture(t, 1)

	rec := post(t, s, "/do/release", url.Values{"session": {"agent-a"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303 even on failure: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Location"), "problem") {
		t.Errorf("redirect %q does not carry a problem flash", rec.Header().Get("Location"))
	}
}

func TestBoardAssignsWithoutClaiming(t *testing.T) {
	s, _ := writableFixture(t)
	id := seedItem(t, s)

	rec := post(t, s, "/do/assign", url.Values{"id": {id}, "assignee": {"  greg  "}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}

	item, err := s.st.GetItem(t.Context(), id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Assignee == nil || *item.Assignee != "greg" {
		t.Errorf("assignee = %v, want %q with the spaces trimmed", item.Assignee, "greg")
	}
	// Assignment is a field, not a claim. If it moved the status, a queue
	// divided up in advance would read as entirely in progress.
	if item.Status != "open" {
		t.Errorf("status = %q, want it left open -- assigning is not starting", item.Status)
	}
}

func TestBoardLaunchesAnAgentInTheProjectDirectory(t *testing.T) {
	s, dbPath, log := interlockFixture(t, 0)
	id := seedItem(t, s)

	rec := post(t, s, "/do/launch", url.Values{"agent": {"claude"}, "id": {id}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}

	got := readLog(t, log)
	for _, want := range []string{"run", "claude", "--item", id, "--no-follow"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("interlock was not asked for %q; argv was:\n%s", want, got)
		}
	}
	// The agent runs in the project's own directory, not the board's. An
	// agent started in the wrong repository is worse than one not started.
	if !strings.Contains(got, "pwd="+repoDirOf(dbPath)+"\n") {
		t.Errorf("the agent was not started in %s; log was:\n%s", repoDirOf(dbPath), got)
	}
}

func TestBoardRefusesAnUnknownAgent(t *testing.T) {
	s, _, log := interlockFixture(t, 0)
	id := seedItem(t, s)

	rec := post(t, s, "/do/launch", url.Values{"agent": {"something-else"}, "id": {id}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Location"), "problem") {
		t.Errorf("redirect %q does not carry a problem flash", rec.Header().Get("Location"))
	}
	if readLog(t, log) != "" {
		t.Errorf("an unknown agent still reached the launcher:\n%s", readLog(t, log))
	}
}

// TestBoardPromptKeepsAHostileFirstCharacterOutOfTheFlags checks that a
// prompt starting with a dash goes after the sentinel. Without the sentinel a prompt
// beginning with a dash is parsed as flags, so --env and --model become
// injectable and the prompt itself is discarded. The text comes from a web
// form, so this is the one argument on the board that is not trusted.
func TestBoardPromptKeepsAHostileFirstCharacterOutOfTheFlags(t *testing.T) {
	s, _, log := interlockFixture(t, 0)

	rec := post(t, s, "/do/prompt", url.Values{
		"agent": {"claude"}, "prompt": {"--model=evil do the thing"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}

	got := readLog(t, log)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	sentinel := -1
	for i, l := range lines {
		if l == "--" && i > 0 {
			sentinel = i
			break
		}
	}
	if sentinel < 0 || sentinel+1 >= len(lines) {
		t.Fatalf("no -- sentinel followed by the prompt; it is being passed as flags:\n%s", got)
	}
	if lines[sentinel+1] != "--model=evil do the thing" {
		t.Errorf("the prompt did not arrive whole behind the sentinel; argv was:\n%s", got)
	}
	for _, before := range lines[:sentinel] {
		if strings.HasPrefix(before, "--model") {
			t.Errorf("the prompt's text reached the flag position:\n%s", got)
		}
	}
}

func TestBoardRefusesAnEmptyPrompt(t *testing.T) {
	s, _, log := interlockFixture(t, 0)

	rec := post(t, s, "/do/prompt", url.Values{"agent": {"claude"}, "prompt": {"   "}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Location"), "problem") {
		t.Errorf("redirect %q does not carry a problem flash", rec.Header().Get("Location"))
	}
	if readLog(t, log) != "" {
		t.Errorf("an empty prompt still started an agent:\n%s", readLog(t, log))
	}
}

// TestTheAgentStartingRoutesAreAllGuarded checks that every route that
// starts a process or releases another agent's holds is behind the guard.
// A missing token must stop the request before anything is executed, not
// merely fail to persist afterwards.
func TestTheAgentStartingRoutesAreAllGuarded(t *testing.T) {
	for _, route := range []struct {
		path string
		form url.Values
	}{
		{"/do/release", url.Values{"session": {"agent-a"}}},
		{"/do/assign", url.Values{"id": {"rnc-000000"}, "assignee": {"x"}}},
		{"/do/launch", url.Values{"agent": {"claude"}, "id": {"rnc-000000"}}},
		{"/do/prompt", url.Values{"agent": {"claude"}, "prompt": {"do a thing"}}},
	} {
		t.Run(route.path, func(t *testing.T) {
			s, _, log := interlockFixture(t, 0)

			form := route.form
			form.Set(csrfField, "not-the-token")
			rec := post(t, s, route.path, form)

			if rec.Code == http.StatusSeeOther {
				t.Fatalf("a request with a bad token was accepted (303)")
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status %d, want 403", rec.Code)
			}
			if readLog(t, log) != "" {
				t.Errorf("interlock was reached despite the guard:\n%s", readLog(t, log))
			}
		})
	}
}
