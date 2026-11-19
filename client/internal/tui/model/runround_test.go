package model_test

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// watchRun runs the engine as sim does, each read taking no time, and
// says each second what the title bar's word is then: STALE, slow,
// back or live.
func watchRun(e *model.Engine, apps []string, d time.Duration, answer func(model.Job, time.Time) error) ([]call, []string) {
	var calls []call
	var words []string
	now, sec := t0, t0
	for now.Before(t0.Add(d)) {
		for !sec.After(now) {
			h := e.Health(sec)
			w := "live"
			switch {
			case h.Misses >= model.StaleAfter:
				w = "STALE"
			case h.Misses > 0:
				w = "slow"
			case h.Back:
				w = "back"
			}
			words = append(words, w)
			sec = sec.Add(time.Second)
		}
		e.Touch(now)
		j, ok, wait := e.Take(now)
		if !ok {
			if wait <= 0 || wait > 100*time.Millisecond {
				wait = 100 * time.Millisecond
			}
			now = now.Add(wait)
			continue
		}
		calls = append(calls, call{j, now})
		err := answer(j, now)
		if j.Kind == model.KApps && err == nil {
			e.SetApps(apps, now)
		}
		e.Done(j, now, err)
	}
	return calls, words
}

// runs is each stretch of seconds the title bar said slow or STALE: its
// first second and its length.
func runs(words []string) (out [][2]int) {
	for i, w := range words {
		if w != "slow" && w != "STALE" {
			continue
		}
		if n := len(out); n > 0 && out[n-1][0]+out[n-1][1] == i {
			out[n-1][1]++
		} else {
			out = append(out, [2]int{i, 1})
		}
	}
	return out
}

// atItsPace fails unless each job in want is read, from the second from
// to the end of calls' 10 minutes, at least once in every interval its
// own pace gives (one late read allowed for).
func atItsPace(t *testing.T, label string, calls []call, from time.Time, want map[model.Job]time.Duration) {
	t.Helper()
	n := map[model.Job]int{}
	for _, c := range calls {
		if !c.at.Before(from) {
			n[c.j]++
		}
	}
	left := t0.Add(10 * time.Minute).Sub(from)
	for j, every := range want {
		if least := int(left/every) - 1; n[j] < least {
			t.Errorf("%s: %v read %d times in the %v after the run, want at least %d (every %v)", label, j, n[j], left, least, every)
		}
	}
}

// TestRunGoesRound: in a run of no answer, the one read at each retry
// goes round the jobs, so a job whose own route the box does not answer
// cannot take every retry while it would answer the others. whoami's
// route alone getting no answer, and one other read getting none once
// (a dropped connection) after whoami's 1st, 2nd, 3rd or 6th failure;
// and the open app's route alone getting none from +60 s, and the read
// its first failure brings forward getting none once. Each begins a run
// (the title bar says slow, then STALE); the run ends at the first
// retry at which a job the box answers is due, since a job whose last
// read was answered is tried before any whose last read failed: here
// the list's 60 s from its dropped read at the latest, and the retry
// after it, at most 60 s on: within 2 minutes, then back, and live from
// then on, every read the box answers made at its own pace. Without the
// rule, whoami (first in rank, due at its backoff) or the open app (due
// every 5 s, before every other app and the list) took every retry: the
// run of the first never ended, and the second lasted 4 minutes.
func TestRunGoesRound(t *testing.T) {
	me, list := model.Job{Kind: model.KWhoAmI}, model.Job{Kind: model.KApps}
	type scenario struct {
		label string
		focus bool
		down  func(apps []string) func(model.Job, time.Time) error
	}
	var scns []scenario
	for _, after := range []int{1, 2, 3, 6} {
		scns = append(scns, scenario{"whoami's route down, a drop after its failure " + string(rune('0'+after)), false, func([]string) func(model.Job, time.Time) error {
			fails, dropped := 0, false
			return func(j model.Job, _ time.Time) error {
				if j.Kind == model.KWhoAmI {
					fails++
					return fail(model.FailNoAnswer)
				}
				if fails >= after && !dropped {
					dropped = true
					return fail(model.FailNoAnswer)
				}
				return nil
			}
		}})
	}
	scns = append(scns, scenario{"the open app's route down, a drop after its first failure", true, func(apps []string) func(model.Job, time.Time) error {
		failed, dropped := false, false
		return func(j model.Job, at time.Time) error {
			switch {
			case at.Sub(t0) < time.Minute:
				return nil
			case j == model.Job{Kind: model.KStatus, App: apps[0]}:
				failed = true
				return fail(model.FailNoAnswer)
			case failed && !dropped:
				dropped = true
				return fail(model.FailNoAnswer)
			}
			return nil
		}
	}})
	for _, sc := range scns {
		for _, n := range []int{1, 3, 12} {
			label := sc.label + ", " + string(rune('0'+n/10)) + string(rune('0'+n%10)) + " apps"
			apps := names(n)
			e := model.NewEngine(t0)
			if sc.focus {
				e.Focus(apps[0], t0)
			}
			calls, words := watchRun(e, apps, 10*time.Minute, sc.down(apps))
			rs := runs(words)
			if len(rs) != 1 {
				t.Errorf("%s: %d runs of slow or STALE, want one: %v", label, len(rs), rs)
				continue
			}
			start, long := rs[0][0], rs[0][1]
			if long > 120 {
				t.Errorf("%s: slow or STALE for %d s from +%d s, want at most 120", label, long, start)
			}
			stale := 0
			for _, w := range words[start : start+long] {
				if w == "STALE" {
					stale++
				}
			}
			if words[start] != "slow" {
				t.Errorf("%s: the run says %v", label, words[start:start+long])
			}
			end := start + long
			if end >= len(words) {
				t.Errorf("%s: slow or STALE from +%d s to the end", label, start)
				continue
			}
			if words[end] != "back" {
				t.Errorf("%s: at the run's end the title bar says %s", label, words[end])
			}
			for i, w := range words[end+1:] {
				if w != "live" {
					t.Errorf("%s: %s at +%d s after the run", label, w, end+1+i)
					break
				}
			}
			// Every read the box answers at its own pace from then on: the
			// list every minute, the open app every 5 s, each other app
			// every 30 s; whoami, when its route is down, at its backoff
			// (60 s at most).
			want := map[model.Job]time.Duration{list: model.AppsEvery}
			for i, a := range apps {
				every := e.Interval(a)
				if sc.focus && i == 0 {
					every = 60 * time.Second // the read that fails: at its backoff
				}
				want[model.Job{Kind: model.KStatus, App: a}] = every
			}
			if !sc.focus {
				want[me] = 60 * time.Second
			}
			atItsPace(t, label, calls, t0.Add(time.Duration(end+1)*time.Second), want)
			t.Logf("%s: slow or STALE %d s from +%d s (STALE %d s)", label, long, start, stale)
		}
	}
}

