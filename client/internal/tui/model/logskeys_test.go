package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// viewerOnly is whoami listing no logs grant for the demo's key.
const viewerOnly = `{"name":"alice@example.com","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`

// TestLogsKeysByState: which of the logs screen's own keys act, in each
// state the screen can be in: none of them while it says why it shows no
// line (no grant, whoami not known, a refusal, kept off), w alone while
// the first read is asked, End and w on a live follow with no line yet
// or a head that leaves the lines no row, and all three with lines in
// view.
func TestLogsKeysByState(t *testing.T) {
	none, wrap, followWrap, all := model.LogsKeysOn{}, model.LogsKeysOn{Wrap: true}, model.LogsKeysOn{Follow: true, Wrap: true}, model.LogsKeysOn{Scroll: true, Follow: true, Wrap: true}
	refused := &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this"}
	for _, c := range []struct {
		name   string
		setup  func(*demo.Fake)
		height int
		says   string
		want   model.LogsKeysOn
	}{
		{"no grant", func(f *demo.Fake) { f.Body("whoami.json", viewerOnly) }, 30, "needs a logs grant", none},
		{"whoami not answered", func(f *demo.Fake) {
			f.Set("whoami", &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
		}, 30, "not known until whoami answers", none},
		{"the box refused", func(f *demo.Fake) { f.Set("logs:blog", refused) }, 30, "does not allow your key this", none},
		{"kept off", func(f *demo.Fake) {
			f.Body("logs/blog.json", `{"kept":"off","lines":[],"more":false,"gap":false}`)
		}, 30, "keeps blog's output on the box", none},
		{"a word not known", func(f *demo.Fake) {
			f.Body("logs/blog.json", `{"kept":"journal","lines":[],"more":false,"gap":false}`)
		}, 30, "names what it keeps of blog's output", none},
		{"a live follow with no line yet", func(f *demo.Fake) {
			f.Body("logs/blog.json", `{"kept":"on","lines":[],"more":false,"gap":false}`)
		}, 30, "holds no line of blog", followWrap},
		{"lines shown", nil, 30, "── following", all},
		{"the head fills the page", nil, 1, "", followWrap},
	} {
		s, _, now := logsOpen(t, "blog", c.setup)
		if v := logsText(s, *now, 80, c.height); !strings.Contains(v, c.says) {
			t.Fatalf("%s: the screen says:\n%s", c.name, v)
		}
		if got := s.LogsKeys("blog", *now, 80, c.height, runes); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	// Still asking: opened, the read not yet answered.
	s, f, now := session(t)
	drive(s, f, *now, 2)
	s.LogsOpen("blog", *now)
	if v := logsText(s, *now, 80, 30); !strings.Contains(v, "asking deploy.example.com for blog's newest lines") {
		t.Fatalf("still asking:\n%s", v)
	}
	if got := s.LogsKeys("blog", *now, 80, 30, runes); got != wrap {
		t.Errorf("still asking: %+v, want %+v", got, wrap)
	}
	// Another app's lines held: this app's first read is still asked, and
	// nothing of the other's counts.
	s, _, now = logsOpen(t, "blog", nil)
	logsText(s, *now, 80, 12)
	s.LogsScroll("blog", -3, 80, runes, false)
	if got := s.LogsKeys("hostile", *now, 80, 12, runes); got != wrap {
		t.Errorf("hostile, blog's lines held: %+v, want %+v", got, wrap)
	}
	// A failed read after lines: the head says so, and the lines are live.
	s, f, now = logsOpen(t, "blog", nil)
	f.Set("logs:blog", &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com did not answer"})
	*now = now.Add(model.LogsEvery)
	drive(s, f, *now, 5)
	if v := logsText(s, *now, 80, 30); !strings.Contains(v, "the last read failed") {
		t.Fatalf("a failure after lines:\n%s", v)
	}
	if got := s.LogsKeys("blog", *now, 80, 30, runes); got != all {
		t.Errorf("a failure after lines: %+v, want %+v", got, all)
	}
	// That head with only its own rows and the rule's: no row for lines.
	if got := s.LogsKeys("blog", *now, 80, 2, runes); got != followWrap {
		t.Errorf("a failure's head filling the page: %+v, want %+v", got, followWrap)
	}
}

// TestLogsKeysHeld: a view held is let go by End wherever the screen no
// longer shows it, and the scroll keys act only where lines show.
func TestLogsKeysHeld(t *testing.T) {
	hold := func(t *testing.T) (*model.Session, *demo.Fake, *time.Time) {
		t.Helper()
		s, f, now := logsOpen(t, "blog", nil)
		logsText(s, *now, 80, 12)
		s.LogsScroll("blog", -3, 80, runes, false)
		if v := logsText(s, *now, 80, 12); !strings.Contains(v, "── held") {
			t.Fatalf("not held:\n%s", v)
		}
		return s, f, now
	}
	s, _, now := hold(t)
	if got, want := s.LogsKeys("blog", *now, 80, 12, runes), (model.LogsKeysOn{Scroll: true, Follow: true, Wrap: true}); got != want {
		t.Errorf("held, lines shown: %+v, want %+v", got, want)
	}
	if got, want := s.LogsKeys("blog", *now, 80, 1, runes), (model.LogsKeysOn{Follow: true, Wrap: true}); got != want {
		t.Errorf("held, the head filling the page: %+v, want %+v", got, want)
	}
	// The grant gone while held: End alone, to let the hold go.
	s, f, now := hold(t)
	f.Body("whoami.json", viewerOnly)
	*now = now.Add(model.AgainAfter)
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	drive(s, f, *now, 10)
	if v := logsText(s, *now, 80, 12); !strings.Contains(v, "needs a logs grant") {
		t.Fatalf("the grant gone:\n%s", v)
	}
	if got, want := s.LogsKeys("blog", *now, 80, 12, runes), (model.LogsKeysOn{Follow: true}); got != want {
		t.Errorf("held, the grant gone: %+v, want %+v", got, want)
	}
}

// TestLogsKeysStopped: a follow the box stopped after lines (kept off)
// still scrolls and wraps its lines, but End follows nothing there:
// it is offered only to let a view held go.
func TestLogsKeysStopped(t *testing.T) {
	s, f, now := logsOpen(t, "blog", nil)
	f.Body("logs/blog.json", `{"kept":"off","lines":[],"more":false,"gap":false}`)
	*now = now.Add(model.LogsEvery)
	drive(s, f, *now, 5)
	if v := logsText(s, *now, 80, 12); !strings.Contains(v, "── not following") {
		t.Fatalf("not stopped:\n%s", v)
	}
	if got, want := s.LogsKeys("blog", *now, 80, 12, runes), (model.LogsKeysOn{Scroll: true, Wrap: true}); got != want {
		t.Errorf("stopped, lines shown: %+v, want %+v", got, want)
	}
	s.LogsScroll("blog", -3, 80, runes, false)
	if v := logsText(s, *now, 80, 12); !strings.Contains(v, "── held") {
		t.Fatalf("not held:\n%s", v)
	}
	if got, want := s.LogsKeys("blog", *now, 80, 12, runes), (model.LogsKeysOn{Scroll: true, Follow: true, Wrap: true}); got != want {
		t.Errorf("stopped and held: %+v, want %+v", got, want)
	}
}
