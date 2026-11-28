package tui

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

func rowsShown(scr string) []string {
	var names []string
	in := false
	for _, l := range strings.Split(scr, "\n") {
		if strings.Contains(l, "NAME") && strings.Contains(l, "STATE") {
			in = true
			continue
		}
		if r := []rune(l); in && len(r) > 2 && !strings.HasPrefix(l, "enter") {
			if f := strings.Fields(string(r[2:])); len(f) > 0 {
				names = append(names, f[0])
			}
		}
	}
	return names
}

// TestFilterShownAgain: / pressed again on a filtered list shows the
// filter for editing, and the list stays filtered until it changes.
func TestFilterShownAgain(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.press("/", "sho", tcell.KeyEnter)
	if got := rowsShown(h.screen()); len(got) != 1 || got[0] != "shop" {
		t.Fatalf("filter sho shows %v", got)
	}
	h.press("/")
	var typed, filter string
	h.on(func() { typed, filter = h.u.prompt.GetText(), h.u.filter })
	if got := rowsShown(h.screen()); typed != "sho" || filter != "sho" || len(got) != 1 {
		t.Fatalf("/ again: the prompt holds %q, the filter is %q, rows %v", typed, filter, got)
	}
	h.press(tcell.KeyBackspace2, tcell.KeyEnter)
	h.on(func() { filter = h.u.filter })
	if got := rowsShown(h.screen()); filter != "sh" || !slices.Contains(got, "shop") {
		t.Fatalf("the filter edited to %q shows %v", filter, got)
	}
}

// TestStepWalksEveryRow: four deploys. p from the newest walks every
// row to the oldest, the deploys table's cursor with it, each record
// read as it is arrived at; n walks back; esc returns to the row
// stepped to.
func TestStepWalksEveryRow(t *testing.T) {
	raw, err := os.ReadFile("demo/fixtures/status/shop.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	ds := m["deploys"].([]any)
	named := func(v string) any {
		d := maps.Clone(ds[1].(map[string]any))
		d["version"] = v
		return d
	}
	m["deploys"] = []any{ds[0], named("v2"), named("v1"), ds[1]}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, 100, 30, func(f *demo.Fake) { f.Body("status/shop.json", string(body)) })
	h.press(":", "status shop", tcell.KeyEnter, tcell.KeyEnter) // the newest deploy's record
	versions := []string{"e9a0b3f2c118", "v2", "v1", "7c1e55d0a913"}
	at := func(want int, key string) {
		t.Helper()
		var v model.View
		var row int
		h.on(func() { v = h.u.top(); row, _ = h.u.deploys.GetSelection() })
		if v.Screen != model.ScreenRecord || v.Version != versions[want] || row != want+1 {
			t.Fatalf("%s: on %+v, the table's row %d; want the record of row %d, %q", key, v, row, want+1, versions[want])
		}
	}
	read := func(key string, want int, step func()) {
		t.Helper()
		before := len(h.fake.Calls)
		step()
		h.now = h.now.Add(model.RecordAgainAfter)
		h.pump()
		at(want, key)
		for _, c := range h.fake.Calls[before:] {
			if c.Job == (model.Job{Kind: model.KDeploy, App: "shop", Version: versions[want]}) {
				return
			}
		}
		t.Fatalf("%s to row %d: its record was not read", key, want+1)
	}
	at(0, "enter")
	for i := 1; i < len(versions); i++ {
		read("p", i, func() { h.press("p") })
	}
	h.press("p")
	at(len(versions)-1, "p on the oldest")
	for i := len(versions) - 2; i >= 0; i-- {
		read("n", i, func() { h.press("n") })
	}
	h.press("p", "p", tcell.KeyEscape)
	var row int
	h.on(func() { row, _ = h.u.deploys.GetSelection() })
	if v := h.top(); v.Screen != model.ScreenApp || row != 3 {
		t.Fatalf("esc after p p is %+v on row %d, want the app on row 3", v, row)
	}
}

