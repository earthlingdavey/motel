package render

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// Series is GET /_motel/apps/<app>/series: an app's numbers over a
// window, as a person reads them in a terminal: each panel's lowest,
// highest and newest point, and how many of the window's steps have
// one. The plot is the screen's (motel alone).
type Series struct {
	// Host is the box asked, App the app asked about: this side's.
	Host, App termtext.Text
	// Window is the box's window, held to the client's set; Store its
	// word, held to a word's shape (only "on" and "off" reach here).
	Window, Store termtext.Text
	Step          time.Duration
	End           time.Time
	Panels        []SeriesPanel
	// NoRequests says the request panels are absent: the box counts
	// requests only for an app an observe handler names.
	NoRequests bool
}

// SeriesPanel is one panel: the client's own label and its values,
// formatted by the client from the box's numbers; or, for a panel the
// client does not know, the box's name and unit, words of their
// grammar, and nothing else.
type SeriesPanel struct {
	Label      string
	Name, Unit termtext.Text
	Known      bool
	// Min, Max and Newest are formatted values with their times; Points
	// is how many of the window's Steps have one. HadRequests says the
	// count is instead Requested, the steps the requests panel has a rate
	// above 0 at (the 5xx share's and the p95's, beside it): a step with
	// no request has no point of theirs. Points is said beside it when
	// the two differ.
	Min, Max, Newest SeriesPoint
	Points, Steps    int
	HadRequests      bool
	Requested        int
}

// SeriesPoint is a value as the client writes it, and its time.
type SeriesPoint struct {
	V string
	T time.Time
}

// WriteSeries writes an app's series: what the box says first, then a
// line a panel, every word and number the client's own, or a checked
// word of the box's.
func WriteSeries(w io.Writer, s Series) error {
	p := &printer{w: w}
	if s.Store.String() == "off" {
		p.line("%s keeps no numbers of its apps: its monitor block has no metrics line", s.Host)
		return p.err
	}
	// A time of another day than the window's end, and every time of the
	// 7 and 14 days, with its date.
	at := func(t time.Time) string {
		if e := s.End.UTC(); s.Step >= 30*time.Minute || t.UTC().YearDay() != e.YearDay() || t.UTC().Year() != e.Year() {
			return t.UTC().Format("01-02 15:04 UTC")
		}
		return t.UTC().Format("15:04 UTC")
	}
	p.line("%s holds %s's numbers over %s, a point every %s, to %s:", s.Host, s.App, s.Window, stepText(s.Step), s.End.UTC().Format("2006-01-02 15:04 UTC"))
	if len(s.Panels) == 0 {
		p.line("none: the store holds no point of %s in the window", s.App)
	}
	for _, pn := range s.Panels {
		switch {
		case !pn.Known:
			p.line("%-10s the box's %s in %s: not one this client draws", "other", pn.Name, pn.Unit)
			continue
		case pn.Points == 0:
			p.line("%-10s no point in the window", pn.Label)
			continue
		}
		count := fmt.Sprintf("%d of %d steps", pn.Points, pn.Steps)
		if pn.HadRequests {
			count = fmt.Sprintf("%d of %d steps had requests", pn.Requested, pn.Steps)
			if pn.Points != pn.Requested {
				// A step with requests and no point here (every one aborted,
				// or, for the p95, cache hits alone) is said, as the screen
				// says it.
				count += fmt.Sprintf(", %d with a point", pn.Points)
			}
		}
		p.line("%-10s min %s (%s)  max %s (%s)  newest %s (%s)  %s", pn.Label, pn.Min.V, at(pn.Min.T), pn.Max.V, at(pn.Max.T),
			pn.Newest.V, at(pn.Newest.T), count)
	}
	if s.NoRequests {
		p.line("%-10s none in the window: the box counts requests only for an app behind observe", "requests")
	}
	return p.err
}

// stepText is a step as a person reads it: 60 s is 1 m, an hour 1 h.
func stepText(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + " m"
	}
	return strconv.Itoa(int(d/time.Second)) + " s"
}
