// Package door is the one way text reaches tview: everything the TUI's
// screens hand a widget is made here.
package door

// The one file that turns text into what tview is given. tview reads
// [...] as tags (colours, styles, regions, and URL tags, which make a
// real OSC 8 hyperlink) in table cells, titles, text views with
// dynamic colours, and the input field's completions. termtext has
// already made every control character visible; this makes every [
// Inert too. tview.Escape is not enough: its pattern skips a tag
// holding a '/', so "[:::https://evil.example/x]" survives it, and the
// documented escaped-tag form ("[" before "]") does not help, since
// the parser's URL state takes every byte up to the ']' as the URL. So
// each [ gets a ZERO WIDTH SPACE after it, which tview's tag parser
// rejects and which it does not draw. Both are tview internals, not
// its API: TestEscapeHolds holds them, in every widget kind used here.
//
// A text view, and an input field's text area, also begin a row
// wherever uniseg reports a mandatory line break, wrapping or not.
// termtext escapes every character that is one, but uniseg v0.4.7
// reports one too after an emoji modifier (U+1F3FB to U+1F3FF) that
// ends a cluster begun by an emoji base or an unassigned
// Extended_Pictographic code point (a thumbs-up and a skin tone,
// U+1F44D U+1F3FD): box text could begin a row of its own, at the
// row's start, where it reads as the client's. So the door asks
// tview's own splitter (WordWrap, with no width to wrap at) where it
// would begin a row inside a text, and writes the last character
// before each such break as an escape, as termtext writes a character
// it will not pass: the thumbs-up, then \U0001f3fd. TestOneRow holds
// it, in every widget kind used here. SetInput alone does not: it puts
// back the person's own keys, as the field drew them when typed.
//
// TestOneDoor holds the rest: no file of the screens' package calls a
// method that hands tview text, calls String, or imports fmt, so text
// reaches a widget only through here.

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// Shown is text made inert for tview. Its field is unexported, so only
// this package makes one (by Inert), and the setters below take
// nothing else: no conversion, alias, generic or assertion elsewhere
// can make one from a string a box sent.
type Shown struct{ s string }

// Inert is t made safe for any tview text that reads tags, and for a
// text view's rows.
func Inert(t termtext.Text) Shown { return Shown{unbreak([]string{tagless(t.String())})[0]} }

// tagless is s with every [ made inert for tview's tag parser.
func tagless(s string) string { return strings.ReplaceAll(s, "[", "[\u200b") }

// unbreak is segs, one row's text in segments as tview reads them (the
// tags between left out: a pair can be split between two), with the
// last character before every row tview would begin inside them
// written as an escape. The row's end counts as inside: what follows
// it, the line feed between two lines, would begin a row after it.
func unbreak(segs []string) []string {
	ascii := true
	for _, s := range segs {
		for i := 0; i < len(s) && ascii; i++ {
			ascii = s[i] < 0x80
		}
	}
	if ascii {
		return segs // no character of ASCII ends a row but the ones termtext escapes
	}
	for {
		at := breaks(strings.Join(segs, ""))
		if len(at) == 0 {
			return segs
		}
		// From the last, so that the offsets before it still hold.
		for k := len(at) - 1; k >= 0; k-- {
			i := at[k]
			for j, s := range segs {
				if i < len(s) {
					r := []rune(s[i:])[0]
					segs[j] = s[:i] + escape(r) + s[i+len(string(r)):]
					break
				}
				i -= len(s)
			}
		}
	}
}

// breaks is the byte offset in row of the last character before each
// row tview would begin inside it or at its end, but for one in ASCII,
// which is left as it is. tview's own splitter says where: "x" stands
// for what follows the row, and the width is no limit, so only a
// mandatory break splits.
func breaks(row string) (at []int) {
	lines := tview.WordWrap(row+"x", 1<<30)
	end := 0
	for _, l := range lines[:len(lines)-1] {
		end += len(l)
		if r := []rune(l); len(r) > 0 && r[len(r)-1] >= 0x80 {
			at = append(at, end-len(string(r[len(r)-1])))
		}
	}
	return at
}

// escape is r as termtext.Quote writes one: \u and four hex digits, or
// \U and eight above U+FFFF.
func escape(r rune) string {
	const hex = "0123456789abcdef"
	n, b := 4, []byte(`\u`)
	if r > 0xffff {
		n, b = 8, []byte(`\U`)
	}
	for i := n - 1; i >= 0; i-- {
		b = append(b, hex[r>>(4*i)&0xf])
	}
	return string(b)
}

// Lit is the TUI's own words, through the same door: a [ in them (the
// help's "[<version>]") must not start a tag either.
func Lit(s string) Shown { return Inert(termtext.From(s)) }

// TerminalColours makes tview draw in the terminal's own colours
// rather than its black theme.
func TerminalColours() {
	d := tcell.ColorDefault
	tview.Styles = tview.Theme{
		PrimitiveBackgroundColor: d, ContrastBackgroundColor: d, MoreContrastBackgroundColor: d,
		BorderColor: d, TitleColor: d, GraphicsColor: d, PrimaryTextColor: d, SecondaryTextColor: d,
		TertiaryTextColor: d, InverseTextColor: d, ContrastSecondaryTextColor: d,
	}
}

