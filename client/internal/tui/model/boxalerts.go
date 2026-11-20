package model

import (
	"strconv"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The alerts screen: what the box's Alertmanager holds (GET
// /_motel/alerts, `motel monitor alerts`), for a viewer of the box:
// every alert, since when, whether a silence or the deploy mute holds
// it and until when, and the silences themselves. The client judges
// nothing of its own: each mark repeats a severity and a state the box
// said. An alert's name, app, instance and summary, and a silence's
// matchers, maker and comment, are the box's words (an operator's, a
// producer's, a deployer's in a refused deploy's summary): each is on a
// row of its own after the client's label, capped and cut at the
// window's edge, so none begins a row.

// SilenceAdvice is said under the silences: adding one and ending one
// are root's, over ssh, until the box offers them.
const SilenceAdvice = "Silences are added and expired as root on the box: moteld monitor silence add, moteld monitor silence expire <id> (docs/monitor.md, \"Silence an alert\")."

// BoxAlertsAsOf is how old the box's answer about its alerts is, for the
// crumb, and that the last read failed when it did.
func (s *Session) BoxAlertsAsOf(now time.Time) Line {
	s.seen(now)
	if s.BoxAlertsSt == nil {
		return nil
	}
	l := s.asOf(s.BoxAlertsSt.At, now)
	if s.BoxAlertsErr != nil {
		l = append(l, ownC(" · the last read failed", Warn))
	}
	return l
}

// monitorSays is the box's monitor word in the client's words.
func monitorSays(b wire.BoxAlerts) Line {
	switch b.Monitor {
	case wire.MonitorOn:
		l := Line{own("on · " + nOf(len(b.Alerts), "alert") + ", " + nOf(len(b.Silences), "silence") + ", as the box's Alertmanager holds them")}
		if b.MoreAlerts || b.MoreSilences {
			l = Line{own("on · " + nOf(len(b.Alerts), "alert") + ", " + nOf(len(b.Silences), "silence") + " sent; the box's Alertmanager holds more")}
		}
		return l
	case wire.MonitorOff:
		return Line{own("off: this box raises no alerts (its monitor block sends nothing to anyone, or it has none)")}
	case wire.MonitorUnanswered:
		return Line{ownC("not known", Warn), own(": the box's Alertmanager did not answer it in time")}
	}
	return Line{ownC("not known", Warn), own(": the box said "), quoted(b.Monitor)}
}

// nOf is n of a thing, said: "1 alert", "3 silences".
func nOf(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return strconv.Itoa(n) + " " + thing + "s"
}

// alertMark is an alert's sign: broken now for a critical one that is
// told to the channels, look for a warning one, nothing for one held
// quiet or of info, and cannot tell for a state the client does not
// know.
func alertMark(a wire.BoxAlert) Mark {
	switch a.State {
	case "suppressed":
		return MarkNone
	case "active", "unprocessed":
	default:
		return MarkUnknown
	}
	switch a.Severity {
	case "critical":
		return MarkBroken
	case "warning":
		return MarkLook
	case "info":
		return MarkNone
	}
	return MarkUnknown
}

// SeverityWord is the severity as the client prints it at a row's
// start (the screen's and the command's): one of motel's three, or the
// client's own word for another.
func SeverityWord(s string) string {
	switch s {
	case "critical", "warning", "info":
		return s
	}
	return "other"
}

// boxAlertHold is what holds an alert, or that nothing does, in the
// client's words, and since when it fires.
func boxAlertHold(a wire.BoxAlert, now time.Time) Line {
	firing := "since " + clock(a.StartsAt, now, false) + " UTC (" + since(a.StartsAt, now) + ")"
	switch a.State {
	case "active":
		return Line{own("firing " + firing)}
	case "unprocessed":
		return Line{own("just received, not yet judged · firing " + firing)}
	case "suppressed":
		var held string
		switch {
		case a.SilencedUntil != nil && a.Inhibited:
			held = "silenced until " + clock(*a.SilencedUntil, now, false) + " UTC (" + since(*a.SilencedUntil, now) + "), and held while a deploy runs"
		case a.SilencedUntil != nil:
			held = "silenced until " + clock(*a.SilencedUntil, now, false) + " UTC (" + since(*a.SilencedUntil, now) + ")"
		case a.Inhibited:
			held = "held while a deploy of its app runs"
		default:
			held = "held quiet by Alertmanager"
		}
		return Line{ownC(held, Dim), own(" · firing " + firing)}
	}
	return Line{ownC("state not known", Warn), own(" · firing " + firing + " · the box said "), quoted(a.State)}
}

// boxField is a row of the box's words under an alert or a silence: the
// client's label, indented under the row it belongs to, then the
// value, capped.
func boxField(label string, v ...Seg) Line {
	return append(Line{ownC("    "+pad(label, 10), Label)}, v...)
}

// BoxAlertsView is the alerts screen, its lines whole.
func (s *Session) BoxAlertsView(now time.Time) []Line { return s.BoxAlertsViewFit(now, 0, nil) }

// BoxAlertsViewFit is the alerts screen laid to width cells; 0 leaves each
// line one line. Every line holding a box's value is cut to the width,
// so a value never begins a row; the client's sentences are laid on
// rows (Sentence).
func (s *Session) BoxAlertsViewFit(now time.Time, width int, cells Cells) []Line {
	s.seen(now)
	switch {
	case NotFound(s.BoxAlertsErr):
		return Sentence(Line{own("Not said here: the box answered 404, so its moteld is older than this read (GET /_motel/alerts).")}, width, cells)
	case s.BoxAlertsSt == nil && s.BoxAlertsErr != nil:
		l := Line{{T: Capped(failureText(s.BoxAlertsErr), 4*LineMax)}}
		if Forbidden(s.BoxAlertsErr) {
			l = append(l, s.askAgain(now)...)
		}
		return Sentence(l, width, cells)
	case s.BoxAlertsSt == nil:
		return Sentence(Line{own("asking " + s.Host + " what its Alertmanager holds…")}, width, cells)
	}
	b := s.BoxAlertsSt.V
	out := Sentence(field("monitor", monitorSays(b)...), width, cells)
	if b.Monitor == wire.MonitorOn {
		out = append(out, nil, Line{ownC("ALERTS", Label)})
		if len(b.Alerts) == 0 {
			out = append(out, Line{own("  nothing is firing")})
		}
		for _, a := range b.Alerts {
			m := alertMark(a)
			head := append(Line{ownC(m.Glyph()+" ", m.Class()), ownC(pad(SeverityWord(a.Severity), 9), m.Class())}, boxAlertHold(a, now)...)
			out = append(out, cutTo(head, width, cells))
			if a.Name != "" {
				out = append(out, cutTo(boxField("alert", boxT(a.Name)), width, cells))
			}
			if a.Severity != "" && SeverityWord(a.Severity) != a.Severity {
				out = append(out, cutTo(boxField("severity", boxT(a.Severity)), width, cells))
			}
			if a.App != "" {
				out = append(out, cutTo(boxField("app", boxT(a.App)), width, cells))
			}
			if a.Instance != "" {
				out = append(out, cutTo(boxField("instance", boxT(a.Instance)), width, cells))
			}
			if a.Summary != "" {
				out = append(out, cutTo(boxField("summary", boxT(a.Summary)), width, cells))
			}
		}
		if b.MoreAlerts {
			out = append(out, Sentence(Line{ownC("  the box holds more alerts than it sends: the newest "+strconv.Itoa(len(b.Alerts))+" are shown", Warn)}, width, cells)...)
		}
		out = append(out, nil, Line{ownC("SILENCES", Label)})
		if len(b.Silences) == 0 {
			out = append(out, Line{own("  none active, and none waiting to start")})
		}
		for _, sl := range b.Silences {
			out = append(out, cutTo(silenceHead(sl, now), width, cells))
			if len(sl.Matchers) > 0 {
				out = append(out, cutTo(boxField("matches", Matchers(sl.Matchers)...), width, cells))
			}
			for _, f := range [][2]string{{"by", sl.CreatedBy}, {"comment", sl.Comment}, {"id", sl.ID}} {
				if f[1] != "" {
					out = append(out, cutTo(boxField(f[0], boxT(f[1])), width, cells))
				}
			}
		}
		if b.MoreSilences {
			out = append(out, Sentence(Line{ownC("  the box holds more silences than it sends: "+strconv.Itoa(len(b.Silences))+" are shown, the active ones and the soonest to start", Warn)}, width, cells)...)
		}
		out = append(out, nil)
		out = append(out, Sentence(Line{own(SilenceAdvice)}, width, cells)...)
	}
	if s.BoxAlertsErr != nil {
		out = append(append(out, nil), Sentence(Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(s.BoxAlertsErr), 2*LineMax)}}, width, cells)...)
	}
	return out
}

