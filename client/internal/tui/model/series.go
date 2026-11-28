package model

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The metrics screen: an app's numbers over a window (GET
// /_motel/apps/<app>/series, `motel metrics <app> --range`), each panel
// a plot of the box's points, opened from the app's screen. The plot is
// the client's own drawing of numbers: nothing of the box's text is in
// it. Its marks are braille dots (U+2801 to U+28FF, of narrow width in
// every East Asian width table, so one cell in a terminal that draws
// the ambiguous characters wide), its axes and deploy marks ASCII, and
// an empty cell a space.

// SeriesRanges are the windows the metrics screen steps through, the
// box's own set (wire.SeriesRanges), the hour first.
var SeriesRanges = wire.SeriesRanges

// seriesSpans are SeriesRanges' spans.
var seriesSpans = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "14d": 14 * 24 * time.Hour}

// SeriesSpan is a window's span; 0 for one not in SeriesRanges.
func SeriesSpan(window string) time.Duration { return seriesSpans[window] }

// appRefused says the box's last word on app's status is a refusal (a
// 403, a 404): nothing of the app is shown then, its series neither.
func (s *Session) appRefused(app string) (error, bool) {
	a := s.apps[app]
	if a == nil || a.st != nil || !Forbidden(a.err) && !NotFound(a.err) {
		return nil, false
	}
	return a.err, true
}

// seriesPanel is a panel the client draws: the box's name and unit, the
// client's label, and how its values are written.
type seriesPanel struct {
	name, unit, label string
}

// seriesPanels are the panels the client knows, in the box's order
// (liveswap/wire/series.go).
var seriesPanels = []seriesPanel{
	{wire.PanelMemory, wire.UnitBytes, "memory"},
	{wire.PanelCPU, wire.UnitCores, "cpu"},
	{wire.PanelRequests, wire.UnitPerSecond, "requests"},
	{wire.Panel5xx, wire.UnitShare, "5xx share"},
	{wire.PanelP95, wire.UnitMs, "p95"},
}

// SeriesLabel is the client's label for a panel the box names, and
// whether the client draws it: a panel of a name it knows, in the unit
// it knows that name in.
func SeriesLabel(name, unit string) (string, bool) {
	for _, p := range seriesPanels {
		if p.name == name && p.unit == unit {
			return p.label, true
		}
	}
	return "", false
}

