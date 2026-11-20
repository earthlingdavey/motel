package model_test

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

func seriesJob(app, rng string) model.Job {
	return model.Job{Kind: model.KSeries, App: app, Range: rng}
}

func count(calls []call, j model.Job) (n int, at []time.Time) {
	for _, c := range calls {
		if c.j == j {
			n++
			at = append(at, c.at)
		}
	}
	return n, at
}

// takeUntil takes reads at now, each answered, until j is taken; then
// it is in flight, with a cancel as Drive gives it.
func takeUntil(t *testing.T, e *model.Engine, j model.Job, now *time.Time, answer func(model.Job, time.Time) error) {
	t.Helper()
	for range 6000 {
		k, okTake, _ := e.Take(*now)
		if !okTake {
			*now = now.Add(100 * time.Millisecond)
			continue
		}
		e.Started(func() {})
		if k == j {
			return
		}
		e.Done(k, *now, answer(k, *now))
	}
	t.Fatalf("%+v was not taken", j)
}

// TestSeriesReadWhileShown: a window of an app's series is read only
// while the metrics screen shows it, at once when it opens, then every
// SeriesEvery; another window is read in its place, the first not; none
// once the screen closes; ctrl-r asks it no sooner than AgainAfter; and
// the box's limit is never passed.
func TestSeriesReadWhileShown(t *testing.T) {
	e := model.NewEngine(t0)
	calls := sim(e, names(3), t0, time.Minute, 10*time.Millisecond, ok)
	for _, c := range calls {
		if c.j.Kind == model.KSeries {
			t.Fatalf("a series read with no metrics screen open: %v", c)
		}
	}
	from := t0.Add(time.Minute)
	e.Focus("app-1", from)
	e.SeriesOpen("app-1", "24h", from)
	calls = sim(e, names(3), from, 2*time.Minute, 10*time.Millisecond, ok)
	boxBucket(t, calls)
	n, at := count(calls, seriesJob("app-1", "24h"))
	if n < 4 || n > 5 || at[0].Sub(from) > time.Second {
		t.Errorf("read %d times in two minutes, first at +%s: want at once, then every %s", n, at[0].Sub(from), model.SeriesEvery)
	}
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < model.SeriesEvery || d > model.SeriesEvery+time.Second {
			t.Errorf("read %s after the last", d)
		}
	}
	from = from.Add(2 * time.Minute)
	e.SeriesOpen("app-1", "7d", from)
	calls = sim(e, names(3), from, time.Minute, 10*time.Millisecond, ok)
	if n, _ := count(calls, seriesJob("app-1", "24h")); n != 0 {
		t.Errorf("the 24h read %d times once 7d was shown", n)
	}
	if n, at := count(calls, seriesJob("app-1", "7d")); n < 2 || at[0].Sub(from) > time.Second {
		t.Errorf("7d read %d times in a minute, first at %v", n, at)
	}
	from = from.Add(time.Minute)
	e.Again(from, seriesJob("app-1", "7d"))
	calls = sim(e, names(3), from, 2*time.Second, 10*time.Millisecond, ok)
	if n, _ := count(calls, seriesJob("app-1", "7d")); n > 1 {
		t.Errorf("ctrl-r read it %d times in 2 s", n)
	}
	from = from.Add(2 * time.Second)
	e.SeriesOpen("", "", from)
	calls = sim(e, names(3), from, time.Minute, 10*time.Millisecond, ok)
	for _, c := range calls {
		if c.j.Kind == model.KSeries {
			t.Fatalf("a series read once the screen closed: %v", c)
		}
	}
}

