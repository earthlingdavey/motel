package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestWideSummaryCut: the apps crumb's summary wider than the window
// (a filter, 200 apps that each need a look, over 100 of them stale,
// at 60 columns: 60 cells) is cut at its end, … its last cell, read from the
// screen's cells, and nothing is drawn past the window.
func TestWideSummaryCut(t *testing.T) {
	var apps []string
	h := start(t, 60, 16, func(f *demo.Fake) {
		for i := range 200 {
			apps = append(apps, fmt.Sprintf(`{"name":"app-%03d"}`, i))
			f.Set(fmt.Sprintf("app-%03d", i), &model.Failure{Kind: model.FailThis, Message: "no"})
		}
		f.Body("apps.json", `{"apps":[`+strings.Join(apps, ",")+`]}`)
	})
	// Every row read once, then a wake: every row still being read is
	// stale until read again.
	h.walk(6*time.Minute, model.Job{})
	h.now = h.now.Add(10 * time.Minute)
	h.on(func() { h.u.S.Engine.Tick(h.now) })
	h.press("/", "app", tcell.KeyEnter)
	crumb := screenRows(h)[1]
	if !strings.Contains(crumb.text, "200 of 200 apps · 200 need a look · ") || !strings.Contains(crumb.text, " stale · by") || crumb.end > 60 {
		t.Fatalf("the crumb is drawn %q, to cell %d", crumb.text, crumb.end)
	}
	if !strings.HasSuffix(crumb.text, "…") || crumb.end != 60 {
		t.Errorf("the crumb's summary is cut at the window's edge with no mark: %q, to cell %d", crumb.text, crumb.end)
	}
}
