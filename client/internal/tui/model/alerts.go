package model

import (
	"regexp"
	"strconv"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// AlertsShown is the most alerts an app's screen lists: the rest are
// counted, and `motel monitor alerts --app <app>` lists them all.
const AlertsShown = 3

// alertSeverities and alertStates are the words the client shows as
// its own: the severities motel's alerts take (notify's Severity*), and
// Alertmanager's states. Any other word of the shape the client holds
// them to is shown as the box's.
var (
	alertSeverities = map[string]bool{"critical": true, "warning": true, "info": true}
	alertStates     = map[string]bool{"active": true, "suppressed": true, "unprocessed": true}
)

// alertLines are an app's screen's lines of its alerts, read while the
// screen is open: none before the first answer; what the box said of
// its monitor; each alert's name, severity, when it began and what
// holds it, and under it its summary, the box's free text, capped.
func (s *Session) alertLines(app string, now time.Time) []Line {
	a := s.apps[app]
	if a == nil || a.alerts == nil && a.alertsErr == nil {
		return nil
	}
	if a.alerts == nil {
		if NotFound(a.alertsErr) {
			// A box older than the read, or (to a viewer of the box) no
			// app of this name now: the box's words say which.
			return []Line{field("alerts", own("none read: "), Seg{T: Capped(failureText(a.alertsErr), 2*LineMax)})}
		}
		return []Line{field("alerts", ownC("not read: ", Warn), Seg{T: Capped(failureText(a.alertsErr), 2*LineMax)})}
	}
	g := a.alerts
	head := Line{}
	switch g.V.Monitor {
	case wire.MonitorOn:
		switch n := len(g.V.Alerts); n {
		case 0:
			head = append(head, own("none held"))
		case 1:
			head = append(head, ownC("1 held", Warn))
		default:
			head = append(head, ownC(strconv.Itoa(n)+" held", Warn))
		}
	case wire.MonitorOff:
		head = append(head, own("none: the box has no monitor, so nothing is raised"))
	case wire.MonitorUnanswered:
		head = append(head, ownC("not known: the box could not ask its Alertmanager", Warn))
	default:
		head = append(head, ownC("not known: the box says ", Warn), boxT(g.V.Monitor), own(" of its monitor"))
	}
	head = append(head, own(" · as of "+Ago(now.Sub(g.At))+" ago"))
	if a.alertsErr != nil {
		head = append(head, ownC(" · the last read failed", Warn))
	}
	out := []Line{field("alerts", head...)}
	for i, al := range g.V.Alerts {
		if i == AlertsShown {
			out = append(out, field("", own("and "+strconv.Itoa(len(g.V.Alerts)-AlertsShown)+" more: "+s.alertsCommand(app)+" lists them")))
			break
		}
		// A name of motel's own shape is a word; any other the client
		// took (a Prometheus rule's, root's by hand) is the box's text.
		name := boxT(al.Name)
		if motelAlertName.MatchString(al.Name) {
			name = own(al.Name)
		}
		l := Line{name}
		if al.Severity != "" {
			c := Plain
			switch al.Severity {
			case "critical":
				c = Bad
			case "warning":
				c = Warn
			}
			l = append(l, own(" · "), boxWordC(al.Severity, alertSeverities, c))
		}
		// What holds it, before when it began: the line is cut at the
		// window's edge. Its state is said only when nothing else says
		// it: suppressed by what is named, active by naming nothing.
		switch {
		case al.SilencedUntil != nil:
			l = append(l, own(" · silenced until "+clock(*al.SilencedUntil, now, false)+" UTC"))
		case al.Inhibited:
			l = append(l, own(" · held while a deploy runs"))
		case al.State != "active":
			l = append(l, own(" · "), boxWord(al.State, alertStates))
		}
		if al.SilencedUntil != nil && al.Inhibited {
			l = append(l, own(" · held while a deploy runs"))
		}
		l = append(l, own(" · since "+since(al.StartsAt, now)+" ("+clock(al.StartsAt, now, false)+" UTC)"))
		out = append(out, field("alert", l...))
		if al.Summary != "" {
			out = append(out, field("summary", boxT(al.Summary)))
		}
	}
	return out
}

// alertsCommand is the command that lists app's alerts on the box this
// session reads, with the key it signs with when the person named it,
// as the printed commands are (commandArgs).
func (s *Session) alertsCommand(app string) string {
	args := []string{"motel", "monitor", "alerts", "--box", s.boxArg()}
	if s.Key.Source == "--key" || s.Key.Source == "MOTEL_KEY" {
		args = append(args, "--key", s.Key.Shown)
	}
	return ShellLine(append(args, "--app", app))
}

// motelAlertName is the shape of the names motel's own alerts take
// (its events, notify's ruleNameRe and alertRe, Watchdog's and
// ChannelFailed's capitals).
var motelAlertName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// boxWord is a word the client knows, shown as its own, or the box's.
func boxWord(w string, known map[string]bool) Seg { return boxWordC(w, known, Plain) }

func boxWordC(w string, known map[string]bool, c Class) Seg {
	if known[w] {
		return ownC(w, c)
	}
	return boxT(w)
}
