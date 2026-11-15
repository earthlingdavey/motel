package tui

import (
	"github.com/gdamore/tcell/v2"
)

// ask opens row 2's prompt for the filter.
func (u *UI) ask() {
	setLabel(u.prompt, "/")
	setInput(u.prompt, u.filter)
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
