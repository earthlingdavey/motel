package cli

import (
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

// TestActionsAreCommands: every view the TUI offers names a motelctl
// command that exists and takes the arguments it names, so the TUI
// shows nothing a command does not read.
func TestActionsAreCommands(t *testing.T) {
	root := (&runner{}).tree()
	if len(model.Actions) == 0 {
		t.Fatal("no actions")
	}
	for _, a := range model.Actions {
		words := strings.Fields(a.Command)
		var path, args []string
		for _, w := range words {
			if strings.HasPrefix(w, "<") {
				args = append(args, w)
			} else {
				path = append(path, w)
			}
		}
		cmd, rest, err := root.Find(path)
		if err != nil || len(rest) != 0 || cmd == root {
			t.Errorf("%q: no command %q", a.Command, strings.Join(path, " "))
			continue
		}
		if err := cmd.Args(cmd, args); err != nil {
			t.Errorf("%q: %s does not take %v: %v", a.Command, cmd.CommandPath(), args, err)
		}
	}
}
