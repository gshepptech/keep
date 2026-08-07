package dash

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/net/html"
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
				// TODO(RED): inline style="" is not yet checked here --
				// deliberately, for this task's own RED step. Added in the
				// immediately following GREEN commit.
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
// check DASH-05's <style>-content scope decision requires: scanRenderedHTML
// never reads a <style> element's text content (comment above scanRenderedHTML
// explains why), which means the one place this stylesheet's own text is
// still worth checking is here, once, statically -- comments stripped first,
// exactly as research identified as the fix for the false positive a raw
// substring check produces on a comment merely mentioning "@import".
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
