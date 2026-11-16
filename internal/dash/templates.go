package dash

import (
	"embed"
	"fmt"
	"html/template"

	"github.com/gshepptech/keep/internal/model"
)

// styleCSS is the whole embedded stylesheet, compiled into the binary,
// the page fetches nothing (no external assets).
//
//go:embed style.css
var styleCSS string

// styleInline is the one permitted use of a template.* trust type in this
// package, the documented exception to the rule doc.go states. styleCSS is
// a compile-time go:embed of a static file living in this repository: it is
// not agent data, cannot become agent data (no code in this package ever
// writes to it), and is reviewable once by a human rather than scanned per
// render.
var styleInline = template.CSS(styleCSS)

// templateFS is every *.html template this package parses. Kept as an
// embed.FS (rather than embedding each file into a string) so ParseFS can
// glob the whole directory in one call.
//
//go:embed templates/*.html
var templateFS embed.FS

// funcMap is registered once on the shared template set.
//   - "style" hands templates/layout.html's "head" block the embedded
//     stylesheet without threading it through every render function's page
//     data.
//   - "state" and "depthClass" (render.go) are this package's only
//     presentation logic; their doc comments say what each decides and
//     what it does not.
var funcMap = template.FuncMap{
	"style":      func() template.CSS { return styleInline },
	"state":      state,
	"depthClass": depthClass,
	"until":      relativeTime,
	"ago":        agoTime,
	"titled": func(p page, title, heading, lede string) titledPage {
		return titledPage{Page: p, Title: title, Heading: heading, Lede: lede, Data: p.Data}
	},
	"act": func(p page, verb, id, label, ret string, danger bool) actionArgs {
		return actionArgs{
			CSRF: p.CSRF, Verb: verb, ID: id, Label: label, Return: ret,
			Project: p.Project, Prefix: p.Prefix, Danger: danger,
		}
	},
	// tag pairs a semantic state with the word shown beside its glyph, so
	// one block draws every state marker on every page and a new state
	// cannot be introduced with a glyph on one page and a pill on another.
	"tag": func(slug, label string) stateTag {
		return stateTag{Slug: slug, Label: label}
	},
	// either picks between two strings without a branch in the template.
	// html/template has no ternary, and an {{if}} around the whole head call
	// would duplicate the line that sets every page's title.
	"either": func(cond bool, yes, no string) string {
		return map[bool]string{true: yes, false: no}[cond]
	},
	"holdsFor":  holdsFor,
	"holdCount": holdCount,
	"base":      base,
	"initial":   initial,
	"stat":      statCardFor,
}

// tmpl is the package's one parsed template set, built once at package
// initialisation from every file under templates/. A template that fails to
// parse is a build-time mistake, not a runtime condition, so template.Must
// panicking here leaves every render function free of a branch for
// something that can never vary once the binary has started.
var tmpl = template.Must(template.New("dash").Funcs(funcMap).ParseFS(templateFS, "templates/*.html"))

// state maps an item to one of exactly three semantic states, the only
// three colour carries (workable / claimed / blocked). Order matters:
// blocked wins over claimed, because a claimed-but-blocked item is not
// workable and the page must say the more important thing. This is a
// presentation mapping over values the store already computed (Status,
// Blocked); it decides nothing about the data, and it is the only piece of
// logic in this package alongside depthClass below.
func state(status string, blocked bool) string {
	if blocked {
		return "blocked"
	}
	if status == model.StatusInProgress {
		return "claimed"
	}
	return "workable"
}

// maxDepthClass caps tree.html's depth-N class names at a bounded set of CSS
// rules (style.css defines depth-0 through depth-12) rather than growing one
// rule per possible depth; ready's own recursion bound is depth < 50, but a
// tree that deep would be unreadable regardless of indent, so rows beyond
// the cap simply share the deepest defined indent.
const maxDepthClass = 12

// depthClass maps a tree row's depth to a class name, never an inline
// style="padding-left:...". An inline style attribute is one of the
// surfaces the off-host scan inspects; keeping it out of the rendered output
// entirely is cheaper than proving each generated padding value safe.
func depthClass(depth int) string {
	if depth < 0 {
		depth = 0
	}
	if depth > maxDepthClass {
		depth = maxDepthClass
	}
	return fmt.Sprintf("depth-%d", depth)
}
