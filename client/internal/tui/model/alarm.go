package model

import (
	"strconv"
	"time"
)

// The alarm (#576): the title bar's mark of what fires, its movement
// while something is new, the bell, and what a key sees. It takes the
// watch's answers (watch.go) and carries only the client's own words
// and numbers: no text a box sent reaches the mark or decides the bell
// but by the count it makes.

const (
	// MoveFor is how long the mark moves after an answer brings a new
	// alert: reversed on alternate seconds of the wall clock, then held
	// reversed, still, until seen. Five seconds needs no switch to stop
	// it (WCAG 2.2.2).
	MoveFor = 5 * time.Second
	// RingFloor is the least time between two bells. A bell owed inside
	// it is held, and rung once when it ends.
	RingFloor = 60 * time.Second
	// SeenAfter is how long after a new alert arrives a key first sees
	// it: a key already on its way (typing on the : line) does not.
	SeenAfter = time.Second
)

// alarm is this session's own: what arrived and is not yet seen, when
// the newest arrived, whether a bell is owed, and when one last rang.
type alarm struct {
	// unseen is what arrived and is not yet seen, with when it arrived.
	unseen map[unseenKey]time.Time
	newAt  time.Time
	owed   bool
	rungAt time.Time
}

type unseenKey struct{ src, key string }

// took is a known answer of the watch's read src: set is what it now
// counts, fresh what is new in it. What was not yet seen and no longer
// fires is no longer new; what is new is not yet seen, and owes a bell.
func (a *alarm) took(src string, set map[string]bool, fresh []string, at time.Time) {
	for k := range a.unseen {
		if k.src == src && !set[k.key] {
			delete(a.unseen, k)
		}
	}
	if len(fresh) == 0 {
		return
	}
	if a.unseen == nil {
		a.unseen = map[unseenKey]time.Time{}
	}
	for _, k := range fresh {
		a.unseen[unseenKey{src, k}] = at
	}
	a.newAt, a.owed = at, true
}

// forget drops what arrived and the bell owed: the session dropped what
// the box said. When a bell last rang is kept, so the floor holds.
func (a *alarm) forget() { a.unseen, a.owed, a.newAt = nil, false, time.Time{} }

// Ring says whether the bell rings now, and takes it: owed, and
// RingFloor since the last. A bell owed inside the floor is held, and
// rung at the first frame after it ends (the clock's tick draws one a
// second), unless by then everything it was owed for (what arrived
// after the last bell) was seen, has resolved, or is no longer watched:
// it calls a person to what is new, and nothing is. While the count is not known it is held too: no bell
// rings beside `alerts not known`. The screens take it at every frame,
// and ring the terminal's bell only when it is on: taken while off, the
// floor counts the same.
func (s *Session) Ring(now time.Time) bool {
	s.seen(now)
	a := &s.alarm
	switch {
	case !a.owed:
		return false
	case s.unseen(a.rungAt) == 0:
		a.owed = false
		return false
	case !a.rungAt.IsZero() && now.Sub(a.rungAt) < RingFloor, s.Firing(now).Why != Known:
		return false
	}
	a.owed, a.rungAt = false, now
	return true
}

// Seen is a key pressed at now: what is new is seen, but what arrived
// less than SeenAfter ago. The count stays while the alerts fire.
func (s *Session) Seen(now time.Time) {
	s.seen(now)
	for k, at := range s.alarm.unseen {
		if now.Sub(at) >= SeenAfter {
			delete(s.alarm.unseen, k)
		}
	}
}

// MarkForms are the title bar's mark, longest first: Long where the bar
// has room for it beside the rest, else Short, Tiny or Glyph, whichever
// fits first. A form that is nil is not drawn: all are nil when nothing
// fires.
type MarkForms struct{ Long, Short, Tiny, Glyph Line }

// Empty says the mark draws nothing.
func (m MarkForms) Empty() bool {
	return m.Long == nil && m.Short == nil && m.Tiny == nil && m.Glyph == nil
}

func marks(c Class, long, short, tiny, glyph string) MarkForms {
	return MarkForms{Line{ownC(long, c)}, Line{ownC(short, c)}, Line{ownC(tiny, c)}, Line{ownC(glyph, c)}}
}

// Mark is the title bar's mark at now: the glyph of the worst alert
// counted and how many fire (`✖ 3 firing`, `✖ 3`, `✖3`, `✖`), how many
// of them are new in the long form, `?` (or `as of 10m ago`) when the
// count is old, `alerts not known` (`alerts ?`, `?`) when it is not
// known, `alerts…` before the first answer, a dim `no monitor` in the
// long form alone when the box has none, and nothing when none fires.
// An old count of none says `0?`, and one the box sent with more
// held `0+`: nothing drawn would read as nothing firing. While something is new it is reversed on alternate seconds
// of the wall clock for MoveFor, then reversed and still until seen.
func (s *Session) Mark(now time.Time) MarkForms {
	f := s.Firing(now)
	switch {
	case f.Nothing:
		return MarkForms{}
	case f.Why == NotAsked:
		return marks(Plain, "alerts…", "alerts…", "…", "…")
	case f.Why != Known:
		return marks(Warn, "alerts not known", "alerts ?", "?", "?")
	case f.Off && !f.Old:
		// An old answer of no monitor is an old count of none, below: a
		// monitor may have been added since.
		return MarkForms{Long: Line{ownC("no monitor", Dim)}}
	case f.N == 0 && f.More && f.Old:
		return marks(Plain, "0+ firing as of "+Ago(now.Sub(f.At))+" ago", "0+?", "0+?", "?")
	case f.N == 0 && f.More:
		// The box sent none that is counted, and holds more than it sent.
		return marks(Plain, "0+ firing", "0+", "0+", "?")
	case f.N == 0 && !f.Old:
		return MarkForms{}
	case f.N == 0:
		return marks(Plain, "0 firing as of "+Ago(now.Sub(f.At))+" ago", "0?", "0?", "?")
	}
	c := f.Worst.Class()
	if f.New > 0 {
		moving := now.Sub(s.alarm.newAt) < MoveFor
		if !moving || now.Unix()%2 == 0 {
			c = loud(c)
		}
	}
	n := strconv.Itoa(f.N)
	if f.More {
		n += "+"
	}
	g := f.Worst.Glyph()
	long, short, tiny, glyph := g+" "+n+" firing", g+" "+n, g+n, g
	if f.New > 0 {
		long += ", " + strconv.Itoa(f.New) + " new"
	}
	if f.Old {
		long, short, tiny, glyph = long+" as of "+Ago(now.Sub(f.At))+" ago", short+"?", tiny+"?", glyph+"?"
	}
	return marks(c, long, short, tiny, glyph)
}

// loud is c reversed: the mark while something is new.
func loud(c Class) Class {
	if c == Bad {
		return LoudBad
	}
	return LoudWarn
}
