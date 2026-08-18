package dash

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"time"

	"github.com/gshepptech/keep/internal/interlock"
	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// The board began read-only, deliberately: docs/design/phase-4.md's first
// non-goal was "Editing from the page. Read-only in this phase; the store is
// the CLI's job", and openStoreReadOnly made that true by construction
// rather than by review.
//
// That decision is reversed here, on request, and the reversal is worth
// recording rather than quietly performing. What made read-only defensible
// was that every mutation already had a tested path through the CLI and the
// MCP tools, so the page had nothing to add. What makes it wrong now is that
// a person watching a queue has to leave the page to act on it, and the
// leaving is where the work stops.
//
// Everything the original decision was protecting is kept:
//
//   - Mutations are POST only. A GET can never change the store, so no link,
//     prefetch, or reload can either.
//   - Every form carries a per-server token. The server is loopback-only, but
//     "loopback" includes every other program on this machine, and a page in
//     the user's browser can be made to POST here by any site they visit.
//   - Origin and Referer, when present, must be this server. Together with
//     the token that is two independent checks, because a browser that sends
//     neither header is a browser the token still covers.
//   - The store is still opened read-only for reads; only the mutating path
//     opens for writing.

// csrfField is the hidden input name every mutating form carries.
const csrfField = "csrf"

// newToken returns a fresh per-server CSRF token. Per server, not per
// session: this page has no accounts, and a token that survived a restart
// would have to be stored somewhere, which is a worse property than asking a
// person to reload a page after restarting their own dashboard.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate csrf token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// checkOrigin refuses a request whose Origin or Referer names a different
// host. A request carrying neither is allowed through to the token check --
// some clients omit both, and refusing them here would break `curl` against
// a person's own dashboard while stopping no attack the token does not.
func (s *Server) checkOrigin(r *http.Request) error {
	for _, header := range []string{"Origin", "Referer"} {
		raw := r.Header.Get(header)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("%s is not a URL", header)
		}
		if u.Host != r.Host {
			return fmt.Errorf("%s names %q, not this server", header, u.Host)
		}
	}
	return nil
}

// mutate wraps every POST handler with the checks above, then redirects.
//
// It redirects rather than rendering, so a reload after acting does not
// repeat the action -- the POST-redirect-GET shape, which matters more here
// than usual because the actions are things like "close this item".
// prefixOf is the URL prefix a request's own project lives under, derived
// rather than trusted -- see the note in flashURL.
func (s *Server) prefixOf(r *http.Request) string {
	name := r.PostFormValue("project")
	if name == "" || name == s.activeName() {
		return ""
	}
	if _, ok := s.pathOf(name); !ok {
		return ""
	}
	return "/p/" + name
}

// writablePathFor is the store a mutating request should write to: the
// project the form was posted from, falling back to the one the board was
// started in.
//
// Every mutating form carries a hidden project field for this. A form that
// omitted it would write to the wrong store and say nothing, so the
// fallback is the started-in project rather than an error -- but the forms
// all carry it, and the tests check that they do.
func (s *Server) writablePathFor(r *http.Request) (string, error) {
	name := r.PostFormValue("project")
	if name == "" || name == s.activeName() {
		return s.dbPath, nil
	}
	path, ok := s.pathOf(name)
	if !ok {
		return "", fmt.Errorf("no project named %q is registered", name)
	}
	return path, nil
}

// guard runs the checks every mutating request must pass, so no handler
// can be written that forgets one.
func (s *Server) guard(r *http.Request) error {
	if err := s.checkOrigin(r); err != nil {
		return err
	}
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("unreadable form")
	}
	if r.PostFormValue(csrfField) != s.csrf {
		return fmt.Errorf("this form is from a different server or an older one -- reload the page")
	}
	return nil
}

