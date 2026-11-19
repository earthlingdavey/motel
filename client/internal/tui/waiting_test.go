package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// fakeKey is the key demo.Fake fails a read by.
func fakeKey(j model.Job) string {
	switch j.Kind {
	case model.KWhoAmI:
		return "whoami"
	case model.KApps:
		return "apps"
	case model.KBox:
		return "box"
	case model.KDeploy:
		return j.App + "@" + j.Version
	}
	return j.App
}

// slowly lets the clock run for d, a second at a time, a key pressed at
// each, as watch does, but each read takes time: one taken at a second
// ends lat(j, at) later (0: at once), failing with the error lat gives
// when not nil, and is then recorded and applied as Drive does. At each
// second, once the reads ending then are applied, a frame is drawn and
// see is given the seconds since the start and the screen.
func (h *harness) slowly(d time.Duration, lat func(j model.Job, at time.Time) (time.Duration, error), see func(sec int, scr string)) {
	type flight struct {
		j   model.Job
		end time.Time
		err error
	}
	var in *flight
	e := h.s.Engine
	for sec := 1; sec <= int(d/time.Second); sec++ {
		h.now = h.now.Add(time.Second)
		h.loop(func() { e.Touch(h.now) })
		for {
			if in != nil {
				if h.now.Before(in.end) {
					break
				}
				h.fake.Set(fakeKey(in.j), in.err)
				apply, err := h.s.Fetch(context.Background(), h.fake, in.j)
				h.fake.Set(fakeKey(in.j), nil)
				e.Done(in.j, h.now, err)
				h.loop(func() { apply(); h.u.stopped() })
				in = nil
			}
			j, ok, _ := e.Take(h.now)
			if !ok {
				break
			}
			l, err := lat(j, h.now)
			in = &flight{j, h.now.Add(l), err}
		}
		h.sync()
		see(sec, h.screen())
	}
}

// said is what a screen says each second: the title bar's right side
// from its word on, the state line, and the first line that says a read
// failed.
type said struct{ title, state, failed string }

func saidOn(scr string) said {
	var s said
	lines := strings.Split(scr, "\n")
	for _, w := range []string{"live ·", "waiting ", "slow ·", "back ·", "STALE ·", "asking…"} {
		if i := strings.Index(lines[0], w); i >= 0 {
			s.title = lines[0][i:]
			break
		}
	}
	for _, l := range lines[1:] {
		t := strings.TrimSpace(l)
		switch {
		case s.state == "" && (strings.HasPrefix(t, "state ") || strings.HasPrefix(t, "▲ blog") || strings.HasPrefix(t, "✖ blog") || strings.HasPrefix(t, "· blog") || strings.HasPrefix(t, "blog ")):
			s.state = t
		case s.failed == "" && strings.Contains(t, "failed"):
			s.failed = t
		}
	}
	return s
}

// word is the title bar's word in what saidOn read.
func (s said) word() string {
	w, _, _ := strings.Cut(s.title, " ")
	return w
}

// table is the seconds at which what is said changed (the title bar's
// word, stale in the state, a line that says a read failed), for the
// log.
func table(rows []said, from int) string {
	var b strings.Builder
	var prev said
	for i, r := range rows {
		stale := strings.Contains(r.state, "stale") != strings.Contains(prev.state, "stale")
		if i == 0 || r.word() != prev.word() || stale || r.failed != prev.failed {
			fmt.Fprintf(&b, "  %+4d s | %s | %s | %s\n", i+1-from, r.title, r.state, r.failed)
		}
		prev = r
	}
	return b.String()
}

func timedOut() error {
	return &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com timed out on 443: no whole answer in 30 s"}
}

// oneApp is a box with one app, blog: the TUI opens on its screen.
func oneApp(f *demo.Fake) { f.Body("apps.json", `{"apps": [{"name": "blog"}]}`) }

