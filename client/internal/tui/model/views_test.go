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

// TestStaleRunningIsStale: a running app whose row went stale is not
// counted by the strip as one not deployed, nor as running: the strip
// says it is stale, and not that every app runs.
func TestStaleRunningIsStale(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "docs"}, {"name": "cron-runner"}]}`)
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 6 {
		step(t, s, f, &now)
	}
	if got := s.AppsView(now, "", 0, 0).AllWell.Text(); got != "all 2 apps running; every newest deploy succeeded" {
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
	if got := s.AppsView(now, "", 0, 0).AllWell.Text(); got != "nothing needs a look; 1 of 2 apps is stale" {
		t.Errorf("beside a stale running row the strip says %q", got)
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
// still shows its own screen, with no such line. Each ctrl-r is pressed
// AgainAfter after the last read of whoami.
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
	*now = now.Add(model.AgainAfter) // ctrl-r reads whoami again no sooner
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
	*now = now.Add(model.AgainAfter) // ctrl-r reads whoami again no sooner
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	step(t, s, f, now)
	if got := failed(); got != "" {
		t.Errorf("after an answer who am I still says %q", got)
	}
	f.Body("whoami.json", `{"name":"mallory@example.com","key":"SHA256:OTHER00000000000000000000000000000000000000","roles":[],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`)
	*now = now.Add(model.AgainAfter) // ctrl-r reads whoami again no sooner
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	step(t, s, f, now)
	if v := s.MeView(*now); s.Engine.Stopped() != model.NotThisKey || len(v) != 1 || !strings.HasPrefix(v[0].Text(), "who you are is not known: the box answered about another key") {
		t.Errorf("about another key, who am I says %v", v)
	}
}

// TestTitleHeldWhoAmIFailed: with an answer of whoami held and its
// newest read failed, while the session runs, the title bar says the
// answer's age and that the read failed, before the answer; the word
// goes when whoami answers again. Without a failure the title is as it
// was, and so it is on a stop, which says its own reason. Each read of
// whoami is a ctrl-r, AgainAfter or more after the last.
func TestTitleHeldWhoAmIFailed(t *testing.T) {
	held := "motel · deploy.example.com · v0.7.0 │ alice@example.com · viewer of the box"
	for _, c := range []struct {
		name string
		err  *model.Failure
		want string
	}{
		{"a release that is not N.N.N", &model.Failure{Kind: model.FailUnusable, Message: "not moteld's answer"}, "motel · deploy.example.com │ whoami failed; 21m old: v0.7.0 · alice@example.com · viewer of the box"},
		{"409", &model.Failure{Kind: model.FailThis, Message: "deploy.example.com answered 409"}, "motel · deploy.example.com │ whoami failed; 21m old: v0.7.0 · alice@example.com · viewer of the box"},
		{"no answer", &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com closed the connection"}, "motel · deploy.example.com │ whoami failed; 21m old: v0.7.0 · alice@example.com · viewer of the box"},
		{"403", &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this"}, "motel · deploy.example.com │ whoami failed; 21m old: v0.7.0 · alice@example.com · viewer of the box"},
		{"a stop: 401", &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com refused the key (401)"}, held},
		{"none", nil, held},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, f, now := session(t)
			for range 3 {
				step(t, s, f, now)
			}
			if left, _ := s.Title(*now); left.Text() != held {
				t.Fatalf("before: %q", left.Text())
			}
			if c.err != nil {
				f.Set("whoami", c.err)
			}
			*now = now.Add(21 * time.Minute)
			s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
			if j := step(t, s, f, now); j.Kind != model.KWhoAmI {
				t.Fatalf("ctrl-r read %v", j)
			}
			if left, _ := s.Title(*now); left.Text() != c.want {
				t.Errorf("the title says %q, want %q", left.Text(), c.want)
			}
			if c.err == nil || c.err.Kind == model.FailSignedOut {
				return
			}
			f.Set("whoami", nil)
			*now = now.Add(model.AgainAfter)
			s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
			step(t, s, f, now)
			if left, _ := s.Title(*now); left.Text() != held {
				t.Errorf("after whoami answered again: %q", left.Text())
			}
		})
	}
	// No answer held: the failure is said in place of who you are, as
	// before (TestWhoAmIFailedIsSaid).
	s, f, now := session(t)
	f.Set("whoami", &model.Failure{Kind: model.FailThis, Message: "deploy.example.com answered 409"})
	for range 3 {
		step(t, s, f, now)
	}
	if left, _ := s.Title(*now); left.Text() != "motel · deploy.example.com │ who you are is not known: deploy.example.com answered 409" {
		t.Errorf("no answer held: %q", left.Text())
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

// TestLongNamesDistinct: two long names that the cut would show alike
// are shown longer until they differ, the same in the table and in the
// strip, and never so long that the mark, NAME and STATE leave the
// screen: two names that differ only in what that leaves out stay
// alike, and STATE stays.
func TestLongNamesDistinct(t *testing.T) {
	stopped, err := os.ReadFile("../demo/fixtures/status/mailer.json")
	if err != nil {
		t.Fatal(err)
	}
	mid := func(c string) string { return strings.Repeat("a", 31) + c + strings.Repeat("a", 31) }
	for _, c := range []struct {
		a, b     string
		width    int
		distinct bool
	}{
		{"acme-website-staging-frontend", "acme-website-production-frontend", 80, true},
		{"acme-website-staging-frontend", "acme-website-production-frontend", 60, true},
		{mid("b"), mid("c"), 0, true},
		{mid("b"), mid("c"), 80, true}, // STATE 13 wide leaves 63 for NAME
		{mid("b"), mid("c"), 60, false},
	} {
		now := t0
		f := &demo.Fake{Now: func() time.Time { return now }}
		f.Body("apps.json", `{"apps": [{"name": "`+c.a+`"}, {"name": "`+c.b+`"}, {"name": "blog"}]}`)
		f.Body("status/"+c.a+".json", string(stopped))
		f.Body("status/"+c.b+".json", string(stopped))
		s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
		for range 6 {
			step(t, s, f, &now)
		}
		v := s.AppsView(now, "", 1, c.width)
		shown := map[string]string{}
		stateW := 0
		for _, r := range v.Rows {
			shown[r.Name] = r.Cells[model.ColName].String()
			stateW = max(stateW, len([]rune(r.StateAged.String())))
		}
		if got := shown[c.a] != shown[c.b]; got != c.distinct {
			t.Errorf("%d columns: %q and %q shown as %q and %q", c.width, c.a, c.b, shown[c.a], shown[c.b])
		}
		if n := len([]rune(shown[c.a])); c.width > 0 && 4+n+stateW > c.width {
			t.Errorf("%d columns: NAME %d wide and STATE %d leave the screen", c.width, n, stateW)
		}
		for _, l := range v.Strip {
			name := strings.Fields(l.Text())[1]
			if name != shown[c.a] && name != shown[c.b] {
				t.Errorf("%d columns: the strip shows %q, the table %q and %q", c.width, name, shown[c.a], shown[c.b])
			}
		}
		if len(v.Strip) != 2 {
			t.Errorf("%d columns: %d strip lines", c.width, len(v.Strip))
		}
	}
}
