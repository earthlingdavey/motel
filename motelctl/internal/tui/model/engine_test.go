package model_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/motelctl/internal/tui/demo"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
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

// TestHeldUntilAsked: a 403 is not asked again until ctrl-r.
func TestHeldUntilAsked(t *testing.T) {
	e := model.NewEngine(t0)
	n := 0
	answer := func(j model.Job, _ time.Time) error {
		if j.App == "app-1" {
			n++
			return fail(model.FailForbidden)
		}
		return nil
	}
	end := t0.Add(5 * time.Minute)
	sim(e, names(3), t0, 5*time.Minute, 10*time.Millisecond, answer)
	if n != 1 {
		t.Fatalf("a 403 asked %d times", n)
	}
	e.Again(end)
	sim(e, names(3), end, time.Second, 10*time.Millisecond, answer)
	if n != 2 {
		t.Fatalf("ctrl-r did not ask the 403 again (%d)", n)
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
	for _, r := range s.Apps(now, "", 1).Rows {
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
				t.Fatalf("after %d missed reads stale is %v", misses, r.Stale)
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
	if v := s.Apps(now, "", 0); !strings.Contains(v.Empty.Text(), "does not allow your key this") {
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