// mutate runs one write and redirects. A `done` of "" means silent success.
//
// Silence is the default for anything whose effect is on the page you land
// on: filing an item redirects to that item, saving redirects to the saved
// fields, claiming moves a row into "Being worked on" a few inches down.
// Announcing those is telling someone what they are already looking at.
//
// A banner is kept for the two cases where the page cannot show it: work
// that LEFT the view (closed, dropped, promoted, released -- you cannot
// see the absence of a thing you were not looking for), and work that
// happened somewhere else entirely (an agent starting in another process).
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, done string, fn func(*store.Store) (string, error)) {
	if err := s.guard(r); err != nil {
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}

	// The project comes from the form, not from startup. Without this a
	// person looking at one project's board files into another's store --
	// which is exactly what happened, and it is silent: the item lands
	// somewhere real, just not where they were looking.
	path, err := s.writablePathFor(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	st, err := s.writableAt(path)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer st.Close()

	dest, err := fn(st)
	if err != nil {
		// The error goes back as a flash on the page the form came from,
		// never as a bare error page: a person mid-action needs to see what
		// happened next to the thing they were acting on.
		http.Redirect(w, r, s.flashURL(r, "", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	// And a success says so. An action that reports nothing looks like an
	// action that did nothing -- particularly here, where closing an item
	// makes it vanish from the list you were looking at, which is
	// indistinguishable from the click having missed.
	http.Redirect(w, r, s.flashURL(r, dest, "done", done), http.StatusSeeOther)
}

// flashURL builds the destination, carrying one message in the query
// string under key. A query parameter rather than a cookie: there is no
// session here, and a cookie would outlive the one page it is meant for.
func (s *Server) flashURL(r *http.Request, dest, key, msg string) string {
	if dest == "" {
		dest = r.PostFormValue("return")
	}
	if dest == "" {
		dest = "/"
	}
	// Stay in the project the action came from. Redirecting to "/" after
	// acting on another project's board silently moves a person somewhere
	// else and makes their change look like it did not happen.
	//
	// The prefix is DERIVED from the project name, never taken from the
	// form. The form does carry one, and trusting it was an open redirect:
	// a posted prefix of "//evil.example" concatenates into a
	// protocol-relative URL that browsers follow off-host. The project name
	// is checked against the registry before it becomes a prefix, so only a
	// project that exists can produce one, and it can only ever be
	// "/p/<name>".
	if prefix := s.prefixOf(r); prefix != "" && strings.HasPrefix(dest, "/") && !strings.HasPrefix(dest, prefix) {
		dest = prefix + dest
	}
	if msg == "" {
		return dest
	}
	sep := "?"
	if strings.Contains(dest, "?") {
		sep = "&"
	}
	return dest + sep + key + "=" + url.QueryEscape(msg)
}

// routesMutating registers the POST handlers. Each one is a verb a person
// recognises, and the URL says the same word the button does.
func (s *Server) routesMutating(mux *http.ServeMux) {
	mux.HandleFunc("POST /do/add", s.doAdd)
	mux.HandleFunc("POST /do/update", s.doUpdate)
	mux.HandleFunc("POST /do/start", s.doStart)
	mux.HandleFunc("POST /do/close", s.doClose)
	mux.HandleFunc("POST /do/drop", s.doDrop)
	mux.HandleFunc("POST /do/unclaim", s.doUnclaim)
	mux.HandleFunc("POST /do/promote", s.doPromote)
	mux.HandleFunc("POST /do/release", s.doRelease)
	mux.HandleFunc("POST /do/assign", s.doAssign)
	mux.HandleFunc("POST /do/launch", s.doLaunch)
	mux.HandleFunc("POST /do/project-add", s.doProjectAdd)
	mux.HandleFunc("POST /do/project-forget", s.doProjectForget)
	mux.HandleFunc("POST /do/watch", s.doWatch)
	mux.HandleFunc("POST /do/prompt", s.doPrompt)
}

func (s *Server) doStart(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, "", func(st *store.Store) (string, error) {
		id := r.PostFormValue("id")
		_, err := st.StartItem(r.Context(), store.StartItemInput{ID: id, Actor: s.actor})
		return "", err
	})
}

func (s *Server) doClose(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, "Closed.", func(st *store.Store) (string, error) {
		_, err := st.CloseItem(r.Context(), store.CloseItemInput{
			ID:     r.PostFormValue("id"),
			Reason: reasonOr(r.PostFormValue("reason"), "closed from the board"),
			Actor:  s.actor,
		})
		return "", err
	})
}

// doUnclaim returns a claimed item to the queue. It is on this board for the
// same reason it is on the CLI: the panel it appears in offered only Close
// and Drop, which are the two exits that record something false about an
// agent that simply stopped.
//
// Reason is required and never invented, exactly as doDrop learned to do.
func (s *Server) doUnclaim(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, "Back in the queue.", func(st *store.Store) (string, error) {
		reason := strings.TrimSpace(r.PostFormValue("reason"))
		if reason == "" {
			return "", fmt.Errorf("putting work back needs a reason -- say why the claim ended")
		}
		_, err := st.UnclaimItem(r.Context(), store.UnclaimItemInput{
			ID:     r.PostFormValue("id"),
			Reason: reason,
			Actor:  s.actor,
		})
		return "", err
	})
}

// doDrop takes the reason the confirm page collected and does NOT invent
// one. `keep drop` refuses without --reason and exits 2; the board used to
// route around that by supplying "dropped from the board" on the user's
// behalf, which is the board holding a lower standard than the command line
// for the same operation, on the record they both write.
func (s *Server) doDrop(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, "Dropped.", func(st *store.Store) (string, error) {
		reason := strings.TrimSpace(r.PostFormValue("reason"))
		if reason == "" {
			return "", fmt.Errorf("dropping needs a reason -- say why it will not be done")
		}
		_, err := st.DropItem(r.Context(), store.DropItemInput{
			ID:     r.PostFormValue("id"),
			Reason: reason,
			Actor:  s.actor,
		})
		return "", err
	})
}

