package dash

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// touchMinimum is the height every control gets once the pointer is a thumb
// or the window is phone-sized. It is stated once here and once in the
// stylesheet; a mismatch is a test failure rather than a slow drift.
const touchMinimum = 44

// touchGuard is the media query that restores the minimum. Matched by its
// text rather than by position, so reordering the sheet cannot break this.
const touchGuard = "@media (pointer: coarse), (max-width: 720px)"

// cssComment strips /* ... */ so a sentence inside one cannot be mistaken
// for a selector. The first draft of this test reported
// `"exactly -- not a minimum"` as a rule pinning a height.
var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// heightRule captures a selector list and an exact pixel height. Only
// `height:` -- a `min-height` is a floor and cannot defeat a larger floor
// set elsewhere, which is the entire distinction this test exists for.
var heightRule = regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*?)\bheight:\s*(\d+)px`)

// clickable reports whether a selector targets something a person aims at.
//
// Pseudo-elements are excluded because they cannot be clicked at all: the
// select's caret and the checkbox's tick are both drawn as ::after boxes a
// few pixels across, and neither has a target size to meet -- the control
// underneath does.
//
// A bare checkbox is excluded for a different and less obvious reason: on
// every page that has one it is wrapped in a <label class="inline">, so the
// target is the label and its text, not the 16px box. That is a real
// exemption resting on real markup, so it is asserted rather than assumed --
// see TestEveryCheckboxIsInsideItsLabel below.
func clickable(sel string) bool {
	if strings.Contains(sel, "::") {
		return false
	}
	if strings.Contains(sel, `input[type="checkbox"]`) {
		return false
	}
	for _, s := range []string{".btn", "select", "input", "textarea", "summary", ".tab", ".nav-item"} {
		if strings.Contains(sel, s) {
			return true
		}
	}
	return false
}

// blockAt returns the body of the brace-delimited block starting at the
// first `{` at or after start, by counting braces. Index-of-"\n}" is not
// good enough for a media query: it stops at the end of the first rule
// inside it, so every later rule looks absent.
func blockAt(css string, start int) string {
	open := strings.Index(css[start:], "{")
	if open < 0 {
		return ""
	}
	open += start
	depth := 0
	for i := open; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[open+1 : i]
			}
		}
	}
	return ""
}

// TestNoControlPinsItselfBelowTheTouchMinimum.
//
// A structural test over the stylesheet rather than a layout test, because
// there is no layout engine in a Go test -- and it is the right shape
// anyway, because the defect is structural rather than visual.
//
// Three separate times a rule has equalised a row of controls to an exact
// height so they line up, and in doing so silently cancelled the touch
// target minimum promised at the top of the sheet:
//
//	.qacts .btn / select      32px
//	.acts .btn / select       32px
//	.prompt-side .btn/select  40px
//
// Every time, both rules read as correct in review. `height` beats a
// `min-height` from a less specific selector, and `.qacts .btn` is more
// specific than `.btn`, so the promise was cancelled by a rule that never
// mentions it. Two of the three were found by redpen's render pass reading
// computed heights off a real page; none was ever found by reading the CSS.
//
// So the invariant is: a control may pin its height, but if it pins it below
// the minimum it must also appear in the block that lifts it back. A new row
// of controls with a shared height fails this the moment it is written.
func TestNoControlPinsItselfBelowTheTouchMinimum(t *testing.T) {
	css := cssComment.ReplaceAllString(string(styleInline), " ")

	// EVERY block with this query, not the first. The sheet has two: one
	// early that sets min-height on the generic controls, and one late that
	// restores the pinned heights. Reading only the first is how the first
	// draft of this test reported the very rules it had just been written to
	// approve -- a test that fails for a reason that is not the defect is
	// worse than no test, because the next person deletes it.
	var blocks []string
	for from := 0; ; {
		at := strings.Index(css[from:], touchGuard)
		if at < 0 {
			break
		}
		at += from
		blocks = append(blocks, blockAt(css, at))
		from = at + len(touchGuard)
	}
	if len(blocks) == 0 {
		t.Fatalf("no %q block in the stylesheet -- the touch-target guard is gone entirely", touchGuard)
	}
	guard := strings.Join(strings.Fields(strings.Join(blocks, " ")), " ")
	if guard == "" {
		t.Fatal("every touch-target guard block is empty")
	}

	var pinned int
	for _, m := range heightRule.FindAllStringSubmatch(css, -1) {
		sel := strings.TrimSpace(m[1])
		// A rule inside any media block is conditional and cannot be the
		// one making the unconditional promise, so it is not the offender.
		if strings.Contains(sel, "@media") || strings.HasPrefix(sel, "@") {
			continue
		}
		// The regex's selector group runs back to the previous rule's
		// closing brace; keep only what follows it.
		if i := strings.LastIndex(sel, "}"); i >= 0 {
			sel = strings.TrimSpace(sel[i+1:])
		}
		h, err := strconv.Atoi(m[3])
		if err != nil || h >= touchMinimum {
			continue
		}
		if sel == "" || !clickable(sel) {
			continue
		}
		pinned++

		// Each comma-separated part is checked on its own, because the one
		// that gets forgotten is exactly the one that stays small.
		for _, part := range strings.Split(sel, ",") {
			part = strings.Join(strings.Fields(part), " ")
			if part == "" || !clickable(part) {
				continue
			}
			if !strings.Contains(guard, part) {
				t.Errorf("%q pins height: %dpx, under the %dpx minimum, and is not restored in the %q block.\n"+
					"Add it there, or that control is %dpx under a thumb.", part, h, touchMinimum, touchGuard, h)
			}
		}
	}

	// A guard that matches nothing passes vacuously. Rules pinning a height
	// exist today; if the count reaches zero the regex has stopped working
	// rather than the problem having gone away.
	if pinned == 0 {
		t.Error("no rules pinning a clickable control's height were found at all -- heightRule has stopped matching, so this test is asserting nothing")
	}
}

// TestEveryCheckboxIsInsideItsLabel holds up the exemption the test above
// takes. A 16px checkbox is under any target-size floor worth having; it is
// allowed here only because the label wraps it, which makes the target the
// whole labelled row rather than the box. If a checkbox is ever written
// outside a label, that reasoning evaporates and this fails.
func TestEveryCheckboxIsInsideItsLabel(t *testing.T) {
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	sources := map[string]string{}
	for _, e := range entries {
		b, err := templateFS.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		sources[e.Name()] = string(b)
	}

	var found int
	for name, body := range sources {
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, `type="checkbox"`) {
				continue
			}
			found++
			// Same line, or the label opened on a line above and the
			// checkbox is indented under it.
			if strings.Contains(line, "<label") {
				continue
			}
			if !strings.Contains(body[:strings.Index(body, line)], "<label") {
				t.Errorf("%s: a checkbox with no <label> around it:\n  %s", name, strings.TrimSpace(line))
			}
		}
	}
	if found == 0 {
		t.Error("no checkboxes found in any template -- this test is asserting nothing")
	}
}
