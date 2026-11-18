package tui

import (
	"bytes"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/hostile"
)

// TestSafeBytes draws the hostile app's screens, and its record and
// JSON, on a real terminfo screen: none of the fixture's attacks is in
// what tcell writes, and the escaped forms are on screen.
func TestSafeBytes(t *testing.T) {
	for _, keys := range [][]any{
		{"/", "hos", tcell.KeyEnter, tcell.KeyEnter},
		{"/", "hos", tcell.KeyEnter, tcell.KeyEnter, tcell.KeyEnter},
		{":", "status hostile", tcell.KeyEnter},
		{":", "deploys hostile v7", tcell.KeyEnter},
		{":", "deploys hostile v7", tcell.KeyEnter, "y"},
	} {
		tty := hostile.NewTTY()
		scr, err := hostile.Screen(tty)
		if err != nil {
			t.Fatal(err)
		}
		h := startOn(t, scr, nil)
		h.press(keys...)
		h.sync()
		out := tty.Bytes()
		if found := hostile.Scan(out); len(found) > 0 {
			t.Errorf("%v: attacks reached the terminal: %v", keys, found)
		}
		if !bytes.Contains(out, []byte(`evil.example`)) {
			t.Errorf("%v: the inert attacks are not on screen", keys)
		}
	}
}
