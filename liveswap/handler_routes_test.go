package liveswap

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"go.uber.org/zap"
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

// TestRouteActions pins the action of each route, written out here
// rather than read from routes: a real request of each shape goes
// through findRoute, and the row it lands on must carry the action
// the case names ("" for none: the 405). Every row must be landed on
// by some case, so a row added is pinned here too. Once people may
// read, a push row labelled app.status would let a reader deploy.
func TestRouteActions(t *testing.T) {
	landed := map[*route]bool{}
	for _, c := range []struct {
		method, query, contentType string
		want                       access.Action
	}{
		{"GET", "", "", access.AppStatus},
		{"GET", "range=1h", "", access.AppStatus},
		{"GET", "deploy=%ZZ", "", access.AppStatus}, // the status route's 422
		{"GET", "deploy=v1", "", access.AppDeployRecord},
		{"GET", "range=24h&deploy=v1", "", access.AppDeployRecord},
		{"GET", "", "application/gzip", access.AppStatus},
		{"POST", "", "application/gzip", access.AppDeploy},
		{"POST", "version=v2", "application/x-gzip", access.AppDeploy},
		{"POST", "rollback=v1", "application/octet-stream", access.AppDeploy}, // a gzip body is a push whatever the query
		{"POST", "", "Application/GZIP; charset=binary", access.AppDeploy},
		{"POST", "rollback=v1", "", access.AppRollback},
		{"POST", "rollback=v1", "application/json", access.AppRollback},
		{"POST", "", "application/json", access.AppDeploy},
		{"POST", "rollback=", "", access.AppDeploy},
		{"DELETE", "", "", ""},
		{"PUT", "", "application/gzip", ""},
		{"HEAD", "", "", ""},
		{"get", "", "", ""},
	} {
		req := httptest.NewRequest(http.MethodGet, "/demo", strings.NewReader("{}"))
		req.Method = c.method
		req.URL.RawQuery = c.query
		if c.contentType != "" {
			req.Header.Set("Content-Type", c.contentType)
		}
		var got access.Action
		if rt := findRoute(req); rt != nil {
			got = rt.action
			landed[rt] = true
		}
		if got != c.want {
			t.Errorf("%s /demo?%s (Content-Type %q) lands on action %q, want %q", c.method, c.query, c.contentType, got, c.want)
		}
	}
	for i := range routes {
		if !landed[&routes[i]] {
			t.Errorf("no case lands on the row %s %s: write its action here", routes[i].method, routes[i].shape)
		}
	}
}

