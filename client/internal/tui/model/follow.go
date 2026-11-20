package model

import "github.com/smallhoursorg/motel/liveswap/wire"

// Following a deploy (#580). The box's status carries the deploy in
// flight with every phase entered so far, timed on the box's clock, and
// once it ends, last_deploy holds its outcome with every phase it
// passed through: a read on a schedule loses no phase between two
// reads, however short (stopping_old's second). What a read cannot say
// alone is that the deploy it no longer shows is one that was seen in
// flight: that is kept here, by the app's screen and by `motel status
// --follow`.

// SameDeploy says whether a and b, each a deploy the box showed in
// flight, are one deploy: the same version, taken at the same time. A
// second deploy of the same version (a relaunch after a deploy) is
// another.
func SameDeploy(a, b wire.DeployInFlight) bool {
	return a.Version == b.Version && a.StartedAt.Equal(b.StartedAt)
}

// OutcomeOf is the outcome st holds of f, a deploy seen in flight in an
// earlier status: its last_deploy when that is f's, and nil when it
// holds none of f's. f's is of its version, and begins with the phases
// f had entered, each begun when f said: the box times a deploy's
// phases once, and its record carries the same times (liveswap/app.go,
// timedPhases), so a later deploy of the same version, ended before the
// read (one refused at once has no phase), is not taken for f once f
// was seen in a phase. f seen in none (a push's upload) is matched by
// its outcome's end, no earlier than f was taken: two box times, which
// a step of the box's clock can put out of order. The box clears the deploy in flight in the step
// that sets last_deploy to its outcome, so the first status without f
// holds it, but where f ended with no outcome (a push whose upload was
// never staged), or another deploy ended after it before the read.
func OutcomeOf(f wire.DeployInFlight, st wire.Status) *wire.DeployResult {
	d := st.LastDeploy
	if d == nil || d.Version != f.Version || len(d.Phases) < len(f.Phases) || len(f.Phases) == 0 && d.FinishedAt.Before(f.StartedAt) {
		return nil
	}
	for i, p := range f.Phases {
		if d.Phases[i].Name != p.Name || !d.Phases[i].StartedAt.Equal(p.StartedAt) {
			return nil
		}
	}
	return d
}

// ended is a deploy the session saw in flight and then saw end: the
// deploy as it was last seen, the outcome the first status without it
// held (nil: none), and that status's last_deploy, whichever's it was.
type ended struct {
	f    wire.DeployInFlight
	d    *wire.DeployResult
	last *wire.DeployResult
}

// follow keeps what a status read says of a deploy in flight, before
// st replaces the status held: the deploy now in flight, and the end of
// the one seen before it, if it is gone. That end is the news until a
// deploy is seen to begin after it, or the box's last deploy is another
// than when it ended: one begun and ended between two reads, never seen
// in flight, whose outcome the deployed line says.
func (a *appData) follow(st wire.Status) {
	now := st.Deploying
	switch {
	case a.flight != nil && (now == nil || !SameDeploy(*a.flight, *now)):
		a.ended = &ended{f: *a.flight, d: OutcomeOf(*a.flight, st), last: st.LastDeploy}
	case a.flight == nil && now != nil:
		a.ended = nil
	case a.ended != nil && !sameResult(a.ended.last, st.LastDeploy):
		a.ended = nil
	}
	a.flight = nil
	if now != nil {
		f := *now
		a.flight = &f
	}
}

// sameResult says whether a and b are one deploy's outcome, or both
// none.
func sameResult(a, b *wire.DeployResult) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Version == b.Version && a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt)
}

// flightLines are an app's screen's lines of a deploy it follows: while
// one runs, the phases it has entered so far; once one seen in flight
// has ended, how it ended, and then, when no other runs, the phases it
// passed through.
func (s *Session) flightLines(a *appData, st wire.Status) []Line {
	var out []Line
	if d := st.Deploying; d != nil && len(d.Phases) > 0 {
		out = append(out, field("phases", phasesLine(d.Phases, "")...))
	}
	e := a.ended
	if e == nil {
		return out
	}
	out = append(out, endedLine(e, st))
	if d := e.d; d != nil && st.Deploying == nil && len(d.Phases) > 0 {
		failed := ""
		if d.Status == "failed" {
			failed = d.Phase
		}
		out = append(out, field("phases", phasesLine(d.Phases, failed)...))
	}
	return out
}

// PhasesShown is the most phases an app's screen lists on its phases
// line, the latest ones: the earlier are marked "… " before them, and
// the record lists them all. Eight phases do not fit 80 columns.
const PhasesShown = 4

// phasesLine is phases, in order, each with its seconds (the last's so
// far, while it runs), at most the last PhasesShown; failed, when not
// empty, is the phase a deploy failed in, marked when it is the last.
func phasesLine(phases []wire.PhaseTiming, failed string) Line {
	var l Line
	if len(phases) > PhasesShown {
		l = append(l, ownC("… ", Dim))
		phases = phases[len(phases)-PhasesShown:]
	}
	for i, p := range phases {
		if i > 0 {
			l = append(l, ownC(" › ", Dim))
		}
		l = append(l, own(p.Name+" "+num(p.Seconds, 1)+"s"))
		if i == len(phases)-1 && failed != "" && p.Name == failed {
			l = append(l, ownC(" ✖", Bad))
		}
	}
	return l
}

// endedLine says how a deploy seen in flight ended, and what serves
// now: what the trial found a reader had to put together from two lines
// after a deploy failed. A failure whose record is the newest says the
// key that opens it.
func endedLine(e *ended, st wire.Status) Line {
	l := Line{own(e.f.Version + " ")}
	d := e.d
	switch {
	case d == nil:
		l = append(l, own("ended with no outcome from the box"))
	case d.Status == "failed":
		w := "failed"
		if d.Phase != "" {
			w += " in " + d.Phase
		}
		l = append(l, ownC(w, Bad))
	default:
		l = append(l, ownC("succeeded", Good))
	}
	switch cur := st.CurrentVersion; {
	case cur == "":
		l = append(l, own(" · nothing serves"))
	case !st.Running:
		l = append(l, own(" · "+cur+" is not running"))
	case cur == e.f.Version && d != nil && d.Status == "succeeded":
		l = append(l, own(" · it serves"))
	default:
		l = append(l, own(" · "+cur+" serves"))
	}
	if d != nil && d.Status == "failed" && newestIs(st, d) {
		l = append(l, ownC(" · d opens its record", Dim))
	}
	return field("ended", l...)
}

// newestIs says whether the newest deploy record st lists is d's: the
// same version, finished at the same time. A relaunch writes no record,
// so the newest of a version may be an older deploy's.
func newestIs(st wire.Status, d *wire.DeployResult) bool {
	if len(st.Deploys) == 0 || st.Deploys[0].Version != d.Version {
		return false
	}
	t, ok := rawTime(st.Deploys[0].FinishedAt)
	return ok && t.Equal(d.FinishedAt)
}

// Newest is the version of the newest deploy record app's status
// lists, for d on its screen; false when none is held.
func (s *Session) Newest(app string) (string, bool) {
	a := s.apps[app]
	if a == nil || a.st == nil || len(a.st.V.Deploys) == 0 {
		return "", false
	}
	return a.st.V.Deploys[0].Version, true
}
