package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestEscForgetsARecordNotRead: a record left with esc before any read
// of it was made is not asked of the box: its read is not wanted.
func TestEscForgetsARecordNotRead(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.open(app("blog"))
	before := len(h.fake.Calls)
	h.on(func() { h.u.open(record("blog", "e9a0b3f2c118")) }) // enter, and no read made yet
	h.press(tcell.KeyEscape)
	if h.top() != app("blog") {
		t.Fatalf("esc from the record: %+v", h.top())
	}
	for _, c := range h.fake.Calls[before:] {
		if c.Job.Kind == model.KDeploy {
			t.Errorf("the record left before its read was read after: %v", c.Job)
		}
	}
}

// TestStopScreenStaysUntilAnswered: after a refused key and r, the
// stop screen stays, saying why every read stopped, while no read is
// answered (whoami gets no answer), and leaves once whoami answers.
func TestStopScreenStaysUntilAnswered(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("whoami", &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com did not accept your key"})
	h.now = h.now.Add(model.MeAlways)
	h.pump()
	if h.top().Screen != model.ScreenRefused {
		t.Fatalf("not stopped: %+v", h.top())
	}
	h.fake.Set("whoami", &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com did not answer"})
	h.press("r")
	if h.top().Screen != model.ScreenRefused || !strings.Contains(h.screen(), "did not accept your key") {
		t.Fatalf("after r, whoami with no answer: on %+v:\n%s", h.top(), h.screen())
	}
	h.fake.Set("whoami", nil)
	h.now = h.now.Add(model.MeEvery) // past the backoff
	h.pump()
	h.sync()
	if h.top().Screen == model.ScreenRefused {
		t.Fatalf("whoami answered and the stop screen stays:\n%s", h.screen())
	}
}