// SeriesValue is v, a point of the panel named name, as the client
// writes it: in MiB or GiB, cores, a rate a second (an hour, under
// 0.001/s), a percentage, or milliseconds, compactly for a plot's axis. Every character is the
// client's own: the box sent a number.
func SeriesValue(name string, v float64, compact bool) string {
	switch name {
	case wire.PanelMemory:
		switch {
		case compact && v < 1<<20:
			return strconv.FormatFloat(v/(1<<10), 'f', 0, 64) + "K"
		case v >= 1<<30:
			if compact {
				return trimZero(strconv.FormatFloat(v/(1<<30), 'f', 1, 64)) + "G"
			}
			return strconv.FormatFloat(v/(1<<30), 'f', 2, 64) + " GiB"
		case compact:
			return strconv.FormatFloat(v/(1<<20), 'f', 0, 64) + "M"
		}
		return strconv.FormatFloat(v/(1<<20), 'f', 1, 64) + " MiB"
	case wire.PanelCPU:
		s := small(v, 0.001, 3)
		if compact {
			return s
		}
		return s + " cores"
	case wire.PanelRequests:
		switch {
		case compact && v >= 1e6:
			return trimZero(strconv.FormatFloat(v/1e6, 'f', 1, 64)) + "M/s"
		case compact && v >= 1e3:
			return trimZero(strconv.FormatFloat(v/1e3, 'f', 1, 64)) + "k/s"
		case v > 0 && v < 0.001:
			// Under a request in 1000 s, an hour's: 0.00028/s is 1.0/h.
			if h := v * 3600; h < 1 {
				return figures(h, 4) + "/h"
			}
			return strconv.FormatFloat(v*3600, 'f', 1, 64) + "/h"
		}
		return small(v, 0.01, 2) + "/s"
	case wire.Panel5xx:
		if v > 0 && v < 0.001 {
			// The box's share is to 0.01 %.
			return figures(v*100, 2) + "%"
		}
		return trimZero(strconv.FormatFloat(v*100, 'f', 1, 64)) + "%"
	case wire.PanelP95:
		if compact && v >= 1000 {
			return trimZero(strconv.FormatFloat(v/1000, 'f', 1, 64)) + "s"
		}
		if compact {
			return strconv.FormatFloat(v, 'f', 0, 64) + "ms"
		}
		return strconv.FormatFloat(v, 'f', 1, 64) + " ms"
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

// small is v to prec places, to two figures under floor but above zero
// (the box's cores and rates are to six places), and to fewer places
// once it is large.
func small(v, floor float64, prec int) string {
	switch {
	case v > 0 && v < floor:
		return figures(v, 6)
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', prec, 64)
}

// figures is v, above zero and under one, to two significant figures
// and at most most places, its trailing zeros dropped; "<" the smallest
// of most places when that is no figure at all.
func figures(v float64, most int) string {
	s := strconv.FormatFloat(v, 'f', min(1-int(math.Floor(math.Log10(v))), most), 64)
	if !strings.ContainsAny(s, "123456789") {
		return "<" + strconv.FormatFloat(math.Pow10(-most), 'f', -1, 64)
	}
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// SeriesExtremes is hi and lo, a panel's highest and lowest, written as
// a pair, compactly for a plot's axis (seriesPair).
func SeriesExtremes(name string, hi, lo float64, compact bool) (string, string) {
	h, l, _ := seriesPair(name, hi, lo, compact)
	return h, l
}

// SeriesNewest is v, a panel's newest, written in full as SeriesExtremes
// writes the panel's highest and lowest, when that tells it; else as
// SeriesValue does. So the newest is never written coarser than the
// highest or lowest it may be.
func SeriesNewest(name string, v, hi, lo float64) string {
	_, _, i := seriesPair(name, hi, lo, false)
	if w := seriesWritings(name, v, hi, false)[i].s; tells(w, v, false) {
		return w
	}
	return SeriesValue(name, v, false)
}

// seriesPair is hi and lo, a panel's highest and lowest, written in the
// first of their writings (seriesWritings, both in hi's unit) in which
// they are one value, or differ, to as many places, to places fine
// enough that neither rounds away from its value by more than the two
// are apart (1.6 ms and 1.4 ms are not 2ms and 1ms), and each tells its
// value; and the index of that writing. With none, each in the first
// of those writings that tells it (1 GiB and a page either side, 1G
// and 1G); with none of those, in its own unit (0.50/s beside 1.0/h).
func seriesPair(name string, hi, lo float64, compact bool) (string, string, int) {
	hs, ls := seriesWritings(name, hi, hi, compact), seriesWritings(name, lo, hi, compact)
	for i := range hs {
		h, l := hs[i], ls[i]
		hp, hu := placesOf(h.s)
		lp, lu := placesOf(l.s)
		if (hi == lo || h.s != l.s && hu == lu && hp == lp && h.res <= math.Abs(hi-lo)) && tells(h.s, hi, compact) && tells(l.s, lo, compact) {
			return h.s, l.s, i
		}
	}
	return fallback(name, hi, hs, compact), fallback(name, lo, ls, compact), 0
}

// fallback is the first of ws, v's writings in another's unit, that
// tells v; else the first of its own unit's; else its plain writing.
func fallback(name string, v float64, ws []writing, compact bool) string {
	for _, list := range [][]writing{ws, seriesWritings(name, v, v, compact)} {
		for _, w := range list {
			if tells(w.s, v, compact) {
				return w.s
			}
		}
	}
	return SeriesValue(name, v, compact)
}

// placesOf is the places of s's number, and what follows it (its unit).
func placesOf(s string) (int, string) {
	s = strings.TrimPrefix(s, "<")
	n := len(s) - len(strings.TrimLeft(s, "0123456789."))
	_, frac, _ := strings.Cut(s[:n], ".")
	return len(frac), s[n:]
}

// tells says s, a writing of v, tells v: it is one (not coarser than
// v's plain writing), with no "<" floor, no zero for a value above it,
// and compact, no wider than a plot's label.
func tells(s string, v float64, compact bool) bool {
	return s != "" && !strings.HasPrefix(s, "<") && (v == 0 || strings.ContainsAny(s, "123456789")) && (!compact || len([]rune(s)) <= plotLabel)
}

// writing is a value written, and the step between two values written
// to its places in its unit.
type writing struct {
	s   string
	res float64
}

// seriesUnit is the unit, and its suffix, v of the panel named name is
// written in; 0 for a panel the client does not know.
func seriesUnit(name string, v float64, compact bool) (float64, string) {
	switch name {
	case wire.PanelMemory:
		switch {
		case compact && v < 1<<20:
			return 1 << 10, "K"
		case v >= 1<<30 && compact:
			return 1 << 30, "G"
		case v >= 1<<30:
			return 1 << 30, " GiB"
		case compact:
			return 1 << 20, "M"
		}
		return 1 << 20, " MiB"
	case wire.PanelCPU:
		if compact {
			return 1, ""
		}
		return 1, " cores"
	case wire.PanelRequests:
		switch {
		case compact && v >= 1e6:
			return 1e6, "M/s"
		case compact && v >= 1e3:
			return 1e3, "k/s"
		case v > 0 && v < 0.001:
			return 1.0 / 3600, "/h"
		}
		return 1, "/s"
	case wire.Panel5xx:
		return 0.01, "%"
	case wire.PanelP95:
		switch {
		case compact && v >= 1000:
			return 1000, "s"
		case compact:
			return 1, "ms"
		}
		return 1, " ms"
	}
	return 0, ""
}

// seriesWritings is the ways v of the panel named name is written in
// ref's unit, coarsest first: as SeriesValue writes it ("" in another
// unit), then to 0 to 6 places (none, "", to fewer places than that),
// then, compact, cores in millicores (an "m") to 0 to 3 places, the
// box's six places, and rates an hour ("/h") to 0 to 3. Every value of
// a panel has as many, so the i-th of two values are written alike.
func seriesWritings(name string, v, ref float64, compact bool) []writing {
	unit, suffix := seriesUnit(name, ref, compact)
	vUnit, vSuffix := seriesUnit(name, v, compact)
	plain := SeriesValue(name, v, compact)
	first, _ := placesOf(plain)
	out := []writing{{plain, vUnit * math.Pow10(-first)}}
	if unit == 0 {
		return out
	}
	if vSuffix != suffix {
		out[0], first = writing{}, 0
	}
	at := func(v, unit float64, p, least int, suffix string) writing {
		if p < least {
			return writing{}
		}
		return writing{strconv.FormatFloat(v/unit, 'f', p, 64) + suffix, unit * math.Pow10(-p)}
	}
	for p := range 7 {
		out = append(out, at(v, unit, p, first, suffix))
	}
	if name == wire.PanelCPU && compact {
		for p := range 4 {
			out = append(out, at(v, 0.001, p, first-3, "m"))
		}
	}
	if name == wire.PanelRequests && compact {
		for p := range 4 {
			out = append(out, at(v, 1.0/3600, p, 0, "/h"))
		}
	}
	return out
}

func trimZero(s string) string {
	if t, ok := strings.CutSuffix(s, ".0"); ok {
		return t
	}
	return s
}

// SeriesStep is a step as a person reads it: 1 m, 5 m, 30 m, 1 h.
func SeriesStep(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + " m"
	}
	return strconv.Itoa(int(d/time.Second)) + " s"
}

// NextRange is the window after rng in SeriesRanges, the first after
// the last.
func NextRange(rng string) string {
	i := slices.Index(SeriesRanges, rng)
	return SeriesRanges[(i+1)%len(SeriesRanges)]
}

// seriesData is an app's series over a window, read while the metrics
// screen shows that window, and its newest read's failure.
type seriesData struct {
	g   *Got[wire.Series]
	err error
}

// Series is the series read for app over rng, if any, and its newest
// read's failure.
func (s *Session) Series(app, rng string) (*Got[wire.Series], error) {
	d := s.series[Job{Kind: KSeries, App: app, Range: rng}]
	if d == nil {
		return nil, nil
	}
	return d.g, d.err
}

// dropSeries forgets every window of app's series: its status was
// refused, so nothing of the app is shown. A read's failure is kept: a
// window refused before is held so, and asked again only by ctrl-r.
func (s *Session) dropSeries(app string) {
	for j, d := range s.series {
		if j.App == app {
			d.g = nil
		}
	}
}

// MetricsAsOf is the metrics screen's crumb: how old the answer is, and
// that the last read failed when it did.
func (s *Session) MetricsAsOf(app, rng string, now time.Time) Line {
	s.seen(now)
	g, err := s.Series(app, rng)
	if _, refused := s.appRefused(app); g == nil || refused {
		return nil
	}
	l := append(s.asOf(g.At, now), s.stale(Job{Kind: KSeries, App: app, Range: rng})...)
	if err != nil {
		l = append(l, ownC(" · the last read failed", Warn))
	}
	return l
}

// plotLabel is the room a plot's row gives its axis label, then " |".
const plotLabel = 7

// MetricsViewFit is the metrics screen of app over rng laid to width
// cells and height rows (the page's): what the box said of its store,
// the window and its deploy marks, then a plot a panel the client
// draws, its newest, highest and lowest values with their times above
// it; a panel absent says why, and one the client does not know is
// named as the box's. Each plot is as tall as lets the first whole on
// the page.
func (s *Session) MetricsViewFit(app, rng string, now time.Time, width, height int, cells Cells) []Line {
	s.seen(now)
	g, err := s.Series(app, rng)
	if refusal, refused := s.appRefused(app); refused {
		// The box refused the app's status: nothing of the app is shown,
		// and the refusal is said, whatever the series' own read did, an
		// answer of it since among it.
		l := Line{{T: Capped(failureText(refusal), 4*LineMax)}}
		if Forbidden(refusal) {
			l = append(l, s.askAgain(now)...)
		}
		return Sentence(l, width, cells)
	}
	switch {
	case g == nil && err == nil:
		return Sentence(Line{own("asking " + s.Host + " for " + app + "'s numbers over " + rng + "…")}, width, cells)
	case g == nil:
		l := Line{{T: Capped(failureText(err), 4*LineMax)}}
		switch {
		case NotFound(err):
			l = append(l, own(" (a moteld older than this read, or no app of this name now)"))
		case Forbidden(err):
			l = append(l, s.askAgain(now)...)
		}
		return Sentence(l, width, cells)
	}
	var out []Line
	if err != nil {
		// Why, beside the answer kept: it may be older than the window's
		// newest step.
		out = append(out, Sentence(Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(err), 2*LineMax)}}, width, cells)...)
	}
	v := g.V
	switch v.Store {
	case wire.StoreOn:
	case wire.StoreOff:
		return append(out, Sentence(Line{own("this box keeps no numbers: its monitor block has no metrics line")}, width, cells)...)
	case wire.StoreUnanswered:
		return append(out, Sentence(Line{ownC("not known: the box could not ask its metrics store", Warn), own(" (it asks at most every 30 s for everyone)")}, width, cells)...)
	default:
		return append(out, Sentence(Line{ownC("not known: the box says ", Warn), boxT(v.Store), own(" of its store")}, width, cells)...)
	}
	span, ok := seriesSpans[v.Window]
	step := time.Duration(v.StepSeconds) * time.Second
	if !ok || v.Window != rng || v.End == nil || step <= 0 || step > span {
		// The client package holds a box's answer to these; a fixture may
		// not.
		return append(out, Sentence(Line{ownC("not known: the box's answer names no window, step or end this screen can draw", Warn)}, width, cells)...)
	}
	end := v.End.UTC()
	start := end.Add(-span)
	// Width 0 is each line whole: the plots at 100 columns'.
	plotW := 100 - plotLabel - 2
	if width > 0 {
		plotW = max(width-plotLabel-2, 10)
	}
	head := Line{own("over " + rng + " · a point every " + SeriesStep(step) + " · to " + clock(end, now, false) + " UTC (" + since(end, now) + ") · t: another window")}
	out = append(out, cutTo(head, width, cells))
	marks, legend := s.deployMarks(app, start, end, plotW, now)
	out = append(out, Sentence(legend, width, cells)...)
	// Each plot as tall as lets the page show the first whole under the
	// lines above: its head, its rows, the axis, the times.
	var heads [][]Line
	var drawn []seriesPanel
	for _, p := range seriesPanels {
		if pn := findPanel(v.Panels, p); pn != nil {
			drawn = append(drawn, p)
			heads = append(heads, panelHead(p, inWindow(pn.Points, start, end), now, width, cells))
		}
	}
	rows := 3
	if len(heads) > 0 {
		rows = min(max(height-len(out)-1-len(heads[0])-2, 3), 10)
	}
	known := true
	for _, pn := range v.Panels {
		if _, ok := SeriesLabel(pn.Name, pn.Unit); !ok {
			known = false
		}
	}
	// The requests panel's rate at each step the store had: 0 where no
	// request came. The 5xx share and the p95 have no point at a step of
	// none, nor the p95 at a step of cache hits alone, which is no gap in
	// what the store had. With no requests point in the window, none is
	// known.
	var rates map[int64]float64
	if rq := findPanel(v.Panels, seriesPanel{name: wire.PanelRequests, unit: wire.UnitPerSecond}); rq != nil {
		for _, pt := range inWindow(rq.Points, start, end) {
			if rates == nil {
				rates = map[int64]float64{}
			}
			rates[int64(pt[0])] = pt[1]
		}
	}
	has := map[string]bool{}
	for i, p := range drawn {
		has[p.name] = true
		out = append(out, nil)
		out = append(out, heads[i]...)
		var r map[int64]float64
		if p.name == wire.Panel5xx || p.name == wire.PanelP95 {
			r = rates
		}
		out = append(out, panelPlot(p, inWindow(findPanel(v.Panels, p).Points, start, end), r, start, end, step, now, plotW, rows, marks, width, cells)...)
	}
	ran := has[wire.PanelMemory] || has[wire.PanelCPU]
	switch {
	case !ran && known:
		out = append(out, nil, cutTo(field("memory, cpu", own("no point in this window: the app did not run, or the box kept none")), width, cells))
	case !ran:
		out = append(out, nil, cutTo(field("memory, cpu", own("none drawn in this window")), width, cells))
	}
	switch {
	case !has[wire.PanelRequests] && known && !ran:
		out = append(out, nil, cutTo(field("requests", own("none in this window")), width, cells))
	case !has[wire.PanelRequests] && known:
		out = append(out, nil, cutTo(field("requests", own("none in this window: the box counts them only for an app behind observe")), width, cells))
	case !has[wire.PanelRequests]:
		// A panel the client does not know may be the requests' under a
		// newer box's name: no cause is said.
		out = append(out, nil, cutTo(field("requests", own("none drawn in this window")), width, cells))
	}
	for _, pn := range v.Panels {
		if _, known := SeriesLabel(pn.Name, pn.Unit); !known {
			out = append(out, cutTo(field("other", own("the box's "), boxT(pn.Name), own(" in "), boxT(pn.Unit), own(": not one this client draws")), width, cells))
		}
	}
	return out
}

