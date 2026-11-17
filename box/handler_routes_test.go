package box

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
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
// allows to someone has a row. The app actions are liveswap's, and so
// are the box actions it serves under /_motel/.
func TestEveryActionHasARoute(t *testing.T) {
	for _, a := range access.Actions() {
		if !strings.HasPrefix(string(a), "box.") || slices.Contains(access.MotelActions, a) {
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

// TestJournalNamesEachRoute: a refused request's journal line names
// the route it matched, the label each row of the table gives the
// preamble, read through ServeHTTP.
func TestJournalNamesEachRoute(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ method, target, want string }{
		{http.MethodGet, "/", "status"},
		{http.MethodGet, "/?", "result"},
		{http.MethodGet, "/?result=" + strings.Repeat("0", 32), "result"},
		{http.MethodPost, "/", "push"},
	} {
		r.h.limiter = deploytrust.NewLimiter(r.clock)
		r.logs.TakeAll()
		if w := r.do(t, req{method: c.method, target: c.target}); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: got %d, want 401", c.method, c.target, w.Code)
		}
		got := r.logs.FilterMessage("webhook auth failed").All()
		if len(got) != 1 || got[0].ContextMap()["box_request"] != c.want {
			t.Errorf("%s %s: want one line with box_request=%s: %v", c.method, c.target, c.want, r.logs.All())
		}
	}
}

// FuzzNoRouteNoAnswer drives the handler with any method, path and
// query, for each kind of caller: a stranger (a liveswap deploy token
// is one here), the box channel's source, a viewer of the box and a
// person whose grant is on an app alone. The route table's functions
// are swapped for ones that record the call (recordRows), so the test
// reads the table and covers a row as it is added. A request reaches a
// row's function only when the box channel's source sends it to `/`
// and a row matches, or a viewer of the box asks for the box's status;
// on `/` every other request is 405 before authentication whoever
// sends it, the flat 401 for a stranger and for a person's token off a
// GET, or the 403 for a person; a request to any other path is passed
// on, answered by nothing here.
func FuzzNoRouteNoAnswer(f *testing.F) {
	// Every row of the table is reached by some seed (checked below,
	// against the real table).
	seeds := []struct {
		method, target, query string
		forceQuery            bool
	}{
		{"GET", "/", "", false},
		{"GET", "/", "", true},
		{"GET", "/", "result=0123456789abcdef0123456789abcdef", false},
		{"POST", "/", "result=1", false},
		{"PUT", "/", "", false},
		{"HEAD", "/", "", false},
		{"get", "/", "", false},
		{"DELETE", "/x", "", false},
		{"GET", "//", "", false},
		{"GET", "/_motel/whoami", "", false},
	}
	reached := map[*route]bool{}
	for _, seed := range seeds {
		f.Add(seed.method, seed.target, seed.query, seed.forceQuery)
		hr := httptest.NewRequest(http.MethodGet, "/", nil)
		hr.Method, hr.URL.Path, hr.URL.RawQuery, hr.URL.ForceQuery = seed.method, seed.target, seed.query, seed.forceQuery
		reached[findRoute(hr)] = true
	}
	for i := range routes {
		if !reached[&routes[i]] {
			f.Fatalf("no seed reaches the row %s / (%s)", routes[i].method, routes[i].journal)
		}
	}

	r := newRig(f)
	served := recordRows(f)
	// People: a viewer of the box and a viewer of an app alone. Each
	// input signs its own tokens: the handler checks them by the real
	// clock, and a token made once for the run would expire in a long
	// one, a 401 no input caused.
	carol, alice := accesstest.Ed25519(), accesstest.SKEd25519()
	var l access.Lines
	for name, k := range map[string]accesstest.Key{"carol": carol, "alice": alice} {
		key, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			f.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, args := range [][]string{{"carol", "viewer", "box"}, {"alice", "viewer", "app", "a"}} {
		g, err := access.ParseGrant(args)
		if err != nil {
			f.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	r.h.app.registry = access.NewRegistry(peopleIdentity, l)
	people := map[string]accesstest.Key{
		"a viewer of the box": carol,
		"a viewer of an app":  alice,
	}

	f.Fuzz(func(t *testing.T, method, target, query string, forceQuery bool) {
		if method == "" || strings.ContainsAny(method, " \t\r\n") {
			return // not a method net/http would hand a handler
		}
		for _, caller := range []string{"a stranger", "the box channel", "a viewer of the box", "a viewer of an app"} {
			channel := caller == "the box channel"
			r.h.limiter = deploytrust.NewLimiter(r.clock)
			r.logs.TakeAll() // the rig lives for the run: its journal must not grow with it
			hr := httptest.NewRequest(http.MethodGet, "http://"+peopleIdentity+"/", nil)
			hr.Method = method
			hr.URL.Path = target
			hr.URL.RawQuery = query
			hr.URL.ForceQuery = forceQuery
			if channel {
				hr.Header.Set("Authorization", "Bearer "+r.token(t))
			}
			if k, ok := people[caller]; ok {
				hr.Header.Set("Authorization", k.Token(peopleIdentity, time.Now(), 10*time.Minute))
			}
			*served = nil
			passed := false
			w := httptest.NewRecorder()
			if err := r.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error {
				passed = true
				return nil
			})); err != nil {
				t.Fatal(err)
			}

			row := findRoute(hr)
			_, person := people[caller]
			want := http.StatusUnauthorized
			switch {
			case target != "/":
				want = 0 // passed on
			case row == nil:
				want = http.StatusMethodNotAllowed
			case channel:
				want = http.StatusOK // the recording function's
			// A person's token is good on a GET alone; the box's status
			// is a viewer of the box's, and a push's result no person's.
			case person && method != http.MethodGet:
			case person && caller == "a viewer of the box" && row.action == access.BoxStatus:
				want = http.StatusOK
			case person:
				want = http.StatusForbidden
			}
			who := caller
			switch {
			case want == 0 && (!passed || *served != nil):
				t.Fatalf("%s: %s %q ?%q: not passed on (answered %d)", who, method, target, query, w.Code)
			case want == 0:
			case passed:
				t.Fatalf("%s: %s %q ?%q: passed on", who, method, target, query)
			case w.Code != want:
				t.Fatalf("%s: %s %q ?%q = %d, want %d: %s", who, method, target, query, w.Code, want, w.Body)
			case want == http.StatusOK && *served != row:
				t.Fatalf("%s: %s %q ?%q was answered by %v, want the row %s", who, method, target, query, *served, row.journal)
			case want != http.StatusOK && *served != nil:
				t.Fatalf("%s: %s %q ?%q reached the row %s and was answered %d", who, method, target, query, (*served).journal, w.Code)
			}
		}
	})
}

// recordRows swaps each row's function for one that records its row
// in *served, until the test ends, so a test sees which row answered
// without reading or writing any state.
func recordRows(tb testing.TB) **route {
	saved := routes
	tb.Cleanup(func() { routes = saved })
	served := new(*route)
	routes = slices.Clone(saved)
	for i := range routes {
		routes[i].serve = func(*Handler, http.ResponseWriter, *http.Request, deploytrust.Identity) error {
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

// TestHandlerAsksDecide: for a request of every row, sent by the box
// channel's source, the handler asks Decide once, with the box
// channel, the row's action and the box ("") as the resource; the
// row's function answers when Decide allows, and when it refuses the
// function does not run and the answer is the 403.
func TestHandlerAsksDecide(t *testing.T) {
	r := newRig(t)
	served := recordRows(t)
	landed := map[*route]bool{}
	for _, c := range []struct{ method, target string }{
		{"GET", "/"},
		{"GET", "/?result=0123456789abcdef0123456789abcdef"},
		{"POST", "/"},
	} {
		for _, refuse := range []error{nil, errors.New("refused")} {
			calls := recordDecide(t, refuse)
			row := findRoute(httptest.NewRequest(c.method, c.target, nil))
			landed[row] = true
			*served = nil
			w := r.do(t, req{method: c.method, target: c.target, token: r.token(t)})

			want := decideCall{access.Principal{Kind: access.BoxChannel}, row.action, ""}
			if len(*calls) != 1 || (*calls)[0] != want {
				t.Errorf("%s %s: Decide asked %+v, want once %+v", c.method, c.target, *calls, want)
			}
			switch {
			case refuse == nil && (w.Code != http.StatusOK || *served != row):
				t.Errorf("%s %s, allowed: %d, answered by %v, want the row %s", c.method, c.target, w.Code, *served, row.journal)
			case refuse != nil && (w.Code != http.StatusForbidden || *served != nil):
				t.Errorf("%s %s, refused: %d %s, answered by %v, want the 403 and no row", c.method, c.target, w.Code, w.Body, *served)
			case refuse != nil && w.Body.String() != "{\"error\":\"forbidden\"}\n":
				t.Errorf("%s %s, refused: body %q", c.method, c.target, w.Body)
			}
		}
	}
	for i := range routes {
		if !landed[&routes[i]] {
			t.Errorf("no case reaches the row %s / (%s)", routes[i].method, routes[i].journal)
		}
	}
}
