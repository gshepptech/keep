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
// This is measured rather than eyeballed because it has now been broken
// twice by changes that looked purely cosmetic. The first was treating
// "faint" as a look rather than a contrast budget. The second was darkening
// the light-mode canvas to make the app surface appear to float: four
// tokens that had cleared 4.5:1 against the old canvas no longer did, and
// nothing on the page looked any different.
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