// silenceHead is a silence's first row: its state and its times, in
// the client's words.
func silenceHead(sl wire.Silence, now time.Time) Line {
	end := "until " + clock(sl.EndsAt, now, false) + " UTC (" + since(sl.EndsAt, now) + ")"
	switch sl.State {
	case wire.SilenceActive:
		return Line{own("  "), ownC(pad("active", 9), Strong), own(end + " · since " + clock(sl.StartsAt, now, false) + " UTC")}
	case wire.SilencePending:
		return Line{own("  "), ownC(pad("pending", 9), Dim), own("from " + clock(sl.StartsAt, now, false) + " UTC (" + since(sl.StartsAt, now) + ") " + end)}
	}
	return Line{own("? "), ownC(pad("other", 9), Warn), own(end + " · the box said "), quoted(sl.State)}
}

// Matchers are a silence's matchers on one row (the screen's and the
// command's): each label's name and
// value the box's, the value quoted, the operator the client's own
// word when it is one of the four.
func Matchers(ms []wire.Matcher) Line {
	var l Line
	for i, m := range ms {
		if i > 0 {
			l = append(l, own(", "))
		}
		op := m.Op
		switch op {
		case wire.MatchEqual, wire.MatchNotEqual, wire.MatchRegexp, wire.MatchNotRegexp:
		default:
			op = "?"
		}
		l = append(l, boxT(m.Name), own(" "+op+" "), quoted(m.Value))
	}
	return l
}

