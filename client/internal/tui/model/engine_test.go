package model_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

var t0 = time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)

type call struct {
	j  model.Job
	at time.Time
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "app-" + strconv.Itoa(i)
	}
	return out
}

// sim runs the engine on a fake clock for d, each read taking lat and
// ending with answer(j, at); the apps list answers apps. It returns
// every read and when it began.
func sim(e *model.Engine, apps []string, from time.Time, d, lat time.Duration, answer func(model.Job, time.Time) error) []call {
	var calls []call
	now := from
	for now.Before(from.Add(d)) {
		e.Touch(now) // a person at the keys: no idle pause
		j, ok, wait := e.Take(now)
		if !ok {
			if wait <= 0 || wait > time.Second {
				wait = 100 * time.Millisecond
			}
			now = now.Add(wait)
			continue
		}
		calls = append(calls, call{j, now})
		now = now.Add(lat)
		err := answer(j, now)
		if j.Kind == model.KApps && err == nil {
			e.SetApps(apps, now)
		}
		e.Done(j, now, err)
	}
	return calls
}

func ok(model.Job, time.Time) error { return nil }

// boxBucket fails when calls ever go past the box's limit for a
// person, written here as the box's own numbers: 2 a second, 60 at
// once, the bucket full at the start.
func boxBucket(t *testing.T, calls []call) {
	t.Helper()
	tokens, last := 60.0, t0
	for _, c := range calls {
		tokens = min(60, tokens+c.at.Sub(last).Seconds()*2)
		last = c.at
		tokens--
		if tokens < 0 {
			t.Fatalf("read %v at +%s goes past the box's limit", c.j, c.at.Sub(t0))
		}
	}
}

// TestSchedule: for 1, 12, 60 and 200 apps the box's limit is never
// passed, the open app is read every 5 s, and every other app at its
// interval, which grows with their number; no row goes stale while the
// box answers.
func TestSchedule(t *testing.T) {
	for _, c := range []struct {
		n     int
		every time.Duration // the others' interval with an app open
	}{{1, 30 * time.Second}, {12, 30 * time.Second}, {60, 55 * time.Second}, {200, 183 * time.Second}} {
		t.Run(strconv.Itoa(c.n)+" apps", func(t *testing.T) {
			apps := names(c.n)
			e := model.NewEngine(t0)
			e.Focus("app-0", t0)
			calls := sim(e, apps, t0, 20*time.Minute, 50*time.Millisecond, ok)
			boxBucket(t, calls)
			if got := e.Interval("app-1"); c.n > 1 && (got < c.every-time.Second || got > c.every+time.Second) {
				t.Errorf("the others' interval is %s, want about %s", got, c.every)
			}
			byApp := map[string][]time.Time{}
			for _, cl := range calls {
				if cl.j.Kind == model.KStatus {
					byApp[cl.j.App] = append(byApp[cl.j.App], cl.at)
				}
			}
			// After the first pass, every gap is within its interval and
			// a little: none starves.
			settle := t0.Add(2 * e.Interval("app-1"))
			for _, a := range apps {
				every := e.Interval(a)
				ts := byApp[a]
				if len(ts) < 2 {
					t.Fatalf("%s read %d times in 20 minutes", a, len(ts))
				}
				for i := 1; i < len(ts); i++ {
					if ts[i-1].Before(settle) {
						continue
					}
					if g := ts[i].Sub(ts[i-1]); g > every+every/5 || g < every-every/5 {
						t.Fatalf("%s read %s apart, its interval %s", a, g, every)
					}
				}
			}
			reads := float64(len(calls)) / (20 * 60)
			t.Logf("%d apps: %.2f reads a second; the open app every %s, the others every %s", c.n, reads, e.Interval("app-0"), e.Interval("app-1"))
		})
	}
}

// TestBurstWithinTheBoxs: the first paint of many apps spends at most
// the engine's own bucket, half the box's burst.
func TestBurstWithinTheBoxs(t *testing.T) {
	if model.Burst > model.BoxBurst/2 || model.Rate > model.BoxRate*0.8 {
		t.Fatalf("burst %d and rate %v are not well inside the box's %d and %v", model.Burst, model.Rate, model.BoxBurst, model.BoxRate)
	}
	e := model.NewEngine(t0)
	calls := sim(e, names(200), t0, 2*time.Second, time.Millisecond, ok)
	first := 0
	for _, c := range calls {
		if c.at.Before(t0.Add(time.Second)) {
			first++
		}
	}
	if first > model.Burst+2 {
		t.Errorf("%d reads in the first second, more than the bucket of %d", first, model.Burst)
	}
}

// TestBucketSpent: with the bucket empty, nothing more is taken until
// it refills.
func TestBucketSpent(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetApps(names(100), t0)
	n := 0
	for {
		j, ok, wait := e.Take(t0)
		if !ok {
			if wait <= 0 {
				t.Fatal("an empty bucket gives no wait")
			}
			break
		}
		e.Done(j, t0, nil)
		n++
	}
	if n != model.Burst {
		t.Fatalf("took %d at once, want the bucket's %d", n, model.Burst)
	}
}

