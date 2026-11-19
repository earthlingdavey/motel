package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/tui"
	tuihostile "github.com/smallhoursorg/motel/client/internal/tui/hostile"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// hostileNow is the clock of the hostile run through the real client.
var hostileNow = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

// fixture is a body of the TUI's fixtures with its times made real, as
// demo.Fake makes them: "@-70m" is seventy minutes before hostileNow.
func fixture(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "tui", "demo", "fixtures", path))
	if err != nil {
		t.Fatal(err)
	}
	units := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}
	b = regexp.MustCompile(`"@(-?[0-9]+)([smhd])"`).ReplaceAllFunc(b, func(m []byte) []byte {
		p := regexp.MustCompile(`"@(-?[0-9]+)([smhd])"`).FindSubmatch(m)
		n, _ := strconv.Atoi(string(p[1]))
		return []byte(`"` + hostileNow.Add(time.Duration(n)*units[string(p[2])]).Format(time.RFC3339) + `"`)
	})
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// attacks is every attack of the hostile fixture, each named, in one
// string: what a box puts in each free-text field below. The URL tag
// comes first, so its words are on screen before any cap cuts the rest.
func attacks() string {
	var names []string
	for n := range tuihostile.Payloads {
		if n != "urltag" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	names = append([]string{"urltag"}, names...)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + " <" + tuihostile.Payloads[n] + "> ")
	}
	return b.String()
}

// hostileRun is a box answering from bodies, the TUI's client and
// session over it as tui.go builds them, and the reads made.
type hostileRun struct {
	t   *testing.T
	now time.Time
	s   *model.Session
	c   model.Client
}

