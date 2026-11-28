package model_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestWatchRates: the watch's reads on the engine's fake clock, a person
// at the keys on the apps screen for 75 minutes, measured over minutes
// 10 to 70 (#576's spike, its table as assertions): a viewer of the box
// reads the box's alerts every 15 s, 0.067 a second whatever the apps,
// and the other apps' statuses keep 30 s to 12 apps, 33.2 s at 40; a
// person with app grants only reads each app's alerts at its status's
// interval, which keeps 30 s to 12 apps and is 63.5 s at 40. The box's
// limit is never passed.
func TestWatchRates(t *testing.T) {
	for _, c := range []struct {
		n      int
		w      model.Watch
		alerts float64
		status time.Duration
	}{
		{1, model.WatchBox, 0.067, 30 * time.Second},
		{5, model.WatchBox, 0.067, 30 * time.Second},
		{12, model.WatchBox, 0.067, 30 * time.Second},
		{40, model.WatchBox, 0.067, 33200 * time.Millisecond},
		{1, model.WatchApps, 0.033, 30 * time.Second},
		{5, model.WatchApps, 0.166, 30 * time.Second},
		{12, model.WatchApps, 0.399, 30 * time.Second},
		{40, model.WatchApps, 0.630, 63500 * time.Millisecond},
	} {
		name := map[model.Watch]string{model.WatchBox: "box", model.WatchApps: "apps"}[c.w] + " " + strconv.Itoa(c.n)
		t.Run(name, func(t *testing.T) {
			e := model.NewEngine(t0)
			e.SetWatch(c.w, t0)
			calls := sim(e, names(c.n), t0, 75*time.Minute, 50*time.Millisecond, ok)
			boxBucket(t, calls)
			n := 0
			for _, cl := range calls {
				if cl.at.Sub(t0) >= 10*time.Minute && cl.at.Sub(t0) < 70*time.Minute && (cl.j.Kind == model.KAlerts || cl.j.Kind == model.KBoxAlerts) {
					n++
				}
			}
			if got := float64(n) / 3600; got < c.alerts*0.97 || got > c.alerts*1.03 {
				t.Errorf("the alerts are read %.3f a second, want %.3f", got, c.alerts)
			}
			if got := e.Interval("app-0"); got < c.status-500*time.Millisecond || got > c.status+500*time.Millisecond {
				t.Errorf("a status every %s, want %s", got, c.status)
			}
		})
	}
}

// watcher is a session of a viewer of the box on the fixtures, with
// none of the box's alerts firing yet.
func watcher(t *testing.T) (*model.Session, *demo.Fake, *time.Time) {
	t.Helper()
	s, f, now := session(t)
	f.Body("box-alerts.json", alerts(false))
	return s, f, now
}

// alerts is a box's alerts body of rows (each made by alert), with
// more_alerts when more.
func alerts(more bool, rows ...string) string {
	m := ""
	if more {
		m = `,"more_alerts":true`
	}
	return `{"monitor":"on","alerts":[` + strings.Join(rows, ",") + `],"silences":[]` + m + `}`
}

// alert is one row, its start pinned (no "@" time, which the fake reads
// from when the body was set); extra is more fields, each `,"k":v`.
func alert(name, severity, state string, start time.Time, extra string) string {
	b, _ := json.Marshal(start.UTC().Format(time.RFC3339Nano))
	return `{"name":"` + name + `","severity":"` + severity + `","summary":"s","starts_at":` + string(b) + `,"state":"` + state + `"` + extra + `}`
}

// watchRead makes the reads due until the box's alerts are read, the
// clock moved to the watch's next read first.
func watchRead(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
	t.Helper()
	*now = now.Add(model.WatchEvery)
	for range 50 {
		if j := step(t, s, f, now); j.Kind == model.KBoxAlerts {
			return
		}
	}
	t.Fatal("the box's alerts were not read")
}

// firing holds the watch's count and its new alerts, and whether the
// bell rings now.
func firing(t *testing.T, s *model.Session, now time.Time, what string, n, fresh int, ring bool) {
	t.Helper()
	f := s.Firing(now)
	if f.Why != model.Known || f.N != n || f.New != fresh {
		t.Errorf("%s: %+v, want %d firing, %d new", what, f, n, fresh)
	}
	if got := s.Ring(now); got != ring {
		t.Errorf("%s: the bell rings %v, want %v", what, got, ring)
	}
}