// tags are the classes' dress. Colour only repeats a mark or a word.
func tags(c model.Class, colour bool) (string, string) {
	switch c {
	case model.Dim:
		return "[::d]", "[::-]"
	case model.Strong:
		return "[::b]", "[::-]"
	case model.Bad:
		if colour {
			return "[red::b]", "[-::-]"
		}
		return "[::b]", "[::-]"
	case model.Warn:
		if colour {
			return "[yellow]", "[-]"
		}
	case model.Good:
		if colour {
			return "[green]", "[-]"
		}
	}
	return "", ""
}

// Line is a model line as tview text.
func Line(l model.Line, colour bool) Shown {
	var b strings.Builder
	segs := make([]string, len(l))
	for i, s := range l {
		segs[i] = tagless(s.T.String())
	}
	for i, t := range unbreak(segs) {
		open, close := tags(l[i].C, colour)
		b.WriteString(open)
		b.WriteString(t)
		b.WriteString(close)
	}
	return Shown{b.String()}
}

// Lines is model lines as one tview text.
func Lines(ls []model.Line, colour bool) Shown {
	var b strings.Builder
	for i, l := range ls {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(Line(l, colour).s)
	}
	return Shown{b.String()}
}

// Texts is box lines as one tview text: a log tail, a JSON body.
func Texts(ts []termtext.Text) Shown {
	var b strings.Builder
	for i, t := range ts {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(Inert(t).s)
	}
	return Shown{b.String()}
}

// SetText sets a text view's text when it changed, so a redraw keeps
// its scroll and costs nothing.
func SetText(tv *tview.TextView, s Shown) {
	if tv.GetText(false) != s.s {
		tv.SetText(s.s)
	}
}

// Cell is a table cell of text.
func Cell(t termtext.Text) *tview.TableCell { return tview.NewTableCell(Inert(t).s) }

// Head is a table heading of the TUI's own.
func Head(s string) *tview.TableCell {
	return tview.NewTableCell(Lit(s).s).SetSelectable(false).SetAttributes(tcell.AttrBold)
}

// Width is how many cells t takes on screen.
func Width(t termtext.Text) int { return tview.TaggedStringWidth(Inert(t).s) }

// AsIs says whether t is drawn as it is, character for character: the
// cells tview draws for it in a text view, as the overlays are drawn,
// read back from a screen, are t. It asks tview, as Width and unbreak
// do, rather than listing characters: one the door writes as an escape
// is drawn as the escape, and tview draws nothing for one of no width
// (a soft hyphen, a zero-width space), which is then not there to
// select. The zero-width space the door puts after a [ is one of
// these, so a [ is drawn as it is.
func AsIs(t termtext.Text) bool {
	s := Inert(t).s
	w := tview.TaggedStringWidth(s)
	if w == 0 {
		return t.String() == ""
	}
	var scr tcell.SimulationScreen
	select {
	case scr = <-screens:
	default:
		scr = tcell.NewSimulationScreen("UTF-8")
		_ = scr.Init()
	}
	defer func() {
		select {
		case screens <- scr:
		default:
		}
	}()
	scr.SetSize(w, 1)
	tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false).SetText(s)
	tv.SetRect(0, 0, w, 1)
	tv.Draw(scr)
	var b strings.Builder
	for x := 0; x < w; {
		c, _, cw := scr.Get(x, 0)
		b.WriteString(c)
		x += max(cw, 1)
	}
	return b.String() == t.String()
}

// screens are the screens AsIs has drawn on, kept to draw on again,
// each taken by one AsIs at a time.
var screens = make(chan tcell.SimulationScreen, 4)

// LitLines is the TUI's own lines: each made safe as Lit makes it,
// the line breaks kept.
func LitLines(s string) Shown {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		ls[i] = Lit(l).s
	}
	return Shown{strings.Join(ls, "\n")}
}

// SetTitle titles a framed widget with the TUI's own words.
func SetTitle(b *tview.Box, s string) { b.SetTitle(" " + Lit(s).s + " ") }

// SetCompletions gives an input field its completions, each made
// inert.
func SetCompletions(f *tview.InputField, complete func(string) []string) {
	f.SetAutocompleteFunc(func(text string) []string { return completions(complete(text)) })
	// Taken with Tab only: moving through the list changes nothing
	// typed, and Enter is the screen's, which runs what is typed.
	f.SetAutocompletedFunc(func(text string, _ int, source int) bool {
		if source != tview.AutocompletedTab {
			return false
		}
		f.SetText(text)
		return true
	})
}

// completions are the command line's entries, made inert.
func completions(entries []string) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = Lit(e).s
	}
	return out
}

// SetInput puts the person's own text back into an input field: made
// inert for tags only, so that Typed gives back what was typed. It is
// the person's keys, which the field drew as they were typed.
func SetInput(f *tview.InputField, s string) { f.SetText(tagless(termtext.From(s).String())) }

// SetLabel labels an input field.
func SetLabel(f *tview.InputField, s string) { f.SetLabel(Lit(s).s) }

// PrintAt writes the TUI's own words on the screen directly.
func PrintAt(scr tcell.Screen, x, y, w int, s string) {
	tview.Print(scr, Lit(s).s, x, y, w, tview.AlignLeft, tcell.ColorDefault)
}

// Typed is what the person typed into an input field: the inverse of
// SetInput on the person's own keys, which hold no tag of tview's.
func Typed(f *tview.InputField) string { return strings.ReplaceAll(f.GetText(), "\u200b", "") }
