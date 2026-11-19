package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// fiveHundred is how the TUI's client hands the engine a 5xx: no answer.
func fiveHundred() error {
	return &model.Failure{Kind: model.FailNoAnswer, Message: `Something at deploy.example.com answered 500, not known to be moteld's own answer; it says: "cannot read it"`}
}

// watched is what the title bar said, second by second, and what was
// read, while a test let the clock run.
type watched struct {
	words map[string]int // the title bar's word: live, slow, back, STALE
	last  string         // its word at the end
	reads []time.Time    // when each read of the job watched was made
	kinds map[model.Kind]int
	jobs  map[model.Job]int
}

// watch lets the clock run for d, a second at a time, a key pressed at
// each so that polling never pauses, making every read due; it reads
// the title bar's word off the screen each second, from a frame drawn
// once that second's reads are applied, and counts the reads made of
// each kind, and when j was read. No frame is drawn between: none is
// read.
func (h *harness) watch(d time.Duration, j model.Job) watched {
	return h.letRun(d, j, true)
}

// walk is watch for a test that reads no screen while the clock runs:
// the same reads at the same seconds, each answer applied on the event
// loop, and one frame drawn at the end, when the title bar's word is
// read once, as last (words is left empty).
func (h *harness) walk(d time.Duration, j model.Job) watched {
	return h.letRun(d, j, false)
}

func (h *harness) letRun(d time.Duration, j model.Job, each bool) watched {
	w := watched{words: map[string]int{}, kinds: map[model.Kind]int{}, jobs: map[model.Job]int{}}
	before := len(h.fake.Calls)
	for end := h.now.Add(d); h.now.Before(end); {
		h.now = h.now.Add(time.Second)
		h.loop(func() { h.u.S.Engine.Touch(h.now) })
		h.pumpUnseen()
		if each {
			h.sync()
			w.last = h.titleWord()
			w.words[w.last]++
		}
	}
	if !each {
		h.sync()
		w.last = h.titleWord()
	}
	for _, c := range h.fake.Calls[before:] {
		w.kinds[c.Job.Kind]++
		w.jobs[c.Job]++
		if c.Job == j {
			w.reads = append(w.reads, c.At)
		}
	}
	return w
}

// titleWord is the title bar's freshness word on screen now: live,
// slow, back, STALE, or ? for none.
func (h *harness) titleWord() string {
	title, _, _ := strings.Cut(h.screen(), "\n")
	word := "?"
	for _, x := range []string{"live ·", "slow ·", "back ·", "STALE ·"} {
		if strings.Contains(title, x) {
			word = strings.TrimSuffix(x, " ·")
		}
	}
	return word
}

// backsOff says whether the reads at ts come further apart, to 60 s.
func backsOff(t *testing.T, ts []time.Time) {
	t.Helper()
	if len(ts) < 4 {
		t.Fatalf("read %d times", len(ts))
	}
	for i := 2; i < len(ts); i++ {
		gap, was := ts[i].Sub(ts[i-1]), ts[i-1].Sub(ts[i-2])
		if gap < was || gap > model.FocusEvery+60*time.Second {
			t.Errorf("read %v apart after %v: %v", gap, was, ts)
			return
		}
	}
	if last := ts[len(ts)-1].Sub(ts[len(ts)-2]); last < 59*time.Second {
		t.Errorf("read %v apart at the end, not backed off to 60 s", last)
	}
}

// TestOneRouteDoesNotSlowTheRest: one read the box does not answer (a
// 5xx on its route: an app's status, open or not, the box's status, or
// whoami's) while the rest are answered backs off alone: the title bar
// says neither slow nor back, whoami is not asked again for it (but at
// its own turn, or, when it is the read that fails, at its backoff from
// then), and the screen says the read failed.
func TestOneRouteDoesNotSlowTheRest(t *testing.T) {
	blog := model.Job{Kind: model.KStatus, App: "blog"}
	for _, c := range []struct {
		name  string
		key   string
		job   model.Job
		open  func(h *harness)
		says  string
		reads int // most whoami reads in the 9 minutes: by its own turn, then its backoff when it fails
	}{
		{"an app's status, open", "blog", blog, func(h *harness) { h.open(app("blog")) }, "the last read failed", 2},
		{"an app's status, not open", "blog", blog, func(*harness) {}, "stale", 2},
		{"the box's status, on the box screen", "box", model.Job{Kind: model.KBox}, func(h *harness) { h.press(":", "box", tcell.KeyEnter) }, "answered 500", 2},
		// At 5 minutes, then 2 s, 4, 8, 16, 32 and 60 s apart.
		{"whoami", "whoami", model.Job{Kind: model.KWhoAmI}, func(*harness) {}, "failed", 9},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, 100, 30, nil)
			h.fake.Set(c.key, fiveHundred())
			c.open(h)
			w := h.watch(9*time.Minute, c.job)
			if w.words["slow"]+w.words["back"]+w.words["STALE"] != 0 || w.words["?"] != 0 {
				t.Errorf("the title bar's word, second by second: %v", w.words)
			}
			if w.kinds[model.KWhoAmI] > c.reads {
				t.Errorf("whoami read %d times in 9 minutes", w.kinds[model.KWhoAmI])
			}
			// Every other app is read at its turn, every 30 s.
			for _, a := range h.s.Names() {
				if j := (model.Job{Kind: model.KStatus, App: a}); j != c.job && w.jobs[j] < 17 {
					t.Errorf("%s was slowed: read %d times in 9 minutes", a, w.jobs[j])
				}
			}
			backsOff(t, w.reads)
			if scr := h.screen(); !strings.Contains(scr, c.says) {
				t.Errorf("the screen does not say %q:\n%s", c.says, scr)
			}
		})
	}
}

// TestOutageBacksEveryReadOff: when nothing answers, every read backs off
// from a few seconds to 60 s, and the title bar says slow, then STALE;
// when everything answers again it says back, and whoami is asked once.
func TestOutageBacksEveryReadOff(t *testing.T) {
	h := start(t, 100, 30, nil)
	keys := append(h.s.Names(), "apps", "whoami")
	for _, k := range keys {
		h.fake.Set(k, fiveHundred())
	}
	before := len(h.fake.Calls)
	w := h.watch(5*time.Minute, model.Job{})
	var at []time.Time
	for _, c := range h.fake.Calls[before:] {
		at = append(at, c.At)
	}
	backsOff(t, at[1:]) // the first read's own backoff is its own
	if w.words["slow"] == 0 || w.words["STALE"] == 0 || w.words["back"] != 0 {
		t.Errorf("the title bar's word, second by second: %v", w.words)
	}
	for _, k := range keys {
		h.fake.Set(k, nil)
	}
	// The next read is made when the wait ends, at most 60 s on.
	w = h.watch(time.Minute+5*time.Second, model.Job{})
	if w.words["back"] == 0 || w.last != "live" {
		t.Errorf("after the box answers, the title bar's word: %v, at the end %s", w.words, w.last)
	}
	if w.kinds[model.KWhoAmI] != 1 {
		t.Errorf("whoami read %d times after the box answered again", w.kinds[model.KWhoAmI])
	}
}