// findPanel is the panel of p's name and unit, or nil.
func findPanel(ps []wire.Panel, p seriesPanel) *wire.Panel {
	for i := range ps {
		if ps[i].Name == p.name && ps[i].Unit == p.unit {
			return &ps[i]
		}
	}
	return nil
}

// inWindow is the points inside [start, end] with a value that is a
// number, oldest first as the box sent them.
func inWindow(pts [][2]float64, start, end time.Time) [][2]float64 {
	in := make([][2]float64, 0, len(pts))
	for _, pt := range pts {
		if t := pt[0]; t >= float64(start.Unix()) && t <= float64(end.Unix()) && finite(pt[1]) {
			in = append(in, pt)
		}
	}
	return in
}

// panelHead is a panel's head: its newest value with its time and age;
// its highest and lowest, as the axis writes them, with their times, on
// one row when it holds both, else a row each; or, every point of one
// value, that value.
func panelHead(p seriesPanel, in [][2]float64, now time.Time, width int, cells Cells) []Line {
	if len(in) == 0 {
		return []Line{cutTo(field(p.label, own("no point in this window")), width, cells)}
	}
	lo, hi := extremes(in)
	newest := in[len(in)-1]
	at := func(pt [2]float64) string { return clock(time.Unix(int64(pt[0]), 0), now, false) }
	out := []Line{cutTo(field(p.label, own("newest "+SeriesNewest(p.name, newest[1], hi[1], lo[1])+" at "+at(newest)+" ("+since(time.Unix(int64(newest[0]), 0), now)+")")), width, cells)}
	if hi[1] == lo[1] {
		// One value: said once.
		v, _ := SeriesExtremes(p.name, hi[1], lo[1], false)
		return append(out, cutTo(field("", own("every point "+v)), width, cells))
	}
	hiS, loS := SeriesExtremes(p.name, hi[1], lo[1], true)
	maxS, minS := "max "+hiS+" "+at(hi), "min "+loS+" "+at(lo)
	if both := field("", own(maxS+" · "+minS)); width <= 0 || both.Width(cells) <= width {
		return append(out, both)
	}
	// Too wide for one row (a year in the times): a row each, neither
	// losing its time.
	return append(out, cutTo(field("", own(maxS)), width, cells), cutTo(field("", own(minS)), width, cells))
}

