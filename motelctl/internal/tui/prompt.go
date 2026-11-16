package tui

import (
	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/motelctl/internal/tui/door"
)

// ask opens row 2's prompt for the filter.
func (u *UI) ask() {
	door.SetLabel(u.prompt, "/")
	door.SetInput(u.prompt, u.filter)
	u.row2.SwitchToPage("prompt")
	u.App.SetFocus(u.prompt)
}

// promptDone is enter or esc on the filter: esc clears it.
func (u *UI) promptDone(k tcell.Key) {
	if k == tcell.KeyEscape {
		u.filter = ""
	}
	u.row2.SwitchToPage("crumb")
	u.show()
}
