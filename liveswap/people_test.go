package liveswap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

const personBox = "deploy.example.test"

// testPeople are the people of the handler's tests: alice views demo,
// carol views the box, dave has no grant.
type testPeople struct {
	alice, carol, dave, stranger accesstest.Key
	reg                          *access.Registry
}

func newTestPeople(t testing.TB) *testPeople {
	t.Helper()
	p := &testPeople{alice: accesstest.Ed25519(), carol: accesstest.ECDSA(), dave: accesstest.Ed25519(), stranger: accesstest.Ed25519()}
	var l access.Lines
	for name, k := range map[string]accesstest.Key{"alice": p.alice, "carol": p.carol, "dave": p.dave} {
		key, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, args := range [][]string{{"alice", "viewer", "app", "demo"}, {"carol", "viewer", "box"}} {
		g, err := access.ParseGrant(args)
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	if _, err := access.CheckLines(l, nil); err != nil {
		t.Fatal(err)
	}
	p.reg = access.NewRegistry(personBox, l)
	return p
}

// token is k's header for this box, made now.
func token(k accesstest.Key) string { return k.Token(personBox, time.Now(), 5*time.Minute) }

// sendTo serves one request to the box's name with the given
// Authorization values.
func sendTo(t testing.TB, h *Handler, method, target string, auth ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://"+personBox+target, nil)
	for _, a := range auth {
		req.Header.Add("Authorization", a)
	}
	w := httptest.NewRecorder()
	var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatal("liveswap_webhook passed a request on")
		return nil
	})
	if err := h.ServeHTTP(w, req, next); err != nil {
		t.Fatal(err)
	}
	return w
}

// sameAnswer fails unless two answers are the same status, headers and
// body, byte for byte.
func sameAnswer(t testing.TB, what string, got, want *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != want.Code || got.Body.String() != want.Body.String() || !headersEqual(got.Header(), want.Header()) {
		t.Errorf("%s: %d %v %q, want %d %v %q", what, got.Code, got.Header(), got.Body, want.Code, want.Header(), want.Body)
	}
}

func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// TestPersonRefusalsAreTheFlat401: every way a person's token is
// refused is answered exactly as a request with no token is, status,
// headers and body, so a caller learns nothing of why; and on a box
// with no people (no registry) a valid-looking token is refused alike.
// The journal names the reason, and the key and its person once the key
// was found listed, and never a key no line lists.
func TestPersonRefusalsAreTheFlat401(t *testing.T) {
	people := newTestPeople(t)
	now := time.Now()
	for _, c := range []struct {
		name, method string
		auth         []string
		noRegistry   bool
		reason       string
		named        bool
	}{
		{"no registry", "GET", []string{token(people.alice)}, true, "key-not-listed", false},
		{"a key no line lists", "GET", []string{token(people.stranger)}, false, "key-not-listed", false},
		{"for another box", "GET", []string{people.alice.Token("other.example.test", now, time.Minute)}, false, "aud", true},
		{"expired", "GET", []string{people.alice.Token(personBox, now.Add(-time.Hour), time.Minute)}, false, "expired", true},
		{"too long a life", "GET", []string{people.alice.Token(personBox, now, time.Hour)}, false, "life", true},
		{"a POST", "POST", []string{token(people.alice)}, false, "method", true},
		{"two headers", "GET", []string{token(people.alice), "Bearer x"}, false, "authorization-not-one-header", false},
		{"no token", "GET", []string{"Motel"}, false, "envelope-prefix", false},
		{"a scheme in lower case, a JWT", "GET", []string{"motel eyJhbGciOiJFZERTQSJ9.e30.x"}, false, "envelope-prefix", false},
		{"a claim the form does not know", "GET", []string{people.alice.Sign(`{"aud":"` + personBox + `","iat":1,"exp":2,"act":{}}`)}, false, "claims-json", true},
	} {
		for _, target := range []string{"/demo", "/ghost"} {
			h, _ := newTestHandler(t)
			core, logs := observer.New(zap.InfoLevel)
			h.logger = zap.New(core)
			if !c.noRegistry {
				h.people = people.reg
			}
			stranger := sendTo(t, h, c.method, target)
			got := sendTo(t, h, c.method, target, c.auth...)
			sameAnswer(t, c.name+" on "+target, got, stranger)
			lines := logs.FilterMessage("webhook auth failed").All()
			if len(lines) != 2 {
				t.Fatalf("%s: journal %v", c.name, logs.All())
			}
			fields := lines[1].ContextMap()
			if !strings.Contains(fields["refused"].(string), c.reason) {
				t.Errorf("%s: refused %q, want the reason %s", c.name, fields["refused"], c.reason)
			}
			if _, named := fields["key"]; named != c.named || (c.named && (fields["key"] != people.alice.Fingerprint() || fields["person"] != "alice")) {
				t.Errorf("%s: journal names key %v, person %v; want named %v", c.name, fields["key"], fields["person"], c.named)
			}
		}
	}
}