// extremes is the lowest and the highest point.
func extremes(in [][2]float64) (lo, hi [2]float64) {
	lo, hi = in[0], in[0]
	for _, pt := range in {
		if pt[1] < lo[1] {
			lo = pt
		}
		if pt[1] > hi[1] {
			hi = pt
		}
	}
	return lo, hi
}

// panelPlot is a panel's plot, its highest and lowest at the top and
// the bottom of its axis (every point of one value, or of values no
// label tells apart: that value once, on the row of its flat line); then the time axis, broken (a space) in each
// column whose span holds a step with no point and marked ^ at each
// deploy; and the window's two ends under it, with how many of its
// steps have a point. rates, when not nil, is the requests panel's rate
// at each step the store had: a step of them with no point here is no
// gap (no request, or none this panel counts: a cache hit has no p95),
// so it breaks no column, and the count is of the steps that had
// requests (a rate above 0), with how many have a point when that
// differs.
func panelPlot(p seriesPanel, in [][2]float64, rates map[int64]float64, start, end time.Time, step time.Duration, now time.Time, plotW, rows int, marks []int, width int, cells Cells) []Line {
	if len(in) == 0 {
		return nil
	}
	// A window of more steps than any box's (a fixture's step of a second
	// over 14 days) is left unbroken, and drawn with no bridge.
	many := end.Sub(start)/step > 4096
	if many {
		rates = nil
	}
	var stored map[int64]bool
	requested := 0
	if rates != nil {
		stored = map[int64]bool{}
		for t, r := range rates {
			stored[t] = true
			if r != 0 {
				requested++
			}
		}
	}
	lo, hi := extremes(in)
	var out []Line
	hiS, loS := SeriesExtremes(p.name, hi[1], lo[1], true)
	// Highest and lowest no label tells apart (a page of memory in an
	// hour) are drawn as one value, flat, as their labels read.
	same := hiS == loS
	bottom := lo[1]
	if same {
		bottom = hi[1]
	}
	plot := Plot(in, stored, start, end, step, bottom, hi[1], plotW, rows)
	flat := -1
	if same {
		for i, r := range plot {
			if strings.TrimSpace(r) != "" {
				flat = i
				break
			}
		}
	}
	for i, r := range plot {
		label := ""
		switch {
		case same:
			if i == flat {
				label = hiS
			}
		case i == 0:
			label = hiS
		case i == len(plot)-1:
			label = loS
		}
		if r := []rune(label); len(r) > plotLabel {
			// A number past every writing's room (none a box sends): cut,
			// marked in ASCII, as an ellipsis is of ambiguous width.
			label = string(r[:plotLabel-1]) + "~"
		}
		out = append(out, cutTo(Line{ownC(padLeft(label, plotLabel)+" |", Dim), own(r)}, width, cells))
	}
	axis := []byte(strings.Repeat("-", plotW))
	have := make(map[int64]bool, len(in))
	for _, pt := range in {
		have[int64(pt[0])] = true
	}
	hits := len(have)
	if !many {
		hits = 0
		for t := start; !t.After(end); t = t.Add(step) {
			if have[t.Unix()] {
				hits++
				continue
			}
			if stored[t.Unix()] {
				continue
			}
			// A step with no point breaks every cell its span (the step up
			// to its time) is drawn in.
			from := cellOf(maxTime(start, t.Add(-step+time.Second)), start, end, plotW)
			for c := from; c <= cellOf(t, start, end, plotW); c++ {
				axis[c] = ' '
			}
		}
	}
	for _, m := range marks {
		axis[m] = '^'
	}
	out = append(out, cutTo(Line{ownC(strings.Repeat(" ", plotLabel)+" +", Dim), own(string(axis))}, width, cells))
	// The window's ends, and between them how many of its steps have a
	// point, which the breaks in the axis show where.
	from, to := clock(start, now, false), clock(end, now, false)
	// The longest that fits: with how many have a point, then without.
	counts := []string{strconv.Itoa(hits) + " of " + strconv.Itoa(int(end.Sub(start)/step)+1) + " steps"}
	if rates != nil {
		had := strconv.Itoa(requested) + " of " + strconv.Itoa(int(end.Sub(start)/step)+1) + " steps had requests"
		counts = []string{had + ", " + strconv.Itoa(hits) + " with a point", had}
		if hits == requested {
			counts = counts[1:]
		}
	}
	room := plotW - len(from) - len(to)
	mid := strings.Repeat(" ", max(room, 1))
	for _, count := range counts {
		if room >= len(count)+2 {
			left := (room - len(count)) / 2
			mid = strings.Repeat(" ", left) + count + strings.Repeat(" ", room-left-len(count))
			break
		}
	}
	out = append(out, cutTo(Line{ownC(strings.Repeat(" ", plotLabel+2)+from+mid+to, Dim)}, width, cells))
	return out
}