// TestStepAfterANewDeploy: a status read that lands a new deploy while
// a record is open moves every row down one. p or n then steps by the
// rows as they are now, and esc must return to the row of the record
// left, as the screen shows it: enter there opens that record again.
// With no new deploy, the same.
func TestStepAfterANewDeploy(t *testing.T) {
	raw, err := os.ReadFile("demo/fixtures/status/shop.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	d0 := m["deploys"].([]any)[1].(map[string]any)
	body := func(vs ...string) string {
		var ds []any
		for i, v := range vs {
			d := maps.Clone(d0)
			d["version"], d["started_at"] = v, "@-"+itoa(100*(i+1))+"s"
			ds = append(ds, d)
		}
		m["deploys"] = ds
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const v4, v3, v2, v1 = "dddd00000004", "cccc00000003", "bbbb00000002", "aaaa00000001"
	for _, c := range []struct {
		name  string
		down  []any // from the table's first row to the record opened
		from  string
		newer bool
		key   string
		to    string
	}{
		{"p after a new deploy", nil, v3, true, "p", v2},
		{"n after a new deploy", []any{"j", "j"}, v1, true, "n", v2},
		{"p p after a new deploy", nil, v3, true, "pp", v1}, // a row the table was not drawn with
		{"p, no new deploy", nil, v3, false, "p", v2},
		{"n, no new deploy", []any{"j", "j"}, v1, false, "n", v2},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, 100, 30, func(f *demo.Fake) { f.Body("status/shop.json", body(v3, v2, v1)) })
			h.open(app("shop"))
			h.press(append(c.down, tcell.KeyEnter)...)
			if v := h.top(); v.Screen != model.ScreenRecord || v.Version != c.from {
				t.Fatalf("opened %+v, want the record of %s", v, c.from)
			}
			if c.newer {
				h.fake.Body("status/shop.json", body(v4, v3, v2, v1))
				before := len(h.fake.Calls)
				h.now = h.now.Add(model.FocusEvery)
				h.pump()
				if !slices.ContainsFunc(h.fake.Calls[before:], func(c demo.Call) bool {
					return c.Job == model.Job{Kind: model.KStatus, App: "shop"}
				}) {
					t.Fatal("no status read while the record was open")
				}
			}
			h.press(c.key)
			if v := h.top(); v.Version != c.to {
				t.Fatalf("%s opened %+v, want %s", c.key, v, c.to)
			}
			h.press(tcell.KeyEscape)
			if got := cursorOn(h, v4, v3, v2, v1); got != c.to {
				t.Fatalf("esc after %s: the screen's cursor is on %q, want %q\n%s", c.key, got, c.to, h.screen())
			}
			h.press(tcell.KeyEnter)
			if v := h.top(); v.Screen != model.ScreenRecord || v.Version != c.to {
				t.Fatalf("enter after esc opened %+v, want the record of %s", v, c.to)
			}
		})
	}
}

// TestBackToTheRecordLeft: d and the : line open an app's screen and
// a record in one step, so the app's screen is never drawn before the
// record is in front, and the deploys table still holds another app's
// rows and cursor, or none. esc from the record must put the cursor on
// the row of the record left, as the screen shows it: enter there
// opens that record again.
func TestBackToTheRecordLeft(t *testing.T) {
	raw, err := os.ReadFile("demo/fixtures/status/shop.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// Every deploy failed, so d opens the newest's record; no version
	// is one of blog's.
	d0 := m["deploys"].([]any)[0].(map[string]any)
	const v4, v3, v2, v1 = "dddd00000004", "cccc00000003", "bbbb00000002", "aaaa00000001"
	var ds []any
	for i, v := range []string{v4, v3, v2, v1} {
		d := maps.Clone(d0)
		d["version"], d["started_at"] = v, "@-"+itoa(100*(i+1))+"s"
		ds = append(ds, d)
	}
	m["deploys"] = ds
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		blog []any // blog's screen first, its cursor moved by these; nil: no app's screen opened
		keys []any // from the apps screen to shop's record
		to   string
	}{
		{"d after blog on row 3", []any{"j", "j"}, []any{"/", "shop", tcell.KeyEnter, "d"}, v4},
		{"d, no app opened before", nil, []any{"/", "shop", tcell.KeyEnter, "d"}, v4},
		{": after blog on row 1", []any{}, []any{":", "deploys shop " + v2, tcell.KeyEnter}, v2},
		{": after blog on row 4", []any{"j", "j", "j"}, []any{":", "deploys shop " + v2, tcell.KeyEnter}, v2},
		{": no app opened before", nil, []any{":", "deploys shop " + v2, tcell.KeyEnter}, v2},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, 100, 30, func(f *demo.Fake) { f.Body("status/shop.json", string(body)) })
			if c.blog != nil {
				h.open(app("blog"))
				h.press(append(c.blog, tcell.KeyEscape)...)
			}
			h.press(c.keys...)
			if v := h.top(); v.Screen != model.ScreenRecord || v.App != "shop" || v.Version != c.to {
				t.Fatalf("opened %+v, want shop's record of %s", v, c.to)
			}
			h.press(tcell.KeyEscape)
			if v := h.top(); v.Screen != model.ScreenApp || v.App != "shop" {
				t.Fatalf("esc went to %+v, want shop's screen", v)
			}
			if got := cursorOn(h, v4, v3, v2, v1); got != c.to {
				t.Fatalf("esc: the screen's cursor is on %q, want %q\n%s", got, c.to, h.screen())
			}
			h.press(tcell.KeyEnter)
			if v := h.top(); v.Screen != model.ScreenRecord || v.Version != c.to {
				t.Fatalf("enter after esc opened %+v, want the record of %s", v, c.to)
			}
		})
	}
}

