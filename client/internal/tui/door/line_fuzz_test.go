package door

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rivo/uniseg"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// lineStyle is the text style a text view draws a line in for these
// targets: the view's own, italic, so that the cells the text is drawn
// in are told from the ones the view clears (the door's tags never set
// italic, and [::-] goes back to the view's own).
func lineStyle() tcell.Style {
	return tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor).Italic(true)
}

// drawLines is ls drawn in a text view as the screens set one, w cells
// wide and h rows high, in lineStyle.
func drawLines(ls []model.Line, colour bool, w, h int) tcell.Screen {
	scr := sim(w, h)
	tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	tv.SetTextStyle(lineStyle())
	SetText(tv, Lines(ls, colour))
	tv.SetRect(0, 0, w, h)
	tv.Draw(scr)
	return scr
}

// written is how many cells of row y tview drew text in.
func written(scr tcell.Screen, y int) (n int) {
	w, _ := scr.Size()
	for x := range w {
		_, st, _ := scr.Get(x, y)
		if _, _, a := st.Decompose(); a&tcell.AttrItalic != 0 {
			n++
		}
	}
	return n
}

// classes are every Class a segment can have.
var classes = []model.Class{model.Plain, model.Label, model.Dim, model.Strong, model.Bad, model.Warn, model.Good, model.LoudBad, model.LoudWarn}

// classStyle is the style a segment of class c is drawn in, with
// colour or without: drawn on "x" alone, once.
func classStyle(c model.Class, colour bool) tcell.Style {
	type key struct {
		c      model.Class
		colour bool
	}
	k := key{c, colour}
	if st, ok := classStyles.Load(k); ok {
		return st.(tcell.Style)
	}
	scr := drawLines([]model.Line{{{T: termtext.From("x"), C: c}}}, colour, 4, 1)
	_, st, _ := scr.Get(0, 0)
	classStyles.Store(k, st)
	return st
}

// classStyles are the styles classStyle has drawn.
var classStyles sync.Map

// doorSegs is l's segments as Line has them before whole, the tags
// left out: each made inert, and the characters before a row tview
// would begin written as escapes. It stops before whole on purpose:
// checkLine finds the segment each cluster begins in from the segments
// as the line gave them, so that what whole does is checked, not taken
// as given.
func doorSegs(l model.Line) []string {
	segs := make([]string, len(l))
	for i, s := range l {
		segs[i] = tagless(s.T.String())
	}
	return unbreak(segs)
}

