package tui

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
)

// The fixes for what the trial of the TUI against a real box found.

// TestDeployInFlight: while another version is deployed, an app's
// screen says the serving version is running, and the deploy in flight
// on a line of its own: its version, phase, when it started and by
// whom. The box's answer had phase "preparing" beside the running
// version, and the screen said "preparing <running version>".
func TestDeployInFlight(t *testing.T) {
	for _, sz := range [][2]int{{60, 16}, {80, 24}} {
		name := "app-deploying-" + strconv.Itoa(sz[0]) + "x" + strconv.Itoa(sz[1])
		t.Run(name, func(t *testing.T) {
			h := start(t, sz[0], sz[1], nil)
			h.open(app("api"))
			rows := screenRows(h)
			var state, deploying string
			for _, r := range rows {
				switch {
				case strings.HasPrefix(r.text, "state "):
					state = r.text
				case strings.HasPrefix(r.text, "deploying "):
					deploying = r.text
				}
			}
			if want := "state       running 3b8812aa0c4f"; !strings.HasPrefix(state, want) {
				t.Errorf("the state is drawn %q, want it to begin %q", state, want)
			}
			if want := "deploying   4c2d9e7b1a05 soaking started 25s ago (14:12"; !strings.HasPrefix(deploying, want) {
				t.Errorf("the deploy in flight is drawn %q, want it to begin %q", deploying, want)
			}
			golden(t, name, h.screen())
		})
	}
}

// TestMetricsSayOnlyWhatTheClientKnows: the metrics line says the
// newest point's time and its age, and nothing of how far behind the
// store runs, which the box does not say (the trial's ran 5 s behind;
// the screen said 30).
func TestMetricsSayOnlyWhatTheClientKnows(t *testing.T) {
	h := start(t, 60, 16, nil)
	h.open(app("blog"))
	var got string
	for _, r := range screenRows(h) {
		if strings.HasPrefix(r.text, "metrics ") {
			got = r.text
		}
	}
	if want := "metrics     over 1h, newest point 14:12:10 UTC (30s ago)"; got != want {
		t.Errorf("the metrics line is drawn %q, want %q", got, want)
	}
}

// TestLongTailLineCut: a log tail's line wider than the window is cut
// at its edge, "…" its last cell, as every other cut line is (the trial
// saw a 300-character line stop at column 80 unmarked); with w on, it
// wraps whole onto the rows below.
func TestLongTailLineCut(t *testing.T) {
	const rec = "deploys/blog/e9a0b3f2c118.json"
	b, err := os.ReadFile("demo/fixtures/" + rec)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 300)
	body := strings.Replace(string(b), `"> blog@1.4.0 start"`, `"`+long+`"`, 1)
	if body == string(b) {
		t.Fatal("the fixture's first log line moved")
	}
	for _, sz := range [][2]int{{60, 16}, {80, 24}} {
		h := start(t, sz[0], sz[1], func(f *demo.Fake) { f.Body(rec, body) })
		h.open(record("blog", "e9a0b3f2c118"))
		h.on(func() { h.u.tail.ScrollToBeginning() })
		var row drawnRow
		for _, r := range screenRows(h) {
			if strings.HasPrefix(r.text, "xxxx") {
				row = r
				break
			}
		}
		if want := strings.Repeat("x", sz[0]-1) + "…"; row.text != want {
			t.Errorf("%dx%d: the long line is drawn %q, want %q", sz[0], sz[1], row.text, want)
		}
		h.press("w")
		n := 0
		for _, r := range screenRows(h) {
			if strings.HasPrefix(r.text, "xxxx") {
				n++
				if strings.HasSuffix(r.text, "…") {
					t.Errorf("%dx%d: with w on, a row of the long line is cut: %q", sz[0], sz[1], r.text)
				}
			}
		}
		if n < 2 {
			t.Errorf("%dx%d: with w on, the long line is on %d rows", sz[0], sz[1], n)
		}
	}
}
