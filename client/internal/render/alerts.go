package render

import (
	"io"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// Alerts is GET /_motel/apps/<app>/alerts: an app's alerts as the
// box's Alertmanager holds them.
type Alerts struct {
	// Host is the box asked, App the app asked about: this side's.
	Host, App termtext.Text
	// Monitor is the box's word: on, off, unanswered, or one this
	// client does not know (held to a word's shape).
	Monitor termtext.Text
	Alerts  []Alert
}

// Alert is one alert: its name, severity and state, words of their
// grammars; the client's own words for the times; its summary, the
// box's free text.
type Alert struct {
	Name, Severity, State termtext.Text
	Since                 time.Time
	Until                 *time.Time
	Inhibited             bool
	Summary               termtext.Text
}

// WriteAlerts writes an app's alerts: what the box says first, then a
// line an alert, its words and times, and under it its summary,
// labelled and capped. No line begins with the box's text.
func WriteAlerts(w io.Writer, a Alerts) error {
	p := &printer{w: w}
	switch a.Monitor.String() {
	case "off":
		p.line("%s has no monitor for %s: nothing is raised there", a.Host, a.App)
		return p.err
	case "unanswered":
		p.line("%s could not ask its Alertmanager about %s's alerts", a.Host, a.App)
		return p.err
	case "on":
	default:
		p.line("%s answered about %s's alerts with a monitor word this client does not know: %s", a.Host, a.App, a.Monitor)
		return p.err
	}
	switch n := len(a.Alerts); n {
	case 0:
		p.line("%s says no alert of %s is held", a.Host, a.App)
		return p.err
	case 1:
		p.line("%s says 1 alert of %s is held:", a.Host, a.App)
	default:
		p.line("%s says %d alerts of %s are held, newest first:", a.Host, n, a.App)
	}
	for _, al := range a.Alerts {
		line := al.Name.String()
		if !al.Severity.Empty() {
			line += "  " + al.Severity.String()
		}
		line += "  since " + al.Since.UTC().Format("2006-01-02 15:04 UTC") + "  " + al.State.String()
		if al.Until != nil {
			line += ", silenced until " + al.Until.UTC().Format("2006-01-02 15:04 UTC")
		}
		if al.Inhibited {
			line += ", held while a deploy of the app runs"
		}
		p.line("alert    %s", line)
		if !al.Summary.Empty() {
			p.line("summary  %s", capped(al.Summary))
		}
	}
	return p.err
}
