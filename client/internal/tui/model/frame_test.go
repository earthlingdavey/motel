package model_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

func session(t *testing.T) (*model.Session, *demo.Fake, *time.Time) {
	t.Helper()
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Key: demo.Key, Now: f.Now, Engine: model.NewEngine(t0)}
	return s, f, &now
}

// TestCommandsAreShellSafe runs each printed command through a real sh,
// with a stub motel on PATH, and compares the arguments it got with
// the ones meant: hostile versions, key paths and a box name.
func TestCommandsAreShellSafe(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("NOT RUN: no sh")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "motel")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		version, key, host string
	}{
		{"-v1", "~/.ssh/my key", "deploy.example.com"},
		{"--key", "/tmp/a;rm -rf x", "deploy.example.com"},
		{"v1'x", "$(id)`id` \"q\" '", "box-1.example.com"},
		{"v2", "back\\slash*?[", "deploy.example.com"},
	} {
		s := &model.Session{Host: c.host, Key: model.Key{Source: "--key", Shown: c.key}, Now: func() time.Time { return t0 }, Engine: model.NewEngine(t0)}
		v := model.View{Screen: model.ScreenRecord, App: "blog", Version: c.version}
		line, ok := s.Command(v)
		if !ok {
			t.Fatal("no command")
		}
		cmd := exec.Command(sh, "-c", line)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		want, _ := s.CommandArgs(v)
		if !slices.Equal(got, want[1:]) {
			t.Errorf("%q gave %q, want %q", line, got, want[1:])
		}
		if want[len(want)-3] != "--" {
			t.Errorf("no -- before the positional arguments: %q", want)
		}
	}
}

// TestOverlayWords: the apps screen's command says it is the list and a
// status a row; no answer yet is said; P has the refusal's person line,
// the command and the JSON, and nothing on a screen that reads nothing.
func TestOverlayWords(t *testing.T) {
	s, f, now := session(t)
	apps := s.Overlay(model.View{Screen: model.ScreenApps}, false, 98)
	text := ""
	for _, l := range apps {
		text += l.Text() + "\n"
	}
	if !strings.Contains(text, "motel apps --box deploy.example.com") || !strings.Contains(text, "each row is") || !strings.Contains(text, "motel status --box deploy.example.com -- <app>") {
		t.Errorf("the apps overlay says:\n%s", text)
	}
	if l := s.Overlay(model.View{Screen: model.ScreenApp, App: "blog"}, true, 98); len(l) != 1 || l[0].Text() != "no answer yet to show" {
		t.Errorf("before any answer the JSON overlay says %v", l)
	}
	if _, ok := s.Printable(model.View{Screen: model.ScreenRefused}, 0); ok {
		t.Error("the refusal screen prints a person line with no refused key")
	}
	if _, ok := s.Printable(model.View{Screen: model.ScreenHelp}, 1); ok {
		t.Error("help prints a command")
	}
	if _, ok := s.Printable(model.View{Screen: model.ScreenApp, App: "blog"}, 2); ok {
		t.Error("JSON printed before an answer came")
	}
	for range 3 {
		step(t, s, f, now)
	}
	body, _, _ := s.Body(model.View{Screen: model.ScreenApps})
	if p, ok := s.Printable(model.View{Screen: model.ScreenApps}, 2); !ok || string(p) != string(body) && string(p) != string(body)+"\n" {
		t.Errorf("P on the JSON prints %q, not the body as --json prints it %q", p, body)
	}
	if p, ok := s.Printable(model.View{Screen: model.ScreenApps}, 1); !ok || string(p) != "motel apps --box deploy.example.com\n" {
		t.Errorf("P on the command prints %q", p)
	}
}

// TestAppsRowTemplateKey: the apps screen's command overlay teaches a
// row's command with the session's key, --key when the person named it
// with --key, and no --key otherwise, as every other printed command.
func TestAppsRowTemplateKey(t *testing.T) {
	for _, c := range []struct {
		source, want string
	}{
		{"--key", "  motel status --box deploy.example.com --key '~/.ssh/my key' -- <app>"},
		{"the default file", "  motel status --box deploy.example.com -- <app>"},
	} {
		s, _, _ := session(t)
		s.Key.Source, s.Key.Shown = c.source, "~/.ssh/my key"
		found := false
		for _, l := range s.Overlay(model.View{Screen: model.ScreenApps}, false, 98) {
			found = found || l.Text() == c.want
		}
		if !found {
			t.Errorf("with the key from %s, the apps overlay has no row line %q", c.source, c.want)
		}
	}
}

// TestJSONMadeOnce: a large answer is indented once, not at each frame.
func TestJSONMadeOnce(t *testing.T) {
	s, f, now := session(t)
	big := `{"apps": [{"name": "blog"}], "pad": "` + strings.Repeat("x", 4<<20) + `"}`
	f.Body("apps.json", big)
	step(t, s, f, now)
	step(t, s, f, now)
	start := time.Now()
	for range 50 {
		s.Overlay(model.View{Screen: model.ScreenApps}, true, 98)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("50 frames of a 4 MB answer took %s", d)
	}
}

