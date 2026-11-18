package model_test

import (
	"os"
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

// TestNegativeMemory: a memory reading below zero draws an empty bar,
// and does not end the TUI.
func TestNegativeMemory(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	b, err := os.ReadFile("../demo/fixtures/status/docs.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Body("apps.json", `{"apps": [{"name": "docs"}]}`)
	f.Body("status/docs.json", strings.Replace(string(b), `"now": 63963136`, `"now": -63963136`, 1))
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 3 {
		step(t, s, f, &now)
	}
	var mem string
	for _, l := range s.Overview("docs", now).Fields {
		if strings.HasPrefix(l.Text(), "memory") {
			mem = l.Text()
		}
	}
	if !strings.Contains(mem, strings.Repeat("░", 20)) {
		t.Errorf("the memory line is %q", mem)
	}
}

// TestWhoAmIFailedIsSaid: a whoami the client cannot take, from a box
// that answers every other read, is said in the title bar and on who
// am I, not "not signed in" and "asking".
func TestWhoAmIFailedIsSaid(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	why := "Something at deploy.example.com answered (HTTP 200, not moteld's answer: its version is not a release). Is this the box's deploy host?"
	f.Set("whoami", &model.Failure{Kind: model.FailUnusable, Message: why})
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 3 {
		step(t, s, f, &now)
	}
	if left, _ := s.Title(now); left.Text() != "motel · deploy.example.com │ who you are is not known: "+why {
		t.Errorf("the title says %q", left.Text())
	}
	if me := s.MeView(now); len(me) == 0 || me[0].Text() != "who you are is not known: "+why {
		t.Errorf("who am I says %v", me)
	}
}
