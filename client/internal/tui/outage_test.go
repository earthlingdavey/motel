package tui

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestOneAppOutageSeen: a person with one app, on its screen, the list
// read a second before the box stops answering, so the list is not due
// for 59 s. The first read with no answer brings the list's forward;
// it gets none too, and the title bar says slow, then STALE, within
// seconds, not live until the list's turn. When the box answers again
// it says back, then live, and whoami is asked once.
func TestOneAppOutageSeen(t *testing.T) {
	h := start(t, 100, 30, func(f *demo.Fake) { f.Body("apps.json", `{"apps": [{"name": "blog"}]}`) })
	h.watch(61*time.Second, model.Job{})
	keys := []string{"blog", "apps", "whoami"}
	for _, k := range keys {
		h.fake.Set(k, fiveHundred())
	}
	w := h.watch(10*time.Second, model.Job{})
	if w.words["live"] > 5 || w.last != "STALE" {
		t.Errorf("the title bar's word in the outage's first 10 s: %v, at the end %s", w.words, w.last)
	}
	w = h.watch(3*time.Minute, model.Job{})
	if w.words["STALE"] != 3*60 {
		t.Errorf("the title bar's word for the next 3 minutes: %v", w.words)
	}
	for _, k := range keys {
		h.fake.Set(k, nil)
	}
	w = h.watch(time.Minute+5*time.Second, model.Job{})
	if w.words["back"] == 0 || w.last != "live" || w.kinds[model.KWhoAmI] != 1 {
		t.Errorf("after the box answers again: %v, at the end %s, whoami read %d times", w.words, w.last, w.kinds[model.KWhoAmI])
	}
}

// TestOneRouteProbesOnce: one app's status the box does not answer,
// while it answers the rest, adds at most one read to the rest in 9
// minutes: the one brought forward when the first read got no answer.
func TestOneRouteProbesOnce(t *testing.T) {
	blog := model.Job{Kind: model.KStatus, App: "blog"}
	others := func(fail bool) int {
		h := start(t, 100, 30, nil)
		if fail {
			h.fake.Set("blog", fiveHundred())
		}
		w := h.watch(9*time.Minute, blog)
		n := 0
		for j, c := range w.jobs {
			if j != blog {
				n += c
			}
		}
		return n
	}
	if base, got := others(false), others(true); got < base || got > base+1 {
		t.Errorf("the other reads in 9 minutes: %d with blog answered, %d with blog getting no answer", base, got)
	}
}