// BoxAlertsLine is the box screen's line of the alerts: how old the
// answer is and what it holds, or why there is none. The age and the
// pointer come first, so a cut at the window's edge cuts neither.
func (s *Session) BoxAlertsLine(now time.Time) Line {
	switch {
	case s.BoxAlertsSt != nil:
		l := Line{own(Ago(now.Sub(s.BoxAlertsSt.At)) + " ago · :alerts · ")}
		if s.BoxAlertsErr != nil {
			l = append(l, ownC("the last read failed · ", Warn))
		}
		b := s.BoxAlertsSt.V
		if b.Monitor != wire.MonitorOn {
			return field("alerts", append(l, monitorSays(b)...)...)
		}
		// Counted as the screen marks them: firing, an alert it marks
		// (critical, warning, or a word it does not know); held quiet, a
		// suppressed one; an info one that fires, apart, unmarked.
		firing, quiet, info := 0, 0, 0
		for _, a := range b.Alerts {
			switch {
			case a.State == "suppressed":
				quiet++
			case alertMark(a) == MarkNone:
				info++
			default:
				firing++
			}
		}
		c := Plain
		if firing > 0 {
			c = Warn
		}
		rest := ", " + strconv.Itoa(quiet) + " held quiet"
		if info > 0 {
			rest += ", " + strconv.Itoa(info) + " info"
		}
		return field("alerts", append(l, ownC(strconv.Itoa(firing)+" firing", c), own(rest+" · "+nOf(len(b.Silences), "silence")))...)
	case NotFound(s.BoxAlertsErr):
		return field("alerts", own("not said here (it answered 404: a moteld older than this read)"))
	case s.BoxAlertsErr != nil:
		return field("alerts", ownC("the last read failed", Warn), own(" · :alerts says why"))
	}
	return field("alerts", own("asking…"))
}