// TestPersonTokenPassesTheGate: a verified person token gets past the
// preamble, whatever Decide then says; a deploy token sent beside it is
// refused, so the two can never both be judged.
func TestPersonTokenPassesTheGate(t *testing.T) {
	people := newTestPeople(t)
	h, _ := newTestHandler(t)
	h.people = people.reg
	if w := sendTo(t, h, "GET", "/demo", token(people.alice)); w.Code == http.StatusUnauthorized {
		t.Errorf("alice's token: %d %s", w.Code, w.Body)
	}
	if w := sendTo(t, h, "GET", "/demo", token(people.alice), "Bearer "+appToken(t)); w.Code != http.StatusUnauthorized {
		t.Errorf("a person's and a deploy token together: %d, want 401", w.Code)
	}
}

// TestPersonReadsByGrant: a person reads an app a grant covers, and is
// told 403, naming the viewer role on an app, for any other name,
// whether it is an app here or not; a viewer of the box reads every app
// and is told 404 for a name that is no app, as a deploy source with
// global trust is; a person with no grant reads nothing.
func TestPersonReadsByGrant(t *testing.T) {
	people := newTestPeople(t)
	h, _ := newTestHandler(t)
	h.people = people.reg
	const viewerApp = `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}` + "\n"
	for _, c := range []struct {
		who    accesstest.Key
		name   string
		target string
		code   int
		body   string // "" for any
	}{
		{people.alice, "alice", "/demo", http.StatusOK, ""},
		{people.alice, "alice", "/x/demo?range=24h", http.StatusOK, ""},
		{people.alice, "alice", "/demo?deploy=v1", http.StatusNotFound, "{\"error\":\"no deploy recorded for version v1\"}\n"},
		{people.alice, "alice", "/demo?range=1w", http.StatusUnprocessableEntity, ""},
		{people.alice, "alice", "/ghost", http.StatusForbidden, viewerApp},
		{people.alice, "alice", "/ghost?deploy=v1", http.StatusForbidden, viewerApp},
		{people.dave, "dave", "/demo", http.StatusForbidden, viewerApp},
		{people.dave, "dave", "/ghost", http.StatusForbidden, viewerApp},
		{people.carol, "carol", "/demo", http.StatusOK, ""},
		{people.carol, "carol", "/ghost", http.StatusNotFound, "{\"error\":\"unknown app \\\"ghost\\\"\"}\n"},
	} {
		w := sendTo(t, h, "GET", c.target, token(c.who))
		if w.Code != c.code || (c.body != "" && w.Body.String() != c.body) {
			t.Errorf("%s GET %s: %d %q, want %d %q", c.name, c.target, w.Code, w.Body, c.code, c.body)
		}
	}
}

// TestOneHandleMaker: an app handle is made in one place, allowApp,
// after Decide allowed the action on that app; no other code of the
// package writes one with an app in it.
func TestOneHandleMaker(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	made := regexp.MustCompile(`appHandle\{[^}]`)
	var where []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for range made.FindAllIndex(b, -1) {
			where = append(where, f)
		}
	}
	if len(where) != 1 || where[0] != "handler.go" {
		t.Errorf("an app handle is made in %v, want once, in handler.go's allowApp", where)
	}
}

// TestEveryMotelRouteHasAnAction: the rows under /_motel/ and the box
// actions served there are the same set, both ways, and every row is a
// GET of an exact path under the prefix.
func TestEveryMotelRouteHasAnAction(t *testing.T) {
	var actions []access.Action
	for _, rt := range motelRoutes {
		if !strings.HasPrefix(rt.path, motelPrefix+"/") || rt.serve == nil {
			t.Errorf("row %s: not under %s/, or no function", rt.path, motelPrefix)
		}
		if !slices.Contains(access.MotelActions, rt.action) {
			t.Errorf("row %s: action %s is not one served under /_motel/", rt.path, rt.action)
		}
		actions = append(actions, rt.action)
	}
	for _, a := range access.MotelActions {
		if !slices.Contains(actions, a) {
			t.Errorf("action %s has no row under /_motel/", a)
		}
		if !slices.Contains(access.Actions(), a) {
			t.Errorf("action %s is in no cell of Decide's table", a)
		}
	}
}