// FuzzNoRouteNoAnswer drives the handler with any method, path,
// query, content type and body, for each kind of caller: a stranger,
// a deploy source of the app demo, and a global source. The route
// table's functions are swapped for ones that record the call
// (recordRows), so the test reads the table
// and covers a row as it is added. A request reaches a row's function
// only when a deployer of the app in its path sends it and a row
// matches; every other request is answered 401, 404 or 405, exactly
// where today's handler answers them: 401 for a token the path's
// sources refuse (an unknown name and a stranger alike), 404 for a
// global source on a name that is no app whatever the method, 405 for
// a deployer of the app with no row. A path under /_motel/ names no app:
// a global source is told 403 on its rows and 404 elsewhere. People
// too: a viewer of demo, of the box, and a person with no grant, whose
// tokens are refused on anything but a GET, reach a row only where
// Decide's table and their grants allow, and are told 403 or 404
// everywhere else, as "Unknown names" says.
func FuzzNoRouteNoAnswer(f *testing.F) {
	// Every row of the table is reached by some seed (checked below,
	// against the real table): the gzip push only with its content type.
	seeds := [][5]string{
		{"GET", "/demo", "", "", ""},
		{"GET", "/demo", "deploy=v1", "", ""},
		{"GET", "/demo", "deploy=%ZZ", "", ""},
		{"GET", "/demo", "range=24h", "application/gzip", ""},
		{"POST", "/demo", "rollback=v1", "", ""},
		{"POST", "/x/demo", "version=v2", "application/gzip", "\x1f\x8b"},
		{"POST", "/demo", "", "application/x-gzip", ""},
		{"POST", "/demo", "rollback=v1", "application/octet-stream", ""},
		{"POST", "/demo", "", "Application/X-GZIP; charset=binary", ""},
		{"POST", "/demo", "", "application/json", `{"url":"https://x/a.tgz","version":"v1"}`},
		{"POST", "/ghost", "", "application/gzip", ""},
		{"DELETE", "/demo", "", "", ""},
		{"PUT", "/ghost", "", "application/gzip", ""},
		{"HEAD", "/demo", "", "", ""},
		{"OPTIONS", "/", "", "", ""},
		{"get", "/demo", "", "", ""},
		{"PATCH", "/_motel/demo", "rollback=v1", "", ""},
		{"GET", "/_motel/whoami", "", "", ""},
		{"GET", "/_motel/apps", "x=1", "", ""},
		{"GET", "/_motel", "", "", ""},
		{"POST", "/_motel/apps", "", "", ""},
		{"GET", "/ghost", "deploy=v1", "", ""},
		{"POST", "/demo/", "", "", ""},
	}
	reached := map[*route]bool{}
	for _, seed := range seeds {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4])
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Method, req.URL.Path, req.URL.RawQuery = seed[0], seed[1], seed[2]
		if seed[3] != "" {
			req.Header.Set("Content-Type", seed[3])
		}
		reached[findRoute(req)] = true
	}
	for i := range routes {
		if !reached[&routes[i]] {
			f.Fatalf("no seed reaches the row %s %s", routes[i].method, routes[i].shape)
		}
	}

	served := recordRows(f)
	// People: a viewer of demo, a viewer of the box, and a person with
	// no grant; their tokens are made once, for the run.
	people := newTestPeople(f)
	personTokens := map[string]string{
		"viewer of demo":       token(people.alice),
		"viewer of the box":    token(people.carol),
		"person with no grant": token(people.dave),
	}

	f.Fuzz(func(t *testing.T, method, target, query, contentType, body string) {
		if method == "" || strings.ContainsAny(method, " \t\r\n") {
			return // not a method net/http would hand a handler
		}
		h, rig := newTestHandler(t)
		h.people = people.reg
		for _, caller := range []string{"stranger", "deployer of demo", "global source", "viewer of demo", "viewer of the box", "person with no grant"} {
			h.limiter = deploytrust.NewLimiter(rig.clock)
			req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/", strings.NewReader(body))
			req.Method = method
			req.URL.Path = target
			req.URL.RawQuery = query
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			switch caller {
			case "deployer of demo":
				req.Header.Set("Authorization", "Bearer "+appToken(t))
			case "global source":
				req.Header.Set("Authorization", "Bearer "+globalToken(t))
			default:
				if tok, ok := personTokens[caller]; ok {
					req.Header.Set("Authorization", tok)
				}
			}
			*served = nil
			w := httptest.NewRecorder()
			var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
				t.Fatal("liveswap_webhook passed a request on")
				return nil
			})
			if err := h.ServeHTTP(w, req, next); err != nil {
				t.Fatal(err)
			}

			// A path under /_motel/ names no app, whatever its last
			// segment: a global source is told 403 on its rows (they are
			// a person's) and 404 on the rest.
			clean := path.Clean(target)
			motel := clean == motelPrefix || strings.HasPrefix(clean, motelPrefix+"/")
			app := !motel && path.Base(clean) == "demo"
			row := findRoute(req)
			_, person := personTokens[caller]
			get := method == http.MethodGet
			want := http.StatusUnauthorized
			switch {
			case caller == "deployer of demo" && app && row != nil:
				want = http.StatusOK // the recording function's
			case caller == "deployer of demo" && app:
				want = http.StatusMethodNotAllowed
			case caller == "global source" && motel && findMotelRoute(req) != nil:
				want = http.StatusForbidden
			case caller == "global source" && !app:
				want = http.StatusNotFound
			// A person's token is good on a GET alone. Under /_motel/ its
			// rows answer every person and nothing else does; on an app's
			// path Decide comes before the app is looked for.
			case person && !get:
			case person && motel && findMotelRoute(req) != nil:
				want = http.StatusOK
				row = nil // answered by its own function, not a recorded row
			case person && motel:
				want = http.StatusNotFound
			case person && app && caller != "person with no grant":
				want = http.StatusOK
			case person && caller == "viewer of the box":
				want = http.StatusNotFound
			case person:
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Fatalf("%s: %s %q ?%q = %d, want %d: %s", caller, method, target, query, w.Code, want, w.Body)
			}
			switch {
			case want == http.StatusOK && *served != row:
				t.Fatalf("%s: %s %q ?%q was answered by %v, want the row %v", caller, method, target, query, *served, row)
			case want != http.StatusOK && *served != nil:
				t.Fatalf("%s: %s %q ?%q reached the row %s %s and was answered %d", caller, method, target, query, (*served).method, (*served).shape, w.Code)
			}
		}
	})
}

