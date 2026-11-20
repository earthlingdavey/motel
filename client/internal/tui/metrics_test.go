package tui

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/hostile"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// seriesReads is the windows of blog's series read so far, in order.
func (h *harness) seriesReads() []string {
	var out []string
	h.on(func() {
		for _, c := range h.fake.Calls {
			if c.Job.Kind == model.KSeries && c.Job.App == "blog" {
				out = append(out, c.Job.Range)
			}
		}
	})
	return out
}

// TestMetricsScreen: g on an app's screen opens its metrics over the
// hour; t steps through the windows, each read when shown, and back to
// the hour; esc goes back to the app's screen, whose alerts and status
// are read again and the series no more; ctrl-r reads the window shown;
// the crumb names the app and the window; at 60x16 the first plot is
// whole, with its deploy marks.
func TestMetricsScreen(t *testing.T) {
	h := start(t, 60, 16, nil)
	h.open(app("blog"))
	h.press("g")
	if v := h.top(); v.Screen != model.ScreenMetrics || v.App != "blog" || v.Range != "1h" {
		t.Fatalf("g opened %+v", v)
	}
	for _, rng := range []string{"24h", "7d", "14d", "1h"} {
		h.press("t")
		if v := h.top(); v.Screen != model.ScreenMetrics || v.Range != rng {
			t.Fatalf("t showed %+v, want %s", v, rng)
		}
		if rows := screenRows(h); !strings.HasPrefix(rows[1].text, "apps › blog › metrics "+rng) {
			t.Errorf("the crumb is %q", rows[1].text)
		}
	}
	if got := h.seriesReads(); strings.Join(got, " ") != "1h 24h 7d 14d" {
		t.Errorf("the windows read: %v", got)
	}
	h.press("t")
	golden(t, "metrics-blog-24h-60x16", h.screen())
	rows := screenRows(h)
	if !strings.HasPrefix(rows[5].text, "memory") || !strings.HasPrefix(rows[13].text, "        +") || !strings.Contains(rows[13].text, "^") {
		t.Errorf("the first plot is not whole, with its marks, at 60x16:\n%s", h.screen())
	}
	h.now = h.now.Add(model.AgainAfter)
	h.pump()
	n0 := len(h.seriesReads())
	h.press(tcell.KeyCtrlR)
	if got := h.seriesReads(); len(got) != n0+1 || got[n0] != "24h" {
		t.Errorf("ctrl-r: the windows read: %v", got)
	}
	// c: the command that makes the same read.
	h.press("c")
	if !strings.Contains(h.screen(), "motel metrics --range 24h --box deploy.example.com -- b") {
		t.Errorf("c on the metrics screen:\n%s", h.screen())
	}
	h.press(tcell.KeyEscape, tcell.KeyEscape)
	if v := h.top(); v.Screen != model.ScreenApp || v.App != "blog" {
		t.Fatalf("esc went to %+v", v)
	}
	n := len(h.seriesReads())
	h.now = h.now.Add(2 * model.SeriesEvery)
	h.pump()
	if got := h.seriesReads(); len(got) != n {
		t.Errorf("read on the app's screen: %v", got[n:])
	}
	// g opens the window last shown.
	h.press("g")
	if v := h.top(); v.Range != "24h" {
		t.Errorf("g opened %+v, want the 24h shown last", v)
	}
}

// TestMetricsFromTheLine: :metrics opens the app's screen under the
// metrics screen without showing it on the way (which would ask the
// app's alerts); typed on that app's metrics screen it shows the other
// window in its place, the stack as it was.
func TestMetricsFromTheLine(t *testing.T) {
	h := start(t, 80, 24, nil)
	alerts := func() int {
		n := 0
		h.on(func() {
			for _, c := range h.fake.Calls {
				if c.Job.Kind == model.KAlerts {
					n++
				}
			}
		})
		return n
	}
	before := alerts()
	h.press(":", "metrics blog 7d", tcell.KeyEnter)
	var stack []model.View
	h.on(func() { stack = append(stack, h.u.stack...) })
	if len(stack) != 3 || stack[1] != app("blog") || stack[2].Screen != model.ScreenMetrics || stack[2].Range != "7d" || alerts() != before {
		t.Errorf(":metrics blog 7d: stack %+v, %d alerts reads", stack, alerts()-before)
	}
	h.press(":", "metrics blog 14d", tcell.KeyEnter)
	h.on(func() { stack = append([]model.View(nil), h.u.stack...) })
	if len(stack) != 3 || stack[2].Range != "14d" {
		t.Errorf("on the metrics screen, :metrics blog 14d: stack %+v", stack)
	}
	h.press(tcell.KeyEscape)
	if v := h.top(); v != app("blog") {
		t.Errorf("esc went to %+v", v)
	}
}

// TestMetricsOldBox: against a moteld older than the read (a 404), the
// screen says so, and the title bar does not call it a refusal.
func TestMetricsOldBox(t *testing.T) {
	h := start(t, 80, 24, nil)
	h.fake.Set("series:blog", &model.Failure{Kind: model.FailNotFound, Message: `deploy.example.com answered 404; the box says: "not found"`})
	h.open(app("blog"))
	h.press("g")
	rows := screenRows(h)
	if strings.Contains(rows[0].text, "refused") || !strings.Contains(h.screen(), "(a moteld older than") {
		t.Errorf("an older box's 404:\n%s", h.screen())
	}
}

// hostileSeries is a series whose every word of the box's is an attack,
// and whose numbers are as large, as small and as out of order as JSON
// carries; the TUI is given it as is, past the client's grammars.
func hostileSeries(end time.Time) string {
	names := make([]string, 0, len(hostile.Payloads))
	for k := range hostile.Payloads {
		names = append(names, k)
	}
	sort.Strings(names)
	s := wire.Series{Store: wire.StoreOn, Window: "24h", StepSeconds: 300, End: &end}
	e := float64(end.Unix())
	pts := [][2]float64{{e - 600, 1e308}, {e - 300, -1e308}, {e - 900, 0}, {e + 9e9, 5}, {e - 9e9, 5}, {e, 1e-300}}
	s.Panels = append(s.Panels, wire.Panel{Name: wire.PanelMemory, Unit: wire.UnitBytes, Points: pts})
	for _, k := range names {
		s.Panels = append(s.Panels, wire.Panel{Name: "evil.example " + hostile.Payloads[k], Unit: hostile.Payloads[k], Points: pts})
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// TestMetricsHostile: the metrics screen of a box whose words are
// attacks and whose numbers are hostile, drawn on a real terminfo
// screen: none of the attacks reaches the terminal, the inert forms are
// on screen, and nothing panics; a store's word that is an attack too.
func TestMetricsHostile(t *testing.T) {
	for _, body := range []func(time.Time) string{
		hostileSeries,
		func(time.Time) string {
			b, _ := json.Marshal(map[string]any{"store": "evil.example " + hostile.Payloads["osc8"], "window": "24h", "panels": []any{}})
			return string(b)
		},
	} {
		tty := hostile.NewTTY()
		scr, err := hostile.Screen(tty)
		if err != nil {
			t.Fatal(err)
		}
		h := startOn(t, scr, nil)
		h.fake.Body("series/hostile/24h.json", body(h.now))
		h.press(":", "metrics hostile 24h", tcell.KeyEnter)
		h.sync()
		out := tty.Bytes()
		if found := hostile.Scan(out); len(found) > 0 {
			t.Errorf("attacks reached the terminal: %v", found)
		}
		if !bytes.Contains(out, []byte(`evil.example`)) {
			t.Errorf("the inert attacks are not on screen")
		}
	}
}
