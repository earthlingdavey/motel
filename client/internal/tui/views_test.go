package tui

import (
	"strings"
	"testing"
	"time"

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
