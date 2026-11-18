package tui

import (
	"encoding/json"
	"maps"
	"os"
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

// TestDeployCursorKeepsItsDeploy: when a new deploy lands above the
// one the cursor is on, the cursor stays on its deploy, and enter opens
// that one.
func TestDeployCursorKeepsItsDeploy(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.press(":", "status blog", tcell.KeyEnter, tcell.KeyDown)
	selected := func() string {
		var v string
		h.on(func() {
			if r, _ := h.u.deploys.GetSelection(); r >= 1 && r-1 < len(h.u.deployRows) {
				v = h.u.deployRows[r-1].Version
			}
		})
		return v
	}
	meant := selected()
	if meant == "" {
		t.Fatal("no deploy selected")
	}
	raw, err := os.ReadFile("demo/fixtures/status/blog.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	ds := m["deploys"].([]any)
	newer := maps.Clone(ds[0].(map[string]any))
	newer["version"] = "ffff00001111"
	m["deploys"] = append([]any{newer}, ds...)
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	h.fake.Body("status/blog.json", string(body))
	h.now = h.now.Add(6 * time.Second)
	h.pump()
	if got := selected(); got != meant {
		t.Errorf("the cursor moved from %s to %s", meant, got)
	}
	h.press(tcell.KeyEnter)
	var top model.View
	h.on(func() { top = h.u.top() })
	if top != record("blog", meant) {
		t.Errorf("enter opens %+v, want the record of %s", top, meant)
	}
}
