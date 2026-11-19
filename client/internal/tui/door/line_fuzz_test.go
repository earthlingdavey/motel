package door

import (
	"strings"
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
var classes = []model.Class{model.Plain, model.Label, model.Dim, model.Strong, model.Bad, model.Warn, model.Good}

// dressStyles is every style a line's cells may be drawn in, with
// colour or without: each class's, drawn on "x", and the view's own.
func dressStyles(colour bool) map[tcell.Style]bool {
	out := map[tcell.Style]bool{}
	for _, c := range classes {
		scr := drawLines([]model.Line{{{T: termtext.From("x"), C: c}}}, colour, 4, 1)
		_, st, _ := scr.Get(0, 0)
		out[st] = true
	}
	scr := drawLines(nil, colour, 1, 1)
	_, st, _ := scr.Get(0, 0)
	out[st] = true
	return out
}

// tagInCluster says whether a tag Line writes for l stands where the
// door does not hold it (the accepted row on door.Line in
// client/README.md): inside a grapheme cluster of the line's text as
// tview steps it with the tags left out, or after a character that
// takes the tag's [ into its own cluster (a Prepend one). tview steps a
// cluster at a time and reads a tag only at a cluster's start: a cluster
// cut by one is drawn twice, and a [ taken into one starts no tag.
func tagInCluster(l model.Line, colour bool) bool {
	segs := make([]string, len(l))
	for i, s := range l {
		segs[i] = tagless(s.T.String())
	}
	segs = unbreak(segs)
	joined := strings.Join(segs, "")
	starts := map[int]bool{len(joined): true}
	for at, rest, state := 0, joined, -1; rest != ""; {
		var c string
		starts[at] = true
		c, rest, _, state = uniseg.FirstGraphemeClusterInString(rest, state)
		at += len(c)
	}
	inside := func(at int) bool { return !starts[at] || takesBracket(joined[:at]) }
	at := 0
	for i, s := range segs {
		open, close := tags(l[i].C, colour)
		if open != "" && inside(at) {
			return true
		}
		at += len(s)
		if close != "" && inside(at) {
			return true
		}
	}
	return false
}

// takesBracket says whether the last character of s would take a [
// after it into its grapheme cluster.
func takesBracket(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return uniseg.GraphemeClusterCount(string(r)+"[") == 1
}

// checkLine holds l drawn as the screens draw a line, in a text view,
// with a plain line under it: l is drawn on one row (the line under it
// is on the next); and, where no tag stands inside a grapheme cluster
// (tagInCluster), in no style but the classes' own (no tag of the box's
// read), its styles closed at its end (the line under it is drawn as
// it is alone), and as many cells wide as l.Width says.
func checkLine(t *testing.T, l model.Line, colour bool) {
	t.Helper()
	w := l.Width(Width) + 4
	scr := drawLines([]model.Line{l, {{T: termtext.From("#")}}}, colour, w, 3)
	if row(scr, 1, w) != "#" || row(scr, 2, w) != "" {
		t.Errorf("%s (colour %v) is not drawn on one row: %q, %q, %q", lineName(l), colour, row(scr, 0, w), row(scr, 1, w), row(scr, 2, w))
		return
	}
	if tagInCluster(l, colour) {
		return
	}
	allowed := dressStyles(colour)
	for st := range styles(scr) {
		if !allowed[st] {
			t.Errorf("%s (colour %v) is drawn in a style of no class: %v", lineName(l), colour, st)
			break
		}
	}
	if _, st, _ := scr.Get(0, 1); st != lineStyle() {
		t.Errorf("%s (colour %v) leaves a style open: the line under it is drawn %v", lineName(l), colour, st)
	}
	if got, want := written(scr, 0), l.Width(Width); got != want {
		t.Errorf("%s (colour %v) is drawn %d cells wide, and its Width says %d", lineName(l), colour, got, want)
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

var className = map[model.Class]string{model.Plain: "plain", model.Label: "label", model.Dim: "dim", model.Strong: "strong", model.Bad: "bad", model.Warn: "warn", model.Good: "good"}

// FuzzLine: a whole Line, its segments the box's text (text cut at each
// NUL, which termtext would write as \x00 anyway, at most eight) in any
// order of classes (one byte of dress each; the first byte's low bit
// is colour), as checkLine holds it. The restriction is the accepted
// row on door.Line: where a tag stands inside a grapheme cluster, or
// after a Prepend character, only the one row is held. Seen to fail:
// with Line's unbreak taken out, on a thumbs-up and skin tone after a
// dressed segment (not on one row); with tagless taken out of Line, on
// a URL tag in box text; with Line writing no closing tag, on the
// first seed (a style left open); and with the restriction off
// (tagInCluster always false), on a spacing mark after a dressed
// segment (4 cells drawn, Width 3) and a Prepend character before a
// tag (the tag drawn as text: 10 cells, Width 6).
func FuzzLine(f *testing.F) {
	for i, s := range bad {
		f.Add([]byte{byte(i), byte(model.Bad), byte(model.Plain)}, "ok\x00"+s)
		f.Add([]byte{1, byte(model.Plain), byte(model.Warn), byte(model.Plain)}, "a "+s+"\x00: \x00"+s)
	}
	f.Add([]byte{1, byte(model.Strong), byte(model.Bad)}, "\U0001f44d\x00\U0001f3fd")
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
	})
}
