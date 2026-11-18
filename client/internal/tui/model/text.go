package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// Class is how a run of text is dressed. Colour, when on, only repeats
// what a mark or a word already says.
type Class int

// The classes.
const (
	Plain Class = iota
	Label
	Dim
	Strong
	Bad
	Warn
	Good
)

// Seg is a run of text. Every one is termtext.Text, the client's own
// words as well as the box's, so the screens have one kind of text to
// make safe for their library, and one place to do it.
type Seg struct {
	T termtext.Text
	C Class
}

// Line is one row of a screen.
type Line []Seg

// own is the client's own words.
func own(s string) Seg { return Seg{T: termtext.From(s)} }

func ownC(s string, c Class) Seg { return Seg{T: termtext.From(s), C: c} }

// boxT is a box's free-text value, labelled by the line it is on and
// capped, so it cannot run on into rows that look like the client's.
func boxT(s string) Seg { return Seg{T: Capped(s, LineMax)} }

// quoted is a box's value inside the client's sentence.
func quoted(s string) Seg { return Seg{T: termtext.Quote(s)} }

// LineMax is the most of a box's value a labelled line shows, as the
// commands print it; TailMax is a log tail or probe body line's.
const (
	LineMax = 200
	TailMax = 2000
)

// Capped is s made safe and cut to max runes, the cut marked.
func Capped(s string, max int) termtext.Text {
	t := termtext.From(s).String()
	n := 0
	for i := range t {
		if n == max {
			return termtext.From(t[:i] + "…[cut]")
		}
		n++
	}
	return termtext.From(t)
}

// Text is the line's text, unstyled: for tests and widths.
func (l Line) Text() string {
	var b strings.Builder
	for _, s := range l {
		b.WriteString(s.T.String())
	}
	return b.String()
}

// Mark is a row's sign, which carries its meaning without colour.
type Mark int

// The marks, in the order the apps are sorted by attention.
const (
	MarkBroken  Mark = iota // ✖ broken now
	MarkLook                // ▲ look
	MarkUnknown             // ? cannot tell
	MarkMoving              // ⟳ deploying
	MarkNone                // running
	MarkNothing             // · nothing to show
)

// Glyph is the mark as shown.
func (m Mark) Glyph() string {
	return [...]string{"✖", "▲", "?", "⟳", " ", "·"}[m]
}

// Class is the colour that repeats the mark.
func (m Mark) Class() Class {
	return [...]Class{Bad, Warn, Warn, Plain, Plain, Dim}[m]
}

// Ago says how old something is, for a list: 4s, 12m, 2h, 3d.
func Ago(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// rawTime reads a time the box carries raw (the response filter may
// have replaced it).
func rawTime(raw json.RawMessage) (time.Time, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func hm(t time.Time) string  { return t.UTC().Format("15:04") }
func hms(t time.Time) string { return t.UTC().Format("15:04:05") }

func mib(b float64) string { return strconv.FormatFloat(b/(1<<20), 'f', 0, 64) + " MiB" }

func pct(f float64) string {
	p := f * 100
	if p >= 10 || p == 0 {
		return strconv.FormatFloat(p, 'f', 0, 64) + "%"
	}
	return strconv.FormatFloat(p, 'f', 1, 64) + "%"
}

func num(f float64, prec int) string { return strconv.FormatFloat(f, 'f', prec, 64) }

// short is a version cut to the 12 runes a list has room for, the cut
// marked; a version is a token of the box's grammar.
func short(v string) string {
	if r := []rune(v); len(r) > 12 {
		return string(r[:11]) + "…"
	}
	return v
}

func secs(d time.Duration) string {
	if d < 10*time.Second {
		return num(d.Seconds(), 1) + " s"
	}
	return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + " s"
}

// Width is the line's width in runes: every rune the client shows takes
// one cell, but for the box's own, which tview measures.
func (l Line) Width() int { return len([]rune(l.Text())) }

// Cut is the line cut to n runes, the cut marked with "…".
func (l Line) Cut(n int) Line {
	if l.Width() <= n {
		return l
	}
	var out Line
	left := max(n-1, 0)
	for _, s := range l {
		r := []rune(s.T.String())
		if len(r) >= left {
			out = append(out, Seg{T: termtext.From(string(r[:left]) + "…"), C: s.C})
			return out
		}
		out = append(out, s)
		left -= len(r)
	}
	return out
}
