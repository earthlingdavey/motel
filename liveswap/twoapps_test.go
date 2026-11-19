package liveswap

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The two-apps-two-people test (DESIGN-access.md, "Scoping as
// structure"): one webhook running two apps, each deployed, each with
// a canary value in its env_file, and every kind of caller. For every
// row of both route tables and every caller it asserts the status the
// action table gives, and that no 200 holds the name, the canary or
// the socket path of an app the caller may not view; and that each row
// of the app list holds nothing of the other app, and its own canary
// only as its filter's marker. Alertmanager holds an alert of each app
// and a box event naming the first: an app-scoped row's answer holds
// its own app's alert alone. It reads the route tables, and a row
// whose shape it does not know fails it, so a route added without its
// expectations fails here.

// twoApps are the test's apps: names no JSON key or word of a body
// holds by chance.
var twoApps = []string{"appalpha", "appbravo"}

type twoAppsRig struct {
	h       *Handler
	deploy  map[string]ed25519.PrivateKey // each app's deploy source
	canary  map[string]string
	sockets map[string]string // each app's run directory, which its socket is under
	people  map[string]accesstest.Key
	views   map[string][]string // the apps each caller may read
}

func newTwoAppsRig(t *testing.T) *twoAppsRig {
	t.Helper()
	r := &twoAppsRig{
		deploy: map[string]ed25519.PrivateKey{}, canary: map[string]string{}, sockets: map[string]string{},
		people: map[string]accesstest.Key{
			"viewer of appalpha":  accesstest.Ed25519(),
			"viewer of appbravo":  accesstest.SKEd25519(),
			"viewer of the box":   accesstest.Ed25519(),
			"no grant":            accesstest.Ed25519(),
			"a key no line lists": accesstest.Ed25519(),
		},
		views: map[string][]string{
			"viewer of appalpha":   {"appalpha"},
			"viewer of appbravo":   {"appbravo"},
			"viewer of the box":    twoApps,
			"deployer of appalpha": {"appalpha"},
			"deployer of appbravo": {"appbravo"},
		},
	}
	app := &App{managed: map[string]*managedApp{}, observed: new(sync.Map)}
	for _, name := range twoApps {
		priv, pub := trusttest.GenerateKey()
		r.deploy[name] = priv
		r.canary[name] = "canary-" + name + "-5f1d0c7e9a"
		spec := testSpec(t)
		spec.name = name
		spec.dirs = newAppDirs(shortTempDir(t), name)
		spec.trust = []deploytrust.Source{localSource(t, pub, name)}
		spec.envFile = filepath.Join(t.TempDir(), name+".env")
		if err := os.WriteFile(spec.envFile, []byte("SECRET_TOKEN="+r.canary[name]+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ma := newManagedApp(name)
		ma.spec = spec
		ma.verifiers = deploytrust.Verifiers(spec.trust, nil)
		ma.runner, ma.prober, ma.fetch, ma.clock, ma.store = &fakeRunner{}, &fakeProber{}, &fakeFetcher{}, newFakeClock(), &fakeStore{}
		ma.logger = zap.NewNop()
		ma.wdCtx, ma.wdCancel = context.WithCancel(context.Background())
		t.Cleanup(ma.wdCancel)
		if err := ma.Deploy(context.Background(), deployRequest{url: "https://x/" + name + ".tgz", version: "v1"}); err != nil {
			t.Fatal(err)
		}
		// Then a deploy whose version and deployer hold the canary, which
		// fails: its record is the newest (the version sorts after v1, for
		// two records written in one tick), so the canary is in two fields
		// the app list's summary carries, newest_deploy's version and
		// deployed_by, as a deployer could shape them.
		fetch := ma.fetch.(*fakeFetcher)
		fetch.err = errors.New("the download failed")
		if err := ma.Deploy(context.Background(), deployRequest{url: "https://x/" + name + "2.tgz", version: "z-" + r.canary[name], by: "local:" + r.canary[name]}); err == nil {
			t.Fatal("the canary's deploy succeeded")
		}
		fetch.err = nil
		r.sockets[name] = spec.dirs.root
		app.managed[name] = ma
	}
	var l access.Lines
	for who, k := range r.people {
		if who == "a key no line lists" {
			continue
		}
		key, err := access.ParsePerson([]string{strings.ReplaceAll(who, " ", "-"), k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, args := range [][]string{
		{"viewer-of-appalpha", "viewer", "app", "appalpha"},
		{"viewer-of-appbravo", "viewer", "app", "appbravo"},
		{"viewer-of-the-box", "viewer", "box"},
	} {
		g, err := access.ParseGrant(args)
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	if _, err := access.CheckLines(l, &access.Around{Apps: twoApps, Channels: true}); err != nil {
		t.Fatal(err)
	}
	r.h = &Handler{app: app, logger: zap.NewNop(), limiter: deploytrust.NewLimiter(newFakeClock()), people: access.NewRegistry(personBox, l)}
	// Alertmanager holds an alert of each app, its summary holding the
	// app's alert canary, and a box event that names appalpha in its
	// instance and summary, as backup_stale/<app> does: no answer to any
	// caller may hold another app's alert, and none the box's.
	held := []AppAlert{{Alert: wire.Alert{Name: "backup_stale", Severity: "critical",
		Summary: "the backup of appalpha is stale: " + boxAlertCanary, State: "active"}}}
	for _, name := range twoApps {
		held = append(held, AppAlert{App: name, Alert: wire.Alert{Name: "errors_high", Severity: "warning",
			Summary: name + ": " + alertCanary(name), State: "active"}})
	}
	app.SetAlertsSource(&fakeAlerts{held: held, ok: true})
	return r
}

// boxAlertCanary is in the box's own alert, which no app's read holds.
const boxAlertCanary = "box-canary-0c41e5"

// alertCanary is in app's alert, which only a read of app holds.
func alertCanary(app string) string { return "alert-canary-" + app + "-77e2" }

// authorization is a caller's header: a person's token, a deploy
// source's, the box channel's (a source liveswap does not trust), or
// none.
func (r *twoAppsRig) authorization(t *testing.T, caller string) string {
	if k, ok := r.people[caller]; ok {
		return token(k)
	}
	switch caller {
	case "deployer of appalpha":
		return "Bearer " + trusttest.Mint(t, r.deploy["appalpha"], "appalpha", nil)
	case "deployer of appbravo":
		return "Bearer " + trusttest.Mint(t, r.deploy["appbravo"], "appbravo", nil)
	case "the box channel's source":
		priv, _ := trusttest.GenerateKey()
		return "Bearer " + trusttest.Mint(t, priv, "box", nil)
	}
	return ""
}

// motelAppRequest is a request of an app-scoped row under /_motel/ on
// app, or nil for a read this test does not know.
func motelAppRequest(rt motelAppRoute, app string) *http.Request {
	if rt.read == "alerts" {
		return httptest.NewRequest(http.MethodGet, "http://"+personBox+motelAppsPrefix+app+"/alerts", nil)
	}
	return nil
}

// rowRequest is a request of the row's shape on app, or nil for a shape
// this test does not know: the row then has no expectations.
func rowRequest(rt route, app string) *http.Request {
	var req *http.Request
	switch rt.shape {
	case "?deploy=<v>":
		req = httptest.NewRequest(rt.method, "http://"+personBox+"/"+app+"?deploy=v1", nil)
	case "[?range=1h|24h]":
		req = httptest.NewRequest(rt.method, "http://"+personBox+"/"+app+"?range=24h", nil)
	case "a gzip body [?version=<v>]":
		req = httptest.NewRequest(rt.method, "http://"+personBox+"/"+app+"?version=v9", strings.NewReader("not a gzip"))
		req.Header.Set("Content-Type", "application/gzip")
	case "?rollback=<v>":
		req = httptest.NewRequest(rt.method, "http://"+personBox+"/"+app+"?rollback=v1", nil)
	case "a JSON body":
		req = httptest.NewRequest(rt.method, "http://"+personBox+"/"+app, strings.NewReader(`{"url":"https://x/`+app+`.tgz","version":"v1"}`))
	default:
		return nil
	}
	return req
}

func TestTwoAppsTwoPeople(t *testing.T) {
	callers := []string{
		"viewer of appalpha", "viewer of appbravo", "viewer of the box", "no grant", "a key no line lists",
		"deployer of appalpha", "deployer of appbravo", "the box channel's source", "no token",
	}
	r := newTwoAppsRig(t)
	views := func(caller, app string) bool {
		for _, a := range r.views[caller] {
			if a == app {
				return true
			}
		}
		return false
	}
	person := func(caller string) bool {
		_, ok := r.people[caller]
		return ok && caller != "a key no line lists"
	}
	// leaks fails a 200 that names an app the caller may not view, its
	// canary, or its socket's directory.
	leaks := func(what, caller, body string, may func(app string) bool) {
		t.Helper()
		for _, other := range twoApps {
			if may(other) {
				continue
			}
			for _, s := range []string{other, r.canary[other], r.sockets[other]} {
				if strings.Contains(body, s) {
					t.Errorf("%s, %s: the 200 holds %q: %s", caller, what, s, body)
				}
			}
		}
	}
	// The rows under /_motel/ first, while each app's newest deploy is
	// still the canary's: the deployers' acts below change it.
	for _, rt := range motelRoutes {
		for _, caller := range callers {
			req := httptest.NewRequest(http.MethodGet, "http://"+personBox+rt.path, nil)
			if auth := r.authorization(t, caller); auth != "" {
				req.Header.Set("Authorization", auth)
			}
			w := httptest.NewRecorder()
			if err := r.h.ServeHTTP(w, req, nil); err != nil {
				t.Fatal(err)
			}
			want := http.StatusUnauthorized // a deploy token is judged against the global sources: none here
			switch {
			case person(caller) && rt.action == access.BoxBackups && caller != "viewer of the box":
				// The box's backups name every app: a viewer of the box's alone.
				want = http.StatusForbidden
			case person(caller):
				want = http.StatusOK
			}
			if w.Code != want {
				t.Errorf("%s, GET %s: %d %s, want %d", caller, rt.path, w.Code, w.Body, want)
			}
			if w.Code == http.StatusOK {
				leaks("GET "+rt.path, caller, w.Body.String(), func(other string) bool { return views(caller, other) })
			}
			if w.Code == http.StatusOK && rt.action == access.BoxApps {
				r.rowsKeepToTheirApp(t, caller, w.Body.Bytes(), r.views[caller])
			}
		}
	}
	// The app-scoped rows under /_motel/: each app's for every caller,
	// a person's only, as far as their grants cover it.
	for _, rt := range motelAppRoutes {
		for _, app := range twoApps {
			for _, caller := range callers {
				req := motelAppRequest(rt, app)
				if req == nil {
					t.Fatalf("the row %s<app>/%s has no request in this test: write its expectations", motelAppsPrefix, rt.read)
				}
				if auth := r.authorization(t, caller); auth != "" {
					req.Header.Set("Authorization", auth)
				}
				w := httptest.NewRecorder()
				if err := r.h.ServeHTTP(w, req, nil); err != nil {
					t.Fatal(err)
				}
				what := "GET " + req.URL.Path
				want := http.StatusUnauthorized // a deploy token is judged against the global sources: none here
				switch {
				case person(caller) && views(caller, app):
					want = http.StatusOK
				case person(caller):
					want = http.StatusForbidden
				}
				if w.Code != want {
					t.Errorf("%s, %s: %d %s, want %d", caller, what, w.Code, w.Body, want)
				}
				body := w.Body.String()
				for _, other := range twoApps {
					if other != app && strings.Contains(body, alertCanary(other)) {
						t.Errorf("%s, %s: the answer holds %s's alert: %s", caller, what, other, body)
					}
				}
				if strings.Contains(body, boxAlertCanary) {
					t.Errorf("%s, %s: the answer holds the box's alert: %s", caller, what, body)
				}
				if w.Code == http.StatusOK {
					leaks(what, caller, body, func(other string) bool { return other == app })
					if !strings.Contains(body, alertCanary(app)) {
						t.Errorf("%s, %s: the 200 does not hold its own app's alert: %s", caller, what, body)
					}
				}
			}
		}
	}
	for _, rt := range routes {
		for _, app := range twoApps {
			for _, caller := range callers {
				req := rowRequest(rt, app)
				if req == nil {
					t.Fatalf("the row %s %s has no request in this test: write its expectations", rt.method, rt.shape)
				}
				if auth := r.authorization(t, caller); auth != "" {
					req.Header.Set("Authorization", auth)
				}
				w := httptest.NewRecorder()
				if err := r.h.ServeHTTP(w, req, nil); err != nil {
					t.Fatal(err)
				}
				what := rt.method + " /" + app + " " + rt.shape
				read := rt.method == http.MethodGet
				var want []int // any of these
				switch {
				case caller == "deployer of "+app && read:
					want = []int{http.StatusOK}
				case caller == "deployer of "+app:
					// The act is taken: its answer is the pipeline's, never the
					// preamble's or Decide's.
					want = []int{http.StatusOK, http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError}
				case person(caller) && read && views(caller, app):
					want = []int{http.StatusOK}
				case person(caller) && read:
					want = []int{http.StatusForbidden}
				default:
					// A stranger, an unlisted key, another app's deployer, the
					// box's source, and a person's token on anything but a GET.
					want = []int{http.StatusUnauthorized}
				}
				ok := false
				for _, code := range want {
					ok = ok || w.Code == code
				}
				if !ok {
					t.Errorf("%s, %s: %d %s, want one of %v", caller, what, w.Code, w.Body, want)
				}
				if w.Code == http.StatusOK {
					leaks(what, caller, w.Body.String(), func(other string) bool { return other == app })
				}
			}
		}
	}
}

// rowsKeepToTheirApp holds GET /_motel/apps's body, row by row: the
// rows are the apps the caller may view, each holds nothing of the
// other app (its name, canary or socket directory), and its own app's
// canary, which its summary carries, comes out as that app's filter's
// marker and never as itself.
func (r *twoAppsRig) rowsKeepToTheirApp(t *testing.T, caller string, body []byte, views []string) {
	t.Helper()
	var list struct {
		Apps []json.RawMessage `json:"apps"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("%s, GET /_motel/apps: %v: %s", caller, err, body)
	}
	if len(list.Apps) != len(views) {
		t.Errorf("%s, GET /_motel/apps: %d rows, want %v: %s", caller, len(list.Apps), views, body)
	}
	for i, raw := range list.Apps {
		row := string(raw)
		if i >= len(views) || !strings.HasPrefix(row, `{"name":"`+views[i]+`",`) {
			t.Errorf("%s, GET /_motel/apps: row %d is %s, want %v in order", caller, i, row, views)
			continue
		}
		app := views[i]
		for _, other := range twoApps {
			if other == app {
				continue
			}
			for _, s := range []string{other, r.canary[other], r.sockets[other]} {
				if strings.Contains(row, s) {
					t.Errorf("%s, GET /_motel/apps: %s's row holds %q: %s", caller, app, s, row)
				}
			}
		}
		if strings.Contains(row, r.canary[app]) || !strings.Contains(row, `"version":"z-[redacted:SECRET_TOKEN]"`) ||
			!strings.Contains(row, `"redacted_env":["SECRET_TOKEN"]`) || strings.Contains(row, r.sockets[app]) {
			t.Errorf("%s, GET /_motel/apps: %s's row does not hold its canary as the marker: %s", caller, app, row)
		}
	}
}
