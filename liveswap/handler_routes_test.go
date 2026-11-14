package liveswap

import (
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

// TestEveryRouteHasAnAction: each row of the route table names an app
// action Decide's table knows, a method, and a function to answer.
func TestEveryRouteHasAnAction(t *testing.T) {
	for _, rt := range routes {
		name := rt.method + " /<app>" + rt.shape
		switch {
		case !slices.Contains(access.Actions(), rt.action):
			t.Errorf("%s: action %q is not in Decide's table", name, rt.action)
		case !strings.HasPrefix(string(rt.action), "app."):
			t.Errorf("%s: action %q is not an app action", name, rt.action)
		}
		if rt.method != http.MethodGet && rt.method != http.MethodPost {
			t.Errorf("%s: a method the 405's Allow does not name", name)
		}
		if rt.serve == nil {
			t.Errorf("%s: no function answers it", name)
		}
	}
}

// TestEveryActionHasARoute is the reverse: every app action Decide
// allows to someone has a row, so a row cannot be removed and leave
// its action allowed with no route. The box actions are box's to hold.
func TestEveryActionHasARoute(t *testing.T) {
	for _, a := range access.Actions() {
		if !strings.HasPrefix(string(a), "app.") {
			continue
		}
		if !slices.ContainsFunc(routes, func(rt route) bool { return rt.action == a }) {
			t.Errorf("action %q has no route on liveswap_webhook", a)
		}
	}
}

// FuzzNoRouteNoAnswer drives the handler with any method, path and
// query, for each kind of caller: a stranger, a deploy source of the
// app demo, and a global source. The route table's functions are
// swapped for ones that record the call, so the test reads the table
// and covers a row as it is added. A request reaches a row's function
// only when a deployer of the app in its path sends it and a row
// matches; every other request is answered 401, 404 or 405, exactly
// where today's handler answers them: 401 for a token the path's
// sources refuse (an unknown name and a stranger alike), 404 for a
// global source on a name that is no app whatever the method, 405 for
// a deployer of the app with no row.
func FuzzNoRouteNoAnswer(f *testing.F) {
	for _, seed := range [][3]string{
		{"GET", "/demo", ""},
		{"GET", "/demo", "deploy=v1"},
		{"GET", "/demo", "deploy=%ZZ"},
		{"GET", "/demo", "range=24h"},
		{"POST", "/demo", "rollback=v1"},
		{"POST", "/x/demo", "version=v2"},
		{"DELETE", "/demo", ""},
		{"PUT", "/ghost", ""},
		{"HEAD", "/demo", ""},
		{"OPTIONS", "/", ""},
		{"get", "/demo", ""},
		{"PATCH", "/_motel/demo", "rollback=v1"},
		{"GET", "/ghost", "deploy=v1"},
		{"POST", "/demo/", ""},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}

	saved := routes
	f.Cleanup(func() { routes = saved })
	var served *route
	routes = slices.Clone(saved)
	for i := range routes {
		routes[i].serve = func(_ *Handler, _ http.ResponseWriter, _ *http.Request, _ *managedApp, _ string) error {
			served = &routes[i]
			return nil
		}
	}

	f.Fuzz(func(t *testing.T, method, target, query string) {
		if method == "" || strings.ContainsAny(method, " \t\r\n") {
			return // not a method net/http would hand a handler
		}
		h, rig := newTestHandler(t)
		for _, caller := range []string{"stranger", "deployer of demo", "global source"} {
			h.limiter = deploytrust.NewLimiter(rig.clock)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Method = method
			req.URL.Path = target
			req.URL.RawQuery = query
			switch caller {
			case "deployer of demo":
				req.Header.Set("Authorization", "Bearer "+appToken(t))
			case "global source":
				req.Header.Set("Authorization", "Bearer "+globalToken(t))
			}
			served = nil
			w := httptest.NewRecorder()
			var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
				t.Fatal("liveswap_webhook passed a request on")
				return nil
			})
			if err := h.ServeHTTP(w, req, next); err != nil {
				t.Fatal(err)
			}

			app := path.Base(path.Clean(target)) == "demo"
			row := findRoute(req)
			want := http.StatusUnauthorized
			switch {
			case caller == "deployer of demo" && app && row != nil:
				want = http.StatusOK // the recording function's
			case caller == "deployer of demo" && app:
				want = http.StatusMethodNotAllowed
			case caller == "global source" && !app:
				want = http.StatusNotFound
			}
			if w.Code != want {
				t.Fatalf("%s: %s %q ?%q = %d, want %d: %s", caller, method, target, query, w.Code, want, w.Body)
			}
			switch {
			case want == http.StatusOK && served != row:
				t.Fatalf("%s: %s %q ?%q was answered by %v, want the row %s %s", caller, method, target, query, served, row.method, row.shape)
			case want != http.StatusOK && served != nil:
				t.Fatalf("%s: %s %q ?%q reached the row %s %s and was answered %d", caller, method, target, query, served.method, served.shape, w.Code)
			}
		}
	})
}