// TestMeRule: an answer about another key or box is shown as that
// warning only; the title names no one; no box viewer is made of it;
// any role on the box makes one; an expired token is said so.
func TestMeRule(t *testing.T) {
	s, _, now := session(t)
	s.Me = &model.Got[wire.WhoAmI]{V: wire.WhoAmI{Name: "mallory@example.com", Key: "SHA256:other", Box: demo.Host, Version: "v0.7.0", Protocol: 1,
		Roles: []wire.RoleHeld{{Role: "viewer", Scope: "box"}}}, At: *now}
	text := ""
	for _, l := range s.MeView(*now) {
		text += l.Text() + "\n"
	}
	if !strings.Contains(text, `it names "SHA256:other"`) || strings.Contains(text, "you are     ") || strings.Contains(text, "from here you may") {
		t.Errorf("who am I about another key shows:\n%s", text)
	}
	if l, _ := s.Title(*now); strings.Contains(l.Text(), "mallory") {
		t.Errorf("the title names %q", l.Text())
	}
	if s.BoxViewer() {
		t.Error("a box viewer made of an answer about another key")
	}
	s.Me.V.Key = demo.Key.Fingerprint
	s.Me.V.Roles = []wire.RoleHeld{{Role: "deployer", Scope: "box"}}
	if !s.BoxViewer() {
		t.Error("a role on the box other than viewer does not offer the box screen")
	}
	s.TokenExpires = func() time.Time { return now.Add(-time.Minute) }
	text = ""
	for _, l := range s.MeView(*now) {
		text += l.Text() + "\n"
	}
	if !strings.Contains(text, "expired at") || strings.Contains(text, "left)") {
		t.Errorf("an expired token is said as:\n%s", text)
	}
}

// TestBoxErrorClears: a failed box read is said, and the word goes when
// it answers again.
func TestBoxErrorClears(t *testing.T) {
	s, f, now := session(t)
	s.Engine.BoxOpen(true, *now)
	f.Set("box", &model.Failure{Kind: model.FailThis, Message: "deploy.example.com answered 409"})
	for range 3 {
		step(t, s, f, now)
	}
	if v := s.BoxView(*now); !strings.Contains(v[0].Text(), "answered 409") {
		t.Errorf("the box's failure is not said: %v", v[0].Text())
	}
	f.Set("box", nil)
	*now = now.Add(model.BoxEvery)
	for range 3 {
		step(t, s, f, now)
	}
	for _, l := range s.BoxView(*now) {
		if strings.Contains(l.Text(), "the last read failed") {
			t.Errorf("the failure is still said after an answer: %q", l.Text())
		}
	}
}

// TestBoxViewIsTheBoxsAnswer: the box screen, under the box answer's
// age, shows nothing from whoami's answer, which may be minutes older:
// not the release it names.
func TestBoxViewIsTheBoxsAnswer(t *testing.T) {
	s, f, now := session(t)
	s.Engine.BoxOpen(true, *now)
	for s.BoxSt == nil || s.Me == nil {
		step(t, s, f, now)
	}
	rel := s.Me.V.Version
	if rel == "" {
		t.Fatal("the sample whoami names no release")
	}
	for _, l := range s.BoxView(*now) {
		if strings.Contains(l.Text(), rel) || strings.Contains(l.Text(), "protocol") {
			t.Errorf("the box screen shows whoami's answer: %q", l.Text())
		}
	}
}

// TestTailCap: a log tail line past TailMax is cut and marked.
func TestTailCap(t *testing.T) {
	s, f, now := session(t)
	long := strings.Repeat("y", model.TailMax+50)
	f.Body("deploys/blog/7c1e55d0a913.json", `{"version":"7c1e55d0a913","status":"failed","phase":"soaking","started_at":"@-1m","finished_at":"@-0s","detail":{"log_tail":["`+long+`"]}}`)
	s.Engine.Once(model.Job{Kind: model.KDeploy, App: "blog", Version: "7c1e55d0a913"}, *now)
	for range 4 {
		step(t, s, f, now)
	}
	v := s.RecordView("blog", "7c1e55d0a913", *now)
	if len(v.Tail) != 1 || !strings.HasSuffix(v.Tail[0].String(), "…[cut]") || len([]rune(v.Tail[0].String())) != model.TailMax+len([]rune("…[cut]")) {
		t.Fatalf("the tail line is %d runes", len([]rune(v.Tail[0].String())))
	}
}

