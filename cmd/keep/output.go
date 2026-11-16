package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gshepptech/keep/internal/model"
)

// defaultTitleWidth is the title-truncation budget used whenever stdout is
// not a character device: titles truncate to the terminal width, or to 100
// columns when not a TTY. This is also what makes every CLI test's captured
// stdout deterministic: a test's stdout is always a pipe, never a real
// terminal, so titleWidth always returns this constant under `go test`.
const defaultTitleWidth = 100

// minTitleBudget is the floor renderItems clamps the title column to once
// the id/status/score/type columns (and their separating spaces) are
// subtracted from the total width. A very narrow terminal combined with
// very wide ids should not be allowed to truncate a title to nothing.
const minTitleBudget = 10

// titleWidth returns the total line-width budget renderItems has to work
// with: $COLUMNS when w is a *os.File connected to a real character-device
// terminal and $COLUMNS parses as an integer above 20, defaultTitleWidth
// otherwise. It uses the stdlib only (os.File.Stat + os.ModeCharDevice);
// pulling in x/term or go-isatty for this single check would make an indirect
// dependency direct for a cosmetic gain.
func titleWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return defaultTitleWidth
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return defaultTitleWidth
	}
	cols, err := strconv.Atoi(os.Getenv("COLUMNS"))
	if err != nil || cols <= 20 {
		return defaultTitleWidth
	}
	return cols
}

// renderOpts controls renderItems's optional columns. showStatus adds a
// status column (list, since it shows non-open items). blockedBy, when
// non-nil, appends "<- blocked by <id>" for any item present in the map
// (blocked).
type renderOpts struct {
	showStatus bool
	blockedBy  map[string]string

	// note is a per-item trailing comment with no fixed wording of its own,
	// for callers whose suffix is not "blocked by". search uses it to say
	// which field a match came from. It is a separate field rather than a
	// reuse of blockedBy because that map's suffix text is fixed, and
	// borrowing it printed "blocked by matched in why".
	note map[string]string
}

// renderItems writes one space-padded line per item (id, [status,] score,
// type, title[, blocked-by suffix]) with every column aligned to the widest
// value in this result set, never a fixed width. There is no box drawing
// and no colour: alignment is plain space padding so `awk`/`cut` keep
// working.
func renderItems(w io.Writer, items []model.Item, opts renderOpts) {
	if len(items) == 0 {
		return
	}

	idW, statusW, scoreW, typeW := 0, 0, 0, 0
	scores := make([]string, len(items))
	for i, it := range items {
		if l := len(it.ID); l > idW {
			idW = l
		}
		if opts.showStatus {
			if l := len(it.Status); l > statusW {
				statusW = l
			}
		}
		scores[i] = strconv.Itoa(it.Score)
		if l := len(scores[i]); l > scoreW {
			scoreW = l
		}
		if l := len(it.Type); l > typeW {
			typeW = l
		}
	}

	// The title column gets whatever remains of the total width budget
	// after the fixed columns and their two-space separators are
	// subtracted: the remaining budget, not the total width itself.
	used := idW + 2 + scoreW + 2 + typeW + 2
	if opts.showStatus {
		used += statusW + 2
	}
	titleBudget := titleWidth(w) - used
	if titleBudget < minTitleBudget {
		titleBudget = minTitleBudget
	}

	for i, it := range items {
		var b strings.Builder
		fmt.Fprintf(&b, "%-*s  ", idW, it.ID)
		if opts.showStatus {
			fmt.Fprintf(&b, "%-*s  ", statusW, it.Status)
		}
		fmt.Fprintf(&b, "%*s  ", scoreW, scores[i])
		fmt.Fprintf(&b, "%-*s  ", typeW, it.Type)
		b.WriteString(truncateTitle(it.Title, titleBudget))
		if opts.blockedBy != nil {
			if blocker, ok := opts.blockedBy[it.ID]; ok {
				fmt.Fprintf(&b, " ← blocked by %s", blocker)
			}
		}
		if opts.note != nil {
			if n, ok := opts.note[it.ID]; ok {
				fmt.Fprintf(&b, "  (%s)", n)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// truncateTitle cuts title to at most width columns, appending a trailing
// "…" when cut. Operates on runes, not bytes, so a multi-byte title is never
// cut mid-rune.
func truncateTitle(title string, width int) string {
	r := []rune(title)
	if len(r) <= width {
		return title
	}
	if width <= 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// emitJSON writes v to w as indented JSON and nothing else, so stdout is
// parseable JSON with nothing mixed in. Indented output is
// still machine-parseable (any JSON decoder ignores whitespace) and is far
// easier to read in a failing test's captured stdout than a single line.
func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
