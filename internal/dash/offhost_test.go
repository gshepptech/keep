package dash

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/gshepptech/keep/internal/model"
	"github.com/gshepptech/keep/internal/store"
)

// resourceAttrs are the attributes whose values a browser resolves as a URL
// and fetches. Only these -- plus the inline `style` attribute -- are
// inspected. Scanning every attribute (or, worse, the page's raw bytes)
// false-positives the moment an agent's title or body legitimately contains
// a URL as prose, which is ordinary content in a work tracker and must
// render as inert text without failing the build.
var resourceAttrs = map[string]bool{
	"href": true, "src": true, "srcset": true, "action": true,
	"formaction": true, "poster": true, "background": true,
	"cite": true, "data": true, "manifest": true,
}

// scanRenderedHTML parses page as real HTML and walks every element's
// attributes, reporting one violation string per resource-fetching
// reference that resolves off-host. It walks a parsed DOM (golang.org/x/net/
// html), never the page's raw bytes, precisely so that legitimate prose
// containing a URL, or an HTML comment mentioning "//", is never inspected
// in the first place -- there is no attribute to check on a text node or a
// comment node.
//
// This walk deliberately never reads any node's TEXT content (html.TextNode
// data), only html.ElementNode attribute values -- which is what keeps
// <style> element content out of scope without any special-casing for it.
// research's own first scanner false-positived on a legitimate <style>
// block containing only the CSS comment "/* no @import, no url() here */",
// because a raw substring check cannot tell a comment from a declaration.
// The correct fix is not a better CSS parser: internal/dash's stylesheet is
// entirely static, go:embed-ed once, and never built from a template
// variable or agent data, so it is not an injection surface and does not
// need re-checking on every render (see
// TestEmbeddedStylesheetHasNoExternalReference below for the compensating
// one-time static check this scope decision requires).
func scanRenderedHTML(page string) []string {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return []string{fmt.Sprintf("parse error: %v", err)}
	}

	var violations []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				name := strings.ToLower(a.Key)
				switch {
				case name == "style":
					if reason, bad := valueViolates(a.Val); bad {
						violations = append(violations, fmt.Sprintf("<%s style=%q>: %s", n.Data, a.Val, reason))
					}
				case name == "srcset":
					for _, candidate := range strings.Split(a.Val, ",") {
						fields := strings.Fields(strings.TrimSpace(candidate))
						if len(fields) == 0 {
							continue
						}
						if reason, bad := valueViolates(fields[0]); bad {
							violations = append(violations, fmt.Sprintf("<%s srcset=%q>: %s", n.Data, a.Val, reason))
						}
					}
				case resourceAttrs[name]:
					if reason, bad := valueViolates(a.Val); bad {
						violations = append(violations, fmt.Sprintf("<%s %s=%q>: %s", n.Data, name, a.Val, reason))
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return violations
}

// valueViolates reports a violation when v: starts with "//"; contains
// "http://" or "https://"; contains "@import"; or contains a "url(" whose
// argument does not begin "data:" (allowing url(data:...), url('data:...'),
// url("data:...")). Everything else -- including "/item/rnc-abc123.1.2",
// "/", "#frag", and an empty value -- passes.
func valueViolates(v string) (string, bool) {
	if strings.HasPrefix(v, "//") {
		return "protocol-relative reference", true
	}
	if strings.Contains(v, "http://") {
		return "absolute http:// reference", true
	}
	if strings.Contains(v, "https://") {
		return "absolute https:// reference", true
	}
	if strings.Contains(v, "@import") {
		return "@import at-rule", true
	}
	if idx := strings.Index(v, "url("); idx != -1 {
		rest := strings.TrimLeft(v[idx+len("url("):], `'" `)
		if !strings.HasPrefix(rest, "data:") {
			return "url(...) not pointing at a data: URI", true
		}
	}
	return "", false
}

// hostileAdversarialPage plants exactly four traps -- an href, a
// protocol-relative img src, an inline style url(), and a legacy
// `background` attribute -- alongside five legitimate shapes that must NOT
// be flagged: prose containing a URL, two internal links, an HTML comment
// mentioning "//" and "http://", a data: URI image, and a <style> block
// whose only content is a CSS comment mentioning "@import".
const hostileAdversarialPage = `<!doctype html>
<html><head><title>t</title></head><body>
<p>see http://example.com/spec for details</p>
<a href="/item/rnc-abc123">internal</a>
<a href="/">home</a>
<!-- a comment mentioning // and http:// -->
<img src="data:image/gif;base64,R0lGOD...">
<style>/* no @import, no url() here */</style>
<a href="http://evil.example/x.png">l</a>
<img src="//evil.example/y.png">
<div style="background:url(http://evil.example/z.png)">d</div>
<table><tbody><tr><td background="//evil.example/w.png">cell</td></tr></tbody></table>
</body></html>`

func TestScanRenderedHTMLCatchesTrapsAndNothingElse(t *testing.T) {
	violations := scanRenderedHTML(hostileAdversarialPage)
	if len(violations) != 4 {
		t.Fatalf("scanRenderedHTML: got %d violations, want exactly 4:\n%s", len(violations), strings.Join(violations, "\n"))
	}
	wantSubstrings := []string{
		"evil.example/x.png",
		"evil.example/y.png",
		"evil.example/z.png",
		"evil.example/w.png",
	}
	for _, want := range wantSubstrings {
		found := false
		for _, v := range violations {
			if strings.Contains(v, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scanRenderedHTML: no violation names %q; got:\n%s", want, strings.Join(violations, "\n"))
		}
	}
}

// TestEmbeddedStylesheetHasNoExternalReference is the compensating static
// check that scanRenderedHTML's <style>-content scope requires: it
// never reads a <style> element's text content (comment above scanRenderedHTML
// explains why), which means the one place this stylesheet's own text is
// still worth checking is here, once, statically -- comments stripped first,
// to avoid the false positive a raw substring check produces on a comment
// merely mentioning "@import".
func TestEmbeddedStylesheetHasNoExternalReference(t *testing.T) {
	css := stripCSSComments(styleCSS)
	for _, forbidden := range []string{"@import", "http://", "https://", "//"} {
		if strings.Contains(css, forbidden) {
			t.Errorf("style.css (comments stripped) contains forbidden substring %q", forbidden)
		}
	}
	idx := 0
	for {
		i := strings.Index(css[idx:], "url(")
		if i == -1 {
			break
		}
		pos := idx + i
		rest := strings.TrimLeft(css[pos+len("url("):], `'" `)
		if !strings.HasPrefix(rest, "data:") {
			t.Errorf("style.css (comments stripped) contains a url(...) not pointing at data:, at byte offset %d", pos)
		}
		idx = pos + len("url(")
	}
}

// stripCSSComments removes every /* ... */ span from s. Used only by
// TestEmbeddedStylesheetHasNoExternalReference, over a fixed, go:embed-ed
// asset, once -- not part of the per-render scan.
func stripCSSComments(s string) string {
	var b strings.Builder
	for {
		start := strings.Index(s, "/*")
		if start == -1 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:start])
		end := strings.Index(s[start:], "*/")
		if end == -1 {
			break
		}
		s = s[start+end+2:]
	}
	return b.String()
}

