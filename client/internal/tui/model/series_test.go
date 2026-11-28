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
// when that is inside the window; a deploy whose finish time does not
// read is not marked, and said; with no status read, no marks, and said.
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
	if got := text(s.MetricsViewFit("blog", "24h", now, 0, 13, nil)); !strings.Contains(got, "^ a version's latest deploy that succeeded (a column is 15m); records from 11-14 18:00; 1 not marked: their finish time does not read") {
		t.Errorf("legend: %q", got)
	}
	// Laid on rows at 60 columns, the rest indented under it.
	if got := ls[1].Text() + "|" + ls[2].Text(); got != "^ a version's latest deploy that succeeded (a column is|  28m); records from 11-14 18:00; 1 not marked: their finish" {
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
	rows := model.Plot(pts, nil, start, end, step, 1, 2, 30, 4)
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
	for _, r := range model.Plot(two, nil, start, start.Add(time.Minute), step, 0, 10, 4, 4) {
		if strings.TrimSpace(r) == "" {
			t.Errorf("two points a step apart not joined: %q", r)
		}
	}
	// Hostile numbers, sizes, orders.
	weird := [][2]float64{{at(3), math.MaxFloat64}, {at(1), -math.MaxFloat64}, {at(2), 0}, {at(2), 1e300}, {at(-90), 5}, {at(900), 5}, {math.NaN(), 1}}
	for _, w := range []int{1, 2, 10, 51, 300} {
		for _, h := range []int{1, 3, 10} {
			for _, lohi := range [][2]float64{{-math.MaxFloat64, math.MaxFloat64}, {0, 0}, {5, 1}, {math.Inf(-1), math.Inf(1)}, {math.NaN(), 1}} {
				rs := model.Plot(weird, nil, start, end, step, lohi[0], lohi[1], w, h)
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
	if model.Plot(pts, nil, start, end, step, 1, 2, 0, 3) != nil || model.Plot(pts, nil, end, start, step, 1, 2, 5, 3) == nil {
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
		{wire.PanelCPU, 0.0004, true, "0.0004"},
		{wire.PanelCPU, 0.000123, false, "0.00012 cores"},
		{wire.PanelCPU, 1e-9, false, "<0.000001 cores"},
		{wire.PanelCPU, 0, true, "0.000"},
		{wire.PanelCPU, 12.5, true, "12.5"},
		{wire.PanelRequests, 2.5, false, "2.50/s"},
		{wire.PanelRequests, 0.004, false, "0.004/s"},
		{wire.PanelRequests, 0.0033333, false, "0.0033/s"},
		{wire.PanelRequests, 1.0 / 3600, false, "1.0/h"},
		{wire.PanelRequests, 0.0001, true, "0.36/h"},
		{wire.Panel5xx, 0.0123, false, "1.2%"},
		{wire.Panel5xx, 0, false, "0%"},
		{wire.Panel5xx, 0.0004, false, "0.04%"},
		{wire.Panel5xx, 0.00001, false, "<0.01%"},
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
	if !strings.Contains(max, "max 0.002/s 2026-12-27 20:30") || !strings.Contains(min, "min 0.000/s 2026-12-27 20:00") || strings.Contains(max+min, "…") {
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

// TestPlotQuiet: a point joined to neither neighbour is drawn across
// its step's span, at its height; two points further apart than a step
// with every step between them stored are bridged by a dot every second
// cell on the line between, none beside either point; a stored step
// missing between them, or no steps stored at all, leaves the gap
// blank; and every point of zero is on the bottom row, every point of
// one other value on the middle.
func TestPlotQuiet(t *testing.T) {
	end := t0
	start := end.Add(-time.Hour)
	step := time.Minute
	at := func(min int) float64 { return float64(start.Add(time.Duration(min) * time.Minute).Unix()) }
	stored := map[int64]bool{}
	for m := 0; m <= 60; m++ {
		stored[int64(at(m))] = true
	}
	// 61 steps on 91 cells: a step is three dots. Two points at minutes 10
	// and 40, at 0 and 1.
	pts := [][2]float64{{at(10), 0}, {at(40), 1}}
	cell := func(min int) int { return int(math.Round(float64(min)/60*181)) / 2 }
	cols := func(rows []string) []bool {
		out := make([]bool, 91)
		for _, r := range rows {
			for i, c := range []rune(r) {
				out[i] = out[i] || c != ' '
			}
		}
		return out
	}
	rows := model.Plot(pts, stored, start, end, step, 0, 1, 91, 4)
	got := cols(rows)
	// Each point across its step's span: the cell of the step before it
	// up to its own.
	for _, m := range []int{10, 40} {
		if !got[cell(m)] || !got[cell(m)-1] {
			t.Errorf("the point at minute %d is not drawn across its step:\n%s", m, strings.Join(rows, "\n"))
		}
	}
	// The bridge: every second cell between, and so none two cells in a
	// row, the first two cells clear of the point before it.
	var dots []int
	for c := cell(10) + 1; c < cell(40)-1; c++ {
		if got[c] {
			dots = append(dots, c)
		}
	}
	if len(dots) < 10 || dots[0] < cell(10)+2 {
		t.Fatalf("the bridge's dots in cells %v:\n%s", dots, strings.Join(rows, "\n"))
	}
	for i := 1; i < len(dots); i++ {
		if dots[i]-dots[i-1] != 2 {
			t.Errorf("the bridge's dots in cells %v, not every second cell", dots)
			break
		}
	}
	// It climbs: its first dot on the bottom row, its last above it.
	first, last := []rune(rows[3])[dots[0]], []rune(rows[3])[dots[len(dots)-1]]
	if first == ' ' || last != ' ' {
		t.Errorf("the bridge does not climb from 0 to 1:\n%s", strings.Join(rows, "\n"))
	}
	// A step between them not stored, or none stored: no bridge.
	delete(stored, int64(at(25)))
	for _, st := range []map[int64]bool{stored, nil} {
		got := cols(model.Plot(pts, st, start, end, step, 0, 1, 91, 4))
		for c := cell(10) + 1; c < cell(40)-1; c++ {
			if got[c] {
				t.Errorf("a dot in cell %d between points a store gap apart (stored %v)", c, st != nil)
				break
			}
		}
	}
	// Points a step apart: joined by the solid line, not spread.
	two := model.Plot([][2]float64{{at(10), 0}, {at(11), 0}}, stored, start, end, step, 0, 0, 91, 4)
	if got := strings.TrimSpace(two[3]); utf8.RuneCountInString(got) != cell(11)-cell(10)+1 {
		t.Errorf("two points a step apart: %q", two[3])
	}
	// Flat: zero on the bottom row, another value on the middle.
	for _, c := range []struct {
		v   float64
		row int
	}{{0, 3}, {0.5, 1}} {
		rows := model.Plot([][2]float64{{at(10), c.v}, {at(11), c.v}}, nil, start, end, step, c.v, c.v, 91, 4)
		for i, r := range rows {
			if (strings.TrimSpace(r) != "") != (i == c.row) {
				t.Errorf("every point %v: drawn on rows\n%s\nwant row %d alone", c.v, strings.Join(rows, "\n"), c.row)
				break
			}
		}
	}
}

// dotCols is the dot columns of a plot's rows that hold a dot.
func dotCols(rows []string) map[int]bool {
	out := map[int]bool{}
	for _, r := range rows {
		for c, ch := range []rune(r) {
			if ch == ' ' {
				continue
			}
			bits := ch - 0x2800
			if bits&(0x01|0x02|0x04|0x40) != 0 {
				out[2*c] = true
			}
			if bits&(0x08|0x10|0x20|0x80) != 0 {
				out[2*c+1] = true
			}
		}
	}
	return out
}

// TestPlotLonePoints: in a window of many steps a column (a day's 289
// on 91 cells), a point joined to neither, or a run of points joined
// all on one dot, is two dots wide, so never one dot as a bridge's are,
// which stay one dot every fourth column; and a point alone on the
// window's first step is two dots wide too.
func TestPlotLonePoints(t *testing.T) {
	end := t0
	start := end.Add(-24 * time.Hour)
	step := 5 * time.Minute
	at := func(k int) float64 { return float64(start.Add(time.Duration(k) * step).Unix()) }
	stored := map[int64]bool{}
	for k := 0; k <= 288; k++ {
		stored[int64(at(k))] = true
	}
	pts := [][2]float64{{at(10), 0}, {at(50), 0}, {at(51), 0}, {at(120), 0}}
	got := dotCols(model.Plot(pts, stored, start, end, step, 0, 0, 91, 4))
	dot := func(k int) int { return int(math.Round(float64(k) / 288 * 181)) }
	for _, k := range []int{10, 120} {
		if !got[dot(k)] || !got[dot(k)-1] {
			t.Errorf("the lone point at step %d is not two dots wide: dots %v", k, got)
		}
	}
	// The bridges' dots: none beside another.
	for c := dot(10) + 1; c < dot(120)-1; c++ {
		if c >= dot(50)-1 && c <= dot(51)+1 {
			continue
		}
		if got[c] && got[c+1] {
			t.Errorf("two dots side by side in a bridge at dot column %d: %v", c, got)
		}
	}
	// A run of points joined all on one dot (two steps of a day at 91
	// cells, three at 51) is two dots wide, as a point alone is.
	for _, c := range []struct{ w, from, to int }{{91, 52, 53}, {51, 2, 4}} {
		var run [][2]float64
		for k := c.from; k <= c.to; k++ {
			run = append(run, [2]float64{at(k), 0})
		}
		got := dotCols(model.Plot(run, stored, start, end, step, 0, 0, c.w, 4))
		if len(got) < 2 {
			t.Errorf("steps %d to %d at %d cells: dots %v, not two wide", c.from, c.to, c.w, got)
		}
	}
	// Alone on the first step of the hour: two dots wide.
	hour := model.Plot([][2]float64{{float64(t0.Add(-time.Hour).Unix()), 1}}, nil, t0.Add(-time.Hour), t0, time.Minute, 1, 1, 91, 4)
	if got := dotCols(hour); !got[0] || !got[1] {
		t.Errorf("a point alone on the first step: dots %v", got)
	}
}

// TestSeriesExtremes: a panel's highest and lowest are written alike
// until they differ, to as many places in one unit, never coarser than
// their plain writing, within a plot label's seven cells when compact,
// never a "<" floor or a zero for a value above it, nor so coarse that
// a narrow spread rounds wide; in the highest's unit, a value either
// side of a unit's boundary too; cores too small for seven cells in
// millicores, and rates in an hour's; one value once; and two values
// no writing in seven cells tells apart in one writing, each in its own
// when none tells it in the highest's unit, or a panel the client does
// not know, as SeriesValue writes them.
func TestSeriesExtremes(t *testing.T) {
	for _, c := range []struct {
		name         string
		hi, lo       float64
		compact      bool
		wantHi, want string
	}{
		{wire.PanelCPU, 0.0007, 0.0001, true, "0.0007", "0.0001"},
		{wire.PanelCPU, 0.0012, 0.0001, true, "0.0012", "0.0001"},
		{wire.PanelCPU, 0.0016, 0.0009, false, "0.0016 cores", "0.0009 cores"},
		{wire.Panel5xx, 0.0015, 0.0009, true, "0.15%", "0.09%"},
		{wire.PanelCPU, 0.000043, 0.000041, true, "0.043m", "0.041m"},
		{wire.PanelCPU, 0.5, 0.0002, true, "0.5000", "0.0002"},
		{wire.PanelCPU, 0.5, 0.0, true, "0.500", "0.000"},
		{wire.PanelCPU, 12.51, 12.54, true, "12.51", "12.54"},
		{wire.PanelCPU, 0.000043, 0.000041, false, "0.000043 cores", "0.000041 cores"},
		{wire.PanelCPU, 0.0004, 0.0004, true, "0.0004", "0.0004"},
		{wire.Panel5xx, 0.0004, 0.0002, true, "0.04%", "0.02%"},
		{wire.Panel5xx, 0.0123, 0.0121, true, "1.23%", "1.21%"},
		{wire.Panel5xx, 0.0123, 0.0121, false, "1.23%", "1.21%"},
		{wire.Panel5xx, 0, 0, true, "0%", "0%"},
		{wire.PanelP95, 0.5, 0.3, true, "0.5ms", "0.3ms"},
		{wire.PanelP95, 0.3, 0.3, true, "0.3ms", "0.3ms"},
		{wire.PanelP95, 1250, 1240, true, "1.25s", "1.24s"},
		{wire.PanelP95, 80.1, 25, true, "80ms", "25ms"},
		{wire.PanelMemory, 190840832, 187695104, true, "182M", "179M"},
		{wire.PanelMemory, 190840832, 190840000, true, "182M", "182M"},
		{wire.PanelRequests, 0.0004, 0.0003, true, "1.4/h", "1.1/h"},
		{wire.PanelRequests, 1.0 / 3600, 0, true, "1.0/h", "0.0/h"},
		{wire.Panel5xx, 0.0151, 0.0149, true, "1.51%", "1.49%"},
		{wire.PanelMemory, 150.51 * (1 << 20), 150.49 * (1 << 20), false, "150.51 MiB", "150.49 MiB"},
		{wire.PanelRequests, 1.0 / 3600, 0, false, "1.0/h", "0.0/h"},
		{"disk_bytes", 2, 1, true, "2", "1"},
		// No pair fits alike: each in its own writing that fits.
		{wire.PanelCPU, 0.2, 0.000012, true, "0.200", "0.012m"},
		{wire.PanelRequests, 0.5, 0.003333, true, "1800/h", "12/h"},
		// A narrow spread, to places fine enough.
		{wire.PanelP95, 1.6, 1.4, true, "1.6ms", "1.4ms"},
		{wire.PanelP95, 12.6, 12.4, true, "12.6ms", "12.4ms"},
		{wire.PanelMemory, 182.6 * (1 << 20), 182.4 * (1 << 20), true, "182.6M", "182.4M"},
		// Either side of a unit's boundary: the highest's unit.
		{wire.PanelMemory, 1<<30 + 4096, 1<<30 - 4096, true, "1G", "1G"},
		{wire.PanelMemory, 1 << 20, 1<<20 - 100, true, "1M", "1M"},
		{wire.PanelP95, 1000.2, 999.9, true, "1.0002s", "0.9999s"},
		{wire.PanelRequests, 1000.4, 999.6, true, "1k/s", "1k/s"},
	} {
		h, l := model.SeriesExtremes(c.name, c.hi, c.lo, c.compact)
		if c.compact && c.name != "disk_bytes" && (utf8.RuneCountInString(h) > 7 || utf8.RuneCountInString(l) > 7) {
			t.Errorf("%s %v %v: a label wider than its room: %q %q", c.name, c.hi, c.lo, h, l)
		}
		if h != c.wantHi || l != c.want {
			t.Errorf("%s %v %v %v: %q %q, want %q %q", c.name, c.hi, c.lo, c.compact, h, l, c.wantHi, c.want)
		}
	}
	// The newest as finely as the highest and lowest, when that tells it.
	for _, c := range []struct {
		name      string
		v, hi, lo float64
		want      string
	}{
		{wire.Panel5xx, 0.0121, 0.0123, 0.0121, "1.21%"},
		{wire.Panel5xx, 0.0122, 0.0123, 0.0121, "1.22%"},
		{wire.Panel5xx, 0, 0.0123, 0, "0.0%"},
		{wire.PanelCPU, 0.0005, 0.0007, 0.0001, "0.0005 cores"},
		{wire.PanelMemory, 182 << 20, 186 << 20, 179 << 20, "182.0 MiB"},
		{wire.PanelCPU, 0.0012, 0.0016, 0.0009, "0.0012 cores"},
		{wire.PanelP95, 1.5, 1.6, 1.4, "1.5 ms"},
	} {
		if got := model.SeriesNewest(c.name, c.v, c.hi, c.lo); got != c.want {
			t.Errorf("newest %s %v of %v..%v: %q, want %q", c.name, c.v, c.lo, c.hi, got, c.want)
		}
	}
}

// quietHour is a 1 h body of shop's ending at end: CPU at every step;
// requests at every step but a store gap from minute 35 to 40, none but
// at minutes 5, 12, 20, 21 and 50; and there alone, but at 12, where
// every request was aborted, the 5xx share, all 0, and the p95; with
// requests "none", no requests panel, "empty", one with no point.
func quietHour(end time.Time, requests string) string {
	start := end.Add(-time.Hour)
	busy := map[int]float64{5: 0.05, 12: 0.02, 20: 0.4, 21: 0.15, 50: 0.03}
	var cpu, rps, share, p95 [][2]float64
	for m := 0; m <= 60; m++ {
		ts := float64(start.Add(time.Duration(m) * time.Minute).Unix())
		cpu = append(cpu, [2]float64{ts, 0.0001 + 0.0001*float64(m%7)})
		if m >= 35 && m <= 40 {
			continue
		}
		rps = append(rps, [2]float64{ts, busy[m]})
		if _, ok := busy[m]; ok && m != 12 {
			share = append(share, [2]float64{ts, 0})
			p95 = append(p95, [2]float64{ts, 0.3 + float64(m%3)/10})
		}
	}
	s := wire.Series{Store: wire.StoreOn, Window: "1h", StepSeconds: 60, End: &end, Panels: []wire.Panel{
		{Name: wire.PanelCPU, Unit: wire.UnitCores, Points: cpu},
		{Name: wire.PanelRequests, Unit: wire.UnitPerSecond, Points: rps},
		{Name: wire.Panel5xx, Unit: wire.UnitShare, Points: share},
		{Name: wire.PanelP95, Unit: wire.UnitMs, Points: p95},
	}}
	switch requests {
	case "none":
		s.Panels = append(s.Panels[:1], s.Panels[2:]...)
	case "empty":
		s.Panels[1].Points = nil
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// plotOf is the lines of the plot under the head that begins with
// label: its rows, the axis (its "+" and what follows), the times.
func plotOf(ls []model.Line, label string) (rows []string, axis, times string) {
	for i, l := range ls {
		if !strings.HasPrefix(l.Text(), label) {
			continue
		}
		for _, l := range ls[i+1:] {
			switch t := l.Text(); {
			case strings.HasPrefix(t, "        +"):
				axis = strings.TrimPrefix(t, "        +")
			case axis != "":
				return rows, axis, t
			case strings.Contains(t, " |"):
				rows = append(rows, t)
			}
		}
	}
	return rows, axis, times
}

// TestMetricsQuiet: a quiet app's hour: the 5xx share's and the p95's
// steps with no point but a requests point are no gap, a request there
// or not (every one aborted), their axes broken only where the
// requests' is (the store's gap) and a bridge across them, and their
// counts say how many steps had requests and how many have a point;
// every point 0% is said once
// and labelled once, on the bottom row; the CPU's and a p95 under a
// millisecond's labels differ, and no value is a "<" floor. With no
// requests panel, each step with no point breaks the axis, as before.
func TestMetricsQuiet(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "shop"}]}`)
	end := now.Truncate(time.Minute)
	f.Body("series/shop/1h.json", quietHour(end, "full"))
	s := seriesSession(t, "shop", "1h", f, &now)
	ls := s.MetricsViewFit("shop", "1h", now, 100, 30, runes)
	got := text(ls)
	if strings.Contains(got, "<") {
		t.Errorf("a floor:\n%s", got)
	}
	_, rqAxis, _ := plotOf(ls, "requests")
	if strings.Count(rqAxis, " ") < 6 {
		t.Fatalf("the requests' axis is not broken at the store's gap: %q", rqAxis)
	}
	for _, label := range []string{"5xx share", "p95"} {
		rows, axis, times := plotOf(ls, label)
		if axis != rqAxis || !strings.Contains(times, "5 of 61 steps had requests, 4 with a point") {
			t.Errorf("%s: axis %q, want the requests' %q; times %q", label, axis, rqAxis, times)
		}
		// The bridge across minute 12, from minute 5's cells to minute
		// 20's: a dot in every second cell.
		dots := 0
		for c := 9; c < 27; c++ {
			for _, r := range rows {
				if []rune(strings.SplitN(r, " |", 2)[1])[c] != ' ' {
					dots++
					break
				}
			}
		}
		if dots < 8 {
			t.Errorf("%s: no bridge across the step with requests and no point:\n%s", label, strings.Join(rows, "\n"))
		}
	}
	// At 60 columns the count without how many have a point, which does
	// not fit.
	for _, label := range []string{"5xx share", "p95"} {
		if _, _, times := plotOf(s.MetricsViewFit("shop", "1h", now, 60, 30, runes), label); !strings.Contains(times, " 5 of 61 steps had requests ") {
			t.Errorf("%s at 60 columns: times %q", label, times)
		}
	}
	rows, _, _ := plotOf(ls, "5xx share")
	if !strings.Contains(got, "5xx share   newest 0% at 13:50 (10m ago)\n            every point 0%\n") || len(rows) < 3 {
		t.Errorf("5xx share's head:\n%s", got)
	}
	for i, r := range rows {
		label := strings.TrimSpace(strings.Split(r, " |")[0])
		if want := map[bool]string{true: "0%", false: ""}[i == len(rows)-1]; label != want || (strings.TrimSpace(strings.SplitN(r, " |", 2)[1]) != "") != (i == len(rows)-1) {
			t.Errorf("every point 0%%: row %d %q", i, r)
		}
	}
	for label, want := range map[string][2]string{"cpu": {" 0.0007 |", " 0.0001 |"}, "p95": {"  0.5ms |", "  0.3ms |"}} {
		rows, _, _ := plotOf(ls, label)
		if len(rows) < 3 || !strings.HasPrefix(rows[0], want[0]) || !strings.HasPrefix(rows[len(rows)-1], want[1]) {
			t.Errorf("%s's labels: %q", label, rows)
		}
	}
	if !strings.Contains(got, "cpu         newest 0.0005 cores at 14:00") || !strings.Contains(got, "max 0.0007 13:06 · min 0.0001 13:00") {
		t.Errorf("cpu's head:\n%s", got)
	}

	// No requests panel, or one with no point: each step with no point
	// is a gap.
	for _, kind := range []string{"none", "empty"} {
		f.Body("series/shop/1h.json", quietHour(end, kind))
		s.Engine.Again(now.Add(time.Minute), seriesJob("shop", "1h"))
		now = now.Add(time.Minute)
		for range 6 {
			if j := step(t, s, f, &now); j.Kind == model.KSeries {
				break
			}
		}
		ls = s.MetricsViewFit("shop", "1h", now, 100, 30, runes)
		_, axis, times := plotOf(ls, "5xx share")
		if strings.Count(axis, "-") > 12 || !strings.Contains(times, "4 of 61 steps ") || strings.Contains(times, "had requests") {
			t.Errorf("requests %s: axis %q, times %q", kind, axis, times)
		}
	}
}

// TestMetricsLabelsAlike: a highest and lowest no label tells apart (a
// page of memory over the hour) are drawn flat, mid-plot, and labelled
// once on that row, as one value is.
func TestMetricsLabelsAlike(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "shop"}]}`)
	end := now.Truncate(time.Minute)
	var pts [][2]float64
	for m := 60; m >= 0; m-- {
		pts = append(pts, [2]float64{float64(end.Add(-time.Duration(m) * time.Minute).Unix()), 150<<20 + float64(m%2*4096)})
	}
	b, _ := json.Marshal(wire.Series{Store: wire.StoreOn, Window: "1h", StepSeconds: 60, End: &end, Panels: []wire.Panel{{Name: wire.PanelMemory, Unit: wire.UnitBytes, Points: pts}}})
	f.Body("series/shop/1h.json", string(b))
	s := seriesSession(t, "shop", "1h", f, &now)
	rows, _, _ := plotOf(s.MetricsViewFit("shop", "1h", now, 100, 30, runes), "memory")
	drawn := 0
	for _, r := range rows {
		label, plot, _ := strings.Cut(r, " |")
		if strings.TrimSpace(plot) == "" {
			if strings.TrimSpace(label) != "" {
				t.Errorf("a label on an empty row: %q", r)
			}
			continue
		}
		drawn++
		if strings.TrimSpace(label) != "150M" {
			t.Errorf("the line's row is labelled %q", label)
		}
	}
	if len(rows) < 4 || drawn != 1 {
		t.Errorf("values the labels do not tell apart drawn on %d rows:\n%s", drawn, strings.Join(rows, "\n"))
	}
}