// recordRows swaps each row's function for one that records its row
// in *served, until the test ends, so a test sees which row answered
// without deploying anything.
func recordRows(tb testing.TB) **route {
	saved := routes
	tb.Cleanup(func() { routes = saved })
	served := new(*route)
	routes = slices.Clone(saved)
	for i := range routes {
		routes[i].serve = func(*Handler, http.ResponseWriter, *http.Request, appHandle, string) error {
			*served = &routes[i]
			return nil
		}
	}
	return served
}

// decideCall is one question the handler put to Decide.
type decideCall struct {
	p        access.Principal
	a        access.Action
	resource string
}

// recordDecide swaps decide for one that records each call and answers
// with refuse's error, until the test ends.
func recordDecide(tb testing.TB, refuse error) *[]decideCall {
	saved := decide
	tb.Cleanup(func() { decide = saved })
	calls := new([]decideCall)
	decide = func(p access.Principal, a access.Action, resource string) error {
		*calls = append(*calls, decideCall{p, a, resource})
		return refuse
	}
	return calls
}

// TestHandlerAsksDecide: for a request of every row, sent by a deploy
// source of the app in its path, the handler asks Decide once, with a
// deployer of that app, the row's action and the app as the resource;
// the row's function answers when Decide allows, and when it refuses
// the function does not run and the answer is the 403. A second app,
// blog, holds the principal's app and the resource to the request's
// app rather than to any one name.
func TestHandlerAsksDecide(t *testing.T) {
	served := recordRows(t)
	landed := map[*route]bool{}
	for _, c := range []struct{ method, target, contentType, app string }{
		{"GET", "/demo", "", "demo"},
		{"GET", "/demo?deploy=v1", "", "demo"},
		{"POST", "/x/demo?version=v2", "application/gzip", "demo"},
		{"POST", "/demo?rollback=v1", "", "demo"},
		{"POST", "/demo", "application/json", "demo"},
		{"GET", "/blog", "", "blog"},
		{"POST", "/x/blog?rollback=v1", "", "blog"},
	} {
		for _, refuse := range []error{nil, errors.New("refused")} {
			h, rig := newTestHandler(t)
			// blog trusts what demo trusts, so the same token is a
			// deployer of either; nothing of blog's runs (recordRows).
			blog := newManagedApp("blog")
			blog.spec, blog.verifiers, blog.logger = rig.spec, rig.ma.verifiers, zap.NewNop()
			h.app.managed["blog"] = blog
			calls := recordDecide(t, refuse)
			req := httptest.NewRequest(c.method, c.target, strings.NewReader("{}"))
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}
			req.Header.Set("Authorization", "Bearer "+appToken(t))
			row := findRoute(req)
			landed[row] = true
			*served = nil
			w := send(t, h, req)

			want := decideCall{access.Principal{Kind: access.AppDeployer, App: c.app}, row.action, c.app}
			if len(*calls) != 1 || (*calls)[0] != want {
				t.Errorf("%s %s: Decide asked %+v, want once %+v", c.method, c.target, *calls, want)
			}
			switch {
			case refuse == nil && (w.Code != http.StatusOK || *served != row):
				t.Errorf("%s %s, allowed: %d, answered by %v, want the row %s %s", c.method, c.target, w.Code, *served, row.method, row.shape)
			case refuse != nil && (w.Code != http.StatusForbidden || *served != nil):
				t.Errorf("%s %s, refused: %d %s, answered by %v, want the 403 and no row", c.method, c.target, w.Code, w.Body, *served)
			case refuse != nil && w.Body.String() != "{\"error\":\"forbidden\"}\n":
				t.Errorf("%s %s, refused: body %q", c.method, c.target, w.Body)
			}
		}
	}
	for i := range routes {
		if !landed[&routes[i]] {
			t.Errorf("no case reaches the row %s %s", routes[i].method, routes[i].shape)
		}
	}
}