// TestWatchNew: what is new is what is counted now and was not at the
// last known answer, by name, app, instance, severity and start, as a
// set: nothing at the first answer; a new alert, a refire (a new
// start), a silence ending and the deploy mute ending each are; an info
// alert, one held quiet and one resolving are not, and ring nothing;
// ChannelFailed's identical rows count once; the row of an app the
// person may not read keeps its key while it stays so. One bell a read that brings something
// new, none sooner than RingFloor after the last: one owed inside it is
// held, and rung once when it ends.
func TestWatchNew(t *testing.T) {
	s, f, now := watcher(t)
	a := t0.Add(-time.Hour)
	crit := alert("cert_failed", "critical", "active", a, `,"instance":"wiki.example.com"`)
	warn := alert("deploy_refused", "warning", "active", a, `,"app":"shop"`)
	f.Body("box-alerts.json", alerts(false, crit, warn))
	for range 3 {
		step(t, s, f, now)
	}
	firing(t, s, *now, "the first answer", 2, 0, false)
	if fw := s.Firing(*now); fw.Worst != model.MarkBroken {
		t.Errorf("the worst is %v, want critical's", fw.Worst)
	}

	watchRead(t, s, f, now)
	firing(t, s, *now, "the same again", 2, 0, false)

	disk := alert("disk_low", "warning", "active", t0, `,"instance":"/srv"`)
	f.Body("box-alerts.json", alerts(false, disk, crit, warn))
	watchRead(t, s, f, now)
	firing(t, s, *now, "a new alert", 3, 1, true)
	rang := *now

	// Inside the floor: owed and held.
	f.Body("box-alerts.json", alerts(false, alert("disk_low", "critical", "active", now.Add(-time.Second), `,"instance":"/srv"`), crit, warn))
	watchRead(t, s, f, now)
	firing(t, s, *now, "a severity change, inside the floor", 3, 1, false)
	if got := s.Ring(rang.Add(model.RingFloor - time.Second)); got {
		t.Error("the held bell rang inside the floor")
	}
	if got := s.Ring(rang.Add(model.RingFloor)); !got {
		t.Error("the held bell did not ring when the floor ended")
	}
	if got := s.Ring(rang.Add(model.RingFloor + time.Second)); got {
		t.Error("the held bell rang twice")
	}
	*now = rang.Add(model.RingFloor + time.Second)
	s.Seen(*now)

	// Resolving: no bell, and the count goes down.
	f.Body("box-alerts.json", alerts(false, crit, warn))
	watchRead(t, s, f, now)
	firing(t, s, *now, "a resolve", 2, 0, false)

	// Held quiet, then the silence ends; held by the mute, then it ends.
	*now = now.Add(model.RingFloor)
	quiet := alert("backup_stale", "warning", "suppressed", a, `,"silenced_until":"`+t0.Add(time.Hour).Format(time.RFC3339)+`"`)
	muted := alert("watchdog_throttled", "critical", "suppressed", a, `,"app":"blog","inhibited":true`)
	info := alert("cert_obtained", "info", "active", t0, "")
	f.Body("box-alerts.json", alerts(false, quiet, muted, info, crit, warn))
	watchRead(t, s, f, now)
	firing(t, s, *now, "held quiet, and an info alert", 2, 0, false)
	f.Body("box-alerts.json", alerts(false, alert("backup_stale", "warning", "active", a, ""), muted, info, crit, warn))
	watchRead(t, s, f, now)
	firing(t, s, *now, "a silence ending", 3, 1, true)
	*now = now.Add(model.RingFloor)
	s.Seen(*now)
	f.Body("box-alerts.json", alerts(false, alert("backup_stale", "warning", "active", a, ""), alert("watchdog_throttled", "critical", "active", a, `,"app":"blog"`), info, crit, warn))
	watchRead(t, s, f, now)
	firing(t, s, *now, "the deploy mute ending", 4, 1, true)
	*now = now.Add(model.RingFloor)
	s.Seen(*now)

	// A refire: gone, then back with a new start.
	f.Body("box-alerts.json", alerts(false, crit))
	watchRead(t, s, f, now)
	firing(t, s, *now, "three resolve", 1, 0, false)
	f.Body("box-alerts.json", alerts(false, alert("deploy_refused", "warning", "active", *now, `,"app":"shop"`), crit))
	watchRead(t, s, f, now)
	firing(t, s, *now, "a refire", 2, 1, true)
	*now = now.Add(model.RingFloor)
	s.Seen(*now)

	// ChannelFailed told to three channels: three identical rows, one
	// alert.
	cf := alert("ChannelFailed", "warning", "active", *now, "")
	f.Body("box-alerts.json", alerts(false, cf, cf, cf, crit))
	watchRead(t, s, f, now)
	firing(t, s, *now, "ChannelFailed's three rows", 2, 1, true)
	*now = now.Add(model.RingFloor)
	s.Seen(*now)

	// The row of an app the person may not read (liveswap/motel_alerts.go):
	// no name, no instance, the same key at each read.
	withheld := `{"name":"","severity":"warning","summary":"response withheld","starts_at":"` + a.Format(time.RFC3339) + `","state":"active","app":"shop"}`
	f.Body("box-alerts.json", alerts(false, withheld, crit))
	watchRead(t, s, f, now)
	firing(t, s, *now, "a withheld row", 2, 1, true)
	*now = now.Add(model.RingFloor)
	s.Seen(*now)
	watchRead(t, s, f, now)
	firing(t, s, *now, "the withheld row again", 2, 0, false)

	// A severity word and a state the client does not know are counted,
	// as the alerts screen marks them; the worst stays critical's.
	f.Body("box-alerts.json", alerts(false, withheld, crit, alert("odd", "page", "active", a, ""), alert("odder", "warning", "pending", a, "")))
	watchRead(t, s, f, now)
	firing(t, s, *now, "words the client does not know", 4, 2, true)
	if fw := s.Firing(*now); fw.Worst != model.MarkBroken {
		t.Errorf("the worst is %v, want critical's", fw.Worst)
	}
}

// TestWatchFlapping: an alert that resolves and fires again, each time
// with a new start, is new at each firing, and rings at each, no two
// bells closer than RingFloor.
func TestWatchFlapping(t *testing.T) {
	s, f, now := watcher(t)
	for range 3 {
		step(t, s, f, now)
	}
	var rang []time.Time
	for i := range 4 {
		f.Body("box-alerts.json", alerts(false, alert("errors_high", "warning", "active", *now, `,"app":"blog"`)))
		watchRead(t, s, f, now)
		if fw := s.Firing(*now); fw.N != 1 || fw.New != 1 {
			t.Errorf("firing %d: %+v", i, fw)
		}
		if s.Ring(*now) {
			rang = append(rang, *now)
		}
		f.Body("box-alerts.json", alerts(false))
		watchRead(t, s, f, now)
		*now = now.Add(model.RingFloor)
	}
	if len(rang) != 4 {
		t.Errorf("rang %d times for 4 firings: %v", len(rang), rang)
	}
	for i := 1; i < len(rang); i++ {
		if rang[i].Sub(rang[i-1]) < model.RingFloor {
			t.Errorf("two bells %s apart", rang[i].Sub(rang[i-1]))
		}
	}
}