// dotOf is the dot column of t on a plot w cells wide over [start, end],
// two a cell: the one mapping the plot, its axis and its marks share.
func dotOf(t float64, start, end time.Time, w int) int {
	t0, t1 := float64(start.Unix()), float64(end.Unix())
	if t1 <= t0 || w <= 0 {
		return 0
	}
	return min(max(int(math.Round((t-t0)/(t1-t0)*float64(2*w-1))), 0), 2*w-1)
}

// cellOf is the cell t's dot is in.
func cellOf(t, start, end time.Time, w int) int { return dotOf(float64(t.Unix()), start, end, w) / 2 }

func padLeft(s string, n int) string {
	if r := len([]rune(s)); r < n {
		return strings.Repeat(" ", n-r) + s
	}
	return s
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// deployMarks is the columns of the plots' time axis that hold a deploy
// that succeeded, from the app's status, which its screen reads, and
// the line that says what the marks are: the latest deploy of each
// version the box keeps a record of (the status's deploys), so a
// version deployed again is marked at its last deploy alone, and the
// records reach back as far as the box keeps them, which the line says
// when that is inside the window. With no status read, no marks, and
// the line says why. A column is a span of the window: two deploys in
// one are one mark.
func (s *Session) deployMarks(app string, start, end time.Time, plotW int, now time.Time) ([]int, Line) {
	col := end.Sub(start) / time.Duration(plotW)
	a := s.apps[app]
	if a == nil || a.st == nil {
		return nil, Line{own("no deploy marks: the app's status is not read yet")}
	}
	var marks []int
	var oldest time.Time
	unread := 0
	for _, d := range a.st.V.Deploys {
		t, ok := rawTime(d.FinishedAt)
		if !ok || t.IsZero() {
			if !noTime(d.FinishedAt) {
				unread++
			}
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
		if d.Status != "succeeded" || t.Before(start) || t.After(end) {
			continue
		}
		c := cellOf(t, start, end, plotW)
		if !slices.Contains(marks, c) {
			marks = append(marks, c)
		}
	}
	l := Line{own("^ a version's latest deploy that succeeded (a column is " + roughSpan(col) + ")")}
	switch {
	case oldest.IsZero() && unread == 0:
		l = Line{own("no deploy marks: the box keeps no deploy record of " + app)}
	case !oldest.IsZero() && oldest.After(start):
		l = append(l, own("; records from "+clock(oldest, now, false)))
	}
	if unread > 0 {
		l = append(l, own("; "+strconv.Itoa(unread)+" not marked (no finish time that reads)"))
	}
	return marks, l
}

// roughSpan is a column's span as a person reads it: 1m12s, 29m, 6h43m.
func roughSpan(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/time.Hour)) + "h" + strconv.Itoa(int(d%time.Hour/time.Minute)) + "m"
}