// checkLine holds l drawn as the screens draw a line, in a text view,
// first, then a plain "-0" (a hyphen and a digit, which uniseg takes no
// break before, the one after a line feed among them), then l again,
// last: each of the three is drawn on a row of its own; l, each time,
// as many cells wide as l.Width says, each cell in the style of the
// segment its grapheme cluster begins in (no tag of the box's read, not
// even to another class's style); its styles closed at its end (the
// line under it is drawn as it is alone); and every [ in its text is a
// tag of the door's or inert (inertBrackets). l first is a line a view
// begins with, as the title bar and the crumb are; l last is one after
// a line feed.
func checkLine(t *testing.T, l model.Line, colour bool) {
	t.Helper()
	w := l.Width(Width) + 4
	scr := drawLines([]model.Line{l, {{T: termtext.From("-0")}}, l}, colour, w, 4)
	if row(scr, 1, w) != "-0" || row(scr, 3, w) != "" {
		t.Errorf("%s (colour %v) is not drawn on a row of its own: %q, %q, %q, %q", lineName(l), colour, row(scr, 0, w), row(scr, 1, w), row(scr, 2, w), row(scr, 3, w))
		return
	}
	if s := Line(l, colour).s; !inertBrackets(s, colour) {
		t.Errorf("%s (colour %v): a [ in %q is neither a tag of the door's nor followed by a zero-width space", lineName(l), colour, s)
	}
	if _, st, _ := scr.Get(0, 1); st != lineStyle() {
		t.Errorf("%s (colour %v) leaves a style open: the line under it is drawn %v", lineName(l), colour, st)
	}
	// The clusters in order, each from the cell the one before ended at,
	// as wide as uniseg says (tview draws one of no width nowhere), with
	// the class of the segment each begins in.
	type cell struct {
		c   string
		w   int
		cls model.Class
	}
	var cells []cell
	segs := doorSegs(l)
	at, seg, end := 0, 0, 0
	for rest, state := strings.Join(segs, ""), -1; rest != ""; {
		var c string
		var b int
		c, rest, b, state = uniseg.StepString(rest, state)
		for seg < len(segs) && at >= end+len(segs[seg]) {
			end += len(segs[seg])
			seg++
		}
		at += len(c)
		if cw := b >> uniseg.ShiftWidth; cw > 0 {
			cells = append(cells, cell{c, cw, l[seg].C})
		}
	}
	for _, y := range []int{0, 2} {
		if got, want := written(scr, y), l.Width(Width); got != want {
			t.Errorf("%s (colour %v) is drawn %d cells wide on row %d, and its Width says %d", lineName(l), colour, got, y, want)
			return
		}
		x := 0
		for _, c := range cells {
			if _, st, _ := scr.Get(x, y); st != classStyle(c.cls, colour) {
				t.Errorf("%s (colour %v): cell %d (%q) on row %d is drawn %v, not as its segment, %s", lineName(l), colour, x, c.c, y, st, className[c.cls])
				return
			}
			x += c.w
		}
	}
}

// inertBrackets says whether every [ in s, a Line's text, begins one
// of the tags tags writes, with colour or without, or is followed by
// the zero-width space that makes it inert.
func inertBrackets(s string, colour bool) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '[' || strings.HasPrefix(s[i+1:], "\u200b") {
			continue
		}
		dress := false
		for _, c := range classes {
			open, close := tags(c, colour)
			dress = dress || (open != "" && strings.HasPrefix(s[i:], open)) || (close != "" && strings.HasPrefix(s[i:], close))
		}
		if !dress {
			return false
		}
	}
	return true
}

// checkJoin holds s, three lines as Texts or LitLines joins them ("#",
// one, "-0"), drawn in a text view w cells wide: each on a row of its
// own.
func checkJoin(t *testing.T, name string, s Shown, w int) {
	t.Helper()
	scr := sim(w, 4)
	tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	SetText(tv, s)
	tv.SetRect(0, 0, w, 4)
	tv.Draw(scr)
	if row(scr, 0, w) != "#" || row(scr, 2, w) != "-0" || row(scr, 3, w) != "" {
		t.Errorf("%s: %q is not drawn a line to a row: %q, %q, %q, %q", name, s.s, row(scr, 0, w), row(scr, 1, w), row(scr, 2, w), row(scr, 3, w))
	}
}

// lineName names l's segments, their code points and classes.
func lineName(l model.Line) string {
	var out []string
	for _, s := range l {
		out = append(out, "["+runes(s.T.String())+" as "+className[s.C]+"]")
	}
	return strings.Join(out, " ")
}

var className = map[model.Class]string{model.Plain: "plain", model.Label: "label", model.Dim: "dim", model.Strong: "strong", model.Bad: "bad", model.Warn: "warn", model.Good: "good", model.LoudBad: "loud bad", model.LoudWarn: "loud warn"}

