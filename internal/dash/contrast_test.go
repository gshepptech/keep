package dash

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// tokenRE finds every `--name: #rrggbb` declaration.
var tokenRE = regexp.MustCompile(`--([a-z-]+):\s*#([0-9a-fA-F]{6})`)

// relativeLuminance is WCAG's own definition, not a perceptual
// approximation: the sRGB channels are linearised and weighted.
func relativeLuminance(hex string) float64 {
	ch := make([]float64, 3)
	for i := 0; i < 3; i++ {
		v, _ := strconv.ParseInt(hex[i*2:i*2+2], 16, 32)
		c := float64(v) / 255
		if c <= 0.04045 {
			ch[i] = c / 12.92
		} else {
			ch[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
}

func contrast(a, b string) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	hi, lo := math.Max(la, lb), math.Min(la, lb)
	return (hi + 0.05) / (lo + 0.05)
}

// tokensIn returns the colour tokens declared in one slice of the sheet.
func tokensIn(css string) map[string]string {
	out := map[string]string{}
	for _, m := range tokenRE.FindAllStringSubmatch(css, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// TestEveryTextTokenClearsContrastOnBothGrounds.
//
// This is measured rather than eyeballed because contrast is easy to break
// with changes that look purely cosmetic: treating "faint" as a look rather
// than a contrast budget, or darkening the canvas so the app surface
// appears to float, can push text tokens below 4.5:1 with nothing on the
// page looking any different.
//
// Both grounds are checked because text sits on both: the surface carries
// the content, and the canvas carries the sidebar.
func TestEveryTextTokenClearsContrastOnBothGrounds(t *testing.T) {
	css := string(styleInline)
	cut := strings.Index(css, "@media (prefers-color-scheme: dark)")
	if cut < 0 {
		t.Fatal("no dark-scheme block in the stylesheet")
	}

	schemes := map[string]map[string]string{
		"light": tokensIn(css[:cut]),
		"dark":  tokensIn(css[cut:]),
	}
	// Colour tokens that carry text, and the two grounds text sits on.
	text := []string{"fg", "fg-muted", "fg-faint", "workable", "claimed", "blocked"}
	grounds := []string{"bg", "panel"}

	const min = 4.5

	for scheme, tok := range schemes {
		for _, fg := range text {
			for _, bg := range grounds {
				fgv, ok := tok[fg]
				if !ok {
					t.Errorf("%s: no --%s token", scheme, fg)
					continue
				}
				bgv, ok := tok[bg]
				if !ok {
					t.Errorf("%s: no --%s token", scheme, bg)
					continue
				}
				if got := contrast(fgv, bgv); got < min {
					t.Errorf("%s: --%s (#%s) on --%s (#%s) is %.2f:1, want at least %.1f:1",
						scheme, fg, fgv, bg, bgv, got, min)
				}
			}
		}
	}
}

// TestTextOnAFilledColourClearsContrast covers the case the test above is
// blind to by construction: it measures text tokens against --bg and
// --panel, the two grounds a *page* provides. A filled button is a third
// ground, and nothing was checking it.
//
// --on-danger fell straight through that gap. It was defined once for the
// light theme, where the danger red is dark and near-white text on it
// measures 6.15:1, and then copied verbatim into the dark block -- where
// the danger red is LIGHTER, because it has to be visible on a near-black
// canvas, and the same near-white text measures 3.14:1. Found by redpen's
// render pass, which reads computed colours off the real page and so was
// never fooled by the token being "already decided".
//
// An on-colour is a function of the colour it sits on. The two themes' reds
// are on opposite sides of the luminance where that function flips, so the
// one value that looked like a constant could not be one.
func TestTextOnAFilledColourClearsContrast(t *testing.T) {
	css := string(styleInline)
	cut := strings.Index(css, "@media (prefers-color-scheme: dark)")
	if cut < 0 {
		t.Fatal("no dark-scheme block in the stylesheet")
	}
	schemes := map[string]map[string]string{
		"light": tokensIn(css[:cut]),
		"dark":  tokensIn(css[cut:]),
	}

	// Every place the board sets text on a filled colour rather than on the
	// page. Extend this when a second filled control appears -- a new
	// --on-* token with no row here is the same hole reopened.
	filled := []struct{ text, fill string }{
		{"on-danger", "blocked"}, // .btn-danger.btn-solid, the confirm page
		{"bg", "fg"},             // .btn-primary, New item / Save changes
	}

	// 4.5, not 3.0: these buttons are 14px at weight 500, which is body
	// text by WCAG's definition, not large text.
	const min = 4.5

	for scheme, tok := range schemes {
		for _, p := range filled {
			fg, ok := tok[p.text]
			if !ok {
				t.Errorf("%s: no --%s token", scheme, p.text)
				continue
			}
			bg, ok := tok[p.fill]
			if !ok {
				t.Errorf("%s: no --%s token", scheme, p.fill)
				continue
			}
			if got := contrast(fg, bg); got < min {
				t.Errorf("%s: --%s (#%s) on filled --%s (#%s) is %.2f:1, want at least %.1f:1",
					scheme, p.text, fg, p.fill, bg, got, min)
			}
		}
	}
}

// TestContrastMathMatchesAKnownPair guards the measurement itself. Black on
// white is exactly 21:1 by definition; a helper that cannot reproduce that
// would pass the test above by being wrong in a convenient direction.
func TestContrastMathMatchesAKnownPair(t *testing.T) {
	if got := contrast("000000", "ffffff"); math.Abs(got-21) > 0.01 {
		t.Errorf("contrast(black, white) = %v, want 21", got)
	}
	if got := contrast("ffffff", "ffffff"); math.Abs(got-1) > 0.001 {
		t.Errorf("contrast(white, white) = %v, want 1", got)
	}
}
