package box

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

// TestEveryRouteHasAnAction: each row of the route table names a box
// action Decide's table knows, a method, a journal name and a
// function to answer; and no two rows match the same request.
func TestEveryRouteHasAnAction(t *testing.T) {
	for i, rt := range routes {
		name := rt.method + " / (" + rt.journal + ")"
		switch {
		case !slices.Contains(access.Actions(), rt.action):
			t.Errorf("%s: action %q is not in Decide's table", name, rt.action)
		case !strings.HasPrefix(string(rt.action), "box."):
			t.Errorf("%s: action %q is not a box action", name, rt.action)
		}
		if rt.method != http.MethodGet && rt.method != http.MethodPost {
			t.Errorf("%s: a method the 405's Allow does not name", name)
		}
		if rt.journal == "" || rt.serve == nil {
			t.Errorf("%s: no journal name or no function to answer", name)
		}
		for _, other := range routes[i+1:] {
			if other.method == rt.method && (other.query == rt.query || other.query == anyQuery || rt.query == anyQuery) {
				t.Errorf("%s and %s / (%s) match the same requests", name, other.method, other.journal)
			}
		}
	}
}

// TestEveryActionHasARoute is the reverse: every box action Decide
// allows to someone has a row. The app actions are liveswap's.
func TestEveryActionHasARoute(t *testing.T) {
	for _, a := range access.Actions() {
		if !strings.HasPrefix(string(a), "box.") {
			continue
		}
		if !slices.ContainsFunc(routes, func(rt route) bool { return rt.action == a }) {
			t.Errorf("action %q has no route on box_webhook", a)
		}
	}
}

// TestRouteActions pins the action of each route, written out here
// rather than read from routes: a real request of each shape goes
// through findRoute, and the row it lands on must carry the action
// the case names ("" for none: the 405). Every row must be landed on
// by some case, so a row added is pinned here too.
func TestRouteActions(t *testing.T) {
	landed := map[*route]bool{}
	for _, c := range []struct {
		method, query string
		forceQuery    bool
		want          access.Action
	}{
		{"GET", "", false, access.BoxStatus},
		{"GET", "", true, access.BoxPushResult}, // `GET /?`: a poll with no id
		{"GET", "result=0123456789abcdef0123456789abcdef", false, access.BoxPushResult},
		{"GET", "%ZZ", false, access.BoxPushResult},
		{"POST", "", false, access.BoxPush},
		{"POST", "result=1", false, access.BoxPush},
		{"POST", "", true, access.BoxPush},
		{"PUT", "", false, ""},
		{"HEAD", "", false, ""},
		{"get", "", false, ""},
		{"DELETE", "result=1", false, ""},
	} {
		hr := httptest.NewRequest(http.MethodGet, "/", nil)
		hr.Method = c.method
		hr.URL.RawQuery = c.query
		hr.URL.ForceQuery = c.forceQuery
		var got access.Action
		if rt := findRoute(hr); rt != nil {
			got = rt.action
			landed[rt] = true
		}
		if got != c.want {
			t.Errorf("%s /?%s (ForceQuery %v) lands on action %q, want %q", c.method, c.query, c.forceQuery, got, c.want)
		}
	}
	for i := range routes {
		if !landed[&routes[i]] {
			t.Errorf("no case lands on the row %s / (%s): write its action here", routes[i].method, routes[i].journal)
		}
	}
}

// FuzzNoRouteNoAnswer drives the handler with any method, path and
// query, for each kind of caller: a stranger (a liveswap deploy token
// is one here) and the box channel's source. The route table's
// functions are swapped for ones that record the call, so the test
// reads the table and covers a row as it is added. A request reaches
// a row's function only when the box channel's source sends it to `/`
// and a row matches; on `/` every other request is 405 before
// authentication whoever sends it, or the flat 401 for a stranger; a
// request to any other path is passed on, answered by nothing here.
func FuzzNoRouteNoAnswer(f *testing.F) {
	f.Add("GET", "/", "", false)
	f.Add("GET", "/", "", true)
	f.Add("GET", "/", "result=0123456789abcdef0123456789abcdef", false)
	f.Add("POST", "/", "result=1", false)
	f.Add("PUT", "/", "", false)
	f.Add("HEAD", "/", "", false)
	f.Add("get", "/", "", false)
	f.Add("DELETE", "/x", "", false)
	f.Add("GET", "//", "", false)
	f.Add("GET", "/_motel/whoami", "", false)

	r := newRig(f)
	saved := routes
	f.Cleanup(func() { routes = saved })
	var served *route
	routes = slices.Clone(saved)
	for i := range routes {
		routes[i].serve = func(*Handler, http.ResponseWriter, *http.Request, deploytrust.Identity) error {
			served = &routes[i]
			return nil
		}
	}

	f.Fuzz(func(t *testing.T, method, target, query string, forceQuery bool) {
		if method == "" || strings.ContainsAny(method, " \t\r\n") {
			return // not a method net/http would hand a handler
		}
		for _, channel := range []bool{false, true} {
			r.h.limiter = deploytrust.NewLimiter(r.clock)
			hr := httptest.NewRequest(http.MethodGet, "/", nil)
			hr.Method = method
			hr.URL.Path = target
			hr.URL.RawQuery = query
			hr.URL.ForceQuery = forceQuery
			if channel {
				hr.Header.Set("Authorization", "Bearer "+r.token(t))
			}
			served = nil
			passed := false
			w := httptest.NewRecorder()
			if err := r.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error {
				passed = true
				return nil
			})); err != nil {
				t.Fatal(err)
			}

			row := findRoute(hr)
			want := http.StatusUnauthorized
			switch {
			case target != "/":
				want = 0 // passed on
			case row == nil:
				want = http.StatusMethodNotAllowed
			case channel:
				want = http.StatusOK // the recording function's
			}
			who := "a stranger"
			if channel {
				who = "the box channel"
			}
			switch {
			case want == 0 && (!passed || served != nil):
				t.Fatalf("%s: %s %q ?%q: not passed on (answered %d)", who, method, target, query, w.Code)
			case want == 0:
			case passed:
				t.Fatalf("%s: %s %q ?%q: passed on", who, method, target, query)
			case w.Code != want:
				t.Fatalf("%s: %s %q ?%q = %d, want %d: %s", who, method, target, query, w.Code, want, w.Body)
			case want == http.StatusOK && served != row:
				t.Fatalf("%s: %s %q ?%q was answered by %v, want the row %s", who, method, target, query, served, row.journal)
			case want != http.StatusOK && served != nil:
				t.Fatalf("%s: %s %q ?%q reached the row %s and was answered %d", who, method, target, query, served.journal, w.Code)
			}
		}
	})
}
