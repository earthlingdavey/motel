package model_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
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

// TestRolesQuoted: a role other than viewer is the box's word, and is
// quoted, in the title bar and on who am I, as the whoami command
// quotes it.
func TestRolesQuoted(t *testing.T) {
	s := &model.Session{Host: demo.Host, Key: demo.Key, Now: func() time.Time { return t0 }, Engine: model.NewEngine(t0)}
	s.Me = &model.Got[wire.WhoAmI]{V: wire.WhoAmI{Name: "alice", Key: demo.Key.Fingerprint, Box: demo.Host,
		Roles: []wire.RoleHeld{{Role: "all-healthy", Scope: "box"}, {Role: "viewer", Scope: "app:blog"}}}, At: t0}
	if left, _ := s.Title(t0); !strings.HasSuffix(left.Text(), `│ alice · "all-healthy" of the box; viewer of blog`) {
		t.Errorf("the title says %q", left.Text())
	}
	var mine []string
	for _, l := range s.MeView(t0) {
		if strings.HasPrefix(l.Text(), "  ") && !strings.HasPrefix(l.Text(), "   ") {
			mine = append(mine, l.Text())
		}
	}
	if want := []string{`  "all-healthy" the box`, "  view       app blog"}; !slices.Equal(mine, want) {
		t.Errorf("who am I says %q, want %q", mine, want)
	}
}

// TestMeLastReadFailed: a whoami that fails after one answered is said
// on who am I, beside the answer held, as the box screen says its own;
// the word goes when whoami answers again. An answer about another key
// still shows its own screen, with no such line.
func TestMeLastReadFailed(t *testing.T) {
	s, f, now := session(t)
	for range 3 {
		step(t, s, f, now)
	}
	failed := func() string {
		for _, l := range s.MeView(*now) {
			if strings.HasPrefix(l.Text(), "the last read failed") {
				return l.Text()
			}
		}
		return ""
	}
	if s.Me == nil || failed() != "" {
		t.Fatalf("before any failure: me %v, %q", s.Me, failed())
	}
	f.Set("whoami", &model.Failure{Kind: model.FailThis, Message: "deploy.example.com answered 409"})
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	if j := step(t, s, f, now); j.Kind != model.KWhoAmI {
		t.Fatalf("ctrl-r read %v", j)
	}
	if s.Me == nil {
		t.Fatal("the answer held was dropped")
	}
	if got := failed(); got != "the last read failed: deploy.example.com answered 409" {
		t.Errorf("who am I says %q of the failed read", got)
	}
	f.Set("whoami", nil)
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	step(t, s, f, now)
	if got := failed(); got != "" {
		t.Errorf("after an answer who am I still says %q", got)
	}
	f.Body("whoami.json", `{"name":"mallory@example.com","key":"SHA256:OTHER00000000000000000000000000000000000000","roles":[],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`)
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	step(t, s, f, now)
	if v := s.MeView(*now); s.Engine.Stopped() != model.NotThisKey || len(v) != 1 || !strings.HasPrefix(v[0].Text(), "who you are is not known: the box answered about another key") {
		t.Errorf("about another key, who am I says %v", v)
	}
}

// TestRecordRefusedAlone: a record read before, read again by ctrl-r,
// is kept beside a read that got no answer, and not beside a 403 or a
// 404: the box refused it, and the screen says that alone, as an app's
// screen does.
func TestRecordRefusedAlone(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		kept bool
	}{
		{"no answer", fail(model.FailNoAnswer), true},
		{"403", fail(model.FailForbidden), false},
		{"404", fail(model.FailNotFound), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := t0
			f := &demo.Fake{Now: func() time.Time { return now }}
			s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
			e := s.Engine
			for range 20 {
				step(t, s, f, &now)
			}
			rec := model.Job{Kind: model.KDeploy, App: "blog", Version: "e9a0b3f2c118"}
			e.Focus("blog", now)
			e.Once(rec, now)
			for step(t, s, f, &now) != rec {
			}
			f.Set("blog@e9a0b3f2c118", c.err)
			now = now.Add(model.RecordAgainAfter)
			e.Again(now, rec)
			for step(t, s, f, &now) != rec {
			}
			v := s.RecordView("blog", rec.Version, now)
			shown := false
			for _, l := range v.Head {
				shown = shown || strings.Contains(l.Text(), "FAILED")
			}
			if shown != c.kept || (v.Note != nil) == c.kept {
				t.Errorf("after a %s the record is shown: %v, the note %v", c.name, shown, v.Note)
			}
			if crumb := s.RecordAsOf("blog", rec.Version, now); (crumb != nil) != c.kept {
				t.Errorf("after a %s the crumb says %q", c.name, crumb.Text())
			}
		})
	}
}