// TestWatchCap: while the box holds more alerts than it sends, an
// alert it no longer sends is not taken as gone, so it is not new when
// it is sent again; the count says it is the least ("+"). Without
// more_alerts an alert not sent is gone, and new when it fires again.
func TestWatchCap(t *testing.T) {
	s, f, now := watcher(t)
	a := t0.Add(-time.Hour)
	x, y := alert("disk_low", "warning", "active", a, `,"instance":"/a"`), alert("disk_low", "warning", "active", a, `,"instance":"/b"`)
	f.Body("box-alerts.json", alerts(true, x, y))
	for range 3 {
		step(t, s, f, now)
	}
	if fw := s.Firing(*now); fw.N != 2 || !fw.More {
		t.Errorf("the first answer: %+v", fw)
	}
	f.Body("box-alerts.json", alerts(true, x))
	watchRead(t, s, f, now)
	f.Body("box-alerts.json", alerts(false, x, y))
	watchRead(t, s, f, now)
	firing(t, s, *now, "one left off the end, sent again", 2, 0, false)
	if s.Mark(*now).Long.Text() != "▲ 2 firing" {
		t.Errorf("the mark says %q", s.Mark(*now).Long.Text())
	}
	f.Body("box-alerts.json", alerts(false, x))
	watchRead(t, s, f, now)
	f.Body("box-alerts.json", alerts(false, x, y))
	watchRead(t, s, f, now)
	firing(t, s, *now, "one gone with no more_alerts, back", 2, 1, true)
	f.Body("box-alerts.json", alerts(true, x, y))
	watchRead(t, s, f, now)
	if m := s.Mark(*now); m.Long.Text() != "▲ 2+ firing, 1 new" || m.Tiny.Text() != "▲2+" {
		t.Errorf("with more_alerts the mark says %q, %q", m.Long.Text(), m.Tiny.Text())
	}
}

// TestWatchUnanswered: `monitor: unanswered` between two answers says
// the count is not known and keeps the last known set apart from the
// screen's answer, which it replaces: at the next answer only what is
// new against the last known set is new, and rings once.
func TestWatchUnanswered(t *testing.T) {
	s, f, now := watcher(t)
	a := t0.Add(-time.Hour)
	x := alert("disk_low", "warning", "active", a, "")
	f.Body("box-alerts.json", alerts(false, x))
	for range 3 {
		step(t, s, f, now)
	}
	f.Body("box-alerts.json", `{"monitor":"unanswered","alerts":[],"silences":[]}`)
	watchRead(t, s, f, now)
	if fw := s.Firing(*now); fw.Why != model.Unanswered {
		t.Errorf("unanswered: %+v", fw)
	}
	if s.Ring(*now) {
		t.Error("unanswered rang the bell")
	}
	if len(s.BoxAlertsSt.V.Alerts) != 0 {
		t.Error("the screen's answer is not the unanswered one")
	}
	y := alert("cert_failed", "critical", "active", t0, "")
	f.Body("box-alerts.json", alerts(false, y, x))
	watchRead(t, s, f, now)
	firing(t, s, *now, "the answer after", 2, 1, true)
}

