package door

import (
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rivo/uniseg"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// sim is a simulation screen of w x h cells.
func sim(w, h int) tcell.Screen {
	scr := tcell.NewSimulationScreen("UTF-8")
	_ = scr.Init()
	scr.SetSize(w, h)
	return scr
}

// row is row y of scr as drawn, w cells of it, trailing spaces cut.
func row(scr tcell.Screen, y, w int) string {
	var b strings.Builder
	for x := range w {
		s, _, _ := scr.Get(x, y)
		b.WriteString(s)
	}
	return strings.TrimRight(b.String(), " ")
}

// extPic says whether r is Extended_Pictographic as uniseg, and so
// tview, reads it: only such a character joins one of its own into one
// grapheme cluster with a zero width joiner between (GB11).
func extPic(r rune) bool {
	x := string(r)
	return uniseg.GraphemeClusterCount(x+"\u200d"+x) == 1 && uniseg.GraphemeClusterCount("a"+x) == 2 && uniseg.GraphemeClusterCount(x+"a") == 2
}

// between says whether r can stand between an emoji base and its
// modifier and keep the pair's effect: a combining mark or a format
// character (the line breaking class CM, and the zero width joiner,
// are among these), or anything that joins the cluster before it.
func between(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Mc, unicode.Me, unicode.Cf, unicode.Cc) || uniseg.GraphemeClusterCount("a"+string(r)) == 1
}

// sweep is every case TestOneRow draws, each between < and >: every
// code point alone (as termtext leaves it or escapes it); every
// Extended_Pictographic character with each emoji modifier after it;
// and the thumbs-up, then each code point that can stand between
// (between), then a modifier.
func sweep() (cases []string) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		cases = append(cases, "<"+string(r)+">")
		if extPic(r) {
			for m := rune(0x1f3fb); m <= 0x1f3ff; m++ {
				cases = append(cases, "<"+string(r)+string(m)+">")
			}
		}
		if between(r) {
			cases = append(cases, "<\U0001f44d"+string(r)+"\U0001f3fd>")
		}
	}
	return cases
}

// kind is a kind of widget the door serves. draw puts the cases, one
// after another as one text and handed it as the screens hand text, in
// a widget on scr; the text begins on row top, and at its first cell
// when start (a title is centred).
type kind struct {
	name  string
	top   int
	start bool
	draw  func(scr tcell.Screen, cases []string)
}

// one says whether k draws the cases on one row: the text is on row
// top, and the row under it holds none of it. A row begun inside the
// text is drawn there, or, in an input field, which shows the row of
// its cursor, on top: the cursor is put at the start of the text's
// last row.
func (k kind) one(cases []string) bool {
	const w = 32
	scr := sim(w, k.top+3)
	k.draw(scr, cases)
	r := row(scr, k.top, w)
	return strings.Contains(r, "<") && (!k.start || strings.HasPrefix(r, "<")) && !strings.ContainsAny(row(scr, k.top+1, w), "<>")
}

// kinds are every kind of widget the door serves.
func kinds() []kind {
	on := func(p tview.Primitive, scr tcell.Screen) {
		w, h := scr.Size()
		p.SetRect(0, 0, w, h)
		p.Draw(scr)
	}
	view := func(name string, set func(tv *tview.TextView, cases []string)) kind {
		return kind{name, 0, true, func(scr tcell.Screen, cases []string) {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
			set(tv, cases)
			on(tv, scr)
		}}
	}
	frame := func(name string, set func(tv *tview.TextView, s string)) kind {
		return kind{name, 0, false, func(scr tcell.Screen, cases []string) {
			tv := tview.NewTextView()
			tv.SetBorder(true)
			set(tv, strings.Join(cases, ""))
			w, _ := scr.Size()
			tv.SetRect(0, 0, w, 2)
			tv.Draw(scr)
		}}
	}
	return []kind{
		// Each case in two segments, < and the rune after it, then the
		// rest: a pair the case holds is cut between two segments and
		// their tags.
		view("text view, a line", func(tv *tview.TextView, cases []string) {
			var l model.Line
			for _, c := range cases {
				_, n := utf8.DecodeRuneInString(c[1:])
				l = append(l, model.Seg{T: termtext.From(c[:1+n]), C: model.Bad}, model.Seg{T: termtext.From(c[1+n:])})
			}
			SetText(tv, Lines([]model.Line{l}, true))
		}),
		view("text view, box lines", func(tv *tview.TextView, cases []string) {
			SetText(tv, Texts([]termtext.Text{termtext.From(strings.Join(cases, ""))}))
		}),
		view("text view, the TUI's own lines", func(tv *tview.TextView, cases []string) {
			// The TUI's own line feed ends its line.
			SetText(tv, LitLines(strings.ReplaceAll(strings.Join(cases, ""), "\n", "")))
		}),
		{"table cell", 0, true, func(scr tcell.Screen, cases []string) {
			on(tview.NewTable().SetCell(0, 0, Cell(termtext.From(strings.Join(cases, "")))), scr)
		}},
		{"completions", 1, true, func(scr tcell.Screen, cases []string) {
			f := tview.NewInputField()
			SetCompletions(f, func(string) []string { return []string{strings.Join(cases, "")} })
			f.Focus(func(tview.Primitive) {})
			f.SetText("x")
			f.Autocomplete()
			on(f, scr)
		}},
		// What Tab puts in the field: a completion as SetCompletions
		// makes it. The field begins a row as a text view does, and
		// shows the row its cursor is on.
		{"completion taken", 0, true, func(scr tcell.Screen, cases []string) {
			f := tview.NewInputField()
			f.SetText(completions([]string{strings.Join(cases, "")})[0])
			on(f, scr)
			f.InputHandler()(tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone), func(tview.Primitive) {})
			on(f, scr)
		}},
		{"input field's label", 0, true, func(scr tcell.Screen, cases []string) {
			f := tview.NewInputField()
			SetLabel(f, strings.Join(cases, ""))
			on(f, scr)
		}},
		frame("framed title, the TUI's own", func(tv *tview.TextView, s string) { SetTitle(tv.Box, s) }),
		{"printed", 0, true, func(scr tcell.Screen, cases []string) {
			w, _ := scr.Size()
			PrintAt(scr, 0, 0, w, strings.Join(cases, ""))
		}},
	}
}

