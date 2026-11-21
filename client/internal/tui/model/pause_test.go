package model_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// unattended runs e for d from t0 with a key pressed at t0 alone, each
// read taking 50 ms and answered, the list answering apps. It returns
// every read, and the signatures the client's token rule would make: a
// token of 600 s with its iat 30 s back, to the second, signed again
// when less than a minute of it is left (sshkey.TimesLasting,
// PersonToken's minLeft).
func unattended(e *model.Engine, apps []string, d time.Duration) (calls []call, signs int) {
	now := t0
	e.Touch(now)
	var exp time.Time
	for now.Before(t0.Add(d)) {
		j, ok, wait := e.Take(now)
		if !ok {
			if wait <= 0 || wait > time.Second {
				wait = time.Second
			}
			now = now.Add(wait)
			continue
		}
		if exp.IsZero() || exp.Sub(now) < time.Minute {
			signs++
			exp = time.Unix(now.Add(-30*time.Second).Unix(), 0).Add(600 * time.Second)
		}
		calls = append(calls, call{j, now})
		now = now.Add(50 * time.Millisecond)
		if j.Kind == model.KApps {
			e.SetApps(apps, now)
		}
		e.Done(j, now, nil)
	}
	return calls, signs
}

// TestWatchThroughThePause: a viewer of the box who leaves the TUI for
// 8 hours: after the ten idle minutes only the box's alerts are read,
// every minute, 470 reads in the 7 h 50 min after (#576's spike
// measured 480 at 60 s from the start), and whoami at its own pace; the
// key signs about every 510 s of it, 54 or so in all.
func TestWatchThroughThePause(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchBox, t0)
	calls, signs := unattended(e, names(5), 8*time.Hour)
	boxBucket(t, calls)
	after, other := 0, 0
	for _, c := range calls {
		if c.at.Sub(t0) < model.Idle {
			continue
		}
		switch c.j.Kind {
		case model.KBoxAlerts:
			after++
		case model.KWhoAmI: // which grants hold, at its own pace
		default:
			other++
		}
	}
	if after < 465 || after > 475 || other != 0 {
		t.Errorf("after the pause: %d alerts reads (want about 470), %d others", after, other)
	}
	if signs < 52 || signs > 58 {
		t.Errorf("%d signatures in 8 hours, want about 54", signs)
	}
	t.Logf("%d reads in 8 h, %d of the alerts after the pause, %d signatures", len(calls), after, signs)
}

// TestAppWatchThroughThePause: a person with app grants only, left for
// 8 hours: after the pause each app's alerts alone are read, each at
// its interval at the keys and never under a minute, inside the
// engine's budget and the box's limit. The counts are logged for the
// owner's decision (READS.md's B” read every app at 30 s or more:
// 18,142 alerts reads at 40 apps).
func TestAppWatchThroughThePause(t *testing.T) {
	for _, n := range []int{1, 5, 12, 40} {
		e := model.NewEngine(t0)
		e.SetWatch(model.WatchApps, t0)
		calls, signs := unattended(e, names(n), 8*time.Hour)
		boxBucket(t, calls)
		last := map[string]time.Time{}
		after, other := 0, 0
		for _, c := range calls {
			if c.at.Sub(t0) < model.Idle+2*time.Minute {
				continue // the pause's first reads still at the keys' pace
			}
			if c.j.Kind == model.KApps || c.j.Kind == model.KWhoAmI {
				continue // which apps are watched, at their own pace
			}
			if c.j.Kind != model.KAlerts {
				other++
				continue
			}
			after++
			if l, ok := last[c.j.App]; ok && c.at.Sub(l) < model.PausedWatchEvery-time.Second {
				t.Errorf("%d apps: %s's alerts read %s apart while paused", n, c.j.App, c.at.Sub(l))
			}
			last[c.j.App] = c.at
		}
		if other != 0 || len(last) != n {
			t.Errorf("%d apps: %d other reads while paused, %d apps' alerts read", n, other, len(last))
		}
		t.Logf("%s apps: %d alerts reads while paused (%.3f a second), %d signatures", strconv.Itoa(n), after, float64(after)/(8*time.Hour-model.Idle-2*time.Minute).Seconds(), signs)
	}
}