// reasonOr supplies a default reason. close and drop both require one,
// deliberately -- the store will not record work as finished or abandoned
// without saying why -- and a board that refused the action for want of a
// sentence would just teach people to type "done".
func reasonOr(given, fallback string) string {
	if strings.TrimSpace(given) == "" {
		return fallback
	}
	return given
}

func (s *Server) doAdd(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, "", func(st *store.Store) (string, error) {
		in := store.AddItemInput{
			Title:      strings.TrimSpace(r.PostFormValue("title")),
			Type:       r.PostFormValue("type"),
			Why:        r.PostFormValue("why"),
			Body:       r.PostFormValue("body"),
			Acceptance: r.PostFormValue("acceptance"),
			Paths:      r.PostFormValue("paths"),
			ParentID:   r.PostFormValue("parent_id"),
			Actor:      s.actor,
		}
		// "park it" files a raw note instead of a ticket -- the same
		// distinction `capture` makes on the CLI, offered as a checkbox
		// because it is the one choice a person actually makes at filing
		// time: is this workable, or is it something I noticed.
		if r.PostFormValue("park") != "" {
			in.Stage = model.StageRaw
		}
		v, c, e := scoreFields(r)
		in.Value, in.Confidence, in.Effort = v, c, e

		item, err := st.AddItem(r.Context(), in)
		if err != nil {
			return "", err
		}
		return "/item/" + item.ID, nil
	})
}

func (s *Server) doUpdate(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, "", func(st *store.Store) (string, error) {
		id := r.PostFormValue("id")
		in := store.UpdateItemInput{ID: id, Actor: s.actor}

		// Only fields the form actually submitted are changed. A form that
		// posted every field would silently blank whatever it did not
		// render, which is how an edit page quietly destroys a body.
		for _, f := range []struct {
			name string
			dest **string
		}{
			{"title", &in.Title}, {"why", &in.Why}, {"body", &in.Body},
			{"acceptance", &in.Acceptance}, {"paths", &in.Paths},
			{"type", &in.Type}, {"assignee", &in.Assignee}, {"repo", &in.Repo},
		} {
			if r.PostForm.Has(f.name) {
				v := r.PostFormValue(f.name)
				*f.dest = &v
			}
		}
		if v, c, e := scoreFields(r); v != nil {
			in.Value, in.Confidence, in.Effort = &v, &c, &e
		}
		_, err := st.UpdateItem(r.Context(), in)
		return "/item/" + id, err
	})
}

// doPromote moves a raw item into the queue. It is its own action rather
// than a stage field on the edit form because it is a decision, not an
// attribute: a backlog item becomes workable when someone has refined it
// into something whose title stands alone.
func (s *Server) doPromote(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, "Promoted to the queue.", func(st *store.Store) (string, error) {
		id := r.PostFormValue("id")
		stage := string(model.StageTicket)
		_, err := st.UpdateItem(r.Context(), store.UpdateItemInput{
			ID: id, Actor: s.actor, Stage: &stage,
		})
		return "/item/" + id, err
	})
}

// scoreFields reads value/confidence/effort. All three or none: a partial
// score is not a score, and keep treats an unscored item as 0 that sorts
// last rather than as something to guess at.
func scoreFields(r *http.Request) (*int, *int, *int) {
	v, okV := intField(r, "value")
	c, okC := intField(r, "confidence")
	e, okE := intField(r, "effort")
	if !okV || !okC || !okE {
		return nil, nil, nil
	}
	return v, c, e
}

func intField(r *http.Request, name string) (*int, bool) {
	raw := strings.TrimSpace(r.PostFormValue(name))
	if raw == "" {
		return nil, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, false
	}
	return &n, true
}

