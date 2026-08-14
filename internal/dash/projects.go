package dash

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gshepptech/rounce/internal/registry"
	"github.com/gshepptech/rounce/internal/store"
)

// A project is one repository with a rounce store in it.
//
// "Repository" was the word everywhere until now, because the registry's own
// field is Repo and that is what a directory with a .git in it is called.
// The board says project instead: what a person is switching between here is
// a body of work, and the git repository is where it happens to live. The
// registry keeps its own vocabulary -- renaming a stored field to match a
// page would be the tail wagging the dog.
type project struct {
	Name string
	Path string // the store file
	Dir  string // the repository directory, which is what an agent runs in

	Ready, Backlog int
	Err            string

	// Active marks the project this request is looking at, so the switcher
	// can show it without the template comparing strings.
	Active bool
}

// projects lists every registered project, plus the one the board was
// started in if it is somehow not registered -- which happens when a store
// was created before the registry existed, or when a registration was lost.
// A board that hid its own project because of a bookkeeping gap would be
// wrong in the most confusing possible way.
func (s *Server) projects(ctx context.Context, active string) []project {
	env, _ := registry.Load(s.home)

	seen := map[string]bool{}
	out := make([]project, 0, len(env.Repos)+1)
	for _, r := range env.Repos {
		seen[r.Path] = true
		out = append(out, s.describe(ctx, r.Name, r.Path, active))
	}
	if s.dbPath != "" && !seen[s.dbPath] {
		out = append(out, s.describe(ctx, registry.NameFor(s.dbPath), s.dbPath, active))
	}
	return out
}

// describe fills one project's counts, read-only and never fatally.
func (s *Server) describe(ctx context.Context, name, path, active string) project {
	p := project{
		Name:   name,
		Path:   path,
		Dir:    repoDirOf(path),
		Active: name == active,
	}
	st, err := store.Open(path, store.OpenOptions{ReadOnly: true})
	if err != nil {
		p.Err = err.Error()
		return p
	}
	defer st.Close()

	if ready, err := st.Ready(ctx, 0); err == nil {
		p.Ready = len(ready)
	}
	if backlog, err := st.Backlog(ctx, ""); err == nil {
		p.Backlog = len(backlog)
	}
	return p
}

// repoDirOf turns a store path into the directory an agent should run in:
// .rounce/db.sqlite sits inside the repository, so the repository is two
// levels up. A path that is not shaped that way is returned as its own
// directory rather than guessed at.
func repoDirOf(storePath string) string {
	dir := filepath.Dir(storePath)
	if filepath.Base(dir) == ".rounce" {
		return filepath.Dir(dir)
	}
	return dir
}

// handleProjects renders the switcher and the add form.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	s.render(w, "projects", s.newPage(r, "projects", s.projects(r.Context(), s.activeName())))
}

// activeName is the project this board was started in.
func (s *Server) activeName() string {
	if s.dbPath == "" {
		return ""
	}
	return registry.NameFor(s.dbPath)
}

// doProjectAdd registers a directory as a project.
//
// It registers rather than creating: `rounce init` is what makes a store,
// and a board that quietly created one would be deciding on a person's
// behalf that a directory should become a project. So the failure here is
// specific -- it says the directory has no store and how to give it one --
// rather than silently succeeding with an empty board.
func (s *Server) doProjectAdd(w http.ResponseWriter, r *http.Request) {
	if err := s.guard(r); err != nil {
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}

	dir := strings.TrimSpace(r.PostFormValue("dir"))
	if dir == "" {
		http.Redirect(w, r, flashURL(r, "/projects", "problem", "Name a directory."), http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(dir, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		http.Redirect(w, r, flashURL(r, "/projects", "problem", err.Error()), http.StatusSeeOther)
		return
	}

	path := store.StorePathIn(abs)
	if _, err := os.Stat(path); err != nil {
		http.Redirect(w, r, flashURL(r, "/projects", "problem",
			fmt.Sprintf("No rounce store in %s. Run `rounce init` there first.", abs)), http.StatusSeeOther)
		return
	}
	if err := registry.Register(s.home, path, registry.NameFor(path), nowStamp()); err != nil {
		http.Redirect(w, r, flashURL(r, "/projects", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, flashURL(r, "/projects", "done", "Project added."), http.StatusSeeOther)
}

// doProjectForget removes a project from the list, never its store.
func (s *Server) doProjectForget(w http.ResponseWriter, r *http.Request) {
	if err := s.guard(r); err != nil {
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}
	if _, err := registry.Forget(s.home, r.PostFormValue("path")); err != nil {
		http.Redirect(w, r, flashURL(r, "/projects", "problem", err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, flashURL(r, "/projects", "done", "Removed from the list. The store itself is untouched."), http.StatusSeeOther)
}