// TestOneInFlight: no second read is taken while one is in flight.
func TestOneInFlight(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetApps(names(3), t0)
	if _, ok, _ := e.Take(t0); !ok {
		t.Fatal("nothing taken")
	}
	if j, ok, _ := e.Take(t0); ok {
		t.Fatalf("took %v with a read in flight", j)
	}
}

// TestIdlePause: ten minutes with no key pressed and nothing is read;
// a key resumes.
func TestIdlePause(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetApps(names(3), t0)
	later := t0.Add(model.Idle)
	if _, ok, _ := e.Take(later); ok {
		t.Fatal("read after ten idle minutes")
	}
	if h := e.Health(later); !h.Paused {
		t.Fatal("not said to be paused")
	}
	e.Touch(later)
	if _, ok, _ := e.Take(later); !ok {
		t.Fatal("a key did not resume the reads")
	}
}

func fail(k model.Fail) *model.Failure { return &model.Failure{Kind: k, Message: "no"} }

// TestSlowedBy429: a 429 is waited out for every read, by its
// Retry-After, or by NoRetryAfter with none; and the rest of the
// session runs slower.
func TestSlowedBy429(t *testing.T) {
	for _, c := range []struct {
		after, want time.Duration
	}{{12 * time.Second, 12 * time.Second}, {0, model.NoRetryAfter}} {
		e := model.NewEngine(t0)
		sent := false
		calls := sim(e, names(3), t0, 2*time.Minute, 10*time.Millisecond, func(j model.Job, _ time.Time) error {
			if j.Kind == model.KStatus && !sent {
				sent = true
				return &model.Failure{Kind: model.FailSlowed, RetryAfter: c.after}
			}
			return nil
		})
		for i, cl := range calls {
			if cl.j.Kind == model.KStatus {
				at := cl.at.Add(10 * time.Millisecond)
				if next := calls[i+1].at; next.Sub(at) < c.want {
					t.Fatalf("Retry-After %s: read again %s after the 429, want %s", c.after, next.Sub(at), c.want)
				}
				break
			}
		}
		if e.RateNow() >= model.Rate {
			t.Errorf("the rate is %v after a 429, not cut", e.RateNow())
		}
	}
}

// TestStopAndRetry: a 401 stops every read after one; r makes one
// whoami, and a second refusal stops again.
func TestStopAndRetry(t *testing.T) {
	for _, k := range []model.Fail{model.FailSignedOut, model.FailNotSigned, model.FailCertificate} {
		e := model.NewEngine(t0)
		n := 0
		sim(e, names(3), t0, time.Minute, 10*time.Millisecond, func(j model.Job, _ time.Time) error {
			if j.Kind == model.KStatus {
				n++
				return fail(k)
			}
			return nil
		})
		if n != 1 || e.Stopped() == model.Running {
			t.Fatalf("kind %d: %d reads refused, stopped %v", k, n, e.Stopped())
		}
		now := t0.Add(2 * time.Minute)
		e.Touch(now)
		e.Retry(now)
		j, ok, _ := e.Take(now)
		if !ok || j.Kind != model.KWhoAmI {
			t.Fatalf("after r, took %v %v, want whoami", j, ok)
		}
		e.Done(j, now, fail(k))
		if _, ok, _ := e.Take(now.Add(time.Hour)); ok {
			t.Fatal("a second refusal did not stop the reads")
		}
	}
}

// TestHeldUntilAsked: a 403 or a 404 stops that read alone until
// ctrl-r: it is not asked again by itself, the other reads carry on,
// and ctrl-r asks it again.
func TestHeldUntilAsked(t *testing.T) {
	for _, k := range []model.Fail{model.FailForbidden, model.FailNotFound} {
		e := model.NewEngine(t0)
		n, others := 0, 0
		answer := func(j model.Job, _ time.Time) error {
			if j.App == "app-1" {
				n++
				return fail(k)
			}
			if j.Kind == model.KStatus {
				others++
			}
			return nil
		}
		end := t0.Add(5 * time.Minute)
		sim(e, names(3), t0, 5*time.Minute, 10*time.Millisecond, answer)
		if n != 1 || others < 2*9 {
			t.Fatalf("kind %d: the refused read asked %d times in 5 minutes, the others %d", k, n, others)
		}
		e.Again(end, model.Job{Kind: model.KStatus, App: "app-1"})
		sim(e, names(3), end, time.Second, 10*time.Millisecond, answer)
		if n != 2 {
			t.Fatalf("kind %d: ctrl-r did not ask the refused read again (%d)", k, n)
		}
	}
}

// TestDropCancels: leaving a record whose read is in flight cancels
// it; asking for it again then is not lost to that cancelled read.
func TestDropCancels(t *testing.T) {
	e := model.NewEngine(t0)
	for _, j := range []model.Job{{Kind: model.KWhoAmI}, {Kind: model.KApps}} {
		got, _, _ := e.Take(t0)
		if got != j {
			t.Fatalf("took %v, want %v", got, j)
		}
		e.Done(got, t0, nil)
	}
	rec := model.Job{Kind: model.KDeploy, App: "blog", Version: "v1"}
	e.Once(rec, t0)
	j, ok, _ := e.Take(t0)
	if !ok || j != rec {
		t.Fatalf("took %v", j)
	}
	cancelled := false
	e.Started(func() { cancelled = true })
	e.Drop(rec)     // esc
	e.Once(rec, t0) // enter again, the first read still in flight
	e.Done(j, t0, context.Canceled)
	if !cancelled {
		t.Fatal("esc did not cancel the read in flight")
	}
	j, ok, _ = e.Take(t0)
	if !ok || j != rec {
		t.Fatalf("the record asked for again is not read: took %v %v", j, ok)
	}
}

