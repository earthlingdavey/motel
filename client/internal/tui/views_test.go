package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestListFailureShown: once a list is held, a later read of it that
// fails is said above the strip, with the age of the list held.
func TestListFailureShown(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("apps", &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com closed the connection"})
	h.now = h.now.Add(61 * time.Second)
	h.pump()
	scr := h.screen()
	if !strings.Contains(scr, "list as of 14:12:40 UTC, 1m ago · the last read failed: deploy.example.com closed the connection") {
		t.Errorf("a failed read of a held list is not said:\n%s", scr)
	}
}

// TestFirstDeployFailed: an app whose first deploy failed has no
// version, and its screen shows the failed deploy and its error.
func TestFirstDeployFailed(t *testing.T) {
	failed := `{"version": "e9a0b3f2c118", "status": "failed", "deployed_by": "github:example/wiki@refs/heads/main", "started_at": "@-782s", "finished_at": "@-12m", "phase": "starting"}`
	h := start(t, 100, 30, func(f *demo.Fake) {
		f.Body("status/wiki.json", `{"app": "wiki", "phase": "idle", "running": false, "available_versions": [], "deploys": [`+failed+`],
			"last_deploy": {"version": "e9a0b3f2c118", "status": "failed", "deployed_by": "github:example/wiki@refs/heads/main", "started_at": "@-782s", "finished_at": "@-12m", "phase": "starting", "error": "the app exited before it listened"}}`)
	})
	h.press(":", "status wiki", tcell.KeyEnter)
	scr := h.screen()
	for _, want := range []string{"wiki has no version yet: no deploy of it serves.", "error       the app exited before it listened", "▲ e9a0b3f2c118 failed"} {
		if !strings.Contains(scr, want) {
			t.Errorf("no %q on the screen:\n%s", want, scr)
		}
	}
}