// The six hostile values below are planted into a real store fixture's
// Title/Body/Why/Acceptance/Repo fields (never into a struct literal --
// see newOffhostFixture's own comment for why that distinction matters
// here specifically). hostileImgTagMarker is checked for presence using a
// metachar-free substring ("evil.example/z.png") rather than the literal
// tag text, because html/template's own escaping of "<img src=\"...\">"
// inside a <pre> turns the angle brackets and quotes into entities --
// which is the correct, desired behaviour (the tag must render as inert
// text, not become a live element), so the presence check must survive
// that escaping rather than defeat the point of the test by asserting
// against unescaped markup.
const (
	hostileHTTP          = "http://evil.example/x.png"
	hostileProtoRelative = "//evil.example/y.png"
	hostileImgTag        = `<img src="http://evil.example/z.png">`
	hostileImgTagMarker  = "evil.example/z.png"
	hostileImportCSS     = "@import url(https://evil.example/a.css)"
	hostileStyleURL      = "background:url(http://evil.example/b.png)"
	hostileJS            = "javascript:alert(1)"
)

// offhostFixtureIDs names every id newOffhostFixture's fixture produces.
type offhostFixtureIDs struct {
	epic, child, grandchild, readyItem, rawNote string
}

// newOffhostFixture builds a Server backed by a REAL store.Store, with every
// hostile value planted through the real store.AddItem/CreateDep API --
// never as model.Item/model.TreeNode struct literals.
//
// This is the opposite fixture strategy from the tree golden test, and
// deliberately so, for a reason worth recording rather than letting a
// future reader "clean up" into one consistent choice: the golden test needs
// byte-stable output, so it fixtures with literal, stable ids and never
// touches a live store or a clock. The off-host scan needs the opposite proof --
// that data which genuinely travelled through AddItem and back out of a
// real query (wall-clock-derived rnc- ids, the real column round-trip,
// the real html/template execution against real model.Item/model.ItemDetail/
// model.TreeNode values) cannot produce an off-host reference at render
// time. A struct-literal fixture here would only prove the template is
// safe against values Go can construct in a test file, not against values
// that actually survived a store round-trip -- and nothing in this test
// compares byte-for-byte, so the wall-clock-derived ids are harmless.
//
// The fixture is also the three-level blocked subtree the tree route (and
// the golden test) uses: epic (blocked, root) -> child -> grandchild, plus
// a separate blocker and one ready item and one raw-backlog item so all
// four views have hostile content to render.
func newOffhostFixture(t *testing.T) (*Server, offhostFixtureIDs) {
	t.Helper()
	st := openTestStore(t)
	ctx := context.Background()
	const actor = "hostile-tester"

	epic := mustAdd(t, st, store.AddItemInput{Title: "Epic root " + hostileHTTP, Actor: actor})
	child := mustAdd(t, st, store.AddItemInput{
		Title: "Child " + hostileProtoRelative, Actor: actor, ParentID: epic.ID,
	})
	// grandchild carries every remaining hostile field (Body/Why/Acceptance/
	// Repo) so a single /item/{id} route exercises Body, Why, Acceptance,
	// and Repo all at once. hostileJS lands in Body, not Title: item.html
	// deliberately renders no title row at all, so a hostile value
	// planted only in Title would never appear in this view's output.
	grandchild := mustAdd(t, st, store.AddItemInput{
		Title: "Grandchild", Actor: actor, ParentID: child.ID,
		Body:       hostileImgTag + " " + hostileStyleURL + " " + hostileJS,
		Why:        hostileImportCSS,
		Acceptance: hostileHTTP,
		Repo:       hostileProtoRelative,
	})
	blocker := mustAdd(t, st, store.AddItemInput{Title: "Blocker", Actor: actor})
	if err := st.CreateDep(ctx, store.DepInput{ItemID: epic.ID, DependsOnID: blocker.ID, Type: model.DepBlocks, Actor: actor}); err != nil {
		t.Fatalf("CreateDep: %v", err)
	}
	readyItem := mustAdd(t, st, store.AddItemInput{Title: "Ready item " + hostileHTTP, Actor: actor})
	rawNote := mustAdd(t, st, store.AddItemInput{
		Title: "Raw note " + hostileProtoRelative, Actor: actor,
		Stage: model.StageRaw, Type: model.TypeNote,
	})

	s, err := NewServer(st, "127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return s, offhostFixtureIDs{
		epic: epic.ID, child: child.ID, grandchild: grandchild.ID,
		readyItem: readyItem.ID, rawNote: rawNote.ID,
	}
}