// cursorOn is the one of versions on the screen's reversed row: the
// table's cursor, as drawn.
func cursorOn(h *harness, versions ...string) string {
	h.t.Helper()
	var got []string
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() {
		defer close(done)
		cells, w, hgt := h.scr.(tcell.SimulationScreen).GetContents()
		for y := range hgt {
			var l strings.Builder
			rev := false
			for x := range w {
				c := cells[y*w+x]
				l.WriteString(string(c.Runes))
				_, _, a := c.Style.Decompose()
				rev = rev || a&tcell.AttrReverse != 0
			}
			for _, v := range versions {
				if rev && strings.Contains(l.String(), v) {
					got = append(got, v)
				}
			}
		}
	})
	<-done
	if len(got) != 1 {
		h.t.Fatalf("reversed rows hold %v, want one version", got)
	}
	return got[0]
}

// TestKeys walks the screens by key: filter, open, back, :, help and
// the overlay, each landing where the footer says.
func TestKeys(t *testing.T) {
	h := start(t, 100, 30, nil)
	top := func() model.View {
		var v model.View
		h.on(func() { v = h.u.top() })
		return v
	}

	h.press("/", "sho")
	if got := rowsShown(h.screen()); len(got) != 1 || got[0] != "shop" {
		t.Fatalf("filter sho shows %v", got)
	}
	h.press(tcell.KeyEnter)
	h.press(tcell.KeyEnter) // the table: open the one row
	if v := top(); v.Screen != model.ScreenApp || v.App != "shop" {
		t.Fatalf("enter opened %+v", v)
	}
	h.press(tcell.KeyEnter) // the newest deploy's record
	if v := top(); v.Screen != model.ScreenRecord || v.Version != "e9a0b3f2c118" {
		t.Fatalf("enter on a deploy opened %+v", v)
	}
	if s := h.screen(); !strings.Contains(s, "✖ FAILED in soaking") || !strings.Contains(s, "log tail: 40 lines") {
		t.Fatalf("the record is not on screen:\n%s", s)
	}
	h.press("p")
	if v := top(); v.Version != "7c1e55d0a913" {
		t.Fatalf("p opened %+v", v)
	}
	h.press(tcell.KeyEscape, tcell.KeyEscape)
	if v := top(); v.Screen != model.ScreenApps {
		t.Fatalf("esc esc is %+v", v)
	}
	if got := rowsShown(h.screen()); len(got) != 1 {
		t.Fatalf("the filter did not stay: %v", got)
	}
	h.press("/", tcell.KeyEscape)
	if got := rowsShown(h.screen()); len(got) != 12 {
		t.Fatalf("esc on the filter left %d rows", len(got))
	}
	h.press("/", "!ok", tcell.KeyEnter)
	if got := rowsShown(h.screen()); len(got) != 5 {
		t.Fatalf("/!ok shows %v, want the five marked", got)
	}
	h.press("/", tcell.KeyEscape)
	h.press(tcell.KeyHome, "j", "j", "d") // the third row, hostile: its newest deploy failed
	if v := top(); v.Screen != model.ScreenRecord || v.App != "hostile" || v.Version != "v7" {
		t.Fatalf("d opened %+v", v)
	}
	h.press(tcell.KeyEscape, tcell.KeyEscape)

	h.press(":", "deploys blog e9a0b3f2c118", tcell.KeyEnter)
	if v := top(); v.Screen != model.ScreenRecord || v.App != "blog" {
		t.Fatalf(":deploys opened %+v", v)
	}
	h.press(tcell.KeyEscape)
	if v := top(); v.Screen != model.ScreenApp || v.App != "blog" {
		t.Fatalf("esc from :deploys is %+v", v)
	}
	h.press("?")
	if s := h.screen(); !strings.Contains(s, "KEYS OF THE SCREEN BELOW") || !strings.Contains(s, "open the deploy's record") {
		t.Fatalf("? on the app is not its help:\n%s", s)
	}
	h.press(tcell.KeyEscape)
	h.press("c")
	if s := h.screen(); !strings.Contains(s, "motel status --box deploy.example.com -- blog") {
		t.Fatalf("c shows no command:\n%s", s)
	}
	h.press("y")
	if s := h.screen(); !strings.Contains(s, `"app": "blog"`) {
		t.Fatalf("y shows no JSON:\n%s", s)
	}
	h.press(tcell.KeyEscape)
	if v := top(); v.Screen != model.ScreenApp {
		t.Fatalf("esc closing the overlay left %+v", v)
	}
	if s := h.screen(); strings.Contains(s, "the box's answer") {
		t.Fatalf("the overlay is still up")
	}
	h.press(":", "me", tcell.KeyEnter)
	if s := h.screen(); !strings.Contains(s, "you are     alice@example.com") {
		t.Fatalf(":me shows:\n%s", s)
	}
	h.press(tcell.KeyEscape, tcell.KeyEscape, tcell.KeyEscape)
	if v := top(); v.Screen != model.ScreenApps {
		t.Fatalf("esc never leaves the first screen; it is %+v", v)
	}
	h.press("s")
	if s := h.screen(); !strings.Contains(s, "by name") {
		t.Fatalf("s did not sort by name")
	}
	h.quit('q')
}

