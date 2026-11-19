package door

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// bad are the inputs known to break a row, a style or a width somewhere
// on the way to tview, each a seed of the targets below: an emoji and a
// skin tone (uniseg's LB30b: a mandatory break after it), the
// combining grapheme joiner and an unassigned Extended_Pictographic
// code point before one, an Arabic number sign (a Prepend character,
// which takes the next character, a tag's [ too, into its cluster), a
// zero width joiner pair, half a flag and a whole one, a Hangul vowel
// after a leading consonant, a spacing mark, a URL tag, and a colour
// tag and a bold one.
var bad = []string{
	"\U0001f44d\U0001f3fd", "ci\U0001f44d\U0001f3fd", "\u034f\U0001f3fd", "\U0001f44d\u034f\U0001f3fd",
	"\U0001faff\U0001f3fd", "\U0001faff", "\u0600", "x\u0600", "\u0d4e", "\u200d❤", "\U0001f468\u200d\U0001f469",
	"\U0001f1e6", "\U0001f1e6\U0001f1e8", "\u1100\u1161", "\u1161", "a\u0903", "\u0903",
	"[:::https://evil.example/x]link[:::-]", "[red]x[-]", "[::b]x[::-]", "\x1b]8;;https://evil.example\x07x", "a\nb", "",
}

// drawn is the screen k draws text on, w cells wide and its row
// top and three rows more high.
func (k kind) drawn(text string, w int) tcell.Screen {
	scr := sim(w, k.top+3)
	k.draw(scr, []string{text})
	return scr
}

// span is where c, a case between < and >, is drawn on row y of scr:
// from the cell its < begins to the end of the last cell ending in >,
// as the screen says how wide each cell is; ok is false when either is
// not on the row.
func span(scr tcell.Screen, y int) (from, to int, ok bool) {
	w, _ := scr.Size()
	from = -1
	for x := 0; x < w; {
		s, _, cw := scr.Get(x, y)
		if from < 0 && strings.HasPrefix(s, "<") {
			from = x
		}
		if strings.HasSuffix(s, ">") {
			to = x + max(cw, 1)
		}
		x += max(cw, 1)
	}
	return from, to, from >= 0 && to > from
}

// styles is every style on scr's cells.
func styles(scr tcell.Screen) map[tcell.Style]bool {
	w, h := scr.Size()
	out := map[tcell.Style]bool{}
	for y := range h {
		for x := range w {
			_, st, _ := scr.Get(x, y)
			out[st] = true
		}
	}
	return out
}

var (
	refOnce   sync.Once
	refStyles map[string]map[tcell.Style]bool
)

// plainStyles is the styles each kind draws "<x>" in, its own words with
// no tag: every style a text the door makes inert may be drawn in.
func plainStyles() map[string]map[tcell.Style]bool {
	refOnce.Do(func() {
		refStyles = map[string]map[tcell.Style]bool{}
		for _, k := range kinds() {
			refStyles[k.name] = styles(k.drawn("<x>", 40))
		}
	})
	return refStyles
}

// measured are the kinds whose text the screens measure with Width (a
// text view's, a table cell's) or whose text is drawn from its first
// cell as it is (the label, the printed line), and how the kind hands
// a case to tview: the TUI's own lines lose their line feeds. The
// title and the completions are centred or framed in a list and hold
// the TUI's own words or app names; "text view, a line" puts a tag
// inside the case, which FuzzLine holds.
var measured = map[string]func(string) string{
	"text view, box lines":           func(c string) string { return c },
	"text view, the TUI's own lines": func(c string) string { return strings.ReplaceAll(c, "\n", "") },
	"table cell":                     func(c string) string { return c },
	"input field's label":            func(c string) string { return c },
	"printed":                        func(c string) string { return c },
}

// checkInert holds what the door hands tview of s, between < and >, in
// every kind of widget it serves: it is drawn on one row (TestOneRow's
// check); in no style the kind does not draw its own words in, and in
// one style from its < to its >, so no tag in it is read, a URL tag's
// link among them; and, where the screens measure it, as many cells
// wide as Width says.
func checkInert(t *testing.T, s string) {
	t.Helper()
	c := "<" + s + ">"
	ref := plainStyles()
	for _, k := range kinds() {
		hand := measured[k.name]
		w := 16 + 10*utf8.RuneCountInString(c) // an escape is at most ten cells
		if hand != nil {
			w = 16 + Width(termtext.From(hand(c)))
		}
		if !k.oneAt([]string{c}, w) {
			t.Errorf("%s: %s (%q) is not drawn on one row", k.name, runes(s), s)
		}
		if k.name == "text view, a line" {
			continue
		}
		scr := k.drawn(c, w)
		for st := range styles(scr) {
			if !ref[k.name][st] {
				t.Errorf("%s: %s (%q) is drawn in a style of its own: %v", k.name, runes(s), s, st)
				break
			}
		}
		// And in one style from its < to its >: a tag read would change
		// it there, even to a style the kind draws elsewhere.
		from, to, ok := span(scr, k.top)
		if ok {
			_, first, _ := scr.Get(from, k.top)
			for x := from; x < to; x++ {
				if _, st, _ := scr.Get(x, k.top); st != first {
					t.Errorf("%s: %s (%q) changes style at cell %d", k.name, runes(s), s, x-from)
					break
				}
			}
		}
		if hand == nil {
			continue
		}
		if want := Width(termtext.From(hand(c))); !ok || to-from != want {
			t.Errorf("%s: %s (%q) is drawn %d cells wide (from %d), and Width says %d", k.name, runes(s), s, to-from, from, want)
		}
	}
}

// FuzzInert: any string, through the door, in every kind of widget it
// serves (kinds), as checkInert holds it. TestOneRow draws every code
// point and every known bad pair; this finds what neither lists. Seen
// to fail: with the uniseg guard taken out of Inert (unbreak not
// called) on the skin tone; with tagless taken out on the URL tag (a
// style of its own) and the colour tag; with Width measuring the text
// before the door (tview.TaggedStringWidth(t.String())) on the
// skin tone, whose escape is ten cells.
func FuzzInert(f *testing.F) {
	for _, s := range bad {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if utf8.RuneCountInString(s) > 64 {
			return
		}
		checkInert(t, s)
	})
}