// TestWatchNotKnown: every state of a read of the box's alerts that
// does not say what fires (#576's READS.md, section 3), and what the
// mark says in it: not known (not asked yet, refused, unanswered, a
// word the client does not know, failed before any answer, forgotten),
// or the last count, old (a read that failed, a 429, an answer it
// cannot use, a stop, the pause, a sleep, a read or a signature that
// waits). A refused watch read never turns the title bar's word to
// refused while the other reads answer.
func TestWatchNotKnown(t *testing.T) {
	a := t0.Add(-time.Hour)
	two := alerts(false, alert("cert_failed", "critical", "active", a, ""), alert("disk_low", "warning", "active", a, ""))
	fail := func(k model.Fail) func(*testing.T, *model.Session, *demo.Fake, *time.Time) {
		return func(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
			f.Set("box-alerts", &model.Failure{Kind: k, Message: "no"})
			watchRead(t, s, f, now)
		}
	}
	for _, c := range []struct {
		name string
		// first: the state is met before the watch's first answer.
		first bool
		do    func(*testing.T, *model.Session, *demo.Fake, *time.Time)
		why   model.Unknown
		old   bool
		long  string
	}{
		{"no answer yet", true, func(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
			step(t, s, f, now) // whoami alone
		}, model.NotAsked, false, "alerts…"},
		{"on", false, func(*testing.T, *model.Session, *demo.Fake, *time.Time) {}, model.Known, false, "✖ 2 firing"},
		{"off", false, func(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
			f.Body("box-alerts.json", `{"monitor":"off","alerts":[],"silences":[]}`)
			watchRead(t, s, f, now)
		}, model.Known, false, "no monitor"},
		{"unanswered", false, func(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
			f.Body("box-alerts.json", `{"monitor":"unanswered","alerts":[],"silences":[]}`)
			watchRead(t, s, f, now)
		}, model.Unanswered, false, "alerts not known"},
		{"a word not known", false, func(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
			f.Body("box-alerts.json", `{"monitor":"degraded","alerts":[],"silences":[]}`)
			watchRead(t, s, f, now)
		}, model.NotAWord, false, "alerts not known"},
		{"403", false, fail(model.FailForbidden), model.WasRefused, false, "alerts not known"},
		{"404", false, fail(model.FailNotFound), model.WasRefused, false, "alerts not known"},
		{"no answer", false, fail(model.FailNoAnswer), model.Known, true, "✖ 2 firing as of 15s ago"},
		{"no answer before any", true, func(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
			f.Set("box-alerts", &model.Failure{Kind: model.FailNoAnswer, Message: "no"})
			for range 3 {
				step(t, s, f, now)
			}
		}, model.FailedFirst, false, "alerts not known"},
		{"429", false, fail(model.FailSlowed), model.Known, true, "✖ 2 firing as of 15s ago"},
		{"an answer it cannot use", false, fail(model.FailUnusable), model.Known, true, "✖ 2 firing as of 15s ago"},
		{"stopped", false, fail(model.FailSignedOut), model.Known, true, "✖ 2 firing as of 15s ago"},
		{"stopped before an answer", true, func(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
			f.Set("apps", &model.Failure{Kind: model.FailSignedOut, Message: "no"})
			for range 2 {
				step(t, s, f, now)
			}
		}, model.FailedFirst, false, "alerts not known"},
		{"answered about another key", false, func(t *testing.T, s *model.Session, f *demo.Fake, now *time.Time) {
			f.Body("whoami.json", strings.Replace(`{"name":"alice@example.com","key":"SHA256:OTHER","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1}`, "OTHER", strings.Repeat("O", 43), 1))
			s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
			for range 30 {
				if step(t, s, f, now).Kind == model.KWhoAmI {
					break
				}
			}
		}, model.Forgotten, false, "alerts not known"},
		{"paused", false, func(_ *testing.T, _ *model.Session, _ *demo.Fake, now *time.Time) {
			*now = now.Add(model.Idle)
		}, model.Known, true, "✖ 2 firing as of 10m ago"},
		{"asleep", false, func(_ *testing.T, s *model.Session, _ *demo.Fake, now *time.Time) {
			s.Engine.Tick(*now)
			*now = now.Add(5 * time.Minute)
			s.Engine.Touch(*now)
			s.Engine.Tick(*now)
		}, model.Known, true, "✖ 2 firing as of 5m ago"},
		{"a read or a signature that waits", false, func(_ *testing.T, s *model.Session, _ *demo.Fake, now *time.Time) {
			*now = now.Add(model.WatchEvery + model.WaitingAfter + time.Second)
			s.Engine.Touch(*now)
		}, model.Known, true, "✖ 2 firing as of 21s ago"},
		{"a read that waits less", false, func(_ *testing.T, s *model.Session, _ *demo.Fake, now *time.Time) {
			*now = now.Add(model.WatchEvery + model.WaitingAfter)
			s.Engine.Touch(*now)
		}, model.Known, false, "✖ 2 firing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, f, now := watcher(t)
			f.Body("box-alerts.json", two)
			if !c.first {
				for range 3 {
					step(t, s, f, now)
				}
				if fw := s.Firing(*now); fw.Why != model.Known || fw.N != 2 {
					t.Fatalf("before: %+v", fw)
				}
			}
			c.do(t, s, f, now)
			fw := s.Firing(*now)
			if fw.Why != c.why || fw.Old != c.old {
				t.Errorf("%+v, want why %v, old %v", fw, c.why, c.old)
			}
			if got := s.Mark(*now).Long.Text(); got != c.long {
				t.Errorf("the mark says %q, want %q", got, c.long)
			}
			if _, r := s.Title(*now); strings.HasPrefix(r.Text(), "refused") {
				t.Errorf("the title bar's word is %q", r.Text())
			}
		})
	}
}

// TestWatchRefusedAskedAgain: a 403 on the watch's read holds it, and
// says not known in the mark, not refused in the title bar; it is asked
// again when whoami next lists the box among the roles, not only by
// ctrl-r. A 404 (a box older than the read) is not.
func TestWatchRefusedAskedAgain(t *testing.T) {
	for _, c := range []struct {
		fail  model.Fail
		again bool
	}{{model.FailForbidden, true}, {model.FailNotFound, false}} {
		s, f, now := watcher(t)
		for range 3 {
			step(t, s, f, now)
		}
		f.Set("box-alerts", &model.Failure{Kind: c.fail, Message: "no"})
		watchRead(t, s, f, now)
		f.Set("box-alerts", nil)
		before := len(f.Calls)
		// Whoami's own turn, and the reads after it.
		*now = now.Add(model.MeAlways)
		for range 20 {
			step(t, s, f, now)
		}
		read := false
		for _, cl := range f.Calls[before:] {
			read = read || cl.Job.Kind == model.KBoxAlerts
		}
		if read != c.again {
			t.Errorf("%v: the box's alerts read again after whoami: %v, want %v", c.fail, read, c.again)
		}
		if fw := s.Firing(*now); (fw.Why == model.Known) != c.again {
			t.Errorf("%v: %+v", c.fail, fw)
		}
	}
}

// appPerson is a session of a person with app grants only, on blog and
// shop.
func appPerson(t *testing.T) (*model.Session, *demo.Fake, *time.Time) {
	t.Helper()
	s, f, now := session(t)
	f.Body("whoami.json", `{"name":"bob@example.com","key":"`+demo.Key.Fingerprint+`","roles":[{"role":"viewer","scope":"app:blog"},{"role":"viewer","scope":"app:shop"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1}`)
	f.Body("apps.json", `{"apps":[{"name":"blog"},{"name":"shop"}]}`)
	f.Body("alerts/blog.json", `{"monitor":"on","alerts":[`+alert("errors_high", "warning", "active", t0.Add(-time.Hour), "")+`]}`)
	f.Body("alerts/shop.json", `{"monitor":"on","alerts":[]}`)
	return s, f, now
}