// TestWhoamiAndApps: who a person is here and which apps they may
// view, for each kind of person; a deploy token on a row under /_motel/
// is told it needs a declared key (a global source's) or the flat 401
// (an app's, which names no app here); any other path, method or query
// under /_motel/ is a 404 once authenticated. Every answer to a
// verified token carries Motel-Protocol, and none says "person".
func TestWhoamiAndApps(t *testing.T) {
	people := newTestPeople(t)
	h, rig := newTestHandler(t)
	h.people = people.reg
	blog := newManagedApp("blog")
	blog.spec, blog.verifiers, blog.logger = rig.spec, rig.ma.verifiers, zap.NewNop()
	h.app.managed["blog"] = blog
	for _, c := range []struct {
		who   accesstest.Key
		name  string
		roles []wire.RoleHeld
		apps  []string
	}{
		{people.alice, "alice", []wire.RoleHeld{{Role: "viewer", Scope: "app:demo"}}, []string{"demo"}},
		{people.carol, "carol", []wire.RoleHeld{{Role: "viewer", Scope: "box"}}, []string{"blog", "demo"}},
		{people.dave, "dave", []wire.RoleHeld{}, []string{}},
	} {
		tok := token(c.who)
		w := sendTo(t, h, "GET", "/_motel/whoami", tok)
		var me wire.WhoAmI
		if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s whoami: %d %s", c.name, w.Code, w.Body)
		}
		if me.Name != c.name || me.Key != c.who.Fingerprint() || me.Box != personBox || me.Protocol != 1 || !slices.Equal(me.Roles, c.roles) || me.TokenExpiresAt.IsZero() {
			t.Errorf("%s whoami: %s", c.name, w.Body)
		}
		if got := w.Result().Header.Get("Motel-Protocol"); got != "1" {
			t.Errorf("%s whoami: Motel-Protocol %q", c.name, got)
		}
		w = sendTo(t, h, "GET", "/_motel/apps", tok)
		var apps wire.Apps
		if err := json.Unmarshal(w.Body.Bytes(), &apps); err != nil || w.Code != http.StatusOK || !slices.Equal(apps.Apps, c.apps) {
			t.Errorf("%s apps: %d %s, want %v", c.name, w.Code, w.Body, c.apps)
		}
		for _, target := range []string{"/_motel/", "/_motel", "/_motel/whoami/x", "/_motel/demo", "/x/_motel/whoami", "/_motel/whoami?x=1", "/_motel/apps?"} {
			w := sendTo(t, h, "GET", target, tok)
			want := http.StatusNotFound
			if target == "/x/_motel/whoami" { // the app whoami, which is none: Decide first
				want = http.StatusForbidden
				if c.name == "carol" {
					want = http.StatusNotFound
				}
			}
			if w.Code != want {
				t.Errorf("%s GET %s: %d %s, want %d", c.name, target, w.Code, w.Body, want)
			}
		}
	}
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"box"}` + "\n"
	for _, target := range []string{"/_motel/whoami", "/_motel/apps"} {
		if w := sendTo(t, h, "GET", target, "Bearer "+globalToken(t)); w.Code != http.StatusForbidden || w.Body.String() != needsKey {
			t.Errorf("a global source on %s: %d %q", target, w.Code, w.Body)
		}
		if w := sendTo(t, h, "GET", target, "Bearer "+appToken(t)); w.Code != http.StatusUnauthorized {
			t.Errorf("an app's source on %s: %d %q, want 401", target, w.Code, w.Body)
		}
		if w := sendTo(t, h, "POST", target, "Bearer "+globalToken(t)); w.Code != http.StatusNotFound {
			t.Errorf("a global source posting to %s: %d %q, want 404", target, w.Code, w.Body)
		}
	}
	if w := sendTo(t, h, "GET", "/_motel/nothing", "Bearer "+globalToken(t)); w.Code != http.StatusNotFound || w.Result().Header.Get("Motel-Protocol") != "1" {
		t.Errorf("a global source on another path: %d %v", w.Code, w.Result().Header)
	}
}

// TestAddressList: a grant with a from list holds only from the listed
// networks, at their bounds: the first and the last address of a prefix
// are in it and the ones either side are not. A person left with no
// grant from where the request came is refused with the flat 401 and a
// journal line naming the key, the person and the address; one with a
// grant left is told what that grant covers; a person with no grant at
// all is still told who they are. The address is Caddy's client_ip when
// it set one, the peer otherwise.
func TestAddressList(t *testing.T) {
	alice, bob, dave := accesstest.Ed25519(), accesstest.ECDSA(), accesstest.Ed25519()
	var l access.Lines
	for name, k := range map[string]accesstest.Key{"alice": alice, "bob": bob, "dave": dave} {
		key, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, args := range [][]string{
		{"alice", "viewer", "app", "demo", "from", "198.51.100.0/24", "2001:db8:5::/48"},
		{"bob", "viewer", "box", "from", "203.0.113.9"},
		{"bob", "viewer", "app", "demo"},
	} {
		g, err := access.ParseGrant(args)
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	reg := access.NewRegistry(personBox, l)
	for _, c := range []struct {
		who            accesstest.Key
		name, from     string
		clientIP       string // "": Caddy set none
		target         string
		code           int
		refusedOutside bool
	}{
		{alice, "alice", "198.51.100.0:1234", "", "/demo", http.StatusOK, false},
		{alice, "alice", "198.51.100.255:1234", "", "/demo", http.StatusOK, false},
		{alice, "alice", "198.51.99.255:1234", "", "/demo", http.StatusUnauthorized, true},
		{alice, "alice", "198.51.101.0:1234", "", "/demo", http.StatusUnauthorized, true},
		{alice, "alice", "[2001:db8:5:ffff:ffff:ffff:ffff:ffff]:1234", "", "/demo", http.StatusOK, false},
		{alice, "alice", "[2001:db8:6::]:1234", "", "/demo", http.StatusUnauthorized, true},
		{alice, "alice", "[::ffff:198.51.100.7]:1234", "", "/demo", http.StatusOK, false},
		{alice, "alice", "192.0.2.1:1234", "198.51.100.7", "/_motel/whoami", http.StatusOK, false},
		{alice, "alice", "198.51.100.7:1234", "192.0.2.1", "/_motel/whoami", http.StatusUnauthorized, true},
		{alice, "alice", "not an address", "", "/demo", http.StatusUnauthorized, true},
		{bob, "bob", "203.0.113.9:1", "", "/demo", http.StatusOK, false},
		{bob, "bob", "203.0.113.10:1", "", "/demo", http.StatusOK, false},
		{bob, "bob", "203.0.113.10:1", "", "/ghost", http.StatusForbidden, false},
		{bob, "bob", "203.0.113.9:1", "", "/ghost", http.StatusNotFound, false},
		{dave, "dave", "192.0.2.1:1", "", "/_motel/whoami", http.StatusOK, false},
	} {
		h, _ := newTestHandler(t)
		core, logs := observer.New(zap.InfoLevel)
		h.logger = zap.New(core)
		h.people = reg
		req := httptest.NewRequest(http.MethodGet, "http://"+personBox+c.target, nil)
		req.RemoteAddr = c.from
		if c.clientIP != "" {
			req = req.WithContext(context.WithValue(req.Context(), caddyhttp.VarsCtxKey, map[string]any{caddyhttp.ClientIPVarKey: c.clientIP}))
		}
		req.Header.Set("Authorization", token(c.who))
		w := httptest.NewRecorder()
		if err := h.ServeHTTP(w, req, nil); err != nil {
			t.Fatal(err)
		}
		if w.Code != c.code {
			t.Errorf("%s from %s (client_ip %q) GET %s: %d %s, want %d", c.name, c.from, c.clientIP, c.target, w.Code, w.Body, c.code)
		}
		lines := logs.FilterMessage("webhook auth failed").All()
		if c.refusedOutside {
			if len(lines) != 1 || !strings.Contains(lines[0].ContextMap()["refused"].(string), "outside-every-address-list") ||
				lines[0].ContextMap()["person"] != c.name || lines[0].ContextMap()["key"] != c.who.Fingerprint() {
				t.Errorf("%s from %s: journal %v", c.name, c.from, logs.All())
			}
			sameAnswer(t, c.name+" from "+c.from, w, sendTo(t, h, http.MethodGet, c.target))
		} else if len(lines) != 0 {
			t.Errorf("%s from %s: journal %v", c.name, c.from, logs.All())
		}
	}
}
