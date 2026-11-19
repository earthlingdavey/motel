package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// TestWhoamiAboutAnotherBeforeItsGrammar: an answer naming another key
// or box is about another whatever else in it is outside its grammar or
// missing, the other of the two among them, through the real client,
// for the command and for the TUI alike: the command prints the warning
// alone and exits 3, and the TUI stops every read, lists nothing and
// holds no who you are. An answer about this key with a release that
// is not N.N.N, or with no box, is still one the client cannot use, and
// no stop.
func TestWhoamiAboutAnotherBeforeItsGrammar(t *testing.T) {
	other := "SHA256:OTHEROTHEROTHEROTHEROTHEROTHEROTHEROTHEROTH"
	for _, c := range []struct {
		name    string
		edit    func(*wire.WhoAmI)
		raw     func(map[string]any)
		another bool
	}{
		{"another key", func(me *wire.WhoAmI) { me.Key = other }, nil, true},
		{"another key, release not N.N.N", func(me *wire.WhoAmI) { me.Key, me.Version = other, "dev" }, nil, true},
		{"another key and box, a role outside the grammar", func(me *wire.WhoAmI) {
			me.Key, me.Box = other, "evil.example.com"
			me.Roles = []wire.RoleHeld{{Role: "Viewer", Scope: "box"}}
		}, nil, true},
		{"another key, no name and an expiry that is no time", func(me *wire.WhoAmI) { me.Key = other },
			func(m map[string]any) { m["name"], m["token_expires_at"] = nil, "soon" }, true},
		{"another key, no box", func(me *wire.WhoAmI) { me.Key = other }, func(m map[string]any) { delete(m, "box") }, true},
		{"another key, a box that is null", func(me *wire.WhoAmI) { me.Key = other }, func(m map[string]any) { m["box"] = nil }, true},
		{"another box, a key that is not a string", func(me *wire.WhoAmI) { me.Box = "evil.example.com" }, func(m map[string]any) { m["key"] = 7 }, true},
		{"this key, release not N.N.N", func(me *wire.WhoAmI) { me.Version = "dev" }, nil, false},
		{"this key, no box", func(*wire.WhoAmI) {}, func(m map[string]any) { delete(m, "box") }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fb := newFakeBox(t)
			fb.apps = wire.Apps{Apps: []wire.AppEntry{{Name: "blog"}}} // the one app this fake answers for
			c.edit(&fb.whoami)
			if c.raw != nil {
				// A body the wire type cannot write: whoami's own route
				// answers it, and the rest as before.
				b, _ := json.Marshal(fb.whoami)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				c.raw(m)
				body, _ := json.Marshal(m)
				next := fb.srv.Config.Handler
				fb.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/_motel/whoami" {
						next.ServeHTTP(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(body)
				}))
				t.Cleanup(fb.srv.Close)
			}

			r := runKey(t, fb, nil, "whoami")
			warned := strings.HasPrefix(r.stdout, "warning  the box ")
			if c.another {
				if r.code != 3 || !warned || !strings.Contains(r.stderr, "is not one to act on") {
					t.Errorf("whoami: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
				}
			} else if why := map[bool]string{false: "its version is not a release", true: "it has no box"}[c.raw != nil]; r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "not moteld's answer: "+why) {
				t.Errorf("whoami: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
			}

			s := tuiOn(t, fb)
			for range 5 {
				s.Engine.Touch(whoamiNow)
				j, ok, _ := s.Engine.Take(whoamiNow)
				if !ok {
					break
				}
				apply, err := s.Fetch(context.Background(), live{s.c}, j)
				apply()
				s.Engine.Done(j, whoamiNow, err)
			}
			stopped := s.Engine.Stopped() == model.NotThisKey
			if stopped != c.another || c.another && (len(s.Names()) != 0 || s.Me != nil) {
				t.Errorf("the TUI: stopped %v (want %v), lists %v, who you are: %v", s.Engine.Stopped(), c.another, s.Names(), s.MeErr)
			}
			if !c.another && (s.Me != nil || s.MeErr == nil) {
				t.Errorf("the TUI took an answer that is not moteld's: %v", s.MeErr)
			}
		})
	}
}

type tuiSession struct {
	*model.Session
	c *client.Client
}

// tuiOn is the TUI's client and session as the screen builds them, on fb.
func tuiOn(t *testing.T, fb *fakeBox) tuiSession {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	now := func() time.Time { return whoamiNow }
	r := &runner{env: Env{Getenv: func(k string) string { return env[k] }, Stderr: &bytes.Buffer{}, Dir: t.TempDir(), Home: home,
		Now: now, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: fb.srv.Client().Transport, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test"}}
	cl, err := r.client(context.Background(), &target{}, nil, "", "the screen")
	if err != nil {
		t.Fatal(err)
	}
	forTUI(cl, now)
	s := &model.Session{Now: now, Engine: model.NewEngine(now())}
	boxSession(s, cl, cl.Cred.(*client.PersonToken))
	return tuiSession{s, cl}
}