// doRelease releases one session's file holds, through Interlock.
//
// It shells out rather than reaching into Interlock's state, for the same
// reason internal/interlock reads status over a CLI contract: keep must
// keep building and working with Interlock absent, and a Go import would
// end that. The command is Interlock's own documented one, confirmation
// argument and all -- keep does not get to invent a gentler version of
// another tool's guard rail.
func (s *Server) doRelease(w http.ResponseWriter, r *http.Request) {
	if err := s.guard(r); err != nil {
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}

	session := r.PostFormValue("session")
	if err := interlockRelease(r.Context(), session); err != nil {
		http.Redirect(w, r, s.flashURL(r, "/", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.flashURL(r, "/", "done", "Holds released."), http.StatusSeeOther)
}

// doAssign records who is meant to do a piece of work.
//
// Assignment is a field on the item, not a claim: `start` is what says
// someone has picked it up. Keeping them separate means a queue can be
// divided up in advance without every assigned item reading as in
// progress.
func (s *Server) doAssign(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, "", func(st *store.Store) (string, error) {
		who := strings.TrimSpace(r.PostFormValue("assignee"))
		_, err := st.UpdateItem(r.Context(), store.UpdateItemInput{
			ID: r.PostFormValue("id"), Actor: s.actor, Assignee: &who,
		})
		return "", err
	})
}

// doLaunch starts an agent on one item, through Interlock.
//
// keep does not start agents. That is stated in its own README as a
// deliberate limit -- it holds the list and never calls a model -- and this
// does not change it: the board shells out to `interlock run`, which is the
// tool whose job is process lifecycle. If Interlock is not installed the
// button is not rendered, so this path is reachable only where something
// exists to do the starting.
//
// --no-follow because this is a web request: the run is supervised by
// Interlock and outlives the POST, and a handler that streamed an agent's
// output would be holding a connection open for as long as the work takes.
func (s *Server) doLaunch(w http.ResponseWriter, r *http.Request) {
	if err := s.guard(r); err != nil {
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}
	agent := r.PostFormValue("agent")
	id := r.PostFormValue("id")
	// The agent runs in the project's own directory, not the board's. An
	// agent started in the wrong repository is worse than one not started.
	path, err := s.writablePathFor(r)
	if err != nil {
		http.Redirect(w, r, s.flashURL(r, "/", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	if err := interlock.Run(r.Context(), agent, id, repoDirOf(path)); err != nil {
		http.Redirect(w, r, s.flashURL(r, "/", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.flashURL(r, "/", "done",
		"Started "+agent+" on "+id+". It appears under Agents below once it starts."), http.StatusSeeOther)
}

// nowStamp is the registry's own timestamp format.
func nowStamp() string { return time.Now().UTC().Format(model.TimeFormat) }

// doWatch sets Interlock up for one project, on request.
//
// Not silent, and not at startup. `interlock init` writes hook
// registrations into each agent's own settings and trust hashes into
// ~/.codex/config.toml -- configuration that makes agents run commands on
// this machine. A board may offer that in one click; it may not take it.
func (s *Server) doWatch(w http.ResponseWriter, r *http.Request) {
	if err := s.guard(r); err != nil {
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}
	path, err := s.writablePathFor(r)
	if err != nil {
		http.Redirect(w, r, s.flashURL(r, "/", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	dir := repoDirOf(path)
	if err := interlock.Init(r.Context(), dir); err != nil {
		http.Redirect(w, r, s.flashURL(r, "/", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	// Confirm rather than assume. The first version of this reported
	// success on an exit code alone and was wrong -- it had run init
	// against a check that tested for something init does not create, so
	// the board said coordination was on while still showing it as off.
	if !interlock.Watches(dir) {
		http.Redirect(w, r, s.flashURL(r, "/", "problem",
			"Ran interlock init, but coordination still is not active here. Run `interlock doctor` in "+dir+" to see what it is missing."),
			http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.flashURL(r, "/", "done",
		"Agent coordination is on for this project. Hooks are wired into your agents' own settings."), http.StatusSeeOther)
}

// doPrompt starts an agent on a free-text instruction.
//
// The queue is still the normal way in: an item carries a brief, acceptance
// criteria and the files it touches, and an agent started from one begins
// with all of that. This is for the thing you want done now and would not
// have written a ticket for -- and it goes through the same launcher, so
// Interlock supervises it and its guard still applies.
func (s *Server) doPrompt(w http.ResponseWriter, r *http.Request) {
	if err := s.guard(r); err != nil {
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}
	path, err := s.writablePathFor(r)
	if err != nil {
		http.Redirect(w, r, s.flashURL(r, "/", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	agent := r.PostFormValue("agent")
	text := strings.TrimSpace(r.PostFormValue("prompt"))
	if err := interlock.Prompt(r.Context(), agent, text, repoDirOf(path)); err != nil {
		http.Redirect(w, r, s.flashURL(r, "/", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.flashURL(r, "/", "done",
		"Started "+agent+". It appears under Agents below once it starts."), http.StatusSeeOther)
}