// TestWaitingHostGone is the trial's host whose packets are dropped
// (C2): the app's screen open, its last answer 3 s before the outage,
// every read made in it waiting out the client's 30 s. The title bar
// says waiting from 5 s into the first read made after, and goes on
// saying it through the read the first one's failure brings forward,
// until the run begins: slow, then STALE. It never says live in the
// outage after that first read's first 5 s, and says back, then live,
// once the box answers again.
func TestWaitingHostGone(t *testing.T) {
	h := start(t, 100, 30, oneApp)
	h.walk(60*time.Second, model.Job{}) // the open app's read at +60 s answered
	// The outage from 3 s on, for 180 s.
	from, to := h.now.Add(3*time.Second), h.now.Add(183*time.Second)
	var rows []said
	h.slowly(260*time.Second, func(_ model.Job, at time.Time) (time.Duration, error) {
		if !at.Before(from) && at.Before(to) {
			return 30 * time.Second, timedOut()
		}
		return 0, nil
	}, func(_ int, scr string) { rows = append(rows, saidOn(scr)) })
	t.Logf("since the outage | title bar | state | the first line that says failed\n%s", table(rows, 3))
	// rows[i] is second i+1 of the run; the outage begins at second 3.
	at := func(s int) said { return rows[s+2] }
	for s := -2; s < 7; s++ {
		if w := at(s).word(); w != "live" {
			t.Errorf("+%d s: %q, want live (the first read in the outage is made at +2 s)", s, at(s).title)
		}
	}
	for s := 7; s < 62; s++ {
		if w := at(s).word(); w != "waiting" {
			t.Errorf("+%d s: %q, want waiting", s, at(s).title)
		}
	}
	if !strings.HasPrefix(at(7).title, "waiting 5s for an answer · 10s ago") || !strings.HasPrefix(at(40).title, "waiting 38s for an answer · 43s ago") {
		t.Errorf("waiting says %q at +7 s, %q at +40 s", at(7).title, at(40).title)
	}
	if w := at(62).word(); w != "slow" {
		t.Errorf("+62 s: %q, want slow", at(62).title)
	}
	if w := at(96).word(); w != "STALE" {
		t.Errorf("+96 s: %q, want STALE", at(96).title)
	}
	for s := 7; s < 180; s++ {
		if at(s).word() == "live" {
			t.Errorf("+%d s: live in the outage", s)
			break
		}
	}
	if last := rows[len(rows)-1]; last.word() != "live" {
		t.Errorf("at the end: %q, want live", last.title)
	}
}

// TestWaitingRefusedLate is the trial's moteld stopped for a minute,
// seen through a NAT that passes each refusal on 10 s late (C1): the
// apps screen open, its one app read every 30 s and the list every
// 60 s. No read is in flight until the app's next turn, and the title
// bar says live, its age growing, until 5 s into that read; then
// waiting, through the read its failure brings forward, then slow and
// STALE; back once moteld answers.
func TestWaitingRefusedLate(t *testing.T) {
	h := start(t, 100, 30, oneApp)
	h.on(func() { h.u.back() }) // esc: the apps screen, the list under the app's
	h.pump()
	h.walk(50*time.Second, model.Job{})
	refused := &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com did not answer on 443 (connection refused)"}
	from, to := h.now.Add(1*time.Second), h.now.Add(61*time.Second)
	var rows []said
	var reads []string
	h.slowly(150*time.Second, func(j model.Job, at time.Time) (time.Duration, error) {
		reads = append(reads, fmt.Sprintf("%s at %+.0f s", fakeKey(j), at.Sub(from).Seconds()))
		if !at.Before(from) && at.Before(to) {
			return 10 * time.Second, refused
		}
		return 0, nil
	}, func(_ int, scr string) { rows = append(rows, saidOn(scr)) })
	t.Logf("reads: %s\nsince the stop | title bar | state | the first line that says failed\n%s", strings.Join(reads, ", "), table(rows, 1))
	first := -1
	for i, r := range rows {
		if i >= 1 && r.word() != "live" {
			first = i
			break
		}
	}
	if first < 0 || rows[first].word() != "waiting" {
		t.Fatalf("the title bar never says waiting after the stop")
	}
	for i := first; i < len(rows) && i < 61; i++ {
		if rows[i].word() == "live" {
			t.Errorf("+%d s: live after waiting was said, the box still stopped", i)
		}
	}
	if last := rows[len(rows)-1]; last.word() != "live" {
		t.Errorf("at the end: %q, want live", last.title)
	}
}

// TestWaitingAt60: the waiting words are drawn whole at the smallest
// window, 60 columns, ending at its edge, the left side cut before them.
func TestWaitingAt60(t *testing.T) {
	h := start(t, 60, 16, oneApp)
	h.walk(60*time.Second, model.Job{})
	var title string
	h.slowly(70*time.Second, func(model.Job, time.Time) (time.Duration, error) {
		return 30 * time.Second, timedOut()
	}, func(sec int, scr string) {
		if sec == 61 {
			title, _, _ = strings.Cut(scr, "\n")
		}
	})
	if want := "… waiting 56s for an answer · 1m ago"; !strings.HasSuffix(title, want) || len([]rune(title)) != 60 {
		t.Errorf("at 60 columns the title bar is %q, want it to end with %q at the edge", title, want)
	}
}
