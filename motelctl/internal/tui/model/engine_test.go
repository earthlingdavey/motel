package model_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/motelctl/internal/client"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/demo"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

var t0 = time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)

// sim runs the engine on a fake clock for d, each read taking lat and
// ending with answer(j, at). It returns every read and when it began.
func sim(e *model.Engine, d, lat time.Duration, answer func(model.Job, time.Time) error) []call {
	var calls []call
	now := t0
	for now.Before(t0.Add(d)) {
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
		if j.Kind == model.KApps {
			e.SetApps(appNames, now)
		}
		e.Done(j, now, answer(j, now))
	}
	return calls
}

type call struct {
	j  model.Job
	at time.Time
}

var appNames []string

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "app-" + strconv.Itoa(i)
	}
	return out
}

func ok(model.Job, time.Time) error { return nil }

// boxBucket fails when calls ever exceed the box's limit for a person:
// 2 requests a second with a burst of 60.
func boxBucket(t *testing.T, calls []call) {
	t.Helper()
	tokens, last := 60.0, t0
	for _, c := range calls {
		tokens = min(60, tokens+c.at.Sub(last).Seconds()*2)
		last = c.at
		tokens--
		if tokens < 0 {
			t.Fatalf("read %v at +%s goes past the box's rate", c.j, c.at.Sub(t0))
		}
	}
}

func TestRateNeverExceeded(t *testing.T) {
	for _, n := range []int{1, 12, 60} {
		t.Run(strconv.Itoa(n)+" apps", func(t *testing.T) {
			appNames = names(n)
			e := model.NewEngine(t0)
			e.Focus("app-0", t0)
			calls := sim(e, 10*time.Minute, 50*time.Millisecond, ok)
			boxBucket(t, calls)
			// Own bucket too: never more than 30 at once, 1.5 a second.
			if got, most := len(calls), 30+int(1.5*600)+1; got > most {
				t.Errorf("%d reads in 10 minutes, more than %d", got, most)
			}
			// Every app was read, and none starved.
			last := map[string]time.Time{}
			for _, c := range calls {
				if c.j.Kind == model.KStatus {
					last[c.j.App] = c.at
				}
			}
			for _, a := range appNames {
				if t0.Add(10*time.Minute).Sub(last[a]) > 2*time.Minute {
					t.Errorf("%s last read at %v", a, last[a])
				}
			}
		})
	}
}

func TestCadence(t *testing.T) {
	appNames = names(12)
	e := model.NewEngine(t0)
	e.Focus("app-3", t0)
	calls := sim(e, 5*time.Minute, 20*time.Millisecond, ok)
	byApp := map[string][]time.Time{}
	for _, c := range calls {
		if c.j.Kind == model.KStatus {
			byApp[c.j.App] = append(byApp[c.j.App], c.at)
		}
	}
	gaps := func(ts []time.Time) (lo, hi time.Duration) {
		lo = time.Hour
		for i := 2; i < len(ts); i++ { // the first gap is the start's
			g := ts[i].Sub(ts[i-1])
			lo, hi = min(lo, g), max(hi, g)
		}
		return lo, hi
	}
	if lo, hi := gaps(byApp["app-3"]); lo < 5*time.Second || hi > 6*time.Second {
		t.Errorf("the focused app is read every %s to %s, want 5 s", lo, hi)
	}
	if lo, hi := gaps(byApp["app-7"]); lo < 29*time.Second || hi > 31*time.Second {
		t.Errorf("another app is read every %s to %s, want 30 s", lo, hi)
	}
	// Spread: after the first pass, no second holds more than two
	// background reads.
	per := map[int64]int{}
	for _, c := range calls {
		if c.j.Kind == model.KStatus && c.at.Sub(t0) > 40*time.Second {
			per[c.at.Unix()]++
		}
	}
	for s, n := range per {
		if n > 3 {
			t.Errorf("%d reads in the second at +%ds: not spread", n, s-t0.Unix())
		}
	}
}

func TestSlowedBy429(t *testing.T) {
	appNames = names(3)
	e := model.NewEngine(t0)
	slow := &client.Failure{Exit: client.ExitUnknown, Status: 429, RetryAfter: 12 * time.Second}
	sent := false
	calls := sim(e, time.Minute, 10*time.Millisecond, func(j model.Job, at time.Time) error {
		if j.Kind == model.KStatus && !sent {
			sent = true
			return slow
		}
		return nil
	})
	var at429 time.Time
	for i, c := range calls {
		if c.j.Kind == model.KStatus {
			at429 = c.at.Add(10 * time.Millisecond)
			if next := calls[i+1].at; next.Sub(at429) < 12*time.Second {
				t.Fatalf("read again %s after a 429 asking for 12 s", next.Sub(at429))
			}
			break
		}
	}
	if h := e.Health(at429.Add(time.Second)); h.SlowIn <= 0 {
		t.Errorf("the title bar is not told of the 429: %+v", h)
	}
}

