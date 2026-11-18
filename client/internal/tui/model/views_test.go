package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestStaleRunningIsRunning: a running app whose row went stale is
// still counted as running by the strip, not as one not deployed.
func TestStaleRunningIsRunning(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "docs"}, {"name": "cron-runner"}]}`)
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 6 {
		step(t, s, f, &now)
	}
	if got := s.AppsView(now, "", 0).AllWell.Text(); got != "all 2 apps running; every newest deploy succeeded" {
		t.Fatalf("both answering: %q", got)
	}
	f.Set("docs", fail(model.FailNoAnswer))
	for i := 0; !row(s, now, "docs").Stale; i++ {
		if i == 100 {
			t.Fatal("docs not stale after 100 reads")
		}
		step(t, s, f, &now)
	}
	if st := row(s, now, "docs").Cells[model.ColState].String(); st != "running · stale" {
		t.Fatalf("docs says %q", st)
	}
	if got := s.AppsView(now, "", 0).AllWell.Text(); !strings.HasPrefix(got, "all 2 apps running") {
		t.Errorf("a stale running row is counted as not running: %q", got)
	}
}