// TestSeriesReopened: shown again within SeriesEvery of its answer, a
// window is read when that answer is SeriesEvery old, not at once;
// refused (a box older than the read answers 404), it is not asked
// again by showing it, only by ctrl-r.
func TestSeriesReopened(t *testing.T) {
	e := model.NewEngine(t0)
	j := seriesJob("app-1", "1h")
	sim(e, names(3), t0, 10*time.Second, 10*time.Millisecond, ok)
	open := t0.Add(10 * time.Second)
	e.Focus("app-1", open)
	e.SeriesOpen("app-1", "1h", open)
	_, at := count(sim(e, names(3), open, 2*time.Second, 10*time.Millisecond, ok), j)
	if len(at) == 0 {
		t.Fatal("not read on opening the screen")
	}
	answered := at[0]
	now := open.Add(2 * time.Second)
	e.SeriesOpen("app-1", "24h", now)
	e.SeriesOpen("app-1", "1h", now.Add(3*time.Second))
	_, again := count(sim(e, names(3), now.Add(3*time.Second), 40*time.Second, 10*time.Millisecond, ok), j)
	if len(again) == 0 || again[0].Sub(answered) < model.SeriesEvery {
		t.Errorf("shown again: read at %v, its answer at %v", again, answered)
	}
	now = now.Add(43 * time.Second)
	notFound := func(k model.Job, _ time.Time) error {
		if k == j {
			return fail(model.FailNotFound)
		}
		return nil
	}
	sim(e, names(3), now, 40*time.Second, 10*time.Millisecond, notFound)
	now = now.Add(40 * time.Second)
	e.SeriesOpen("", "", now)
	e.SeriesOpen("app-1", "1h", now)
	if n, _ := count(sim(e, names(3), now, time.Minute, 10*time.Millisecond, notFound), j); n != 0 {
		t.Errorf("a refused read asked %d times by showing it again", n)
	}
	now = now.Add(time.Minute)
	e.Again(now, j)
	if n, _ := count(sim(e, names(3), now, 2*time.Second, 10*time.Millisecond, notFound), j); n != 1 {
		t.Errorf("ctrl-r asked the refused read %d times", n)
	}
}

// TestSeriesLeftWhileInFlight: the screen left and shown again while a
// window's first read is in flight: the read is cancelled, and the
// window is read again, not forgotten while it is shown.
func TestSeriesLeftWhileInFlight(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(3), t0, 10*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(10 * time.Second)
	j := seriesJob("app-1", "14d")
	e.Focus("app-1", now)
	e.SeriesOpen("app-1", "14d", now)
	takeUntil(t, e, j, &now, ok)
	e.SeriesOpen("", "", now)
	e.SeriesOpen("app-1", "14d", now)
	e.Done(j, now, context.Canceled)
	if n, _ := count(sim(e, names(3), now, 3*time.Second, 10*time.Millisecond, ok), j); n != 1 {
		t.Errorf("read %d times after the cancelled read, want once", n)
	}
	// Another window shown while one is in flight: the same.
	k := seriesJob("app-1", "7d")
	e.SeriesOpen("app-1", "7d", now)
	takeUntil(t, e, k, &now, ok)
	e.SeriesOpen("app-1", "1h", now)
	e.SeriesOpen("app-1", "7d", now)
	e.Done(k, now, context.Canceled)
	if n, _ := count(sim(e, names(3), now, 3*time.Second, 10*time.Millisecond, ok), k); n != 1 {
		t.Errorf("7d read %d times after its cancelled read, want once", n)
	}
}

// TestSeriesClosedAsAnswered: a window whose screen closed as its
// answer came (a 403) keeps nothing of it, as the session keeps
// nothing: shown again, it is read, not held unseen.
func TestSeriesClosedAsAnswered(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(3), t0, 10*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(10 * time.Second)
	j := seriesJob("app-1", "24h")
	e.Focus("app-1", now)
	e.SeriesOpen("app-1", "24h", now)
	takeUntil(t, e, j, &now, ok)
	e.SeriesOpen("", "", now)
	e.Done(j, now, fail(model.FailForbidden))
	now = now.Add(time.Second)
	e.SeriesOpen("app-1", "24h", now)
	if n, _ := count(sim(e, names(3), now, 3*time.Second, 10*time.Millisecond, ok), j); n != 1 {
		t.Errorf("read %d times on showing it again, want once", n)
	}
}

