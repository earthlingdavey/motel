package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestWhoAmIMissedAtStart: whoami's first read gets no answer (one
// dropped connection) and the box then answers every read: within 3 s
// the title bar names who you are. With whoami's route alone never
// answering, the title bar says who you are is not known and live,
// every second of 3 minutes, and never slow, back or STALE.
func TestWhoAmIMissedAtStart(t *testing.T) {
	dropped := &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com closed the connection on 443 before a whole answer came."}
	h := start(t, 100, 30, func(f *demo.Fake) { f.Set("whoami", dropped) })
	h.fake.Set("whoami", nil)
	h.watch(3*time.Second, model.Job{Kind: model.KWhoAmI})
	if title, _, _ := strings.Cut(h.screen(), "\n"); !strings.Contains(title, "alice@example.com") {
		t.Errorf("3 s after whoami got no answer, the title bar says %q", title)
	}

	h = start(t, 100, 30, func(f *demo.Fake) { f.Set("whoami", fiveHundred()) })
	before := len(h.fake.Calls)
	for range 180 {
		h.watch(time.Second, model.Job{Kind: model.KWhoAmI})
		title, _, _ := strings.Cut(h.screen(), "\n")
		if !strings.Contains(title, "who you are is not known: ") || !strings.Contains(title, " live · ") {
			t.Fatalf("at %v the title bar says %q", h.now.Sub(t0), title)
		}
	}
	me := 0
	for _, c := range h.fake.Calls[before:] {
		if c.Job.Kind == model.KWhoAmI {
			me++
		}
	}
	// 2 s, 4, 8, 16, 32 and 60 s apart: at 2, 6, 14, 30, 62, 122 and 182 s.
	if me != 6 {
		t.Errorf("whoami asked %d times in 3 minutes", me)
	}
}
