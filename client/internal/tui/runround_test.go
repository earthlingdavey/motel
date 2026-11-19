package tui

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestRunNotHeldByOneRoute, through the screens: whoami's route never
// answers (a 5xx), and the box answers the rest but for one dropped
// connection, the list's first read, which whoami's first failure
// brought forward. That begins a run of no answer at the start: the
// title bar says slow, then STALE. Its retries go round the jobs, so
// they do not all go to whoami, first in rank and due at each: the
// list takes the first retry once it is due again, and its answer ends
// the run (within 2 minutes: the list's 60 s, and a retry gap of 60 s
// at most). The title bar says back, then live, and the list and the
// app are read at their own pace.
func TestRunNotHeldByOneRoute(t *testing.T) {
	blog := model.Job{Kind: model.KStatus, App: "blog"}
	h := start(t, 100, 30, func(f *demo.Fake) {
		f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
		f.Set("whoami", fiveHundred())
		f.Set("apps", fiveHundred())
	})
	h.fake.Set("apps", nil)
	w := h.watch(2*time.Minute, model.Job{})
	if w.words["slow"] == 0 || w.words["STALE"] == 0 || w.words["slow"]+w.words["STALE"] > 120 || w.words["back"] == 0 || w.last != "live" {
		t.Errorf("in the first 2 minutes the title bar's word: %v, at the end %s", w.words, w.last)
	}
	w = h.watch(5*time.Minute, model.Job{})
	if w.words["live"] != 5*60 || w.kinds[model.KApps] < 4 || w.jobs[blog] < 9 {
		t.Errorf("in the 5 minutes after: the title bar's word %v, the list read %d times, blog %d", w.words, w.kinds[model.KApps], w.jobs[blog])
	}
}