// TestRunWithOneJobAnswering is the rule's worst honest case: J jobs
// (whoami, the list, n apps), from +60 s the box answering one read
// alone, the last app's status. The run's retry takes a due job whose
// last read was answered before any whose last read failed, by rank
// and then by when it was due; so at worst the one that answers waits
// for its own turn to come due (30 s) and for each of the J-1 others to
// fail once, a retry each, the retries 4 s, 8, 16, 32, then 60 s apart:
// for 12 apps, J = 14, 630 s. Its
// answer ends the run, which the next two reads that get none begin
// again within the second (the title bar says STALE throughout, as it
// should of a box that answers one read in J); every other job having
// failed since, it is then tried at the first retry at which it is
// due: at least once in its interval (30 s) and the longest gap
// between two retries (60 s), 90 s.
func TestRunWithOneJobAnswering(t *testing.T) {
	for _, n := range []int{1, 3, 12} {
		apps := names(n)
		last := model.Job{Kind: model.KStatus, App: apps[n-1]}
		e := model.NewEngine(t0)
		d := 15 * time.Minute
		calls, words := watchRun(e, apps, d, func(j model.Job, at time.Time) error {
			if at.Sub(t0) < time.Minute || j == last {
				return nil
			}
			return fail(model.FailNoAnswer)
		})
		var reads []time.Time
		for _, c := range calls {
			if c.j == last && c.at.After(t0.Add(time.Minute)) {
				reads = append(reads, c.at)
			}
		}
		// The worst wait for its first read: its own interval, and a retry
		// for each other job.
		first := model.BackgroundEvery
		for k := 2; k <= n+2; k++ {
			first += min(60*time.Second, time.Second<<min(k, 6))
		}
		stale := 0
		for _, w := range words[60:] {
			if w == "STALE" {
				stale++
			}
		}
		if len(reads) == 0 {
			t.Fatalf("%d apps: %s, which the box answers, not read after +60 s", n, last.App)
		}
		gap := time.Duration(0)
		for i := 1; i < len(reads); i++ {
			gap = max(gap, reads[i].Sub(reads[i-1]))
		}
		t.Logf("%d apps: %s first read %v after +60 s (bound %v), then %d times at most %v apart; STALE %d of %d s", n, last.App, reads[0].Sub(t0.Add(time.Minute)), first, len(reads)-1, gap, stale, len(words)-60)
		if reads[0].Sub(t0.Add(time.Minute)) > first || gap > 90*time.Second {
			t.Errorf("%d apps: %s, which the box answers, first read %v after the box stopped answering the rest, then at most %v apart", n, last.App, reads[0].Sub(t0.Add(time.Minute)), gap)
		}
	}
}