// TestNotThisKey: whoami about another key, or as another box, stops
// every read, says why with the box's values quoted, and sets no one
// as who you are.
func TestNotThisKey(t *testing.T) {
	for _, body := range []string{
		`{"name":"mallory@example.com","key":"SHA256:OTHER00000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`,
		`{"name":"alice@example.com","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[],"box":"evil.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`,
	} {
		s, f, now := session(t)
		f.Body("whoami.json", body)
		step(t, s, f, now)
		if s.Engine.Stopped() != model.NotThisKey || s.Me != nil {
			t.Fatalf("stopped %v, me %v", s.Engine.Stopped(), s.Me)
		}
		msg := s.StopErr.Error()
		if !strings.Contains(msg, `it names "SHA256:OTHER`) && !strings.Contains(msg, `it names "evil.example.com"`) {
			t.Errorf("the stop says %q", msg)
		}
		if _, ok := s.Printable(model.View{Screen: model.ScreenRefused}, 0); ok {
			t.Error("P offered on the stop of an answer about another key")
		}
		if _, r := s.Title(*now); !strings.Contains(r.Text(), "another key or box") {
			t.Errorf("the title says %q", r.Text())
		}
	}
}

// TestPrintedBox: the printed command names the port the session asks,
// and --key exactly when the person named the key with it; a leading =
// or ~ is quoted.
func TestPrintedBox(t *testing.T) {
	s := &model.Session{Host: "deploy.example.com", Addr: "deploy.example.com:8443", Key: model.Key{Source: "MOTEL_KEY", Shown: "~/k.pub"}, Now: func() time.Time { return t0 }, Engine: model.NewEngine(t0)}
	if c, _ := s.Command(model.View{Screen: model.ScreenApps}); c != "motel apps --box deploy.example.com:8443" {
		t.Errorf("printed %q", c)
	}
	s.Key.Source = "--key"
	if c, _ := s.Command(model.View{Screen: model.ScreenApps}); c != "motel apps --box deploy.example.com:8443 --key '~/k.pub'" {
		t.Errorf("printed %q", c)
	}
	for _, a := range []string{"=ls", "~root", "~"} {
		if q := model.ShellQuote(a); q == a {
			t.Errorf("%q printed bare", a)
		}
	}
}

// TestOverlayFollowsTheAnswer: the JSON overlay is made once per
// answer, and a new answer makes it again.
func TestOverlayFollowsTheAnswer(t *testing.T) {
	s, f, now := session(t)
	for range 2 {
		step(t, s, f, now)
	}
	v := model.View{Screen: model.ScreenApps}
	text := func() string {
		out := ""
		for _, l := range s.Overlay(v, true, 98) {
			out += l.Text() + "\n"
		}
		return out
	}
	if !strings.Contains(text(), `"blog"`) {
		t.Fatal("the list is not in the overlay")
	}
	f.Body("apps.json", `{"apps": [{"name": "zeta"}]}`)
	*now = now.Add(model.AppsEvery)
	for range 20 {
		step(t, s, f, now)
		if names := s.Names(); len(names) == 1 {
			break
		}
	}
	if got := text(); !strings.Contains(got, `"zeta"`) || strings.Contains(got, `"blog"`) {
		t.Errorf("after a new answer the overlay shows:\n%s", got)
	}
}

// TestJSONLinesCut: an answer over OverlayLines is cut for the screen
// and says how much; one that does not indent says so.
func TestJSONLinesCut(t *testing.T) {
	big := "[" + strings.Repeat("1,", model.OverlayLines+10) + "1]"
	lines, cut, indented := model.JSONLines([]byte(big))
	if len(lines) != model.OverlayLines || cut <= 0 || !indented {
		t.Errorf("%d lines, %d cut, indented %v", len(lines), cut, indented)
	}
	if _, _, indented := model.JSONLines([]byte("not json\nat all")); indented {
		t.Error("a body that does not indent is said to")
	}
}

// TestForbiddenAsksWhoAmI: a 403 asks whoami again (a grant may be
// gone), at most once in MeEvery; and whoami is read every MeAlways.
// The whoami a 403 asks is refused, so its last answer stays old and
// only the once in MeEvery holds back the 403s of the other apps.
func TestForbiddenAsksWhoAmI(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(3), t0, time.Second, time.Millisecond, ok)
	at := t0.Add(2 * time.Minute)
	n := 0
	calls := sim(e, names(3), at, 2*time.Minute, 10*time.Millisecond, func(j model.Job, _ time.Time) error {
		switch j.Kind {
		case model.KStatus:
			n++
			return fail(model.FailForbidden)
		case model.KWhoAmI:
			return fail(model.FailThis)
		}
		return nil
	})
	me := 0
	for _, c := range calls {
		if c.j.Kind == model.KWhoAmI {
			me++
		}
	}
	if me != 1 {
		t.Errorf("%d 403s asked whoami %d times in two minutes, want once", n, me)
	}
	calls = sim(e, names(3), at.Add(2*time.Minute), model.MeAlways+time.Second, 10*time.Millisecond, ok)
	me = 0
	for _, c := range calls {
		if c.j.Kind == model.KWhoAmI {
			me++
		}
	}
	if me < 1 {
		t.Error("whoami not read in MeAlways")
	}
}
