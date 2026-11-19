package tui

import (
	"strconv"
	"strings"
	"testing"
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
