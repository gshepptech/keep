package dash

import (
	"embed"
	"fmt"
	"html/template"

	"github.com/gshepptech/rounce/internal/model"
)

// styleCSS is the whole embedded stylesheet, compiled into the binary --
// the page fetches nothing (DASH-05's "no external asset" requirement).
//
//go:embed style.css
var styleCSS string

// styleInline is the ONE permitted use of a template.* trust type in this
// package -- doc.go states the rule this is the deliberate, documented
// exception to. styleCSS is a compile-time go:embed of a static file living
// in this repository: it is not agent data, cannot become agent data (no
// code in this package ever writes to it), and is reviewable once by a
// human rather than scanned per render. Plan 06's zero-occurrence grep for
// template.HTML/HTMLAttr/URL/JS/CSS would otherwise have no way to
// distinguish this line from a genuine violation; naming it here, at the
// declaration, is what keeps the exception visible rather than rediscovered
// later.
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
//     presentation logic -- see render.go's doc comments for what each
//     decides and, just as importantly, what it does not decide.
var funcMap = template.FuncMap{
	"style":      func() template.CSS { return styleInline },
	"state":      state,
	"depthClass": depthClass,
}

// tmpl is the package's one parsed template set, built once at package
// initialisation from every file under templates/. A template that fails to
// parse is a build-time mistake, not a runtime condition -- template.Must
// panicking here means every render function is free of a branch for
// something that can never vary once the binary has started.
var tmpl = template.Must(template.New("dash").Funcs(funcMap).ParseFS(templateFS, "templates/*.html"))

// state maps an item to one of exactly three semantic states -- the three
// docs/design/phase-4.md permits colour to carry (workable / claimed /
// blocked). Order matters: blocked wins over claimed, because a
// claimed-but-blocked item is not workable and the page must say the more
// important thing. This is a presentation mapping over values the store
// already computed (Status, Blocked); it decides nothing about the data --
// the only piece of logic in this package alongside depthClass below.
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
// surfaces DASH-05's scan inspects; keeping it out of the rendered output
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