// TestNoExternalReferencesInRenderedViews is the off-host proof: all
// four rendered views, driven through the real Handler() via httptest (real
// HTTP responses, not template source), scan clean -- and the hostile text
// is confirmed PRESENT in each body first, so a view that silently dropped
// a field could not make the scan pass vacuously.
func TestNoExternalReferencesInRenderedViews(t *testing.T) {
	s, ids := newOffhostFixture(t)

	cases := []struct {
		name           string
		path           string
		wantSubstrings []string
	}{
		{"ready queue", "/", []string{hostileHTTP}},
		{"backlog", "/backlog", []string{hostileProtoRelative}},
		{
			"item detail", "/item/" + ids.grandchild,
			[]string{hostileJS, hostileImgTagMarker, hostileStyleURL, hostileImportCSS, hostileHTTP, hostileProtoRelative},
		},
		// tree.html renders only Title (never Body/Why/Acceptance/Repo) --
		// epic's and child's titles carry hostileHTTP/hostileProtoRelative;
		// grandchild's own hostile fields (checked above, under item detail)
		// are Body-only and therefore not expected to appear in this view.
		{"tree", "/tree/" + ids.epic, []string{hostileHTTP, hostileProtoRelative}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", tc.path, rec.Code)
			}
			body := rec.Body.String()
			if body == "" {
				t.Fatalf("%s: empty response body", tc.path)
			}

			// Confirm the hostile text is actually present (escaped, as
			// inert text) BEFORE trusting a clean scan -- a view that
			// silently dropped the field would pass the scan vacuously.
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(body, want) {
					t.Errorf("%s: hostile text %q not present in rendered body -- scan would pass vacuously", tc.path, want)
				}
			}

			if v := scanRenderedHTML(body); len(v) != 0 {
				t.Errorf("%s: off-host references in rendered output:\n%s", tc.path, strings.Join(v, "\n"))
			}
		})
	}

	// The tree route's fixture is the three-level blocked subtree -- confirm
	// the tree view under scan here is also the blocked one, so this
	// test is not accidentally scanning an all-workable tree.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tree/"+ids.epic, nil))
	if !strings.Contains(rec.Body.String(), "blocked") {
		t.Fatalf("/tree/%s: body does not contain the word \"blocked\" -- fixture is not exercising the blocked-subtree shape", ids.epic)
	}
}
