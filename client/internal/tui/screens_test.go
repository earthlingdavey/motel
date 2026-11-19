package tui

import (
	"strconv"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// sizes are the design size and the narrow one every screen must
// still work at.
var sizes = [][2]int{{100, 30}, {80, 24}}

// TestScreens renders each screen from the fixtures, the hostile app's
// among them, at both sizes.
func TestScreens(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*demo.Fake)
		keys  []any
	}{
		{"apps", nil, nil},
		{"app-blog", nil, []any{":", "status blog", tcell.KeyEnter}},
		{"record-blog-failed", nil, []any{":", "deploys blog e9a0b3f2c118", tcell.KeyEnter}},
		{"record-blog-succeeded", nil, []any{":", "deploys blog 7c1e55d0a913", tcell.KeyEnter}},
		{"app-hostile", nil, []any{":", "status hostile", tcell.KeyEnter}},
		{"record-hostile", nil, []any{":", "deploys hostile v7", tcell.KeyEnter}},
		{"me", nil, []any{":", "me", tcell.KeyEnter}},
		{"box", nil, []any{":", "box", tcell.KeyEnter}},
		{"backups", nil, []any{":", "backups", tcell.KeyEnter}},
		{"help", nil, []any{"?"}},
		{"command", nil, []any{":", "status blog", tcell.KeyEnter, "c"}},
		{"json", nil, []any{":", "deploys hostile v7", tcell.KeyEnter, "y"}},
		{"refused", func(f *demo.Fake) {
			f.Set("whoami", &model.Failure{Kind: model.FailSignedOut, Message: demo.Host + " did not accept your key SHA256:SAMPLE0000000000000000000000000000000000000 (~/.ssh/id_ed25519.pub). The box says no more to a key it does not accept, so it is one of these: no person or signer line on the box lists it; your address is outside the from list on every grant you hold; " + demo.Host + " is not the box's main name; or the box's moteld is too old to take a key's token at all. Send the operator the line `motel key` prints."})
		}, nil},
		{"one-app", func(f *demo.Fake) { f.Body("apps.json", `{"apps": [{"name": "blog"}]}`) }, nil},
	}
	for _, c := range cases {
		for _, sz := range sizes {
			name := c.name + "-" + strconv.Itoa(sz[0]) + "x" + strconv.Itoa(sz[1])
			t.Run(name, func(t *testing.T) {
				h := start(t, sz[0], sz[1], c.setup)
				h.press(c.keys...)
				golden(t, name, h.screen())
			})
		}
	}
}