// TestOneRow: no text the door hands tview begins a row inside it. A
// text view, and an input field's text area, begins one wherever
// uniseg reports a mandatory line break, wrapping or not: termtext
// escapes every character that is one (line feed, carriage return,
// vertical tab, form feed, NEL, U+2028, U+2029), and uniseg v0.4.7
// also reports one after an emoji modifier that ends a cluster begun
// by an emoji base or an unassigned Extended_Pictographic code point.
// tview itself draws every case of sweep, a few thousand at a time, in
// every kind of widget the door serves (a quarter of them in each of
// four parts at once), and they are drawn on one row.
func TestOneRow(t *testing.T) {
	cases := sweep()
	const batch, parts = 8000, 4
	for _, k := range kinds() {
		for p := range parts {
			t.Run(k.name+"/"+strconv.Itoa(p+1), func(t *testing.T) {
				t.Parallel()
				for i := p * batch; i < len(cases); i += parts * batch {
					b := cases[i:min(i+batch, len(cases))]
					if k.one(b) {
						continue
					}
					named := false
					for _, c := range b {
						if !k.one([]string{c}) {
							t.Errorf("%s is not drawn on one row", runes(c))
							named = true
							break
						}
					}
					if !named {
						t.Errorf("the cases from %s to %s are not drawn on one row", runes(b[0]), runes(b[len(b)-1]))
					}
				}
			})
		}
	}
}

// runes names a case's code points.
func runes(c string) string {
	var out []string
	for _, r := range c {
		out = append(out, "U+"+strings.ToUpper(strconv.FormatInt(int64(r), 16)))
	}
	return strings.Join(out, " ")
}

// TestAsIsIsWhatIsDrawn: AsIs says a text is drawn as it is exactly
// when the cells a text view draws for it (the overlays' kind, given
// the text as the screens give it, by Lines), read back from the
// simulation screen, are the text: for 'x, then each code point, then
// y' (as termtext leaves it or escapes it), a few thousand at a time
// on one screen, a quarter of them in each of four parts at once.
// tview draws nothing for a character of no width (a soft hyphen, a
// zero-width space, the one the door puts after a [), and draws the
// escape for one the door escapes.
func TestAsIsIsWhatIsDrawn(t *testing.T) {
	var cases []termtext.Text
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r < 0xd800 || r > 0xdfff {
			cases = append(cases, termtext.From("'x"+string(r)+"y'"))
		}
	}
	const batch, parts, w = 2000, 4, 24
	for p := range parts {
		t.Run(strconv.Itoa(p+1), func(t *testing.T) {
			t.Parallel()
			scr := sim(w, batch)
			wrong := 0
			for i := p * batch; i < len(cases); i += parts * batch {
				b := cases[i:min(i+batch, len(cases))]
				ls := make([]model.Line, len(b))
				for j, c := range b {
					ls[j] = model.Line{{T: c}}
				}
				tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
				SetText(tv, Lines(ls, false))
				scr.Clear()
				tv.SetRect(0, 0, w, batch)
				tv.Draw(scr)
				for y, c := range b {
					drawn := cellsOf(scr, y, w)
					if as := drawn == c.String(); AsIs(c) != as {
						if wrong++; wrong <= 20 {
							t.Errorf("%s: drawn %q for %q, but AsIs says %v", runes(c.String()), drawn, c.String(), !as)
						}
					}
				}
			}
			if wrong > 20 {
				t.Errorf("and %d more", wrong-20)
			}
		})
	}
}

// cellsOf is row y of scr as its cells hold it, w cells of it: a wide
// character once, as the screen says how wide each is; trailing spaces
// cut.
func cellsOf(scr tcell.Screen, y, w int) string {
	var b strings.Builder
	for x := 0; x < w; {
		s, _, cw := scr.Get(x, y)
		b.WriteString(s)
		x += max(cw, 1)
	}
	return strings.TrimRight(b.String(), " ")
}