func TestStopOn401(t *testing.T) {
	appNames = names(3)
	e := model.NewEngine(t0)
	refused := &client.Failure{Exit: client.ExitRefused, Status: 401}
	n := 0
	calls := sim(e, time.Minute, 10*time.Millisecond, func(j model.Job, _ time.Time) error {
		if j.Kind == model.KStatus {
			n++
			return refused
		}
		return nil
	})
	if n != 1 {
		t.Fatalf("%d reads refused with 401, want 1 and a stop; calls %d", n, len(calls))
	}
	if h := e.Health(t0.Add(time.Minute)); h.Stop != model.SignedOut {
		t.Fatalf("health %+v, want SignedOut", h)
	}
	// r: one whoami, and nothing else until it answers.
	now := t0.Add(2 * time.Minute)
	e.Touch(now)
	e.Retry(now)
	j, ok, _ := e.Take(now)
	if !ok || j.Kind != model.KWhoAmI {
		t.Fatalf("after r, took %v %v, want whoami", j, ok)
	}
	e.Done(j, now, refused)
	if _, ok, _ := e.Take(now.Add(time.Hour)); ok {
		t.Fatal("a second refusal did not stop the reads")
	}
}

func TestBackoffAndStale(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	step := func() model.Job {
		e := s.Engine
		e.Touch(now)
		for range 1000 {
			j, ok, wait := e.Take(now)
			if ok {
				apply, err := s.Fetch(context.Background(), f, j)
				apply()
				e.Done(j, now, err)
				return j
			}
			now = now.Add(max(wait, 100*time.Millisecond))
		}
		t.Fatal("no read in 100 s")
		return model.Job{}
	}
	for range 20 {
		step()
	}
	if r := row(s, now, "blog"); r.Stale {
		t.Fatalf("blog stale while the box answers")
	}
	// Every read gets no answer now: the box is gone.
	for _, n := range append(s.Names(), "apps", "whoami") {
		f.Set(n, &client.Failure{Exit: client.ExitUnknown, Message: "no answer"})
	}
	var gaps []time.Duration
	last := now
	for i := range 4 {
		step()
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
	h := s.Engine.Health(now)
	if h.Misses < model.StaleAfter {
		t.Fatalf("misses %d", h.Misses)
	}
	_, right := s.Title(now)
	if got := right.Text(); got[:5] != "STALE" {
		t.Errorf("title says %q, want STALE", got)
	}
	if r := row(s, now.Add(5*time.Minute), "blog"); !r.Stale || r.Cells[model.ColAge].String() != "stale" {
		t.Errorf("blog not stale: %+v", r.Cells)
	}
}

func row(s *model.Session, now time.Time, name string) model.AppRow {
	for _, r := range s.Apps(now, "", 1).Rows {
		if r.Name == name {
			return r
		}
	}
	return model.AppRow{}
}

// countSigner stands in for ssh-keygen; each signature is new.
type countSigner struct{ n int }

func (c *countSigner) Sign(context.Context, sshkey.Key, string, []byte) ([]byte, error) {
	c.n++
	return []byte("-----BEGIN SSH SIGNATURE-----\nU1NIU0lH" + strings.Repeat("A", c.n*4) + "\n-----END SSH SIGNATURE-----\n"), nil
}

// TestTokenLife: the TUI's token lasts the longest a box takes, and is
// signed again a minute before it ends, and not before: about every
// nine minutes, one touch of a hardware key.
func TestTokenLife(t *testing.T) {
	pk, err := sshkey.ParsePublic("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn") // gitleaks:allow — made up for this test
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	sig := &countSigner{}
	p := &client.PersonToken{Key: sshkey.Key{Public: pk}, Signer: sig, Box: client.Box{Host: demo.Host},
		Now: func() time.Time { return now }, Life: sshkey.MaxLife}
	ask := func(at time.Duration) int {
		now = t0.Add(at)
		if _, err := p.Authorization(context.Background()); err != nil {
			t.Fatal(err)
		}
		return sig.n
	}
	if n := ask(0); n != 1 {
		t.Fatalf("%d signatures to start", n)
	}
	if got := p.Expires().Sub(t0); got != 570*time.Second {
		t.Errorf("expires %s after signing, want 570 s (600 s from an iat 30 s back)", got)
	}
	if n := ask(510 * time.Second); n != 1 {
		t.Errorf("signed again at 510 s, with a minute still left")
	}
	if n := ask(511 * time.Second); n != 2 {
		t.Errorf("not signed again at 511 s, under a minute before the end")
	}
}
