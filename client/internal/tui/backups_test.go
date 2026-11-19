package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestBackupsKeptAfterAFailedReadSaysSo: at 80x24, where the failure's
// own line under the box's lines is below the fold, a backups answer
// kept after a read that got no answer still shows the box's verdict,
// and the crumb at the top says the last read failed.
func TestBackupsKeptAfterAFailedReadSaysSo(t *testing.T) {
	h := start(t, 80, 24, nil)
	h.press(":", "backups", tcell.KeyEnter)
	h.pump()
	if s := h.screen(); !strings.Contains(s, "not healthy, the box says") || strings.Contains(s, "the last read failed") {
		t.Fatalf("the backups screen before the failure:\n%s", s)
	}
	h.fake.Set("backups", &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
	h.now = h.now.Add(model.BoxEvery + time.Second)
	h.pump()
	s := h.screen()
	rows := strings.Split(s, "\n")
	if len(rows) < 3 || !strings.HasPrefix(rows[1], ":backups") || !strings.Contains(rows[1], "the last read failed") || !strings.Contains(s, "not healthy, the box says") {
		t.Errorf("after a read that got no answer, the screen:\n%s", s)
	}
}