// TestWatchApps: a person with app grants only watches each app's
// alerts, read in the background; the count is known once every app's
// is, and is their sum; an alert new in one app is new; a 403 on one
// app's alerts says not known, and is asked again when whoami lists
// that app. When whoami lists the box, the box's alerts are watched
// instead, from a first answer that is nothing new.
func TestWatchApps(t *testing.T) {
	s, f, now := appPerson(t)
	read := map[string]bool{}
	for range 6 {
		if j := step(t, s, f, now); j.Kind == model.KAlerts {
			read[j.App] = true
		}
	}
	if !read["blog"] || !read["shop"] {
		t.Fatalf("each app's alerts are not read: %v", read)
	}
	firing(t, s, *now, "the first answers", 1, 0, false)
	f.Body("alerts/shop.json", `{"monitor":"on","alerts":[`+alert("app_memory_high", "critical", "active", *now, "")+`]}`)
	*now = now.Add(model.AppWatchEvery)
	for range 6 {
		step(t, s, f, now)
	}
	firing(t, s, *now, "a new alert in shop", 2, 1, true)
	if m := s.Mark(*now); !strings.HasPrefix(m.Long.Text(), "✖ 2 firing, 1 new") {
		t.Errorf("the mark says %q", m.Long.Text())
	}

	f.Set("alerts:blog", &model.Failure{Kind: model.FailForbidden, Message: "no"})
	*now = now.Add(model.AppWatchEvery)
	for range 6 {
		step(t, s, f, now)
	}
	if fw := s.Firing(*now); fw.Why != model.WasRefused {
		t.Errorf("blog's alerts refused: %+v", fw)
	}
	if _, r := s.Title(*now); strings.HasPrefix(r.Text(), "refused") {
		t.Errorf("the title bar's word is %q", r.Text())
	}
	f.Set("alerts:blog", nil)
	*now = now.Add(model.MeAlways)
	for range 10 {
		step(t, s, f, now)
	}
	if fw := s.Firing(*now); fw.Why != model.Known || fw.N != 2 {
		t.Errorf("after whoami lists blog: %+v", fw)
	}

	f.Body("whoami.json", `{"name":"bob@example.com","key":"`+demo.Key.Fingerprint+`","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1}`)
	f.Body("box-alerts.json", alerts(false, alert("cert_failed", "critical", "active", t0, "")))
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	box := false
	for range 6 {
		if j := step(t, s, f, now); j.Kind == model.KBoxAlerts {
			box = true
		}
	}
	if !box {
		t.Fatal("the box's alerts are not read once whoami lists the box")
	}
	firing(t, s, *now, "the box's first answer", 1, 0, false)
}

// TestMarkForms: the mark's words in each form, for each state.
func TestMarkForms(t *testing.T) {
	s, f, now := watcher(t)
	a := t0.Add(-time.Hour)
	if m := s.Mark(*now); m.Long.Text() != "alerts…" || m.Tiny.Text() != "…" {
		t.Errorf("before any answer: %q, %q", m.Long.Text(), m.Tiny.Text())
	}
	for range 3 {
		step(t, s, f, now)
	}
	if m := s.Mark(*now); !m.Empty() {
		t.Errorf("none firing: %q", m.Long.Text())
	}
	f.Body("box-alerts.json", alerts(false, alert("disk_low", "warning", "active", a, "")))
	watchRead(t, s, f, now)
	m := s.Mark(*now)
	for got, want := range map[string]string{m.Long.Text(): "▲ 1 firing, 1 new", m.Short.Text(): "▲ 1", m.Tiny.Text(): "▲1", m.Glyph.Text(): "▲"} {
		if got != want {
			t.Errorf("a new warning: %q, want %q", got, want)
		}
	}
	s.Seen(*now) // under SeenAfter: not seen
	if m := s.Mark(*now); m.Long.Text() != "▲ 1 firing, 1 new" {
		t.Errorf("a key at once saw it: %q", m.Long.Text())
	}
	s.Seen(now.Add(model.SeenAfter))
	if m := s.Mark(*now); m.Long.Text() != "▲ 1 firing" || m.Long[0].C != model.Warn {
		t.Errorf("seen: %q (%v)", m.Long.Text(), m.Long[0].C)
	}
	*now = now.Add(model.Idle)
	m = s.Mark(*now)
	for got, want := range map[string]string{m.Long.Text(): "▲ 1 firing as of 10m ago", m.Short.Text(): "▲ 1?", m.Tiny.Text(): "▲1?", m.Glyph.Text(): "▲?"} {
		if got != want {
			t.Errorf("paused: %q, want %q", got, want)
		}
	}
	s.Engine.Touch(*now)
	f.Body("box-alerts.json", alerts(false))
	watchRead(t, s, f, now)
	if m := s.Mark(*now); !m.Empty() {
		t.Errorf("none firing again: %q", m.Long.Text())
	}
	*now = now.Add(model.Idle)
	if m := s.Mark(*now); m.Short.Text() != "0?" || m.Long.Text() != "0 firing as of 10m ago" {
		t.Errorf("none firing, paused: %q, %q", m.Long.Text(), m.Short.Text())
	}
	s.Engine.Touch(*now)
	f.Body("box-alerts.json", `{"monitor":"off","alerts":[],"silences":[]}`)
	watchRead(t, s, f, now)
	if m := s.Mark(*now); m.Long.Text() != "no monitor" || m.Long[0].C != model.Dim || m.Short != nil || m.Tiny != nil || m.Glyph != nil {
		t.Errorf("monitor off: %+v", m)
	}
}

// TestMarkMoves: while something is new the mark is reversed on even
// seconds of the wall clock and plain on odd ones, for MoveFor; then
// reversed on both, until seen.
func TestMarkMoves(t *testing.T) {
	s, f, now := watcher(t)
	for range 3 {
		step(t, s, f, now)
	}
	if now.Add(model.WatchEvery).Unix()%2 != 0 {
		*now = now.Add(time.Second)
	}
	f.Body("box-alerts.json", alerts(false, alert("cert_failed", "critical", "active", t0, "")))
	watchRead(t, s, f, now)
	at := *now
	if at.Unix()%2 != 0 {
		t.Fatalf("the arrival is at an odd second: %v", at)
	}
	// In order: the session takes a time before the last it was given
	// for the clock set back.
	for _, c := range []struct {
		d    time.Duration
		loud bool
	}{{0, true}, {time.Second, false}, {2 * time.Second, true}, {3 * time.Second, false}, {model.MoveFor, true}, {model.MoveFor + time.Second, true}, {time.Minute + time.Second, true}} {
		if got := s.Mark(at.Add(c.d)).Long[0].C == model.LoudBad; got != c.loud {
			t.Errorf("%s after the arrival: reversed %v, want %v", c.d, got, c.loud)
		}
	}
	later := at.Add(time.Minute + 2*time.Second)
	s.Seen(later)
	if c := s.Mark(later).Long[0].C; c != model.Bad {
		t.Errorf("seen: %v", c)
	}
}