// TestCancelledAnswerNotApplied: a read whose context was cancelled
// changes nothing on screen.
func TestCancelledAnswerNotApplied(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }, Delay: time.Second}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	apply, err := s.Fetch(ctx, f, model.Job{Kind: model.KDeploy, App: "blog", Version: "7c1e55d0a913"})
	apply()
	if err == nil {
		t.Fatal("a cancelled read answered")
	}
	if _, _, ok := s.Record("blog", "7c1e55d0a913"); ok {
		t.Fatal("a cancelled read's answer was applied")
	}
}

func step(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) model.Job {
	t.Helper()
	e := s.Engine
	e.Touch(*now)
	for range 2000 {
		j, ok, wait := e.Take(*now)
		if ok {
			apply, err := s.Fetch(context.Background(), f, j)
			apply()
			e.Done(j, *now, err)
			return j
		}
		*now = now.Add(max(wait, 100*time.Millisecond))
	}
	t.Fatal("no read in 200 s")
	return model.Job{}
}

func row(s *model.Session, now time.Time, name string) model.AppRow {
	for _, r := range s.AppsView(now, "", 1, 0).Rows {
		if r.Name == name {
			return r
		}
	}
	return model.AppRow{}
}

// TestStaleAfterThree: a row is stale after exactly three missed reads
// of its own, not two, and it says so in a word at every width; the
// box not answering at all backs off and the title says STALE.
func TestStaleAfterThree(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	if r := row(s, now, "blog"); r.Stale {
		t.Fatalf("blog stale while the box answers")
	}
	f.Set("blog", fail(model.FailThis))
	misses := 0
	for misses < 3 {
		if j := step(t, s, f, &now); j.App == "blog" {
			misses++
			r := row(s, now, "blog")
			if want := misses >= 3; r.Stale != want {
				m, h, ev, w := s.Engine.Row("blog")
				t.Fatalf("after %d missed reads stale is %v (misses %d held %v every %s woke %v age %s)", misses, r.Stale, m, h, ev, w, r.Cells[model.ColAge])
			}
		}
	}
	r := row(s, now, "blog")
	if !strings.Contains(r.Cells[model.ColState].String(), "stale") || r.Cells[model.ColAge].String() != "stale" {
		t.Errorf("a stale row says %q, %q", r.Cells[model.ColState], r.Cells[model.ColAge])
	}
	// The box gone: every read gets no answer.
	for _, n := range append(s.Names(), "apps", "whoami") {
		f.Set(n, fail(model.FailNoAnswer))
	}
	var gaps []time.Duration
	last := now
	for i := range 4 {
		step(t, s, f, &now)
		if i > 0 {
			gaps = append(gaps, now.Sub(last))
		}
		last = now
	}
	for i := 1; i < len(gaps); i++ {
		if gaps[i] <= gaps[i-1] {
			t.Errorf("no back-off: waits %v", gaps)
		}
	}
	if _, right := s.Title(now); !strings.HasPrefix(right.Text(), "STALE") {
		t.Errorf("title says %q, want STALE", right.Text())
	}
}

