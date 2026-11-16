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
// TestOneDoor holds the rest: no file of the screens' package calls a
// method that hands tview text, calls String, or imports fmt, so text
// reaches a widget only through here.

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

// Shown is text made inert for tview. Its field is unexported, so only
// this package makes one (by Inert), and the setters below take
// nothing else: no conversion, alias, generic or assertion elsewhere
// can make one from a string a box sent.
type Shown struct{ s string }

// Inert is t made safe for any tview text that reads tags.
func Inert(t termtext.Text) Shown { return Shown{strings.ReplaceAll(t.String(), "[", "[\u200b")} }

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
	for _, s := range l {
		open, close := tags(s.C, colour)
		b.WriteString(open)
		b.WriteString(Inert(s.T).s)
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

// SetInput puts the person's own text back into an input field.
func SetInput(f *tview.InputField, s string) { f.SetText(Lit(s).s) }

// SetLabel labels an input field.
func SetLabel(f *tview.InputField, s string) { f.SetLabel(Lit(s).s) }

// PrintAt writes the TUI's own words on the screen directly.
func PrintAt(scr tcell.Screen, x, y, w int, s string) {
	tview.Print(scr, Lit(s).s, x, y, w, tview.AlignLeft, tcell.ColorDefault)
}

// Typed is what the person typed into an input field: the inverse of
// Lit on the person's own keys, which hold no tag of tview's.
func Typed(f *tview.InputField) string { return strings.ReplaceAll(f.GetText(), "\u200b", "") }