// TestHeldBellDropped: a bell held inside the floor is not rung at its
// end when everything it was owed for has been seen by then, or has
// resolved; it is when something it was owed for is still new.
func TestHeldBellDropped(t *testing.T) {
	a := t0.Add(-time.Hour)
	x := alert("disk_low", "warning", "active", a, "")
	for _, c := range []struct {
		name string
		// then is what happens inside the floor, y the first alert.
		then func(s *model.Session, f *demo.Fake, now *time.Time, y string)
		ring bool
	}{
		{"still new", func(*model.Session, *demo.Fake, *time.Time, string) {}, true},
		{"seen", func(s *model.Session, _ *demo.Fake, now *time.Time, _ string) {
			s.Seen(now.Add(model.SeenAfter))
		}, false},
		{"resolved", func(s *model.Session, f *demo.Fake, now *time.Time, y string) {
			f.Body("box-alerts.json", alerts(false, y))
			watchRead(t, s, f, now)
		}, false},
	} {
		s, f, now := watcher(t)
		for range 3 {
			step(t, s, f, now)
		}
		y := alert("cert_failed", "critical", "active", *now, "")
		f.Body("box-alerts.json", alerts(false, y))
		watchRead(t, s, f, now)
		firing(t, s, *now, c.name+": the first", 1, 1, true)
		rang := *now
		s.Seen(now.Add(model.SeenAfter))
		f.Body("box-alerts.json", alerts(false, x, y))
		watchRead(t, s, f, now)
		firing(t, s, *now, c.name+": the second, inside the floor", 2, 1, false)
		c.then(s, f, now, y)
		if got := s.Ring(rang.Add(model.RingFloor)); got != c.ring {
			t.Errorf("%s: the held bell rings %v at the floor's end, want %v", c.name, got, c.ring)
		}
	}
}

// TestWatchAppsLists: for a person with app grants only, an app that
// leaves the list leaves the count, and what arrived on it is no longer
// new and owes no bell; an app not listed, its screen opened by name,
// is not watched; with no app listed the mark is empty, paused or not.
func TestWatchAppsLists(t *testing.T) {
	s, f, now := appPerson(t)
	for range 6 {
		step(t, s, f, now)
	}
	f.Body("alerts/shop.json", `{"monitor":"on","alerts":[`+alert("app_memory_high", "critical", "active", *now, "")+`]}`)
	*now = now.Add(model.AppWatchEvery)
	for range 6 {
		step(t, s, f, now)
	}
	if fw := s.Firing(*now); fw.N != 2 || fw.New != 1 {
		t.Fatalf("a new alert in shop: %+v", fw)
	}
	f.Body("apps.json", `{"apps":[{"name":"blog"}]}`)
	s.Engine.Again(*now, model.Job{Kind: model.KApps})
	for range 30 {
		if step(t, s, f, now).Kind == model.KApps {
			break
		}
	}
	if fw := s.Firing(*now); fw.N != 1 || fw.New != 0 {
		t.Errorf("shop gone from the list: %+v", fw)
	}
	if s.Ring(*now) {
		t.Error("the bell rang for an app no longer listed")
	}

	f.Body("alerts/ghost.json", `{"monitor":"on","alerts":[`+alert("errors_high", "critical", "active", *now, "")+`]}`)
	s.Engine.AlertsOpen("ghost", *now)
	for range 30 {
		if j := step(t, s, f, now); j.Kind == model.KAlerts && j.App == "ghost" {
			break
		}
	}
	if fw := s.Firing(*now); fw.N != 1 || fw.New != 0 {
		t.Errorf("an app not listed, its screen opened: %+v", fw)
	}

	s, f, now = appPerson(t)
	f.Body("apps.json", `{"apps":[]}`)
	for range 2 {
		step(t, s, f, now)
	}
	*now = now.Add(model.Idle)
	if m := s.Mark(*now); !m.Empty() {
		t.Errorf("no app listed, paused: the mark says %q", m.Long.Text())
	}
}

// TestWatchHeldBeforeChange: the box's alerts held by a 404 before the
// watch changed (a box viewer's grant dropped from whoami, then back)
// are asked again when the watch takes them, so the mark says why it
// does not know, not alerts… for ever.
func TestWatchHeldBeforeChange(t *testing.T) {
	s, f, now := watcher(t)
	for range 3 {
		step(t, s, f, now)
	}
	f.Set("box-alerts", &model.Failure{Kind: model.FailNotFound, Message: "no"})
	watchRead(t, s, f, now)
	who := func(scope string) {
		f.Body("whoami.json", `{"name":"alice@example.com","key":"`+demo.Key.Fingerprint+`","roles":[{"role":"viewer","scope":"`+scope+`"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1}`)
		*now = now.Add(model.AgainAfter)
		s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
		for range 30 {
			if step(t, s, f, now).Kind == model.KWhoAmI {
				return
			}
		}
		t.Fatal("whoami was not read")
	}
	who("app:blog")
	who("box")
	before := len(f.Calls)
	for range 10 {
		step(t, s, f, now)
	}
	read := false
	for _, c := range f.Calls[before:] {
		read = read || c.Job.Kind == model.KBoxAlerts
	}
	if fw := s.Firing(*now); !read || fw.Why != model.WasRefused {
		t.Errorf("after the box's grant is back: read %v, %+v", read, fw)
	}
}

