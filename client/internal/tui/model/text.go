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
	// LoudBad and LoudWarn are Bad and Warn reversed: the title bar's
	// mark while what fires is new (Session.Mark), reversed with colour
	// or without.
	LoudBad
	LoudWarn
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

// rawTime reads a time the box carries raw, as the record has it (one
// an older moteld wrote can hold its filter's marker there).
func rawTime(raw json.RawMessage) (time.Time, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// noTime says a raw time is missing or null: none given, which is not
// one that does not read.
func noTime(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func hms(t time.Time) string { return t.UTC().Format("15:04:05") }

// clock is a box's time as a time of day in UTC (with its seconds when
// secs), after its date when it is not today ("11-12 14:00") and its
// year too when it is not this year ("2025-11-12 14:00"). Today and
// this year are now's by the date in UTC, the zone every time shown is
// in, not by this machine's own.
func clock(t, now time.Time, secs bool) string {
	l := "15:04"
	if secs {
		l = "15:04:05"
	}
	return t.UTC().Format(datePart(t, now, false) + l)
}

// date is a box's time as a date and a time of day in UTC, the year
// before them when it is not this year: a table's column of dates.
func date(t, now time.Time) string { return t.UTC().Format(datePart(t, now, true) + "15:04") }

func datePart(t, now time.Time, always bool) string {
	t, now = t.UTC(), now.UTC()
	switch {
	case t.Year() != now.Year():
		return "2006-01-02 "
	case always || t.YearDay() != now.YearDay():
		return "01-02 "
	}
	return ""
}

// since is how long before now a box's time is ("12m ago"), or after
// it, by a second or more ("3m from now"): a box whose clock is ahead
// of this machine's.
func since(t, now time.Time) string {
	if d := t.Sub(now); d >= time.Second {
		return Ago(d) + " from now"
	}
	return Ago(now.Sub(t)) + " ago"
}

// span is how long from start to end, two box times, and false when
// either is missing or the end is before the start: no length is said
// then.
func span(start, end time.Time) (string, bool) {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return "", false
	}
	return secs(end.Sub(start)), true
}

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

// Width is the line's width in cells, as cells measures the whole line
// as the screens draw it: a wide character takes two, a combining mark
// none, and the door writes some characters as escapes (an emoji
// modifier as \U0001f3fd), so a line can be wider than its runes.
func (l Line) Width(cells Cells) int { return cells(termtext.From(l.Text())) }

// Cut is the line cut to n cells as cells measures them, the cut marked
// with "…" as its last cell; a line that fits is as it is. The cut is
// at the last character after which the line, with "…", still fits.
func (l Line) Cut(n int, cells Cells) Line {
	t := l.Text()
	if cells(termtext.From(t)) <= n {
		return l
	}
	fits := func(end int) bool { return cells(termtext.From(t[:end]+"…")) <= n }
	if !fits(0) {
		return nil
	}
	// Where each character begins; a longer start never takes fewer
	// cells, so the last that fits is found by halves.
	var at []int
	for i := range t {
		at = append(at, i)
	}
	lo, hi := 0, len(at)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(at[mid]) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	end := at[lo]
	var out Line
	for _, s := range l {
		st := s.T.String()
		if end <= len(st) {
			return append(out, Seg{T: termtext.From(st[:end] + "…"), C: s.C})
		}
		out = append(out, s)
		end -= len(st)
	}
	return out
}