// Plot draws points (oldest first, inside [start, end]) on w cells by
// rows, each cell two dots wide and four high (a braille character):
// a point's dot at its time and its value between lo, the bottom row,
// and hi, the top; two points a step or less apart joined by a line,
// so a step with no point is a gap, never a zero or a line across it.
// A point joined to neither is drawn across its step's span (the step
// up to its time), and two dots wide at the least, so it shows and is
// never one dot of a bridge; so too a run of points joined all on one
// dot. stored, when not nil, is the steps the
// store had (the requests panel's): two points further apart than a
// step with every step between them stored (no gap) are bridged by a
// dotted line, a dot every second cell. An empty cell is a space.
// Values outside lo and hi are drawn at the edge; with lo equal to hi,
// every point on the middle row, or at zero on the bottom row.
func Plot(pts [][2]float64, stored map[int64]bool, start, end time.Time, step time.Duration, lo, hi float64, w, rows int) []string {
	if w <= 0 || rows <= 0 {
		return nil
	}
	dx, dy := 2*w, 4*rows
	grid := make([][]bool, dy)
	for i := range grid {
		grid[i] = make([]bool, dx)
	}
	x := func(t float64) int {
		if math.IsNaN(t) {
			return 0
		}
		return dotOf(t, start, end, w)
	}
	y := func(v float64) int {
		f := 0.5
		if hi == 0 && lo == 0 {
			f = 0
		}
		if hi > lo {
			f = (v - lo) / (hi - lo)
			if !finite(hi - lo) {
				// A span past the float's range: halved, it fits.
				f = (v/2 - lo/2) / (hi/2 - lo/2)
			}
		}
		if !finite(f) {
			f = 0.5
		}
		return min(max(int(math.Round(f*float64(dy-1))), 0), dy-1)
	}
	set := func(cx, cy int) { grid[dy-1-cy][cx] = true }
	s := step.Seconds()
	// joined says point i is joined to the one before.
	joined := func(i int) bool { return i > 0 && i < len(pts) && !(pts[i][0]-pts[i-1][0] > s) }
	for i, pt := range pts {
		cx, cy := x(pt[0]), y(pt[1])
		set(cx, cy)
		left := cx
		// A run of points joined from here to j: alone, or all on one dot
		// (several steps a dot column), it is drawn across its first
		// step's span, at its height, two dots wide at the least (the one
		// after it, at the window's start), so never one dot as a
		// bridge's are.
		j, one := i, !joined(i)
		for one && joined(j+1) {
			j++
			one = x(pts[j][0]) == cx && y(pts[j][1]) == cy
		}
		if one {
			left = x(math.Max(pt[0]-s+1, float64(start.Unix())))
			right := cx
			switch {
			case left == cx && cx > 0:
				left--
			case left == cx && cx+1 < dx:
				right++
			}
			for xx := left; xx <= right; xx++ {
				set(xx, cy)
			}
		}
		if i == 0 {
			continue
		}
		px, py := x(pts[i-1][0]), y(pts[i-1][1])
		if !joined(i) {
			if bridged(stored, pts[i-1][0], pt[0], s) {
				// No gap between (no request, or none this panel counts): a
				// dot every fourth column (every second cell) on the line
				// between, none beside either.
				for xx := px + 4; xx < left-1; xx += 4 {
					set(xx, lerp(px, py, cx, cy, xx))
				}
			}
			continue
		}
		// Joined to the one before: a line of dots between the two.
		if px == cx {
			// In one column (more points than dots): every dot between.
			for yy := min(py, cy); yy <= max(py, cy); yy++ {
				set(cx, yy)
			}
			continue
		}
		for xx := px; xx <= cx; xx++ {
			// The line's height at xx and at the next column, and every
			// dot between, so a steep line is unbroken.
			ya, yb := lerp(px, py, cx, cy, xx), lerp(px, py, cx, cy, min(xx+1, cx))
			for yy := min(ya, yb); yy <= max(ya, yb); yy++ {
				set(xx, yy)
			}
		}
	}
	out := make([]string, rows)
	for r := range rows {
		var b strings.Builder
		for c := range w {
			var bits rune
			for dyy := range 4 {
				for dxx := range 2 {
					if grid[r*4+dyy][c*2+dxx] {
						bits |= brailleBit[dxx][dyy]
					}
				}
			}
			if bits == 0 {
				b.WriteByte(' ')
			} else {
				b.WriteRune(0x2800 + bits)
			}
		}
		out[r] = b.String()
	}
	return out
}

// brailleBit is the dot of a braille cell at its column and its row
// from the top (Unicode's dots 1 to 8).
var brailleBit = [2][4]rune{{0x01, 0x02, 0x04, 0x40}, {0x08, 0x10, 0x20, 0x80}}

// bridged says every step between the points at a and b, more than a
// step apart, is in stored.
func bridged(stored map[int64]bool, a, b, step float64) bool {
	if stored == nil || step <= 0 || !(b-a > step) {
		return false
	}
	for t := a + step; t < b; t += step {
		if !stored[int64(t)] {
			return false
		}
	}
	return true
}

// lerp is the height at x of the line from (x0, y0) to (x1, y1).
func lerp(x0, y0, x1, y1, x int) int {
	if x1 == x0 {
		return y1
	}
	return y0 + int(math.Round(float64((y1-y0)*(x-x0))/float64(x1-x0)))
}
