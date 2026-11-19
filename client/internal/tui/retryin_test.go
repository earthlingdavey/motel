package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestRetryInIsTheNextRead: through an outage on the apps screen and
// on the box screen (one app; the list, its status, whoami, the box's
// status, its backups and its alerts all answering 500), the title bar's `retry in` counts
// down, second by second, to the next read the engine really makes:
// not to the run's retry alone, which can pass long before any read is
// due (the list's 60 s, the box's 30 s), the title bar then saying
// `retry in 0s` for as long. The harness's reads take no time, and each
// is made in the second it comes due, before the frame, so a 0s drawn
// here is counted wrong. On a box whose reads take time, 0s is drawn
// while the retry is in flight (up to the client's 30 s timeout on a
// host that does not answer), which is so: it is being tried.
func TestRetryInIsTheNextRead(t *testing.T) {
	for name, scr := range map[string]model.Screen{"the apps screen": model.ScreenApps, "the box screen": model.ScreenBox} {
		h := start(t, 120, 30, func(f *demo.Fake) { f.Body("apps.json", `{"apps": [{"name": "blog"}]}`) })
		h.open(model.View{Screen: scr})
		h.watch(61*time.Second, model.Job{})
		for _, k := range []string{"blog", "apps", "whoami", "box", "backups", "box-alerts"} {
			h.fake.Set(k, fiveHundred())
		}
		type said struct {
			at time.Time
			in time.Duration
		}
		var says []said
		before := len(h.fake.Calls)
		for range 150 {
			h.watch(time.Second, model.Job{})
			title, _, _ := strings.Cut(h.screen(), "\n")
			_, rest, ok := strings.Cut(title, "retry in ")
			if !ok {
				continue
			}
			n, _, _ := strings.Cut(rest, "s")
			secs, err := strconv.Atoi(n)
			if err != nil {
				if strings.HasPrefix(rest, "1m") {
					secs = 60
				} else {
					t.Fatalf("%s: the title bar says %q", name, title)
				}
			}
			says = append(says, said{h.now, time.Duration(secs) * time.Second})
		}
		calls := h.fake.Calls[before:]
		if len(says) < 100 {
			t.Fatalf("%s: `retry in` said %d of 150 seconds", name, len(says))
		}
		wrong := 0
		for _, s := range says {
			var next time.Time
			for _, c := range calls {
				if c.At.After(s.at) {
					next = c.At
					break
				}
			}
			if next.IsZero() {
				continue // after the 150 s watched
			}
			if d := next.Sub(s.at); s.in == 0 || d < s.in || d >= s.in+time.Second && s.in < time.Minute {
				if wrong++; wrong <= 5 {
					t.Errorf("%s: at +%v the title bar says retry in %v; the next read was %v later", name, s.at.Sub(t0), s.in, d)
				}
			}
		}
		if wrong > 5 {
			t.Errorf("%s: %d seconds of %d wrong in all", name, wrong, len(says))
		}
	}
}