// TestWatchHeldWhileNotKnown: a bell held inside the floor is not rung
// at its end while the count is not known, and is rung when it is known
// again with what it was for still firing.
func TestWatchHeldWhileNotKnown(t *testing.T) {
	s, f, now := watcher(t)
	for range 3 {
		step(t, s, f, now)
	}
	y := alert("cert_failed", "critical", "active", *now, "")
	f.Body("box-alerts.json", alerts(false, y))
	watchRead(t, s, f, now)
	firing(t, s, *now, "the first", 1, 1, true)
	rang := *now
	x := alert("disk_low", "warning", "active", *now, "")
	f.Body("box-alerts.json", alerts(false, x, y))
	watchRead(t, s, f, now)
	f.Body("box-alerts.json", `{"monitor":"unanswered","alerts":[],"silences":[]}`)
	watchRead(t, s, f, now)
	*now = rang.Add(model.RingFloor)
	if s.Ring(*now) {
		t.Error("the held bell rang while the count is not known")
	}
	f.Body("box-alerts.json", alerts(false, x, y))
	watchRead(t, s, f, now)
	if !s.Ring(*now) {
		t.Error("the held bell did not ring once the count was known again")
	}
}

// TestWatchAppOpenedNotOld: a person with app grants only opening an
// app's screen, whose alerts were read in the background a while ago,
// does not make the count old: it is judged by the interval its answer
// was due again by.
func TestWatchAppOpenedNotOld(t *testing.T) {
	s, f, now := appPerson(t)
	for range 6 {
		step(t, s, f, now)
	}
	*now = now.Add(model.AppWatchEvery - time.Second)
	s.Engine.Touch(*now)
	s.Engine.AlertsOpen("blog", *now)
	if fw := s.Firing(*now); fw.Why != model.Known || fw.Old {
		t.Errorf("blog's screen opened: %+v", fw)
	}
}

// TestWatchAppsHeldNotBudgeted: an app's alerts held (a 404 from a box
// older than the read) take no share of the budget the statuses are
// read in.
func TestWatchAppsHeldNotBudgeted(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	apps := names(40)
	calls := sim(e, apps, t0, 10*time.Minute, 50*time.Millisecond, func(j model.Job, _ time.Time) error {
		if j.Kind == model.KAlerts {
			return &model.Failure{Kind: model.FailNotFound, Message: "no"}
		}
		return nil
	})
	boxBucket(t, calls)
	plain := model.NewEngine(t0)
	sim(plain, apps, t0, 10*time.Minute, 50*time.Millisecond, ok)
	if got, want := e.Interval("app-1"), plain.Interval("app-1"); got != want {
		t.Errorf("with every app's alerts held a status comes every %s, want %s as with no watch", got, want)
	}
}

// TestWatchMoreNoneCounted: the box holding more than it sends, and
// none it sends counted (held quiet, info), the mark says 0+, not
// nothing.
func TestWatchMoreNoneCounted(t *testing.T) {
	s, f, now := watcher(t)
	f.Body("box-alerts.json", alerts(true, alert("disk_low", "warning", "suppressed", t0, "")))
	for range 3 {
		step(t, s, f, now)
	}
	if m := s.Mark(*now); m.Long.Text() != "0+ firing" || m.Tiny.Text() != "0+" {
		t.Errorf("the mark says %q, %q", m.Long.Text(), m.Tiny.Text())
	}
}

// TestSeenEach: a key sees what arrived SeenAfter or more before it,
// though something else arrived since that it does not yet see.
func TestSeenEach(t *testing.T) {
	s, f, now := watcher(t)
	for range 3 {
		step(t, s, f, now)
	}
	x := alert("disk_low", "warning", "active", *now, "")
	f.Body("box-alerts.json", alerts(false, x))
	watchRead(t, s, f, now)
	f.Body("box-alerts.json", alerts(false, alert("cert_failed", "critical", "active", *now, ""), x))
	watchRead(t, s, f, now)
	s.Seen(now.Add(model.SeenAfter / 2))
	if fw := s.Firing(now.Add(model.SeenAfter / 2)); fw.New != 1 {
		t.Errorf("a key half a second after the second: %d new, want 1", fw.New)
	}
}

// TestHeldBellOldOnly: a bell held for an alert that then resolves is
// not rung for an older one, still unseen, that rang already.
func TestHeldBellOldOnly(t *testing.T) {
	s, f, now := watcher(t)
	for range 3 {
		step(t, s, f, now)
	}
	y := alert("cert_failed", "critical", "active", *now, "")
	f.Body("box-alerts.json", alerts(false, y))
	watchRead(t, s, f, now)
	firing(t, s, *now, "the first", 1, 1, true)
	rang := *now
	f.Body("box-alerts.json", alerts(false, alert("disk_low", "warning", "active", *now, ""), y))
	watchRead(t, s, f, now)
	f.Body("box-alerts.json", alerts(false, y))
	watchRead(t, s, f, now)
	if s.Ring(rang.Add(model.RingFloor)) {
		t.Error("the held bell rang for the first, which rang already")
	}
}

// TestWatchAppsStatusesFirst: for a person with app grants only, the
// apps' statuses are read before their alerts when both are due, so the
// apps screen fills as fast as before the watch.
func TestWatchAppsStatusesFirst(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	calls := sim(e, names(30), t0, time.Minute, 50*time.Millisecond, ok)
	statuses := 0
	for _, c := range calls {
		switch c.j.Kind {
		case model.KStatus:
			statuses++
		case model.KAlerts:
			if statuses < 30 {
				t.Fatalf("an app's alerts read after %d of 30 statuses", statuses)
			}
		}
	}
}