// TestSeriesKeepTheBackoff: a failing series read is asked no sooner
// than its backoff by showing it again and again, and one cancelled by
// leaving the screen keeps its count of failures.
func TestSeriesKeepTheBackoff(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(3), t0, 10*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(10 * time.Second)
	j := seriesJob("app-1", "1h")
	e.Focus("app-1", now)
	e.SeriesOpen("app-1", "1h", now)
	unusable := func(k model.Job, _ time.Time) error {
		if k == j {
			return fail(model.FailUnusable)
		}
		return nil
	}
	calls := sim(e, names(3), now, time.Second, 10*time.Millisecond, unusable)
	now = now.Add(time.Second)
	for range 20 {
		e.SeriesOpen("", "", now)
		e.SeriesOpen("app-1", "1h", now)
		calls = append(calls, sim(e, names(3), now, 50*time.Millisecond, 10*time.Millisecond, unusable)...)
		now = now.Add(50 * time.Millisecond)
	}
	if n, _ := count(calls, j); n > 2 {
		t.Errorf("the failing read %d times in a second of showing it again and again", n)
	}

	// A fresh engine: the read has never been answered or refused.
	e = model.NewEngine(t0)
	sim(e, names(3), t0, 10*time.Second, 10*time.Millisecond, ok)
	now = t0.Add(10 * time.Second)
	e.Focus("app-1", now)
	e.SeriesOpen("app-1", "1h", now)
	failing := func(k model.Job, _ time.Time) error {
		if k == j {
			return fail(model.FailNoAnswer)
		}
		return nil
	}
	sim(e, names(3), now, 2*time.Minute, 10*time.Millisecond, failing)
	now = now.Add(2 * time.Minute)
	takeUntil(t, e, j, &now, ok)
	e.SeriesOpen("", "", now)
	e.Done(j, now, context.Canceled)
	e.SeriesOpen("app-1", "1h", now)
	_, at := count(sim(e, names(3), now, 3*time.Minute, 10*time.Millisecond, failing), j)
	if len(at) < 2 || at[1].Sub(at[0]) <= model.SeriesEvery+time.Second {
		t.Errorf("after the cancelled read, the failing reads at %v", at)
	}
}

// TestHeldSeriesTakeNoShare: the other apps' interval leaves room for
// the series only while it is read: refused and held, the share is
// given back.
func TestHeldSeriesTakeNoShare(t *testing.T) {
	apps := names(60)
	e := model.NewEngine(t0)
	sim(e, apps, t0, 10*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(10 * time.Second)
	e.Focus("app-1", now)
	plain := e.Interval("app-2")
	e.SeriesOpen("app-1", "24h", now)
	if e.Interval("app-2") <= plain {
		t.Fatalf("with the series read, the others' interval is %s, not over %s", e.Interval("app-2"), plain)
	}
	notFound := func(k model.Job, _ time.Time) error {
		if k.Kind == model.KSeries {
			return fail(model.FailNotFound)
		}
		return nil
	}
	sim(e, apps, now, 5*time.Second, 10*time.Millisecond, notFound)
	if got := e.Interval("app-2"); got != plain {
		t.Errorf("with the series held, the others' interval is %s, want %s", got, plain)
	}
}

// session is a session over the fake, the app's status and its series
// over rng read.
func seriesSession(t *testing.T, app, rng string, f *demo.Fake, now *time.Time) *model.Session {
	t.Helper()
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(*now)}
	s.Engine.Focus(app, *now)
	s.Engine.SeriesOpen(app, rng, *now)
	for range 12 {
		step(t, s, f, now)
	}
	return s
}

func text(ls []model.Line) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.Text() + "\n")
	}
	return b.String()
}

// TestMetricsStates: what the screen says for each answer that is not
// a window to draw: no answer yet, no metrics line, a store that did
// not answer, a word it does not know, an older moteld's 404 (and the
// title bar not saying refused), a 403; and an answer kept beside a
// read that failed since.
func TestMetricsStates(t *testing.T) {
	for _, c := range []struct {
		name, body string
		err        error
		want       string
	}{
		{"off", `{"store":"off","window":"24h","panels":[]}`, nil, "this box keeps no numbers: its monitor block has no metrics line"},
		{"unanswered", `{"store":"unanswered","window":"24h","panels":[]}`, nil, "not known: the box could not ask its metrics store"},
		{"a word", `{"store":"sleeping","window":"24h","panels":[]}`, nil, "not known: the box says sleeping of its store"},
		{"404", "", &model.Failure{Kind: model.FailNotFound, Message: `deploy.example.com answered 404; the box says: "not found"`}, `answered 404; the box says: "not found" (a moteld older than this read, or no app of this name now)`},
		{"403", "", &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com refused: this needs the viewer role on this app."}, "refused: this needs the viewer role on this app."},
		{"no window", `{"store":"on","window":"24h","panels":[]}`, nil, "not known: the box's answer names no window, step or end"},
	} {
		now := t0
		f := &demo.Fake{Now: func() time.Time { return now }}
		f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
		if c.body != "" {
			f.Body("series/blog/24h.json", c.body)
		}
		if c.err != nil {
			f.Set("series:blog", c.err)
		}
		s := seriesSession(t, "blog", "24h", f, &now)
		got := text(s.MetricsViewFit("blog", "24h", now, 0, 27, nil))
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if c.name == "404" {
			if a, b := s.Title(now); strings.Contains(a.Text()+b.Text(), "refused") {
				t.Errorf("the title bar says refused after the series' 404: %q", a.Text()+b.Text())
			}
		}
	}
	// No answer yet.
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(now)}
	if got := text(s.MetricsViewFit("blog", "7d", now, 0, 27, nil)); !strings.Contains(got, "asking deploy.example.com for blog's numbers over 7d") {
		t.Errorf("no answer yet: %q", got)
	}
	// An answer kept beside a failure since.
	f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
	s = seriesSession(t, "blog", "1h", f, &now)
	f.Set("series:blog", fail(model.FailUnusable))
	s.Engine.Again(now.Add(time.Minute), seriesJob("blog", "1h"))
	now = now.Add(time.Minute)
	for range 6 {
		if j := step(t, s, f, &now); j.Kind == model.KSeries {
			break
		}
	}
	got := text(s.MetricsViewFit("blog", "1h", now, 0, 27, nil))
	if !strings.Contains(got, "the last read failed: no") || !strings.Contains(got, "memory      newest") || !strings.Contains(s.MetricsAsOf("blog", "1h", now).Text(), "the last read failed") {
		t.Errorf("an answer beside a failure: %q", got)
	}
}