// TestPrintOnExit: P on the command overlay quits and leaves the line
// to print below the prompt.
func TestPrintOnExit(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.press(":", "status blog", tcell.KeyEnter, "c")
	h.quit('P')
	if h.u.Print != "motel status --box deploy.example.com -- blog\n" {
		t.Fatalf("P leaves %q", h.u.Print)
	}
}

// TestLongCommandCut: a command wider than the overlay is cut at its
// edge with the cut marked, and the overlay says not to copy it and
// that P prints it whole, in place of asking to select it; P prints
// it whole. Where it fits, it is drawn whole and selecting it is
// offered.
func TestLongCommandCut(t *testing.T) {
	const a, ver = "billing-reconciliation-worker-eu-west", "release-2026.11.18-build.4471-rc10"
	want := "motel deploys --box deploy.example.com -- " + a + " " + ver
	for _, c := range []struct {
		w   int
		cut bool
	}{{80, true}, {130, false}} {
		h := start(t, c.w, 24, nil)
		h.open(record(a, ver))
		h.press("c")
		s := h.screen()
		told := strings.Contains(s, "a line above is cut at the window's edge, marked …: do not copy it. P quits") &&
			strings.Contains(s, "and prints the first command whole below your prompt")
		offered := strings.Contains(s, "select it with the mouse")
		switch {
		case c.cut && (!told || offered || strings.Contains(s, want) || !strings.Contains(s, "  motel deploys --box deploy.example.com -- billing-reconciliation-worker-eu-…║")):
			t.Errorf("%d columns, the command cut: told %v, selecting offered %v:\n%s", c.w, told, offered, s)
		case !c.cut && (told || !offered || !strings.Contains(s, "  "+want+" ") || !strings.Contains(s, "--json -- "+a+" "+ver+" ")):
			t.Errorf("%d columns, the command whole: told %v, selecting offered %v:\n%s", c.w, told, offered, s)
		}
		h.quit('P')
		if h.u.Print != want+"\n" {
			t.Errorf("%d columns: P leaves %q", c.w, h.u.Print)
		}
	}
}

// TestTooSmall: below 60x16 the screen says what it needs.
func TestTooSmall(t *testing.T) {
	h := start(t, 59, 20, nil)
	if s := h.screen(); !strings.Contains(s, "needs 60x16") {
		t.Fatalf("too small a screen shows:\n%s", s)
	}
}