// TestListRefused: an apps list the box refuses is said on screen, and
// the title does not say live.
func TestListRefused(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Set("apps", &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this"})
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	step(t, s, f, &now) // whoami
	step(t, s, f, &now) // apps, refused
	if v := s.AppsView(now, "", 0, 0); !strings.Contains(v.Empty.Text(), "does not allow your key this") {
		t.Errorf("the screen says %q", v.Empty.Text())
	}
	if _, right := s.Title(now.Add(time.Minute)); strings.HasPrefix(right.Text(), "live") {
		t.Errorf("the title says %q of a refused list", right.Text())
	}
}

// TestTitleWords: SAMPLE DATA on the demo, and TOUCH YOUR KEY while a
// hardware key signs.
func TestTitleWords(t *testing.T) {
	s := &model.Session{Host: demo.Host, Now: func() time.Time { return t0 }, Engine: model.NewEngine(t0), Demo: true}
	if l, _ := s.Title(t0); !strings.Contains(l.Text(), "SAMPLE DATA") {
		t.Errorf("the demo's title is %q", l.Text())
	}
	s.Key.Hardware, s.Signing = true, true
	if _, r := s.Title(t0); r.Text() != "TOUCH YOUR KEY to sign" {
		t.Errorf("a hardware key signing: %q", r.Text())
	}
	s.Key.Hardware = false
	if _, r := s.Title(t0); strings.Contains(r.Text(), "TOUCH") {
		t.Errorf("a key with no touch: %q", r.Text())
	}
}

// TestActionsMakeTheReads: a read whose kind has no action is not made.
func TestActionsMakeTheReads(t *testing.T) {
	f := &demo.Fake{Now: func() time.Time { return t0 }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	if _, err := s.Fetch(context.Background(), f, model.Job{Kind: model.Kind(99)}); err == nil || len(f.Calls) != 0 {
		t.Fatalf("a read with no action: err %v, %d calls", err, len(f.Calls))
	}
}

// TestDroppedAppCancelled: an app the box no longer lists has its read
// in flight cancelled, and is not read again.
func TestDroppedAppCancelled(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetApps([]string{"blog"}, t0)
	var j model.Job
	for {
		got, ok, _ := e.Take(t0)
		if !ok {
			t.Fatal("blog was never taken")
		}
		if got.Kind == model.KStatus {
			j = got
			break
		}
		e.Done(got, t0, nil)
	}
	cancelled := false
	e.Started(func() { cancelled = true })
	e.SetApps(nil, t0)
	e.Done(j, t0, context.Canceled)
	if !cancelled {
		t.Fatal("the dropped app's read was not cancelled")
	}
	for range 10 {
		got, ok, _ := e.Take(t0.Add(time.Hour))
		if !ok {
			break
		}
		if got.App == "blog" {
			t.Fatal("a dropped app was read again")
		}
		e.Done(got, t0.Add(time.Hour), nil)
	}
}

// TestSpread: after the first pass, background reads come at their own
// places in the cycle, never a second's burst.
func TestSpread(t *testing.T) {
	apps := names(200)
	e := model.NewEngine(t0)
	calls := sim(e, apps, t0, 15*time.Minute, 20*time.Millisecond, ok)
	settle := t0.Add(2 * e.Interval("app-1"))
	per := map[int64]int{}
	for _, c := range calls {
		if c.j.Kind == model.KStatus && c.at.After(settle) {
			per[c.at.Unix()]++
		}
	}
	for s, n := range per {
		if n > 3 {
			t.Fatalf("%d reads in the second at +%ds: not spread", n, s-t0.Unix())
		}
	}
}

// TestFocusShare: opening an app reads it at once only if its answer is
// older than the open app's interval; opening one after another every
// 200 ms reads opened apps out of the open app's share, and the others
// keep their turn; an app closed goes back to its place in the cycle.
func TestFocusShare(t *testing.T) {
	apps := names(12)
	e := model.NewEngine(t0)
	sim(e, apps, t0, 40*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(40 * time.Second)
	e.Touch(now)
	e.Focus("app-5", now)
	if j, ok, _ := e.Take(now); !ok || j.App != "app-5" {
		t.Fatalf("an app with an old answer was not read at once on opening: %v %v", j, ok)
	}
	e.Done(model.Job{Kind: model.KStatus, App: "app-5"}, now, nil)
	e.Focus("", now)
	e.Focus("app-5", now.Add(time.Second))
	if j, ok, _ := e.Take(now.Add(time.Second)); ok && j.App == "app-5" {
		t.Fatal("an app answered a second ago was read again on opening")
	}
	// Opening one app after another for two minutes.
	e = model.NewEngine(t0)
	sim(e, apps, t0, 40*time.Second, 10*time.Millisecond, ok)
	opened, others := 0, 0
	now = t0.Add(40 * time.Second)
	end := now.Add(2 * time.Minute)
	i := 0
	next := now
	for now.Before(end) {
		if !now.Before(next) {
			e.Focus(apps[i%len(apps)], now)
			i++
			next = now.Add(200 * time.Millisecond)
		}
		e.Touch(now)
		j, ok, _ := e.Take(now)
		if !ok {
			now = now.Add(50 * time.Millisecond)
			continue
		}
		if j.Kind == model.KStatus {
			if j.App == apps[(i-1)%len(apps)] {
				opened++
			} else {
				others++
			}
		}
		e.Done(j, now, nil)
		now = now.Add(10 * time.Millisecond)
	}
	if most := int(2*time.Minute/model.FocusEvery) + 2; opened > most {
		t.Errorf("%d reads of opened apps in two minutes, more than the open app's share of %d", opened, most)
	}
	if least := 11 * 3; others < least {
		t.Errorf("%d reads of the other apps in two minutes, fewer than %d", others, least)
	}
	// Closed: back to its slot, not every 5 s.
	e = model.NewEngine(t0)
	e.Focus("app-3", t0)
	sim(e, apps, t0, time.Minute, 10*time.Millisecond, ok)
	closed := t0.Add(time.Minute)
	e.Focus("", closed)
	calls := sim(e, apps, closed, 2*time.Minute, 10*time.Millisecond, ok)
	var reads []time.Time
	for _, c := range calls {
		if c.j.App == "app-3" {
			reads = append(reads, c.at)
		}
	}
	if len(reads) < 2 || reads[1].Sub(reads[0]) < e.Interval("app-3")-time.Second {
		t.Errorf("a closed app is read at %v, not at its place in the cycle", reads)
	}
}

// TestNoRetryAfterIsTheBurst: a 429 with no Retry-After waits the time
// the box's burst takes to refill, not a number of its own.
func TestNoRetryAfterIsTheBurst(t *testing.T) {
	if want := time.Duration(model.BoxBurst/model.BoxRate) * time.Second; model.NoRetryAfter != want {
		t.Fatalf("NoRetryAfter %s, want %s", model.NoRetryAfter, want)
	}
}

// TestResumeAfter429: after the wait the slowed read comes first, at the
// wait's end, and the reads resume at the rate, not in a burst.
func TestResumeAfter429(t *testing.T) {
	e := model.NewEngine(t0)
	sent := false
	var slowAt time.Time
	calls := sim(e, names(60), t0, 2*time.Minute, 10*time.Millisecond, func(j model.Job, at time.Time) error {
		if j.Kind == model.KStatus && j.App == "app-40" && !sent {
			sent, slowAt = true, at
			return &model.Failure{Kind: model.FailSlowed, RetryAfter: 30 * time.Second}
		}
		return nil
	})
	until := slowAt.Add(30 * time.Second)
	first, n := model.Job{}, 0
	for _, c := range calls {
		if !c.at.Before(until) {
			if first == (model.Job{}) {
				first = c.j
				if c.at.Sub(until) > time.Second {
					t.Errorf("the first read after the wait at +%s", c.at.Sub(until))
				}
			}
			if c.at.Before(until.Add(time.Second)) {
				n++
			}
		}
	}
	if first.App != "app-40" {
		t.Errorf("the first read after the wait is %v, not the one slowed", first)
	}
	if n > 2 {
		t.Errorf("%d reads in the first second after the wait: a burst", n)
	}
}

// TestWhoAmIGoneAfterAStop: a whoami refused at the start is asked again
// by r, once, and then not until its MeAlways interval.
func TestWhoAmIGoneAfterAStop(t *testing.T) {
	e := model.NewEngine(t0)
	j, _, _ := e.Take(t0)
	e.Done(j, t0, fail(model.FailSignedOut))
	now := t0.Add(time.Minute)
	e.Touch(now)
	if !e.Retry(now) {
		t.Fatal("r refused")
	}
	j, _, _ = e.Take(now)
	e.Done(j, now, nil)
	calls := sim(e, names(2), now, model.MeAlways-time.Minute, 10*time.Millisecond, ok)
	for _, c := range calls {
		if c.j.Kind == model.KWhoAmI {
			t.Fatalf("whoami read again at +%s", c.at.Sub(now))
		}
	}
}

// takeRec takes reads at now until rec, answering the others; false
// when rec is not among them.
func takeRec(e *model.Engine, now time.Time, rec model.Job) bool {
	e.Touch(now)
	for range 50 {
		j, ok, _ := e.Take(now)
		if !ok {
			return false
		}
		if j == rec {
			return true
		}
		e.Done(j, now, nil)
	}
	return false
}

// TestRecordOnceAndAgain: a record asked for twice while in flight is
// read once; one that failed is read again on opening it again and on
// ctrl-r; one answered is read again on opening it again, but not
// within RecordAgainAfter; ctrl-r asks again the record on the screen,
// within RecordAgainAfter of its last read only once that has passed.
func TestRecordOnceAndAgain(t *testing.T) {
	for _, k := range []model.Fail{model.FailNotFound, model.FailThis} {
		e := model.NewEngine(t0)
		sim(e, names(1), t0, time.Second, time.Millisecond, ok)
		now := t0.Add(time.Minute)
		e.Touch(now)
		rec := model.Job{Kind: model.KDeploy, App: "app-0", Version: "v1"}
		e.Once(rec, now)
		if !takeRec(e, now, rec) {
			t.Fatal("the record was not read")
		}
		e.Once(rec, now) // asked again while in flight
		e.Done(rec, now, fail(k))
		if takeRec(e, now.Add(time.Hour), rec) {
			t.Fatal("a failed record read again on its own")
		}
		e.Once(rec, now.Add(time.Hour)) // left and opened again
		if !takeRec(e, now.Add(time.Hour), rec) {
			t.Fatalf("kind %d: a failed record not read again on opening it", k)
		}
		e.Done(rec, now.Add(time.Hour), fail(k))
		later := now.Add(2 * time.Hour)
		e.Again(later, rec)
		if !takeRec(e, later, rec) {
			t.Fatalf("kind %d: ctrl-r did not ask a failed record again", k)
		}
		e.Done(rec, later, nil)
		if takeRec(e, later.Add(time.Hour), rec) {
			t.Fatal("an answered record read again with nothing asking")
		}
		last := later.Add(2 * time.Hour)
		e.Once(rec, last) // left and opened again: the box may have rewritten it
		if !takeRec(e, last, rec) {
			t.Fatal("an answered record not read again on opening it")
		}
		e.Done(rec, last, nil)
		soon := last.Add(model.RecordAgainAfter - time.Second)
		e.Once(rec, soon)
		if takeRec(e, soon, rec) {
			t.Fatalf("a record read again on opening it within %s", model.RecordAgainAfter)
		}
		e.Again(soon, rec)
		if takeRec(e, soon, rec) {
			t.Fatalf("ctrl-r read a record again within %s", model.RecordAgainAfter)
		}
		if !takeRec(e, last.Add(model.RecordAgainAfter), rec) {
			t.Fatal("ctrl-r on the record's screen did not ask it again")
		}
	}
}

// TestStaleBounds: an answer is stale past StaleAfter intervals and one
// of grace, not at it.
func TestStaleBounds(t *testing.T) {
	every := 30 * time.Second
	edge := (model.StaleAfter + 1) * every
	if model.Stale(edge, every) || !model.Stale(edge+time.Nanosecond, every) {
		t.Fatalf("stale at %s: %v; a nanosecond after: %v", edge, model.Stale(edge, every), model.Stale(edge+time.Nanosecond, every))
	}
}

// TestNotStaleWhenNotDue: a held row is not stale, nor an app just
// opened, judged by the interval it had.
func TestNotStaleWhenNotDue(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	f.Set("blog", fail(model.FailForbidden))
	for range 20 {
		step(t, s, f, &now)
	}
	// In the clock's order: a time before one given is the clock set
	// back.
	at := now.Add(25 * time.Second)
	s.Engine.Focus("docs", at)
	if r := row(s, at, "docs"); r.Stale {
		t.Errorf("an app just opened is stale by the open app's interval")
	}
	later := now.Add(10 * time.Minute)
	if r := row(s, later, "blog"); r.Stale || strings.Contains(r.Cells[model.ColState].String(), "stale") {
		t.Errorf("a held row says %q", r.Cells[model.ColState])
	}
}

// TestUnusableAnswer: an answer that could not be used is said on its
// row; nothing else backs off, and the title still says live.
func TestUnusableAnswer(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	f.Set("blog", &model.Failure{Kind: model.FailUnusable, Message: "deploy.example.com answered, but its started_at could not be read"})
	for range 20 {
		step(t, s, f, &now)
	}
	if h := s.Engine.Health(now); h.Misses != 0 || h.RetryIn != 0 {
		t.Errorf("an unusable answer backs off every read: %+v", h)
	}
	if _, r := s.Title(now); !strings.HasPrefix(r.Text(), "live") {
		t.Errorf("the title says %q", r.Text())
	}
	if r := row(s, now, "blog"); !strings.Contains(r.Why.Text(), "could not be read") {
		t.Errorf("the row says %q", r.Why.Text())
	}
}

// TestFocusReadsByAnswerAge: an app answered a second ago is not read
// at once on opening, but at its answer and the open app's interval;
// one answered long ago is read at once. Its interval is the open
// app's after a focused answer, and the background's again from the
// moment it is closed.
func TestFocusReadsByAnswerAge(t *testing.T) {
	apps := names(200)
	e := model.NewEngine(t0)
	calls := sim(e, apps, t0, 20*time.Second, 10*time.Millisecond, ok)
	// The app read last: its answer a moment ago, the clock not
	// going back.
	var answered time.Time
	app := ""
	for _, c := range calls {
		if c.j.Kind == model.KStatus {
			answered, app = c.at.Add(10*time.Millisecond), c.j.App
		}
	}
	at := answered.Add(time.Second)
	e.Touch(at)
	e.Focus(app, at)
	for {
		j, ok, _ := e.Take(at)
		if !ok {
			break
		}
		if j.App == app {
			t.Fatalf("an app answered %s before was read at once on opening", at.Sub(answered))
		}
		e.Done(j, at, nil)
	}
	later := answered.Add(model.FocusEvery)
	e.Touch(later)
	if !takeApp(e, later, app) {
		t.Fatal("the open app was not read at its answer and the open app's interval")
	}
	e.Done(model.Job{Kind: model.KStatus, App: app}, later, nil)
	if _, _, every, _ := e.Row(app); every != model.FocusEvery {
		t.Errorf("after a focused answer the interval is %s", every)
	}
	closed := later.Add(time.Second)
	e.Focus("", closed)
	if _, _, every, _ := e.Row(app); every != e.Interval("app-x") {
		t.Errorf("a closed app is judged by %s, not the background's %s", every, e.Interval("app-x"))
	}
	// Answered long ago: read at once.
	old := later.Add(time.Hour)
	e.Touch(old)
	e.Focus("app-7", old)
	if !takeApp(e, old, "app-7") {
		t.Fatal("an app with an old answer was not read at once on opening")
	}
}

// takeApp takes reads from now until app's, within a second, waiting
// for the bucket as the driver does and answering the others.
func takeApp(e *model.Engine, now time.Time, app string) bool {
	end := now.Add(time.Second)
	for range 100 {
		j, ok, wait := e.Take(now)
		if !ok {
			if wait <= 0 || now.Add(wait).After(end) {
				return false
			}
			now = now.Add(wait)
			continue
		}
		if j.App == app {
			return true
		}
		e.Done(j, now, nil)
	}
	return false
}

// TestClosedAppNotStale: with 200 apps, an app closed is not stale
// while its next background read is not yet due.
func TestClosedAppNotStale(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	s.Engine.Focus("blog", now)
	for range 5 {
		step(t, s, f, &now)
	}
	s.Engine.Focus("", now)
	if r := row(s, now.Add(time.Minute), "blog"); r.Stale {
		t.Errorf("a closed app is stale a minute later, its next read not yet due")
	}
}

// TestSlowedFirstOverAll: after a 429's wait the slowed read goes first,
// though whoami, the list and the open app came due during the wait.
func TestSlowedFirstOverAll(t *testing.T) {
	e := model.NewEngine(t0)
	e.Focus("app-0", t0)
	sim(e, names(5), t0, 10*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(10 * time.Second)
	e.Touch(now)
	if !takeApp(e, now.Add(30*time.Second), "app-3") {
		t.Fatal("app-3 not taken")
	}
	e.Done(model.Job{Kind: model.KStatus, App: "app-3"}, now, &model.Failure{Kind: model.FailSlowed, RetryAfter: 2 * time.Minute})
	if e.CanAskAgain(now.Add(time.Minute)) {
		t.Error("ctrl-r said to ask again during a 429's wait")
	}
	end := now.Add(2*time.Minute + time.Second)
	if !e.CanAskAgain(end) {
		t.Error("ctrl-r not offered after the wait")
	}
	e.Touch(end)
	j, ok, wait := e.Take(end)
	for !ok && wait > 0 && wait < 5*time.Second {
		end = end.Add(wait)
		j, ok, wait = e.Take(end)
	}
	if !ok || j.App != "app-3" {
		t.Fatalf("the first read after the wait is %v, not the slowed one", j)
	}
}

// TestWake: a clock that jumps ten minutes (a laptop asleep) makes
// every row stale until read again, spreads the reads again, asks
// whoami again, and leaves the bucket empty.
func TestWake(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	e := s.Engine
	if e.Tick(now) || e.Tick(now.Add(time.Second)) {
		t.Fatal("a second's tick taken for a wake")
	}
	now = now.Add(10 * time.Minute)
	if !e.Tick(now) {
		t.Fatal("ten minutes between two ticks not taken for a wake")
	}
	if r := row(s, now, "docs"); !r.Stale {
		t.Error("a row not re-read since the wake is not stale")
	}
	e.Touch(now)
	j, ok, wait := e.Take(now)
	if ok || wait <= 0 {
		t.Fatalf("a full bucket after the wake: took %v", j)
	}
	now = now.Add(wait)
	if j, ok, _ = e.Take(now); !ok || j.Kind != model.KWhoAmI {
		t.Fatalf("the first read after a wake is %v, not whoami", j)
	}
	apply, err := s.Fetch(context.Background(), f, j)
	apply()
	e.Done(j, now, err)
	// Spread: the background reads do not all come due at once.
	due := 0
	for range 20 {
		now = now.Add(time.Second)
		e.Touch(now)
		if j, ok, _ := e.Take(now); ok {
			due++
			e.Done(j, now, nil)
		}
	}
	if due > 20 {
		t.Errorf("%d reads in 20 s after a wake", due)
	}
}

// TestWords: the title's 429 says until when; an empty apps screen
// while stopped says it is not asking.
func TestWords(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	step(t, s, f, &now) // whoami
	f.Set("apps", &model.Failure{Kind: model.FailSlowed, RetryAfter: time.Hour})
	step(t, s, f, &now)
	if _, r := s.Title(now); !strings.Contains(r.Text(), "until "+now.Add(time.Hour).UTC().Format("15:04:05")+" UTC") || !strings.Contains(r.Text(), "1h left") {
		t.Errorf("the title says %q", r.Text())
	}
	s2 := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	f.Set("whoami", fail(model.FailSignedOut))
	step(t, s2, f, &now)
	if v := s2.AppsView(now, "", 0, 0); !strings.HasPrefix(v.Empty.Text(), "not asking") {
		t.Errorf("an empty apps screen while stopped says %q", v.Empty.Text())
	}
}

// TestWaitIsForTheSoonest: the wait Take gives, which the driver sleeps
// for, is the time to the soonest read of any kind. With apps due at
// different times and the list far off, a fake clock steps a tick at a
// time and asks Take at every step: no read may come before the wait it
// last gave runs out, one must come when it does, and over ten minutes
// each app is read within a tick of its interval. An engine that gives
// the wait for a later kind (the list's) fails the first rule as soon as
// an app falls due before it.
func TestWaitIsForTheSoonest(t *testing.T) {
	const tick = 100 * time.Millisecond
	apps := names(4)
	e := model.NewEngine(t0)
	now := t0
	var promised time.Time // when the last wait given runs out; zero: none
	byApp := map[string][]time.Time{}
	var lists []time.Time
	waits := 0
	for now.Before(t0.Add(10 * time.Minute)) {
		e.Touch(now)
		j, ok, wait := e.Take(now)
		if !ok {
			if wait <= 0 {
				t.Fatalf("at %s: nothing to read and no wait", now.Sub(t0))
			}
			at := now.Add(wait)
			if promised.IsZero() {
				promised, waits = at, waits+1
			} else if d := at.Sub(promised); d > tick || d < -tick {
				t.Fatalf("at %s: the wait moved from %s to %s with nothing read", now.Sub(t0), promised.Sub(t0), at.Sub(t0))
			}
			if now.Sub(promised) > tick {
				t.Fatalf("at %s: the wait ran out at %s and nothing was read", now.Sub(t0), promised.Sub(t0))
			}
			now = now.Add(tick)
			continue
		}
		if !promised.IsZero() && promised.Sub(now) >= tick {
			t.Fatalf("at %s: %v read, before the wait given ran out at %s", now.Sub(t0), j, promised.Sub(t0))
		}
		promised = time.Time{}
		switch j.Kind {
		case model.KStatus:
			byApp[j.App] = append(byApp[j.App], now)
		case model.KApps:
			lists = append(lists, now)
			e.SetApps(apps, now)
		}
		e.Done(j, now, nil)
	}
	if waits < 20 {
		t.Fatalf("only %d waits given in ten minutes", waits)
	}
	every := e.Interval(apps[0])
	if every != model.BackgroundEvery {
		t.Fatalf("four apps are read every %s, want %s", every, model.BackgroundEvery)
	}
	// Different times: the first reads after the first pass are spread.
	seen := map[time.Time]bool{}
	for _, a := range apps {
		ts := byApp[a]
		if len(ts) < 19 {
			t.Fatalf("%s read %d times in ten minutes, every %s", a, len(ts), every)
		}
		seen[ts[1]] = true
		for i := 2; i < len(ts); i++ {
			if g := ts[i].Sub(ts[i-1]); g > every+tick || g < every-tick {
				t.Fatalf("%s read %s apart at %s, its interval %s", a, g, ts[i].Sub(t0), every)
			}
		}
	}
	if len(seen) < len(apps) {
		t.Fatalf("the apps came due together: %v", seen)
	}
	for i := 1; i < len(lists); i++ {
		if g := lists[i].Sub(lists[i-1]); g > model.AppsEvery+tick {
			t.Fatalf("the list read %s apart, its interval %s", g, model.AppsEvery)
		}
	}
}

// TestMeAndBoxReads: whoami is read while who am I is open, every
// MeEvery, and again on ctrl-r (AgainAfter after its last read); the
// box only while its screen is open, every BoxEvery; neither while
// closed.
func TestMeAndBoxReads(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(2), t0, 10*time.Second, 10*time.Millisecond, ok)
	count := func(calls []call, k model.Kind) []time.Time {
		var at []time.Time
		for _, c := range calls {
			if c.j.Kind == k {
				at = append(at, c.at)
			}
		}
		return at
	}
	now := t0.Add(10 * time.Second)
	if n := count(sim(e, names(2), now, 4*time.Minute, 10*time.Millisecond, ok), model.KWhoAmI); len(n) != 0 {
		t.Fatalf("whoami read %d times in 4 minutes with who am I closed", len(n))
	}
	now = now.Add(4 * time.Minute)
	e.MeOpen(true, now)
	me := count(sim(e, names(2), now, 3*time.Minute+time.Second, 10*time.Millisecond, ok), model.KWhoAmI)
	if len(me) != 4 || me[0].Sub(now) > time.Second || me[1].Sub(me[0]) < model.MeEvery {
		t.Errorf("who am I open 3 minutes: whoami at %v", me)
	}
	now = now.Add(3*time.Minute + time.Second + model.AgainAfter)
	e.Again(now, model.Job{Kind: model.KWhoAmI})
	if !takeKind(e, now, model.KWhoAmI) {
		t.Error("ctrl-r on who am I did not read whoami")
	}
	e.Done(model.Job{Kind: model.KWhoAmI}, now, nil)
	e.MeOpen(false, now)
	e.BoxOpen(true, now)
	box := count(sim(e, names(2), now, 2*time.Minute, 10*time.Millisecond, ok), model.KBox)
	if len(box) != 4 || box[1].Sub(box[0]) < model.BoxEvery-time.Second {
		t.Errorf("the box open 2 minutes: read at %v", box)
	}
	now = now.Add(2 * time.Minute)
	e.BoxOpen(false, now)
	if n := count(sim(e, names(2), now, 2*time.Minute, 10*time.Millisecond, ok), model.KBox); len(n) != 0 {
		t.Errorf("the box read %d times with its screen closed", len(n))
	}
}

func takeKind(e *model.Engine, now time.Time, k model.Kind) bool {
	e.Touch(now)
	for range 50 {
		j, ok, _ := e.Take(now)
		if !ok {
			return false
		}
		if j.Kind == k {
			return true
		}
		e.Done(j, now, nil)
	}
	return false
}

// TestWhoAmIBeforeARecordAfterAStop: r's whoami goes before a record
// the person had open.
func TestWhoAmIBeforeARecordAfterAStop(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(1), t0, time.Second, time.Millisecond, ok)
	now := t0.Add(time.Minute)
	e.Touch(now)
	rec := model.Job{Kind: model.KDeploy, App: "app-0", Version: "v1"}
	e.Once(rec, now)
	j, _, _ := e.Take(now)
	e.Done(j, now, fail(model.FailSignedOut))
	e.Once(rec, now)
	if !e.Retry(now) {
		t.Fatal("r refused")
	}
	if j, ok, _ := e.Take(now); !ok || j.Kind != model.KWhoAmI {
		t.Fatalf("after r the first read is %v, not whoami", j)
	}
}