// TestStatusRefusedDropsTheSeries: a series read before the box refused
// the app's status is not shown again.
func TestStatusRefusedDropsTheSeries(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
	s := seriesSession(t, "blog", "24h", f, &now)
	if g, _ := s.Series("blog", "24h"); g == nil {
		t.Fatal("no series read")
	}
	f.Set("blog", fail(model.FailForbidden))
	s.Engine.Again(now, model.Job{Kind: model.KStatus, App: "blog"})
	for range 4 {
		if j := step(t, s, f, &now); j.Kind == model.KStatus {
			break
		}
	}
	if g, _ := s.Series("blog", "24h"); g != nil {
		t.Error("the series is kept after the status was refused")
	}
	if got := text(s.MetricsViewFit("blog", "24h", now, 0, 27, nil)); strings.Contains(got, "asking") || !strings.Contains(got, "no") {
		t.Errorf("the screen after the status was refused: %q", got)
	}
	// A series answer that comes after the status's refusal is not shown
	// under it either.
	s.Engine.Again(now.Add(time.Minute), seriesJob("blog", "24h"))
	now = now.Add(time.Minute)
	for range 6 {
		if j := step(t, s, f, &now); j.Kind == model.KSeries {
			break
		}
	}
	if got := text(s.MetricsViewFit("blog", "24h", now, 0, 27, nil)); strings.Contains(got, "memory") || !strings.Contains(got, "no") {
		t.Errorf("a series answered after the status was refused is shown: %q", got)
	}
	// Nor in the JSON overlay or the crumb.
	if _, _, have := s.Body(model.View{Screen: model.ScreenMetrics, App: "blog", Range: "24h"}); have || s.MetricsAsOf("blog", "24h", now) != nil {
		t.Error("the overlay or the crumb holds the series beside the status's refusal")
	}
	// Refused first, the series is held; the status refused after drops
	// nothing that would let the screen say it is asking.
	f2 := &demo.Fake{Now: func() time.Time { return now }}
	f2.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
	f2.Set("series:blog", fail(model.FailForbidden))
	s2 := seriesSession(t, "blog", "24h", f2, &now)
	f2.Set("blog", fail(model.FailForbidden))
	s2.Engine.Again(now, model.Job{Kind: model.KStatus, App: "blog"})
	for range 4 {
		if j := step(t, s2, f2, &now); j.Kind == model.KStatus {
			break
		}
	}
	if got := text(s2.MetricsViewFit("blog", "24h", now, 0, 27, nil)); strings.Contains(got, "asking") {
		t.Errorf("a held series beside a refused status says it is asking: %q", got)
	}
}

// seriesBody is a 24 h body of blog's, ending at end, whose memory has
// a point at each of the given steps back from the end.
func seriesBody(end time.Time, back ...int) string {
	var pts [][2]float64
	for i := len(back) - 1; i >= 0; i-- {
		pts = append(pts, [2]float64{float64(end.Add(-time.Duration(back[i]) * 5 * time.Minute).Unix()), float64(100+back[i]) * (1 << 20)})
	}
	s := wire.Series{Store: wire.StoreOn, Window: "24h", StepSeconds: 300, End: &end, Panels: []wire.Panel{{Name: wire.PanelMemory, Unit: wire.UnitBytes, Points: pts}}}
	b, _ := json.Marshal(s)
	return string(b)
}