func newHostileRun(t *testing.T, bodies map[string]any) *hostileRun {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		v, ok := bodies[r.URL.RequestURI()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}` + "\n"))
			return
		}
		_, _ = w.Write(encode(v))
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &hostileRun{t: t, now: hostileNow}
	wall := func() time.Time { return h.now }
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": srv.URL + "/blog"}
	r := &runner{env: Env{Getenv: func(k string) string { return env[k] }, Stderr: &bytes.Buffer{}, Dir: t.TempDir(), Home: home,
		Now: wall, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: srv.Client().Transport, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test"}}
	cl, err := r.client(context.Background(), &target{}, nil, "", "tui")
	if err != nil {
		t.Fatal(err)
	}
	sig := forTUI(cl, wall)
	h.s = &model.Session{Now: wall, Engine: model.NewEngine(wall()), Version: "test"}
	boxSession(h.s, cl, cl.Cred.(*client.PersonToken))
	h.s.TokenExpires = sig.Expires
	h.c = live{cl}
	// Every read: whoami, the list, the statuses, then blog open, its
	// record v7 and the box.
	h.read()
	h.s.Engine.Focus("blog", h.now)
	h.s.Engine.Once(model.Job{Kind: model.KDeploy, App: "blog", Version: "v7"}, h.now)
	h.s.Engine.BoxOpen(true, h.now)
	h.read()
	return h
}

// read makes every read the engine gives within a few seconds.
func (h *hostileRun) read() {
	for range 50 {
		h.s.Engine.Touch(h.now)
		j, ok, wait := h.s.Engine.Take(h.now)
		if !ok {
			if wait <= 0 || wait > 2*time.Second {
				return
			}
			h.now = h.now.Add(wait)
			continue
		}
		apply, err := h.s.Fetch(context.Background(), h.c, j)
		apply()
		h.s.Engine.Done(j, h.now, err)
		h.now = h.now.Add(10 * time.Millisecond)
	}
}

// draw shows v, then presses keys, on tcell's real terminfo screen, and
// returns every byte tcell wrote.
func (h *hostileRun) draw(v model.View, keys string) []byte {
	h.t.Helper()
	tty := tuihostile.NewTTY()
	scr, err := tuihostile.Screen(tty)
	if err != nil {
		h.t.Fatal(err)
	}
	u := tui.New(h.s, false, v)
	u.App.SetScreen(scr)
	done := make(chan struct{})
	go func() { defer close(done); _ = u.App.Run() }()
	on := func(f func()) {
		var wg sync.WaitGroup
		wg.Add(1)
		u.App.QueueUpdateDraw(func() { f(); wg.Done() })
		wg.Wait()
		wg.Add(1)
		u.App.QueueUpdate(wg.Done) // after the frame drawn above
		wg.Wait()
	}
	on(func() {})
	for _, k := range keys {
		on(func() { u.App.GetInputCapture()(tcell.NewEventKey(tcell.KeyRune, k, tcell.ModNone)) })
	}
	u.App.QueueUpdate(u.App.Stop) // from inside the loop, as the TUI stops
	<-done
	return tty.Bytes()
}

// TestHostileThroughTheClient: the hostile fixture's bodies, and bodies
// the client does take with every attack in each free-text field, sent
// by a box through the real client into the TUI, drawn on a real
// terminfo screen: none of the attacks is in what tcell writes, and the
// text that carried them is on screen, made inert, or the client's
// sentence saying it took no such answer.
func TestHostileThroughTheClient(t *testing.T) {
	who := map[string]any{"name": "alice@example.com", "key": meFingerprint(),
		"roles": []any{map[string]any{"role": "viewer", "scope": "box"}, map[string]any{"role": "viewer", "scope": "app:blog"}},
		"box":   "127.0.0.1", "version": "v0.7.0", "protocol": 1, "token_expires_at": hostileNow.Add(9 * time.Minute).Format(time.RFC3339)}
	apps := map[string]any{"apps": []any{map[string]any{"name": "blog"}}}
	box := fixture(t, "box.json")
	box["box_webhook"] = "127.0.0.1"
	status := fixture(t, "status/hostile.json")
	status["app"] = "blog"
	record := fixture(t, "deploys/hostile/v7.json")

	// The fixture as it is: the client does not take its status (a
	// watchdog state outside the grammar), and does take its record.
	asIs := map[string]any{"/_motel/whoami": who, "/_motel/apps": apps, "/blog": status, "/blog?deploy=v7": record, "/": box}

	// Taken: the status held to its grammars, and every free-text field
	// of each answer the client takes carrying every attack.
	a := attacks()
	taken := fixture(t, "status/hostile.json")
	taken["app"] = "blog"
	wd := taken["watchdog"].(map[string]any)
	wd["state"], wd["last_failure"], wd["last_restart_cause"] = "watching", a, a
	taken["metrics"].(map[string]any)["window"] = "1h"
	taken["command"] = []any{"node", a}
	taken["socket"] = a
	taken["deploying"] = map[string]any{"version": "v8", "deployed_by": a, "started_at": hostileNow.Add(-time.Minute).Format(time.RFC3339)}
	for _, d := range taken["deploys"].([]any) {
		d.(map[string]any)["deployed_by"] = a
	}
	free := func(d map[string]any) map[string]any {
		d["deployed_by"], d["error"], d["note"] = a, a, a
		d["redacted_env"] = []any{a}
		det := d["detail"].(map[string]any)
		det["exit"], det["log_tail"], det["log_tail_error"] = a, []any{a, a}, a
		det["probe"].(map[string]any)["location"] = a
		det["probe"].(map[string]any)["body"] = a
		return d
	}
	free(taken["last_deploy"].(map[string]any))
	takenBox := fixture(t, "box.json")
	takenBox["box_webhook"], takenBox["recipient"] = "127.0.0.1", a
	every := map[string]any{"/_motel/whoami": who, "/_motel/apps": apps, "/blog": taken, "/blog?deploy=v7": free(fixture(t, "deploys/hostile/v7.json")), "/": takenBox}

	views := []struct {
		name string
		v    model.View
		keys string
	}{
		{"the apps", model.View{Screen: model.ScreenApps}, ""},
		{"blog", model.View{Screen: model.ScreenApp, App: "blog"}, ""},
		{"blog's answer", model.View{Screen: model.ScreenApp, App: "blog"}, "y"},
		{"the record", model.View{Screen: model.ScreenRecord, App: "blog", Version: "v7"}, ""},
		{"the record's answer", model.View{Screen: model.ScreenRecord, App: "blog", Version: "v7"}, "y"},
		{"the box", model.View{Screen: model.ScreenBox}, ""},
		{"the box's answer", model.View{Screen: model.ScreenBox}, "y"},
	}
	for _, c := range []struct {
		name   string
		bodies map[string]any
		// shown is what each view's bytes must hold: the inert attack
		// words, or the client's own sentence.
		shown map[string]string
	}{
		{"the fixture as it is", asIs, map[string]string{
			"blog": "not moteld's answer", "the record": "evil.example", "the record's answer": "evil.example"}},
		{"every free-text field", every, map[string]string{
			"blog": "evil.example", "blog's answer": "evil.example",
			"the record": "evil.example", "the record's answer": "evil.example", "the box's answer": "evil.example"}},
	} {
		h := newHostileRun(t, c.bodies)
		if h.s.Engine.Stopped() != model.Running || h.s.Me == nil || len(h.s.Names()) != 1 {
			t.Fatalf("%s: the session did not read: stopped %v, %v", c.name, h.s.Engine.Stopped(), h.s.StopErr)
		}
		if g, err, ok := h.s.Record("blog", "v7"); !ok || g == nil {
			t.Fatalf("%s: the client did not take the record: %v", c.name, err)
		}
		for _, v := range views {
			out := h.draw(v.v, v.keys)
			if found := tuihostile.Scan(out); len(found) > 0 {
				t.Errorf("%s, %s: attacks reached the terminal: %v", c.name, v.name, found)
			}
			if want := c.shown[v.name]; want != "" && !bytes.Contains(out, []byte(want)) {
				t.Errorf("%s, %s: %q is not on screen", c.name, v.name, want)
			}
		}
	}
}
