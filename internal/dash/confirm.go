package dash

import (
	"fmt"
	"net/http"

	"github.com/gshepptech/keep/internal/model"
)

// confirmData is the page that stands between a destructive control and the
// thing it destroys.
type confirmData struct {
	// Verb picks the copy; Post is the route the form submits to. They are
	// separate because "forget" reads better on a page about removing a
	// project than "project-forget" does, and the handler is registered
	// under the longer name.
	Verb    string
	Post    string
	Heading string
	Lede    string
	Warning string
	Button  string
	// AskReason puts a required reason field on the page. The CLI refuses to
	// close or drop without one -- `--reason` is required and its absence is
	// exit 2 -- and the board must not quietly hold a lower standard than the
	// command line for the same operation, on the record that both of them
	// write.
	AskReason bool
	Item      *model.Item
	Session   string
	Path      string
	Name      string
	Return    string
}

// handleConfirm renders the confirmation step for a destructive action.
//
// A GET, and a real page rather than a dialog, because this board ships no
// JavaScript: window.confirm is not available and neither is anything that
// would fake one. That turns out to be the better shape anyway -- a page can
// show WHAT is about to happen and take the reason at the same time, which
// a yes/no box cannot.
//
// Only genuinely lossy actions come through here. Claim, close and promote
// do not: they move an item along its normal path and the event trail says
// who did it. Drop abandons work, and releasing a lease interrupts an agent
// that is mid-edit; neither is undoable from this board.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	st, done, name, err := s.storeFor(r)
	if err != nil {
		s.missOrFail(w, err)
		return
	}
	defer done()

	verb := r.URL.Query().Get("verb")
	d := confirmData{
		Verb:   verb,
		Post:   verb,
		Return: r.URL.Query().Get("return"),
	}

	switch verb {
	case "drop":
		item, err := st.GetItem(r.Context(), r.URL.Query().Get("id"))
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Item = &item.Item
		d.Heading = "Drop this item?"
		d.Lede = "Dropping records that this work will not be done. It stays in the store and in the event trail, but it leaves every queue and there is no undo on this board."
		d.Button = "Drop it"
		d.AskReason = true
	case "unclaim":
		item, err := st.GetItem(r.Context(), r.URL.Query().Get("id"))
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Item = &item.Item
		d.Heading = "Put this back in the queue?"
		d.Lede = "This ends the claim on this item and makes it workable again. It does not say the work was done or that it was abandoned -- only that whoever had it no longer does."
		d.Warning = "If that agent is still running, it will keep working on an item the queue has now offered to someone else. Check that it has actually stopped."
		d.Button = "Put it back"
		d.AskReason = true
	case "release":
		d.Session = r.URL.Query().Get("session")
		d.Path = r.URL.Query().Get("path")
		d.Heading = "Release this hold?"
		d.Lede = fmt.Sprintf("Session %s is holding %s. Releasing it hands the file back immediately, which means an agent that is still working may have its next write land on top of someone else's.", d.Session, orAny(d.Path))
		d.Warning = "Interlock takes these leases when an agent opens a file. If that agent is still running, the safe thing is to wait for the lease to expire on its own."
		d.Button = "Release the hold"
	case "forget":
		d.Path = r.URL.Query().Get("path")
		d.Heading = "Remove " + r.URL.Query().Get("name") + " from the list?"
		d.Lede = "This forgets the project. Its store, its items and its history are left exactly where they are on disk -- keep simply stops listing it, and `keep init` in that directory brings it back."
		d.Button = "Remove it"
		d.Post = "project-forget"
	default:
		s.missOrFail(w, errNoProject)
		return
	}

	s.render(w, "confirm", s.pageFor(r, "confirm", name, d))
}

// orAny names the held path, or says it plainly when the caller did not
// pass one rather than rendering an empty gap in a sentence.
func orAny(path string) string {
	if path == "" {
		return "a file in this repository"
	}
	return path
}
