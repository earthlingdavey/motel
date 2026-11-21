package tui

import (
	"encoding/json"
	"maps"
	"os"
	"strconv"
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

// TestLongNames: two 63-character names that share their start are cut
// in the middle, so NAME leaves STATE, and stale, on a 60- and an
// 80-column screen, and the strip leaves the reason of the one stopped;
// the whole name is what is filtered and opened.
func TestLongNames(t *testing.T) {
	staging := strings.Repeat("a", 55) + "-staging"
	production := strings.Repeat("a", 52) + "-production"
	st, err := os.ReadFile("demo/fixtures/status/blog.json")
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := os.ReadFile("demo/fixtures/status/mailer.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, sz := range [][2]int{{60, 16}, {80, 24}} {
		name := "apps-long-names-" + strconv.Itoa(sz[0]) + "x" + strconv.Itoa(sz[1])
		t.Run(name, func(t *testing.T) {
			h := start(t, sz[0], sz[1], func(f *demo.Fake) {
				f.Body("apps.json", `{"apps": [{"name": "`+staging+`"}, {"name": "`+production+`"}, {"name": "blog"}, {"name": "mailer"}]}`)
				f.Body("status/"+staging+".json", string(stopped))
				f.Body("status/"+production+".json", string(st))
			})
			// production's reads stop answering until its row is stale.
			h.fake.Set(production, &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
			stale := func() bool {
				for _, r := range h.s.AppsView(h.now, "", 0, 0).Rows {
					if r.Name == production {
						return r.Stale
					}
				}
				return false
			}
			for i := 0; !stale() && i < 60; i++ {
				h.now = h.now.Add(5 * time.Second)
				h.pump()
			}
			if !stale() {
				t.Fatal("production is not stale")
			}
			h.sync()
			golden(t, name, h.screen())
			if s := h.screen(); !strings.Contains(s, "\n✖ aaaaaaaaaaaa…staging stopped: 9f02bb1c4e7a is not running") {
				t.Errorf("the strip has no reason for the long name:\n%s", s)
			}
			h.press("/", "-staging", tcell.KeyEnter, tcell.KeyEnter)
			var top model.View
			h.on(func() { top = h.u.top() })
			if top != app(staging) {
				t.Errorf("enter opens %+v, want %s", top, staging)
			}
		})
	}
}

// TestHiddenVersionNoCommand: on the record of a deploy whose version
// the box's filter hid, c and y say that no command can ask for it, and
// P prints no command line: none would work.
func TestHiddenVersionNoCommand(t *testing.T) {
	const hidden = "[hidden by the box's filter]"
	raw, err := os.ReadFile("demo/fixtures/status/shop.json")
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, 100, 30, func(f *demo.Fake) {
		f.Body("status/shop.json", strings.Replace(string(raw), `"version": "e9a0b3f2c118"`, `"version": "`+hidden+`"`, 1))
	})
	h.press(":", "status shop", tcell.KeyEnter)
	selected := func() string {
		var v string
		h.on(func() {
			if r, _ := h.u.deploys.GetSelection(); r >= 1 && r-1 < len(h.u.deployRows) {
				v = h.u.deployRows[r-1].Version
			}
		})
		return v
	}
	for i := 0; selected() != hidden && i < 10; i++ {
		h.press(tcell.KeyDown)
	}
	h.press(tcell.KeyEnter)
	var top model.View
	h.on(func() { top = h.u.top() })
	if top != record("shop", hidden) {
		t.Fatalf("enter opened %+v", top)
	}
	const says = "the box's filter hid this deploy's version: no motel command can ask for its record"
	h.press("c")
	if s := h.screen(); !strings.Contains(s, says) || strings.Contains(s, "motel deploys") {
		t.Fatalf("c on the record shows:\n%s", s)
	}
	h.press("P")
	if h.u.Print != "" {
		t.Fatalf("P prints %q", h.u.Print)
	}
	h.press("y")
	if s := h.screen(); !strings.Contains(s, says) || strings.Contains(s, "motel deploys") {
		t.Fatalf("y on the record shows:\n%s", s)
	}
	h.press("P")
	if h.u.Print != "" {
		t.Fatalf("P on the JSON prints %q", h.u.Print)
	}
}

// TestHiddenVersionSentenceFits: on the record of a deploy whose
// version the box's filter hid, at 60 and 80 columns, c and y show the
// sentence that no command can ask for it whole, laid on as many rows
// as it takes inside the overlay (wrapped, never cut), with no motel
// line and no P in the footer; P prints nothing.
func TestHiddenVersionSentenceFits(t *testing.T) {
	const hidden = "[hidden by the box's filter]"
	const says = "the box's filter hid this deploy's version: no motel command can ask for its record"
	raw, err := os.ReadFile("demo/fixtures/status/shop.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []int{60, 80} {
		h := start(t, w, 24, func(f *demo.Fake) {
			f.Body("status/shop.json", strings.Replace(string(raw), `"version": "e9a0b3f2c118"`, `"version": "`+hidden+`"`, 1))
		})
		h.press(":", "status shop", tcell.KeyEnter)
		selected := func() string {
			var v string
			h.on(func() {
				if r, _ := h.u.deploys.GetSelection(); r >= 1 && r-1 < len(h.u.deployRows) {
					v = h.u.deployRows[r-1].Version
				}
			})
			return v
		}
		for i := 0; selected() != hidden && i < 10; i++ {
			h.press(tcell.KeyDown)
		}
		h.press(tcell.KeyEnter)
		var top model.View
		h.on(func() { top = h.u.top() })
		if top != record("shop", hidden) {
			t.Fatalf("%d columns: enter opened %+v", w, top)
		}
		for _, key := range []string{"c", "y"} {
			h.press(key)
			s := h.screen()
			var inside []string
			for _, l := range strings.Split(s, "\n") {
				if a, b := strings.Index(l, "║"), strings.LastIndex(l, "║"); a >= 0 && b > a {
					inside = append(inside, strings.Fields(l[a+len("║"):b])...)
				}
			}
			if got := strings.Join(inside, " "); got != says {
				t.Errorf("%d columns, %s: the overlay says %q, not the sentence whole:\n%s", w, key, got, s)
			}
			if strings.Contains(s, "motel deploys") || strings.Contains(s, "P quit") {
				t.Errorf("%d columns, %s: a command is offered:\n%s", w, key, s)
			}
			h.press("P")
			if h.u.Print != "" || h.u.Shown != "" {
				t.Errorf("%d columns, %s: P prints %q (%q)", w, key, h.u.Print, h.u.Shown)
			}
			h.press(tcell.KeyEscape)
		}
	}
}

// TestLongNamesAlike: two names the cut would show alike are shown so
// that they differ, on a 60- and an 80-column screen, each row with its
// STATE.
func TestLongNamesAlike(t *testing.T) {
	staging, production := "acme-website-staging-frontend", "acme-website-production-frontend"
	stopped, err := os.ReadFile("demo/fixtures/status/mailer.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []int{60, 80} {
		h := start(t, w, 24, func(f *demo.Fake) {
			f.Body("apps.json", `{"apps": [{"name": "`+staging+`"}, {"name": "`+production+`"}, {"name": "blog"}]}`)
			f.Body("status/"+staging+".json", string(stopped))
			f.Body("status/"+production+".json", string(stopped))
		})
		h.sync()
		scr := h.screen()
		got := rowsShown(scr)
		if len(got) != 3 || got[0] == got[1] {
			t.Errorf("%d columns: the rows show %q:\n%s", w, got, scr)
		}
		for _, l := range strings.Split(scr, "\n") {
			if strings.HasPrefix(l, "✖ acme") && !strings.Contains(l, " stopped") {
				t.Errorf("%d columns: a row without its STATE: %q", w, l)
			}
		}
	}
}

// TestDeployCursorAmongAlike: among deploys that show one version (the
// box's filter hid theirs), the cursor stays on the row it is on as the
// screen is drawn again, not on the last of them.
func TestDeployCursorAmongAlike(t *testing.T) {
	const hidden = "[hidden by the box's filter]"
	raw, err := os.ReadFile("demo/fixtures/status/shop.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	ds := m["deploys"].([]any)
	var alike []any
	for range 3 {
		d := maps.Clone(ds[0].(map[string]any))
		d["version"] = hidden
		alike = append(alike, d)
	}
	m["deploys"] = append(alike, ds[1:]...)
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, 100, 30, func(f *demo.Fake) { f.Body("status/shop.json", string(body)) })
	h.press(":", "status shop", tcell.KeyEnter, tcell.KeyDown)
	row := func() int {
		var r int
		h.on(func() { r, _ = h.u.deploys.GetSelection() })
		return r
	}
	if r := row(); r != 2 {
		t.Fatalf("down from the first deploy: row %d", r)
	}
	h.now = h.now.Add(6 * time.Second)
	h.pump()
	if r := row(); r != 2 {
		t.Errorf("drawn again, the cursor moved from row 2 to row %d", r)
	}
}

// TestTitleHeldWhoAmIFailedFits: the mark of an answer of whoami held
// whose newest read failed is drawn whole beside the box's name and the
// freshness word, at 60 and at 80 columns, as the simulation screen
// shows them; it names whoami, so the failure is not read as the
// release's beside it (the trial: "5m old, failed: v0.8.0-tui1 · …").
// With alerts firing, their mark goes first: at 80 columns the whoami
// mark is still whole after it; at 60 it is cut, as the host is (the
// mark's place, the owner's decision of 2026-11-20).
func TestTitleHeldWhoAmIFailedFits(t *testing.T) {
	quiet := func(f *demo.Fake) { f.Body("box-alerts.json", `{"monitor":"on","alerts":[],"silences":[]}`) }
	for _, c := range []struct {
		w    int
		set  func(*demo.Fake)
		mark string
	}{{60, quiet, ""}, {80, quiet, ""}, {80, nil, "✖ 2 firing · "}} {
		w := c.w
		h := start(t, w, 25, c.set)
		h.fake.Set("whoami", &model.Failure{Kind: model.FailUnusable, Message: "not moteld's answer"})
		h.now = h.now.Add(21 * time.Minute)
		h.s.Engine.Touch(h.now)
		h.s.Engine.Again(h.now, model.Job{Kind: model.KWhoAmI})
		h.pump()
		h.sync()
		top := strings.SplitN(h.screen(), "\n", 2)[0]
		if !strings.HasPrefix(top, c.mark+"motel · deploy.example.com │ whoami failed") || !strings.HasSuffix(top, " live · 0s ago") {
			t.Errorf("at %d columns the title bar is %q", w, top)
		}
		// Whose age it is, after whose failure, where there is room.
		if w == 80 && c.mark == "" && !strings.HasPrefix(top, "motel · deploy.example.com │ whoami failed; 21m old: v0.7.0 · ") {
			t.Errorf("at %d columns the title bar is %q", w, top)
		}
	}
}