// TestWatchBackFromThePause: a key after the pause brings the box's
// alerts read back to every WatchEvery from its last answer, not at the
// paused minute's end.
func TestWatchBackFromThePause(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchBox, t0)
	end := t0.Add(model.Idle + 5*time.Minute)
	calls, _ := unattended(e, names(1), end.Sub(t0))
	var last time.Time
	for _, c := range calls {
		if c.j.Kind == model.KBoxAlerts {
			last = c.at
		}
	}
	now := end
	e.Touch(now)
	for range 200 {
		j, ok, wait := e.Take(now)
		if !ok {
			now = now.Add(max(wait, 100*time.Millisecond))
			continue
		}
		if j.Kind == model.KBoxAlerts {
			if due := maxT(end, last.Add(model.WatchEvery+time.Second)); now.After(due) {
				t.Errorf("read at +%s after the key, its last answer %s before the key", now.Sub(end), end.Sub(last))
			}
			return
		}
		e.Done(j, now, nil)
	}
	t.Error("the box's alerts were not read after the key")
}

// TestAppWatchBackFromThePause: a person with app grants only, paused on
// an app's screen: a key brings that app's alerts read back to every
// AlertsEvery from its last answer, not the paused minute's end.
func TestAppWatchBackFromThePause(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	e.Focus("app-0", t0)
	e.AlertsOpen("app-0", t0)
	open := model.Job{Kind: model.KAlerts, App: "app-0"}
	end := t0.Add(model.Idle + 5*time.Minute)
	calls, _ := unattended(e, names(3), end.Sub(t0))
	var last time.Time
	for _, c := range calls {
		if c.j == open {
			last = c.at
		}
	}
	now := end
	e.Touch(now)
	for range 200 {
		j, ok, wait := e.Take(now)
		if !ok {
			now = now.Add(max(wait, 100*time.Millisecond))
			continue
		}
		if j == open {
			if due := maxT(end, last.Add(model.AlertsEvery+time.Second)); now.After(due) {
				t.Errorf("read at +%s after the key, its last answer %s before the key", now.Sub(end), end.Sub(last))
			}
			return
		}
		e.Done(j, now, nil)
	}
	t.Error("the open app's alerts were not read after the key")
}

// TestPausedWordReads: the paused word says the alerts are still read
// only while a watch's read is made and answers: none with no app
// listed; one app's alerts refused leaves the others read; a box's
// alerts refused, none.
func TestPausedWordReads(t *testing.T) {
	idle := t0.Add(model.Idle)
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	if e.Health(idle).WatchRead {
		t.Error("no app listed: said read")
	}
	e = model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	e.SetApps(names(3), t0)
	for _, a := range names(3) {
		j := model.Job{Kind: model.KAlerts, App: a}
		for range 50 {
			got, ok, _ := e.Take(t0)
			if !ok {
				break
			}
			var err error
			if got == j && a == "app-0" {
				err = &model.Failure{Kind: model.FailForbidden, Message: "no"}
			}
			e.Done(got, t0, err)
			if got == j {
				break
			}
		}
	}
	if !e.Health(idle).WatchRead {
		t.Error("one app's alerts refused, two read: not said read")
	}
	e = model.NewEngine(t0)
	e.SetWatch(model.WatchBox, t0)
	for range 10 {
		j, ok, _ := e.Take(t0)
		if !ok {
			break
		}
		var err error
		if j.Kind == model.KBoxAlerts {
			err = &model.Failure{Kind: model.FailForbidden, Message: "no"}
		}
		e.Done(j, t0, err)
	}
	if e.Health(idle).WatchRead {
		t.Error("the box's alerts refused: said read")
	}
}