// TestRecoverWithR: a refused key stops every read and says so; once
// the box takes the key again, r asks once and the reads carry on.
func TestRecoverWithR(t *testing.T) {
	h := start(t, 100, 30, nil)
	refused := &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com did not accept your key"}
	h.fake.Set("whoami", refused)
	h.fake.Set("blog", refused)
	h.press(tcell.KeyCtrlR)
	s := h.screen()
	if !strings.Contains(s, "SIGNED OUT") || !strings.Contains(s, "deploy.example.com did not accept your key") {
		t.Fatalf("a 401 is not said:\n%s", s)
	}
	h.press("r") // still refused: stopped again after one whoami
	if h.s.Engine.Stopped() == model.Running {
		t.Fatal("r with the key still refused left the reads running")
	}
	h.fake.Set("whoami", nil)
	h.fake.Set("blog", nil)
	// Held: r is taken at most once in RetryEvery, each a signature.
	before := len(h.fake.Calls)
	h.press("r", "r", "r")
	if h.s.Engine.Stopped() == model.Running || len(h.fake.Calls) != before {
		t.Fatalf("r within %s of the last asked again (%d reads)", model.RetryEvery, len(h.fake.Calls)-before)
	}
	h.now = h.now.Add(model.RetryEvery)
	resigned := 0
	h.u.Resign = func() { resigned++ }
	h.press("r")
	if h.s.Engine.Stopped() != model.Running {
		t.Fatal("r did not resume the reads")
	}
	if resigned != 1 {
		t.Fatalf("r dropped the token %d times, want once", resigned)
	}
	if s := h.screen(); strings.Contains(s, "SIGNED OUT") || strings.Contains(s, "not asking:") {
		t.Fatalf("the stop is still said:\n%s", s)
	}
	for _, v := range h.u.stack {
		if v.Screen == model.ScreenRefused {
			t.Fatalf("the refusal screen stays after the reads resumed: %+v", h.u.stack)
		}
	}
}

// TestRForgetsFirst: r forgets the refused token before it lets any
// read go: when Resign runs, the reads are still stopped.
func TestRForgetsFirst(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("blog", &model.Failure{Kind: model.FailSignedOut, Message: "refused"})
	h.press(tcell.KeyCtrlR)
	if h.s.Engine.Stopped() == model.Running {
		t.Fatal("no stop")
	}
	h.fake.Set("blog", nil)
	order := ""
	h.u.Resign = func() {
		if h.s.Engine.Stopped() == model.Running {
			order = "retried before forgetting"
		} else {
			order = "forgot first"
		}
	}
	h.press("r")
	if order != "forgot first" {
		t.Fatalf("r: %q", order)
	}
}

// TestCtrlRRecord: a record whose read failed is read again by ctrl-r
// on its screen, RecordAgainAfter after its last read when pressed
// sooner.
func TestCtrlRRecord(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("blog@e9a0b3f2c118", &model.Failure{Kind: model.FailThis, Message: "deploy.example.com answered 500"})
	h.open(app("blog"))
	h.open(record("blog", "e9a0b3f2c118"))
	if s := h.screen(); !strings.Contains(s, "answered 500") {
		t.Fatalf("the failure is not on screen:\n%s", s)
	}
	h.fake.Set("blog@e9a0b3f2c118", nil)
	h.press(tcell.KeyCtrlR)
	if s := h.screen(); strings.Contains(s, "FAILED in soaking") {
		t.Fatalf("ctrl-r read the record again within %s:\n%s", model.RecordAgainAfter, s)
	}
	h.now = h.now.Add(model.RecordAgainAfter)
	h.pump()
	h.sync()
	if s := h.screen(); !strings.Contains(s, "FAILED in soaking") {
		t.Fatalf("ctrl-r did not read the record again:\n%s", s)
	}
}

// TestNoCtrlRHintWhileSlowed: during a 429's wait the footer offers no
// ctrl-r, which would do nothing.
func TestNoCtrlRHintWhileSlowed(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("blog", &model.Failure{Kind: model.FailSlowed, RetryAfter: time.Minute})
	h.press(tcell.KeyCtrlR)
	s := h.screen()
	if strings.Contains(s, "ctrl-r") || !strings.Contains(s, "slowed by the box (429) until") {
		t.Fatalf("during the wait the screen shows:\n%s", s)
	}
}