// FuzzLine: a whole Line, its segments the box's text (text cut at each
// NUL, which termtext would write as \x00 anyway, at most eight) in any
// order of classes (one byte of dress each; the first byte's low bit
// is colour), as checkLine holds it. Seen to fail: with Line's unbreak
// taken out, on a thumbs-up and skin tone after a dressed segment (not
// on one row); with tagless taken out of Line, on a URL tag in box
// text; with Line writing no closing tag, on the first seed (a style
// left open); with Line dressing each segment as the next one, on the
// first seed (its first cell drawn as plain where its segment is bad);
// with neither whole nor dressTag's zero-width space, on each seed of a
// tag inside a cluster (a spacing mark: 4 cells drawn, Width 3; a
// Hangul vowel: 3, Width 2; a flag: 4, Width 2; a pictograph after a
// zero width joiner: 2, Width 1) and of a Prepend character before a
// tag (the tag drawn as text, its style left open); with whole but
// no zero-width space, on the two Prepend seeds (8 cells drawn, Width
// 4, a style left open; 7 cells drawn, Width 3); and with whole leaving
// the zero-width space after a [ in the segment after it, on the URL
// tag after a Prepend character (a [ with a tag after it).
func FuzzLine(f *testing.F) {
	for i, s := range bad {
		f.Add([]byte{byte(i), byte(model.Bad), byte(model.Plain)}, "ok\x00"+s)
		f.Add([]byte{1, byte(model.Plain), byte(model.Warn), byte(model.Plain)}, "a "+s+"\x00: \x00"+s)
	}
	f.Add([]byte{1, byte(model.Strong), byte(model.Bad)}, "\U0001f44d\x00\U0001f3fd")
	// A tag inside a cluster: a spacing mark, a Hangul vowel, a flag's
	// second half, a pictograph after a zero width joiner, each across
	// the tag from the character it joins.
	f.Add([]byte{1, byte(model.Bad), byte(model.Plain)}, "a\x00\u0903b")
	f.Add([]byte{0, byte(model.Plain), byte(model.Strong)}, "\u1100\x00\u1161")
	f.Add([]byte{1, byte(model.Warn), byte(model.Good)}, "\U0001f1e6\x00\U0001f1e8")
	f.Add([]byte{1, byte(model.Good), byte(model.Plain)}, "\u2764\u200d\x00\u2764")
	// A Prepend character before a tag: a closing one at the line's end,
	// and an opening one before a character that ends its cluster.
	f.Add([]byte{1, byte(model.Bad)}, "ok \u0600")
	f.Add([]byte{0, byte(model.Plain), byte(model.Dim)}, "a\u0d4e\x00\u2060b")
	// A Prepend character before the box's [ across a tag: the [ goes
	// into the segment before, with its zero-width space.
	f.Add([]byte{1, byte(model.Bad), byte(model.Plain)}, "x\u0600\x00[:::https://evil.example/x]link")
	// A row tview begins at a tag that uniseg does not without it: a
	// thumbs-up and skin tone, or an unassigned pictograph and one, then
	// a tag, then a hyphen and a digit (the first, as the fuzzer found
	// it), and with the tag after the hyphen.
	f.Add([]byte("00"), "\U0001f44d\U0001f3fd\x00-0")
	f.Add([]byte{1, byte(model.Plain), byte(model.Strong)}, "\U0001f44d\U0001f3fd-\x000")
	f.Add([]byte{1, byte(model.Bad), byte(model.Plain)}, "a\U0001faff\U0001f3fd\x00-0")
	f.Fuzz(func(t *testing.T, dress []byte, text string) {
		if utf8.RuneCountInString(text) > 64 {
			return
		}
		pieces := strings.Split(text, "\x00")
		if len(pieces) > 8 {
			return
		}
		colour := len(dress) > 0 && dress[0]&1 == 1
		l := make(model.Line, len(pieces))
		for i, p := range pieces {
			c := model.Plain
			if i+1 < len(dress) {
				c = classes[int(dress[i+1])%len(classes)]
			}
			l[i] = model.Seg{T: termtext.From(p), C: c}
		}
		checkLine(t, l, colour)
		lt := termtext.From(l.Text())
		w := Width(lt) + 4
		checkJoin(t, "Texts", Texts([]termtext.Text{termtext.From("#"), lt, termtext.From("-0")}), w)
		checkJoin(t, "LitLines", LitLines("#\n"+lt.String()+"\n-0"), w)
	})
}
