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
// uniseg, as tview would, where tview would begin a row inside a text,
// a tag beside a cluster or not (breaks), and writes the last character
// before each such break as an escape, as termtext writes a character
// it will not pass: the thumbs-up, then \U0001f3fd. TestOneRow holds
// it, in every widget kind used here, and TestPairsBesideATag and
// FuzzLine beside a tag. SetInput alone does not: it puts back the
// person's own keys, as the field drew them when typed.
//
// TestOneDoor holds the rest: no file of the screens' package calls a
// method that hands tview text, calls String, or imports fmt, so text
// reaches a widget only through here.

import (
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"

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
		// Each segment a break falls in is written once, its breaks in
		// order (at is in order).
		k, base := 0, 0
		out := make([]string, 0, len(segs))
		for _, s := range segs {
			if k < len(at) && at[k] < base+len(s) {
				var b strings.Builder
				from := 0
				for ; k < len(at) && at[k] < base+len(s); k++ {
					i := at[k] - base
					r, n := utf8.DecodeRuneInString(s[i:])
					b.WriteString(s[from:i])
					b.WriteString(escape(r))
					from = i + n
				}
				b.WriteString(s[from:])
				base += len(s)
				out = append(out, b.String())
				continue
			}
			base += len(s)
			out = append(out, s)
		}
		segs = out
	}
}

// breaks is the byte offset in row of the last character, above ASCII,
// of each cluster after which tview would begin a row, inside row or at
// its end ("x" stands for what follows it), with a tag after the cluster
// or not. tview's step (strings.go, v0.42.0) judges the break after a
// cluster a tag follows by that cluster and the one character after the
// tag, as if the text ended there, and Line may write a tag after any
// cluster: so breaks asks uniseg that of each. Without a tag tview
// judges the break by the text after it too, and uniseg reads a second
// character after a cluster only to take a break away (LB25: none
// before a hyphen a digit follows), so each break tview begins with no
// tag is found too: TestOneRow, TestClassesOnOneRow and FuzzInert hold
// that, drawn. So a thumbs-up and skin tone before "-0" is written as
// an escape, as before any other character, in a Line and in Inert
// alike, and Width agrees. A break after ASCII is left as it is: no
// character of ASCII ends a row but the ones termtext escapes.
func breaks(row string) (at []int) {
	row += "x"
	state := -1
	for i, rest := 0, row; rest != ""; {
		pre := state
		var c string
		c, rest, _, state = uniseg.StepString(rest, state)
		if r, n := utf8.DecodeLastRuneInString(c); rest != "" && r >= 0x80 {
			_, m := utf8.DecodeRuneInString(rest)
			if _, _, b, _ := uniseg.StepString(row[i:i+len(c)+m], pre); b&uniseg.MaskLine == uniseg.LineMustBreak {
				at = append(at, i+len(c)-n)
			}
		}
		i += len(c)
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
	case model.LoudBad:
		// Reversed, so it reads without colour too.
		if colour {
			return "[red::rb]", "[-::-]"
		}
		return "[::rb]", "[::-]"
	case model.LoudWarn:
		if colour {
			return "[yellow::r]", "[-::-]"
		}
		return "[::r]", "[::-]"
	}
	return "", ""
}

// Line is a model line as tview text, each segment dressed by tags.
// tview steps a grapheme cluster at a time and reads a tag only at a
// cluster's start, so the door puts each tag between two clusters of
// the line's text (whole: a character that joins the one before it
// goes with it, into its segment and its style), and a zero-width
// space between a tag and a Prepend character before it (dressTag).
// A character that joins the one before it across two segments is so
// drawn in the earlier segment's style: a box's free text directly
// before a dressed segment can take that segment's first character, and
// no test holds the screens to that. FuzzLine and TestClassesBesideATag
// hold every line to one row, its styles, its styles closed at its end,
// and Width; TestPairsBesideATag to one row with two characters beside
// a tag.
func Line(l model.Line, colour bool) Shown {
	var b strings.Builder
	segs := make([]string, len(l))
	for i, s := range l {
		segs[i] = tagless(s.T.String())
	}
	for i, t := range whole(unbreak(segs)) {
		open, close := tags(l[i].C, colour)
		dressTag(&b, open)
		b.WriteString(t)
		dressTag(&b, close)
	}
	return Shown{b.String()}
}

// dressTag writes tag after b, with a zero-width space before it when
// b ends in a character that takes a [ after it into its own grapheme
// cluster (a Prepend one), where tview would draw the tag as text. The
// space is a cluster of its own whatever is beside it, and of no width.
func dressTag(b *strings.Builder, tag string) {
	if tag == "" {
		return
	}
	if r, _ := utf8.DecodeLastRuneInString(b.String()); r >= 0x80 && uniseg.GraphemeClusterCount(string(r)+"[") == 1 {
		b.WriteString("\u200b")
	}
	b.WriteString(tag)
}

// whole is segs with each grapheme cluster of their joined text in the
// segment it begins in: tview reads a tag only between two clusters, so
// one a tag cuts is drawn twice. The zero-width space tagless writes
// after a [ goes with it, though it is a cluster of its own, so that no
// tag comes between them (a [ ends a cluster a Prepend character
// begins in the segment before). Each end is moved to the start of the
// next cluster and the joined text sliced there, once each.
func whole(segs []string) []string {
	ascii := true
	for _, s := range segs {
		for i := 0; i < len(s) && ascii; i++ {
			ascii = s[i] < 0x80
		}
	}
	if ascii || len(segs) < 2 {
		return segs // a cluster of ASCII is one character: termtext escapes CR and LF
	}
	joined := strings.Join(segs, "")
	out := make([]string, len(segs))
	j, start, end, bracket, state := 0, 0, len(segs[0]), false, -1
	for at, rest := 0, joined; rest != ""; {
		var c string
		c, rest, _, state = uniseg.FirstGraphemeClusterInString(rest, state)
		if !bracket || c != "\u200b" {
			for j < len(segs)-1 && end <= at {
				out[j], start = joined[start:at], at
				j++
				end += len(segs[j])
			}
		}
		bracket = strings.HasSuffix(c, "[")
		at += len(c)
	}
	for ; j < len(segs); j++ {
		out[j], start = joined[start:], len(joined)
	}
	return out
}

// rowEnd is what ends each line but the last in Lines, Texts and
// LitLines: a line feed, then a zero-width space, which tview draws
// nowhere. uniseg v0.4.7 takes no break before a hyphen a digit follows
// (LB25, as it reads it), the mandatory one after a line feed among
// them, so a line that began "-1" would be drawn on the row above it;
// after the space, a cluster of its own, it begins its own row.
// TestLinesEachOnARow holds it, in each of the three.
const rowEnd = "\n\u200b"

// Lines is model lines as one tview text.
func Lines(ls []model.Line, colour bool) Shown {
	var b strings.Builder
	for i, l := range ls {
		if i > 0 {
			b.WriteString(rowEnd)
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
			b.WriteString(rowEnd)
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
	return Shown{strings.Join(ls, rowEnd)}
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