// TestWatchMoreOld: 0+ says when its count is old.
func TestWatchMoreOld(t *testing.T) {
	s, f, now := watcher(t)
	f.Body("box-alerts.json", alerts(true, alert("disk_low", "warning", "suppressed", t0, "")))
	for range 3 {
		step(t, s, f, now)
	}
	*now = now.Add(model.Idle)
	if m := s.Mark(*now); m.Long.Text() != "0+ firing as of 10m ago" || m.Tiny.Text() != "0+?" {
		t.Errorf("the mark says %q, %q", m.Long.Text(), m.Tiny.Text())
	}
}

// TestAlertKeysApart: the box's fields may hold any byte: two alerts
// whose fields would join alike (a NUL, a backslash) are two, and the
// second is new.
func TestAlertKeysApart(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"name":"a\u0000b","app":"c","severity":"warning","summary":"s","starts_at":"2026-11-15T13:00:00Z","state":"active"}`,
			`{"name":"a","app":"b\u0000c","severity":"warning","summary":"s","starts_at":"2026-11-15T13:00:00Z","state":"active"}`},
		{`{"name":"a\\","app":"\\b","severity":"warning","summary":"s","starts_at":"2026-11-15T13:00:00Z","state":"active"}`,
			`{"name":"a\\\\","app":"b","severity":"warning","summary":"s","starts_at":"2026-11-15T13:00:00Z","state":"active"}`},
	} {
		s, f, now := watcher(t)
		f.Body("box-alerts.json", alerts(false, pair[0]))
		for range 3 {
			step(t, s, f, now)
		}
		f.Body("box-alerts.json", alerts(false, pair[1], pair[0]))
		watchRead(t, s, f, now)
		firing(t, s, *now, pair[1], 2, 1, true)
	}
}

// TestWatchAppsScopes: a grant on an app that whoami no longer lists
// leaves the count at whoami's answer, before the list is read again.
func TestWatchAppsScopes(t *testing.T) {
	s, f, now := appPerson(t)
	for range 6 {
		step(t, s, f, now)
	}
	if fw := s.Firing(*now); fw.N != 1 {
		t.Fatalf("blog's alert: %+v", fw)
	}
	f.Body("whoami.json", `{"name":"bob@example.com","key":"`+demo.Key.Fingerprint+`","roles":[{"role":"viewer","scope":"app:shop"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1}`)
	*now = now.Add(model.AgainAfter)
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	for range 30 {
		if step(t, s, f, now).Kind == model.KWhoAmI {
			break
		}
	}
	if fw := s.Firing(*now); fw.Why != model.Known || fw.N != 0 {
		t.Errorf("blog's grant gone, the list not read again: %+v", fw)
	}
}

// TestWatchAppsRelisted: an app that leaves the list and comes back is
// watched from a first answer: what it held before is neither counted
// before its read nor taken for its baseline, and what arrived on it
// unseen is gone.
func TestWatchAppsRelisted(t *testing.T) {
	s, f, now := appPerson(t)
	for range 6 {
		step(t, s, f, now)
	}
	f.Body("alerts/shop.json", `{"monitor":"on","alerts":[`+alert("app_memory_high", "critical", "active", *now, "")+`]}`)
	*now = now.Add(model.AppWatchEvery)
	for range 6 {
		step(t, s, f, now)
	}
	if fw := s.Firing(*now); fw.N != 2 || fw.New != 1 {
		t.Fatalf("a new alert in shop: %+v", fw)
	}
	list := func(body string) {
		f.Body("apps.json", body)
		*now = now.Add(model.ListAgainAfter)
		s.Engine.Again(*now, model.Job{Kind: model.KApps})
		for range 30 {
			if step(t, s, f, now).Kind == model.KApps {
				return
			}
		}
		t.Fatal("the list was not read")
	}
	list(`{"apps":[{"name":"blog"}]}`)
	f.Body("alerts/shop.json", `{"monitor":"on","alerts":[`+alert("errors_high", "warning", "active", *now, "")+`]}`)
	list(`{"apps":[{"name":"blog"},{"name":"shop"}]}`)
	if fw := s.Firing(*now); fw.Why != model.NotAsked {
		t.Errorf("shop back, not yet read: %+v", fw)
	}
	for range 30 {
		if j := step(t, s, f, now); j.Kind == model.KAlerts && j.App == "shop" {
			break
		}
	}
	firing(t, s, *now, "shop back, read", 2, 0, false)
}

// TestWatchOffOld: an answer of no monitor that is old (the pause, a
// read that failed since) says its count is old, as none firing does:
// a monitor may have been added since.
func TestWatchOffOld(t *testing.T) {
	s, f, now := watcher(t)
	f.Body("box-alerts.json", `{"monitor":"off","alerts":[],"silences":[]}`)
	for range 3 {
		step(t, s, f, now)
	}
	if m := s.Mark(*now); m.Long.Text() != "no monitor" {
		t.Fatalf("fresh: %q", m.Long.Text())
	}
	f.Set("box-alerts", &model.Failure{Kind: model.FailNoAnswer, Message: "no"})
	watchRead(t, s, f, now)
	if m := s.Mark(*now); m.Long.Text() != "0 firing as of 15s ago" || m.Short.Text() != "0?" {
		t.Errorf("after a read that failed: %q, %q", m.Long.Text(), m.Short.Text())
	}
}

// TestWatchAppsSlowReads: reads that take 2 s each, so the statuses of
// 40 apps are always due: every app's alerts are still read, taking
// their turn by when each is due.
func TestWatchAppsSlowReads(t *testing.T) {
	e := model.NewEngine(t0)
	e.SetWatch(model.WatchApps, t0)
	calls := sim(e, names(40), t0, 15*time.Minute, 2*time.Second, ok)
	read := map[string]int{}
	for _, c := range calls {
		if c.j.Kind == model.KAlerts {
			read[c.j.App]++
		}
	}
	for _, a := range names(40) {
		if read[a] < 2 {
			t.Errorf("%s's alerts read %d times in 15 minutes", a, read[a])
		}
	}
}