// TestMetricsDeployMarks: the time axis marks each version's latest
// deploy that succeeded inside the window, at its column, and says what
// the marks are, how long a column is, and from when the records reach
// when that is inside the window; a deploy whose time the box's filter
// hid is not marked, and said; with no status read, no marks, and said.
func TestMetricsDeployMarks(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
	end := now.Truncate(5 * time.Minute)
	f.Body("series/blog/24h.json", seriesBody(end, 0, 1, 2, 100, 200, 287))
	f.Body("status/blog.json", `{"app":"blog","phase":"idle","running":true,"current_version":"v3","available_versions":["v3"],"deploys":[`+
		`{"version":"v3","status":"succeeded","finished_at":"@-6h"},`+
		`{"version":"v2","status":"failed","finished_at":"@-8h"},`+
		`{"version":"v1","status":"succeeded","finished_at":"@-20h"},`+
		`{"version":"v0","status":"succeeded","finished_at":"[redacted:K]"}]}`)
	s := seriesSession(t, "blog", "24h", f, &now)
	ls := s.MetricsViewFit("blog", "24h", now, 60, 13, runes)
	if got := text(s.MetricsViewFit("blog", "24h", now, 0, 13, nil)); !strings.Contains(got, "^ a version's latest deploy that succeeded (a column is 15m); records from 11-14 18:00; 1 not marked: the box's filter hid when") {
		t.Errorf("legend: %q", got)
	}
	// Laid on rows at 60 columns, the rest indented under it.
	if got := ls[1].Text() + "|" + ls[2].Text(); got != "^ a version's latest deploy that succeeded (a column is|  28m); records from 11-14 18:00; 1 not marked: the box's" {
		t.Errorf("legend at 60 columns: %q", got)
	}
	var axis string
	for _, l := range ls {
		if strings.HasPrefix(l.Text(), "        +") {
			axis = strings.TrimPrefix(l.Text(), "        +")
			break
		}
	}
	// 51 columns over 24 h: v1 at 4 h in (column 8), v3 at 18 h in
	// (column 38); the failed v2 not marked.
	if strings.Count(axis, "^") != 2 || strings.IndexByte(axis, '^') != 8 || strings.LastIndexByte(axis, '^') != 38 {
		t.Errorf("axis %q", axis)
	}
	// No status read: no marks.
	s2 := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(now)}
	s2.Engine.SeriesOpen("blog", "24h", now)
	for range 6 {
		if j := step(t, s2, f, &now); j.Kind == model.KSeries {
			break
		}
	}
	if got := text(s2.MetricsViewFit("blog", "24h", now, 60, 13, runes)); !strings.Contains(got, "no deploy marks: the app's status is not read yet") || strings.Contains(got, "^-") || strings.Contains(got, "-^") {
		t.Errorf("no status: %q", got)
	}
}

