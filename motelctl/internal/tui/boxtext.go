package tui

// The one file that turns text into what tview is given. tview reads
// [...] as tags (colours, styles, regions, and URL tags, which make a
// real OSC 8 hyperlink) in table cells, titles, text views with
// dynamic colours, and the input field's completions. termtext has
// already made every control character visible; this makes every [
// inert too. tview.Escape is not enough: its pattern skips a tag
// holding a '/', so "[:::https://evil.example/x]" survives it, and the
// documented escaped-tag form ("[" before "]") does not help, since
// the parser's URL state takes every byte up to the ']' as the URL. So
// each [ gets a ZERO WIDTH SPACE after it, which tview's tag parser
// rejects and which it does not draw. Both are tview internals, not
// its API: TestEscapeHolds holds them, in every widget kind used here.
//
// TestOneDoor holds the rest: no other file of this package calls a
// method that hands tview text, calls String, or imports fmt, so text
// reaches a widget only through here.

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

// shown is text made inert for tview: only this file makes one (by
// inert), and the setters below take nothing else, so a string from a
// box cannot reach a widget unescaped without a conversion here
// (TestOneDoor fails on a conversion to shown anywhere else).
type shown string

// inert is t made safe for any tview text that reads tags.
func inert(t termtext.Text) shown { return shown(strings.ReplaceAll(t.String(), "[", "[\u200b")) }

// lit is the TUI's own words, through the same door: a [ in them (the
// help's "[<version>]") must not start a tag either.
func lit(s string) shown { return inert(termtext.From(s)) }

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

// line is a model line as tview text.
func line(l model.Line, colour bool) shown {
	var b strings.Builder
	for _, s := range l {
		open, close := tags(s.C, colour)
		b.WriteString(open)
		b.WriteString(string(inert(s.T)))
		b.WriteString(close)
	}
	return shown(b.String())
}

// lines is model lines as one tview text.
func lines(ls []model.Line, colour bool) shown {
	var b strings.Builder
	for i, l := range ls {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(string(line(l, colour)))
	}
	return shown(b.String())
}

// texts is box lines as one tview text: a log tail, a JSON body.
func texts(ts []termtext.Text) shown {
	var b strings.Builder
	for i, t := range ts {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(string(inert(t)))
	}
	return shown(b.String())
}

// setText sets a text view's text when it changed, so a redraw keeps
// its scroll and costs nothing.
func setText(tv *tview.TextView, s shown) {
	if tv.GetText(false) != string(s) {
		tv.SetText(string(s))
	}
}

// cell is a table cell of text.
func cell(t termtext.Text) *tview.TableCell { return tview.NewTableCell(string(inert(t))) }

// head is a table heading of the TUI's own.
func head(s string) *tview.TableCell {
	return tview.NewTableCell(string(lit(s))).SetSelectable(false).SetAttributes(tcell.AttrBold)
}

// width is how many cells t takes on screen.
func width(t termtext.Text) int { return tview.TaggedStringWidth(string(inert(t))) }

// setInput puts the person's own text back into an input field.
func setInput(f *tview.InputField, s string) { f.SetText(string(lit(s))) }

// setLabel labels an input field.
func setLabel(f *tview.InputField, s string) { f.SetLabel(string(lit(s))) }

// printAt writes the TUI's own words on the screen directly.
func printAt(scr tcell.Screen, x, y, w int, s string) {
	tview.Print(scr, string(lit(s)), x, y, w, tview.AlignLeft, tcell.ColorDefault)
}

// typed is what the person typed into an input field: the inverse of
// lit on the person's own keys, which hold no tag of tview's.
func typed(f *tview.InputField) string { return strings.ReplaceAll(f.GetText(), "\u200b", "") }