// TestAppPhasesAfterThePause: an apps list applied just after the pause
// began lays each app's alerts in the paused minute's cycle; back at
// the keys, each is read again within the 30 s cycle, not up to a
// minute on.
func TestAppPhasesAfterThePause(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	now := t0.Add(model.Idle + time.Second)
	e.SetApps(names(5), now)
	e.Touch(now)
	done := map[string]time.Time{}
	for end := now.Add(2 * time.Minute); now.Before(end); {
		e.Touch(now)
		j, ok, wait := e.Take(now)
		if !ok {
			now = now.Add(max(wait, 100*time.Millisecond))
			continue
		}
		e.Done(j, now, nil)
		if j.Kind != model.KAlerts {
			continue
		}
		if last, ok := done[j.App]; ok && now.Sub(last) > model.AppWatchEvery+time.Second {
			t.Errorf("%s's alerts read %s apart at the keys", j.App, now.Sub(last))
		}
		done[j.App] = now
	}
}

// TestWatchIntervalNotesNoTime: asked with an answer's time from before
// a time the reads' goroutine gave, the watch's interval is not taken
// for the clock set back: the pause begins Idle after the key.
func TestWatchIntervalNotesNoTime(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchBox, t0)
	at := t0.Add(time.Minute)
	e.Touch(at)
	e.WatchInterval(model.Job{Kind: model.KBoxAlerts}, at.Add(-time.Second))
	if e.Health(at.Add(model.Idle - 500*time.Millisecond)).Paused {
		t.Error("paused before Idle after the key")
	}
}

// TestAppAddedWhilePaused: for a person with app grants only, the list
// is read through the pause too, so an app added meanwhile has its
// alerts read before a key.
func TestAppAddedWhilePaused(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	e.Touch(t0)
	apps := names(2)
	now := t0
	read := false
	for end := t0.Add(model.Idle + 10*time.Minute); now.Before(end); {
		if now.After(t0.Add(model.Idle + time.Minute)) {
			apps = names(3)
		}
		j, ok, wait := e.Take(now)
		if !ok {
			now = now.Add(max(wait, time.Second))
			continue
		}
		if j.Kind == model.KApps {
			e.SetApps(apps, now)
		}
		e.Done(j, now, nil)
		read = read || j == model.Job{Kind: model.KAlerts, App: "app-2"}
	}
	if !read {
		t.Error("an app added while paused: its alerts never read")
	}
}

// pausedRead moves the clock, with no key, to the next read of kind k
// and takes it, making the reads due before it.
func pausedRead(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time, k model.Kind, _ error) model.Job {
	t.Helper()
	for range 5000 {
		j, ok, wait := s.Engine.Take(*now)
		switch {
		case !ok:
			*now = now.Add(max(wait, 100*time.Millisecond))
		case j.Kind == k:
			return j
		default:
			end(s, f, j, *now, nil)
		}
	}
	t.Fatalf("no read of kind %d", k)
	return model.Job{}
}

// TestPausedRetrySaid: while paused, the retry of a watch read that got
// no answer, itself waited for 6 s, is said as waiting, not hidden
// under PAUSED.
func TestPausedRetrySaid(t *testing.T) {
	s, f, now := session(t)
	for range 100 {
		j, ok, _ := s.Engine.Take(*now)
		if !ok {
			break
		}
		end(s, f, j, *now, nil)
	}
	*now = t0.Add(model.Idle + time.Minute)
	j := pausedRead(t, s, f, now, model.KBoxAlerts, nil)
	end(s, f, j, *now, nil)
	j = pausedRead(t, s, f, now, model.KBoxAlerts, nil)
	end(s, f, j, *now, &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
	pausedRead(t, s, f, now, model.KBoxAlerts, nil)
	if w := word(s, now.Add(6*time.Second)); !strings.HasPrefix(w, "waiting 6s for an answer") {
		t.Errorf("paused, the retry waited for 6 s: %q", w)
	}
}

// TestPausedFirstAnswer: an app listed while paused is not said read
// until its alerts have answered once.
func TestPausedFirstAnswer(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	now := t0.Add(model.Idle + time.Minute)
	e.SetApps(names(1), now)
	if e.Health(now).WatchRead {
		t.Error("said read before any alerts answer")
	}
	for range 10 {
		j, ok, _ := e.Take(now)
		if !ok {
			break
		}
		e.Done(j, now, nil)
	}
	if !e.Health(now).WatchRead {
		t.Error("not said read once its alerts answered")
	}
}