// TestMetricsAt60x16: the screen at the smallest window: every line at
// most 60 cells, the first plot whole on the page's 13 rows, its axis
// labels the highest and lowest values; and a panel absent says why,
// a panel the client does not know is named as the box's.
func TestMetricsAt60x16(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "docs"}]}`)
	s := seriesSession(t, "docs", "24h", f, &now)
	ls := s.MetricsViewFit("docs", "24h", now, 60, 13, runes)
	for i, l := range ls {
		if n := utf8.RuneCountInString(l.Text()); n > 60 {
			t.Errorf("line %d is %d cells: %q", i, n, l.Text())
		}
	}
	got := text(ls)
	// The first plot whole in 13 rows: its head's two rows, its rows,
	// the axis, the times with the count of steps.
	head := strings.Split(got, "\n")
	first := -1
	for i, l := range head {
		if strings.HasPrefix(l, "memory") {
			first = i
			break
		}
	}
	if first < 0 || len(head) < 13 || !strings.HasPrefix(head[first+1], "            max ") || !strings.HasPrefix(head[11], "        +") ||
		!strings.HasPrefix(head[12], "         ") || !strings.Contains(head[12], " of 289 steps") || 11-first-2 < 5 {
		t.Errorf("the first plot is not whole in 13 rows, with five rows or more:\n%s", got)
	}
	if got := text(s.MetricsViewFit("docs", "24h", now, 0, 13, nil)); !strings.Contains(got, "requests    none in this window: the box counts them only for an app behind observe") {
		t.Errorf("no request panels unsaid:\n%s", got)
	}
	end := now.Truncate(5 * time.Minute)
	b := wire.Series{Store: wire.StoreOn, Window: "24h", StepSeconds: 300, End: &end, Panels: []wire.Panel{
		{Name: "disk_bytes", Unit: "bytes", Points: [][2]float64{{float64(end.Unix()), 1}}},
		{Name: wire.PanelCPU, Unit: "millicores", Points: [][2]float64{{float64(end.Unix()), 1}}},
	}}
	body, _ := json.Marshal(b)
	f.Body("series/docs/24h.json", string(body))
	s.Engine.Again(now.Add(time.Minute), seriesJob("docs", "24h"))
	now = now.Add(time.Minute)
	for range 6 {
		if j := step(t, s, f, &now); j.Kind == model.KSeries {
			break
		}
	}
	got = text(s.MetricsViewFit("docs", "24h", now, 0, 13, nil))
	for _, want := range []string{"other       the box's disk_bytes in bytes: not one this client draws", "other       the box's cpu_cores in millicores: not one this client draws", "requests    none drawn in this window"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
	// A panel it does not know may be one the filter renamed: no cause.
	if strings.Contains(got, "observe") || strings.Contains(got, "did not run") {
		t.Errorf("a cause said beside a panel the client does not know:\n%s", got)
	}
}

// TestPlot: a dot a point, two points a step apart joined and two
// further apart not (a gap is a gap, never a line across it, never a
// zero); the highest on the top row and the lowest on the bottom;
// every row as wide as asked, an empty cell a space, and no blank
// braille pattern; and no panic or row of another width whatever the
// numbers.
func TestPlot(t *testing.T) {
	end := t0
	start := end.Add(-time.Hour)
	step := time.Minute
	at := func(min int) float64 { return float64(start.Add(time.Duration(min) * time.Minute).Unix()) }
	// Points at 0..9 and 40..50, minutes into the hour, at 1 and 2: the
	// gap between 9 and 40 is no line.
	var pts [][2]float64
	for m := 0; m <= 9; m++ {
		pts = append(pts, [2]float64{at(m), 1})
	}
	for m := 40; m <= 50; m++ {
		pts = append(pts, [2]float64{at(m), 2})
	}
	rows := model.Plot(pts, start, end, step, 1, 2, 30, 4)
	if len(rows) != 4 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, r := range rows {
		if utf8.RuneCountInString(r) != 30 || strings.ContainsRune(r, 0x2800) {
			t.Errorf("row %q", r)
		}
	}
	// The gap, columns 5 to 19 (minutes 10 to 39 at two minutes a
	// column): nothing in any row.
	for _, r := range rows {
		if gap := string([]rune(r)[6:19]); strings.TrimSpace(gap) != "" {
			t.Errorf("a line across the gap: %q", r)
		}
	}
	if strings.TrimSpace(rows[0]) == "" || strings.TrimSpace(rows[3]) == "" {
		t.Errorf("highest not on the top row or lowest not on the bottom:\n%s", strings.Join(rows, "\n"))
	}
	// A step joined: two points a step apart at the two ends, a line of
	// dots between them in every row.
	two := [][2]float64{{at(0), 0}, {at(1), 10}}
	for _, r := range model.Plot(two, start, start.Add(time.Minute), step, 0, 10, 4, 4) {
		if strings.TrimSpace(r) == "" {
			t.Errorf("two points a step apart not joined: %q", r)
		}
	}
	// Hostile numbers, sizes, orders.
	weird := [][2]float64{{at(3), math.MaxFloat64}, {at(1), -math.MaxFloat64}, {at(2), 0}, {at(2), 1e300}, {at(-90), 5}, {at(900), 5}, {math.NaN(), 1}}
	for _, w := range []int{1, 2, 10, 51, 300} {
		for _, h := range []int{1, 3, 10} {
			for _, lohi := range [][2]float64{{-math.MaxFloat64, math.MaxFloat64}, {0, 0}, {5, 1}, {math.Inf(-1), math.Inf(1)}, {math.NaN(), 1}} {
				rs := model.Plot(weird, start, end, step, lohi[0], lohi[1], w, h)
				if len(rs) != h {
					t.Fatalf("%dx%d: %d rows", w, h, len(rs))
				}
				for _, r := range rs {
					if utf8.RuneCountInString(r) != w {
						t.Fatalf("%dx%d %v: row %q", w, h, lohi, r)
					}
				}
			}
		}
	}
	if model.Plot(pts, start, end, step, 1, 2, 0, 3) != nil || model.Plot(pts, end, start, step, 1, 2, 5, 3) == nil {
		t.Error("a plot of no width is no rows; one of a window backwards is drawn")
	}
}

// TestSeriesValue: the client's writing of each panel's numbers.
func TestSeriesValue(t *testing.T) {
	for _, c := range []struct {
		name    string
		v       float64
		compact bool
		want    string
	}{
		{wire.PanelMemory, 190840832, false, "182.0 MiB"},
		{wire.PanelMemory, 190840832, true, "182M"},
		{wire.PanelMemory, 3 << 30, true, "3G"},
		{wire.PanelMemory, 1.5 * (1 << 30), false, "1.50 GiB"},
		{wire.PanelCPU, 0.004, false, "0.004 cores"},
		{wire.PanelCPU, 0.0004, true, "<0.001"},
		{wire.PanelCPU, 0, true, "0.000"},
		{wire.PanelCPU, 12.5, true, "12.5"},
		{wire.PanelRequests, 2.5, false, "2.50/s"},
		{wire.PanelRequests, 0.004, false, "<0.01/s"},
		{wire.Panel5xx, 0.0123, false, "1.2%"},
		{wire.Panel5xx, 0, false, "0%"},
		{wire.Panel5xx, 0.0004, false, "<0.1%"},
		{wire.PanelP95, 80.1, false, "80.1 ms"},
		{wire.PanelP95, 1250, true, "1.2s"},
		{wire.PanelP95, 80.1, true, "80ms"},
		{wire.PanelMemory, 400 << 10, true, "400K"},
		{wire.PanelRequests, 150000, true, "150k/s"},
		{wire.PanelRequests, 2.5e6, true, "2.5M/s"},
	} {
		if got := model.SeriesValue(c.name, c.v, c.compact); got != c.want {
			t.Errorf("%s %v %v: %q, want %q", c.name, c.v, c.compact, got, c.want)
		}
	}
	if model.NextRange("1h") != "24h" || model.NextRange("14d") != "1h" || model.NextRange("bogus") != "1h" {
		t.Error("NextRange")
	}
}

// TestMetricsAxisBreaks: the time axis is broken (a space) in each
// column whose span holds a step with no point, so a gap of a step or
// two shows where the plot's dots run on (several steps a column), and
// the count of steps with a point is under it; a deploy's mark is in
// the column its time's dot is in, as the plot draws it (the mark was
// one column right of it in the window's right half).
func TestMetricsAxisBreaks(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
	end := now.Truncate(5 * time.Minute)
	var back []int
	for k := 0; k <= 288; k++ {
		if k != 100 && k != 101 {
			back = append(back, k)
		}
	}
	f.Body("series/blog/24h.json", seriesBody(end, back...))
	start := end.Add(-24 * time.Hour)
	deploy := start.Add(67765 * time.Second)
	f.Body("status/blog.json", `{"app":"blog","phase":"idle","running":true,"current_version":"v3","available_versions":["v3"],"deploys":[`+
		`{"version":"v3","status":"succeeded","finished_at":"`+deploy.Format(time.RFC3339)+`"}]}`)
	s := seriesSession(t, "blog", "24h", f, &now)
	const w = 51
	cell := func(at time.Time) int {
		return int(math.Round(at.Sub(start).Seconds()/end.Sub(start).Seconds()*float64(2*w-1))) / 2
	}
	var axis, times string
	ls := s.MetricsViewFit("blog", "24h", now, 60, 13, runes)
	for i, l := range ls {
		if strings.HasPrefix(l.Text(), "        +") {
			axis, times = strings.TrimPrefix(l.Text(), "        +"), ls[i+1].Text()
			break
		}
	}
	// Each missing step breaks the cells of its span, the step up to it.
	want := map[int]byte{}
	for _, k := range []int{100, 101} {
		at := end.Add(-time.Duration(k) * 5 * time.Minute)
		for c := cell(at.Add(-5*time.Minute + time.Second)); c <= cell(at); c++ {
			want[c] = ' '
		}
	}
	want[cell(deploy)] = '^'
	for i := 0; i < len(axis); i++ {
		c, ok := want[i]
		if !ok {
			c = '-'
		}
		if axis[i] != c {
			t.Errorf("axis column %d is %q, want %q: %q", i, axis[i], c, axis)
		}
	}
	if cell(deploy) != 39 || !strings.Contains(times, "287 of 289 steps") {
		t.Errorf("the deploy's column %d, want 39 (not 40); times %q", cell(deploy), times)
	}
}

// TestMetricsHeadsAndBreaksAtTheEdges: a head too wide for one row of
// its highest and lowest (a year in their times, across New Year) is
// two rows, each whole at 60 columns with its time; a step with no
// point breaks every cell its span is drawn in, where the window has
// fewer steps than the plot has cells (the hour at 100 columns); with
// no point of the app at all, no cause is said for the request panels;
// and an axis label longer than its room is cut, marked.
func TestMetricsHeadsAndBreaksAtTheEdges(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 30, 0, 0, time.UTC)
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
	end := now.Truncate(30 * time.Minute)
	pts := func(step time.Duration, n int, skip int, v func(i int) float64) [][2]float64 {
		var out [][2]float64
		for i := n; i >= 0; i-- {
			if i != skip {
				out = append(out, [2]float64{float64(end.Add(-time.Duration(i) * step).Unix()), v(i)})
			}
		}
		return out
	}
	week := wire.Series{Store: wire.StoreOn, Window: "7d", StepSeconds: 1800, End: &end, Panels: []wire.Panel{
		{Name: wire.PanelRequests, Unit: wire.UnitPerSecond, Points: pts(30*time.Minute, 336, -1, func(i int) float64 { return float64(i%201) * 0.00001 })},
	}}
	b, _ := json.Marshal(week)
	f.Body("series/blog/7d.json", string(b))
	s := seriesSession(t, "blog", "7d", f, &now)
	ls := s.MetricsViewFit("blog", "7d", now, 60, 30, runes)
	var max, min string
	for _, l := range ls {
		if n := utf8.RuneCountInString(l.Text()); n > 60 {
			t.Errorf("a row of %d cells: %q", n, l.Text())
		}
		switch {
		case strings.HasPrefix(l.Text(), "            max "):
			max = l.Text()
		case strings.HasPrefix(l.Text(), "            min "):
			min = l.Text()
		}
	}
	if !strings.Contains(max, "max <0.01/s 2026-12-27 20:30") || !strings.Contains(min, "min 0.00/s 2026-12-27 20:00") || strings.Contains(max+min, "…") {
		t.Errorf("max %q, min %q", max, min)
	}

	// The hour at 100 columns: 61 steps on 91 cells; the step 30 back is
	// missing, so the cells from the step before it to it are broken.
	hourEnd := now.Truncate(time.Minute).Add(-time.Minute)
	hour := wire.Series{Store: wire.StoreOn, Window: "1h", StepSeconds: 60, End: &hourEnd, Panels: []wire.Panel{
		{Name: wire.PanelMemory, Unit: wire.UnitBytes, Points: nil}}}
	for i := 60; i >= 0; i-- {
		if i != 30 {
			hour.Panels[0].Points = append(hour.Panels[0].Points, [2]float64{float64(hourEnd.Add(-time.Duration(i) * time.Minute).Unix()), 1 << 30})
		}
	}
	b, _ = json.Marshal(hour)
	f.Body("series/blog/1h.json", string(b))
	s.Engine.SeriesOpen("blog", "1h", now)
	for range 6 {
		if j := step(t, s, f, &now); j.Kind == model.KSeries {
			break
		}
	}
	var axis string
	for _, l := range s.MetricsViewFit("blog", "1h", now, 100, 30, runes) {
		if strings.HasPrefix(l.Text(), "        +") {
			axis = strings.TrimPrefix(l.Text(), "        +")
		}
	}
	if gap := strings.Count(strings.TrimSpace(axis), " "); gap < 2 || strings.Count(axis, " ") != gap {
		t.Errorf("the hour's missing step breaks %d cells: %q", gap, axis)
	}

	// No point of the app at all: no cause said.
	f.Body("series/blog/24h.json", `{"store":"on","window":"24h","step_seconds":300,"end":"`+now.Truncate(5*time.Minute).Format(time.RFC3339)+`","panels":[]}`)
	s.Engine.SeriesOpen("blog", "24h", now)
	for range 6 {
		if j := step(t, s, f, &now); j.Kind == model.KSeries {
			break
		}
	}
	if got := text(s.MetricsViewFit("blog", "24h", now, 0, 30, nil)); strings.Contains(got, "observe") || !strings.Contains(got, "requests    none in this window") {
		t.Errorf("no point at all: %q", got)
	}
}
