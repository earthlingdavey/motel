package liveswap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

const personBox = "deploy.example.test"

// testPeople are the people of the handler's tests: alice views demo,
// carol views the box, erin reads demo's logs, dave has no grant.
type testPeople struct {
	alice, carol, dave, erin, stranger accesstest.Key
	reg                                *access.Registry
}

func newTestPeople(t testing.TB) *testPeople {
	t.Helper()
	p := &testPeople{alice: accesstest.Ed25519(), carol: accesstest.SKEd25519(), dave: accesstest.Ed25519(), erin: accesstest.Ed25519(), stranger: accesstest.Ed25519()}
	var l access.Lines
	for name, k := range map[string]accesstest.Key{"alice": p.alice, "carol": p.carol, "dave": p.dave, "erin": p.erin} {
		key, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, args := range [][]string{{"alice", "viewer", "app", "demo"}, {"carol", "viewer", "box"}, {"erin", "logs", "app", "demo"}} {
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
		{"no registry", "GET", []string{token(people.alice)}, true, "no bearer token", false},
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

// TestNoOneAdmittedTakesTheDeployPath: on a box whose registry lists no
// key (none at all, or one built from lines that declare no one), a
// request in the Motel scheme never enters a person's path: a
// well-formed token, junk of its length and short junk are each
// answered as the same request with no Authorization header, status,
// headers and body, and the journal's reason is the deploy token's own,
// never a person token's refusal. A deploy token sent first beside a
// Motel header is served as it would be alone, the first header being
// all the deploy token's path reads; on a box with people the pair is
// the flat 401, and a key no line lists is still refused as
// key-not-listed.
func TestNoOneAdmittedTakesTheDeployPath(t *testing.T) {
	people := newTestPeople(t)
	for _, reg := range []struct {
		name string
		reg  *access.Registry
	}{
		{"no registry", nil},
		{"a registry that lists no key", access.NewRegistry(personBox, access.Lines{})},
	} {
		good := token(people.alice)
		for _, auth := range []string{
			good,
			access.TokenScheme + " " + strings.Repeat("x", len(good)-len(access.TokenScheme+" ")),
			access.TokenScheme + " junk",
			access.TokenScheme,
		} {
			for _, target := range []string{"/demo", "/ghost", "/_motel/whoami", "/_motel/apps"} {
				h, _ := newTestHandler(t)
				core, logs := observer.New(zap.InfoLevel)
				h.logger = zap.New(core)
				h.people = reg.reg
				what := fmt.Sprintf("%s, %.12s… on %s", reg.name, auth, target)
				sameAnswer(t, what, sendTo(t, h, "GET", target, auth), sendTo(t, h, "GET", target))
				lines := logs.FilterMessage("webhook auth failed").All()
				if len(lines) != 2 {
					t.Fatalf("%s: journal %v", what, logs.All())
				}
				none, got := lines[1].ContextMap()["refused"], lines[0].ContextMap()["refused"]
				if got != none || strings.Contains(fmt.Sprint(got), "person token") {
					t.Errorf("%s: refused %q, want the deploy path's %q", what, got, none)
				}
			}
		}
		// Two headers: the deploy token's path reads the first alone, so
		// the deploy token first is judged as if alone, and the Motel one
		// first is no bearer token, the flat 401.
		h, _ := newTestHandler(t)
		h.people = reg.reg
		bearer := "Bearer " + appToken(t)
		alone := sendTo(t, h, "GET", "/demo", bearer)
		if alone.Code != http.StatusOK {
			t.Fatalf("%s: the deploy token alone: %d %s", reg.name, alone.Code, alone.Body)
		}
		if w := sendTo(t, h, "GET", "/demo", bearer, good); w.Code != alone.Code || w.Body.String() != alone.Body.String() {
			t.Errorf("%s: a deploy token, then a Motel header: %d %s, want %d %s", reg.name, w.Code, w.Body, alone.Code, alone.Body)
		}
		sameAnswer(t, reg.name+": a Motel header, then a deploy token", sendTo(t, h, "GET", "/demo", good, bearer), sendTo(t, h, "GET", "/demo"))
	}
	// A box with people: the pair is refused whichever comes first, and
	// a stranger's token is judged as a person's and refused by lookup.
	h, _ := newTestHandler(t)
	core, logs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	h.people = people.reg
	bearer := "Bearer " + appToken(t)
	for _, pair := range [][]string{{bearer, token(people.alice)}, {token(people.alice), bearer}} {
		sameAnswer(t, "a box with people, two headers", sendTo(t, h, "GET", "/demo", pair...), sendTo(t, h, "GET", "/demo"))
	}
	logs.TakeAll()
	sameAnswer(t, "a box with people, a stranger's token", sendTo(t, h, "GET", "/demo", token(people.stranger)), sendTo(t, h, "GET", "/demo"))
	if lines := logs.FilterMessage("webhook auth failed").All(); len(lines) != 2 || lines[0].ContextMap()["refused"] != "person token refused: key-not-listed" {
		t.Errorf("a box with people, a stranger's token: journal %v, want key-not-listed", logs.All())
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

// TestEveryMotelRouteHasAnAction: the rows under /_motel/ and the
// actions served there are the same set, both ways; every box row is a
// GET of an exact path under the prefix with a box action, and every
// app-scoped row a read of one segment under /_motel/apps/<app>/ with
// an app action, one row a read.
func TestEveryMotelRouteHasAnAction(t *testing.T) {
	var actions []access.Action
	for _, rt := range motelRoutes {
		if !strings.HasPrefix(rt.path, motelPrefix+"/") || rt.serve == nil {
			t.Errorf("row %s: not under %s/, or no function", rt.path, motelPrefix)
		}
		if !slices.Contains(access.MotelActions, rt.action) || !strings.HasPrefix(string(rt.action), "box.") {
			t.Errorf("row %s: action %s is not a box action served under /_motel/", rt.path, rt.action)
		}
		actions = append(actions, rt.action)
	}
	reads := map[string]bool{}
	for _, rt := range motelAppRoutes {
		where := motelAppsPrefix + "<app>/" + rt.read
		if !regexp.MustCompile(`^[a-z]+$`).MatchString(rt.read) || rt.serve == nil || reads[rt.read] {
			t.Errorf("row %s: not one lowercase segment, no function, or a read twice", where)
		}
		reads[rt.read] = true
		if !slices.Contains(access.MotelActions, rt.action) || !strings.HasPrefix(string(rt.action), "app.") {
			t.Errorf("row %s: action %s is not an app action served under /_motel/", where, rt.action)
		}
		actions = append(actions, rt.action)
	}
	for _, a := range access.MotelActions {
		if !slices.Contains(actions, a) && !slices.Contains(arrivesWithItsRead, a) {
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
// under /_motel/ is a 404 once authenticated. No answer carries a
// Motel- header, and none says "person".
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
		wantNoMotelHeader(t, w)
		w = sendTo(t, h, "GET", "/_motel/apps", tok)
		var apps wire.Apps
		if err := json.Unmarshal(w.Body.Bytes(), &apps); err != nil || w.Code != http.StatusOK || !slices.Equal(appNames(apps), c.apps) {
			t.Errorf("%s apps: %d %s, want %v", c.name, w.Code, w.Body, c.apps)
		}
		wantNoMotelHeader(t, w)
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
			wantNoMotelHeader(t, w)
		}
	}
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"box"}` + "\n"
	for _, target := range []string{"/_motel/whoami", "/_motel/apps"} {
		w := sendTo(t, h, "GET", target, "Bearer "+globalToken(t))
		if w.Code != http.StatusForbidden || w.Body.String() != needsKey {
			t.Errorf("a global source on %s: %d %q", target, w.Code, w.Body)
		}
		wantNoMotelHeader(t, w)
		if w := sendTo(t, h, "GET", target, "Bearer "+appToken(t)); w.Code != http.StatusUnauthorized {
			t.Errorf("an app's source on %s: %d %q, want 401", target, w.Code, w.Body)
		}
		w = sendTo(t, h, "POST", target, "Bearer "+globalToken(t))
		if w.Code != http.StatusNotFound {
			t.Errorf("a global source posting to %s: %d %q, want 404", target, w.Code, w.Body)
		}
		wantNoMotelHeader(t, w)
	}
	w := sendTo(t, h, "GET", "/_motel/nothing", "Bearer "+globalToken(t))
	if w.Code != http.StatusNotFound {
		t.Errorf("a global source on another path: %d %v", w.Code, w.Result().Header)
	}
	wantNoMotelHeader(t, w)
}

// TestWhoamiRoles: whoami lists one role per grant that applies from
// the request's address: the box's first, then each app by name, viewer
// before logs on one scope; a logs grant from a list only from inside
// it. A logs grant lists its apps on GET /_motel/apps as a viewer grant
// does.
func TestWhoamiRoles(t *testing.T) {
	lara, lars, gus := accesstest.Ed25519(), accesstest.Ed25519(), accesstest.SKEd25519()
	var l access.Lines
	for name, k := range map[string]accesstest.Key{"lara": lara, "lars": lars, "gus": gus} {
		key, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, args := range [][]string{
		{"lara", "viewer", "app", "demo"},
		{"lara", "logs", "app", "demo", "from", "198.51.100.0/24"},
		{"lars", "logs", "app", "demo"},
		{"gus", "logs", "app", "zeta"},
		{"gus", "viewer", "app", "blog"},
		{"gus", "logs", "box", "from", "198.51.100.0/24"},
		{"gus", "viewer", "box"},
		{"gus", "logs", "app", "blog"},
		{"gus", "viewer", "app", "alpha"},
	} {
		g, err := access.ParseGrant(args)
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	if _, err := access.CheckLines(l, nil); err != nil {
		t.Fatal(err)
	}
	h, rig := newTestHandler(t)
	h.people = access.NewRegistry(personBox, l)
	blog := newManagedApp("blog")
	blog.spec, blog.verifiers, blog.logger = rig.spec, rig.ma.verifiers, zap.NewNop()
	h.app.managed["blog"] = blog
	inside, outside := "198.51.100.7:1", "203.0.113.9:1"
	for _, c := range []struct {
		who   accesstest.Key
		name  string
		from  string
		roles []string
		apps  []string
	}{
		{lara, "lara", inside, []string{"viewer app:demo", "logs app:demo"}, []string{"demo"}},
		{lara, "lara", outside, []string{"viewer app:demo"}, []string{"demo"}},
		{lars, "lars", outside, []string{"logs app:demo"}, []string{"demo"}},
		{gus, "gus", inside, []string{"viewer box", "logs box", "viewer app:alpha", "viewer app:blog", "logs app:blog", "logs app:zeta"}, []string{"blog", "demo"}},
		{gus, "gus", outside, []string{"viewer box", "viewer app:alpha", "viewer app:blog", "logs app:blog", "logs app:zeta"}, []string{"blog", "demo"}},
	} {
		get := func(target string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "http://"+personBox+target, nil)
			req.RemoteAddr = c.from
			req.Header.Set("Authorization", token(c.who))
			w := httptest.NewRecorder()
			if err := h.ServeHTTP(w, req, nil); err != nil {
				t.Fatal(err)
			}
			return w
		}
		w := get("/_motel/whoami")
		var me wire.WhoAmI
		if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s from %s whoami: %d %s", c.name, c.from, w.Code, w.Body)
		}
		var roles []string
		for _, r := range me.Roles {
			roles = append(roles, r.Role+" "+r.Scope)
		}
		if !slices.Equal(roles, c.roles) {
			t.Errorf("%s from %s: roles %q, want %q", c.name, c.from, roles, c.roles)
		}
		w = get("/_motel/apps")
		var apps wire.Apps
		if err := json.Unmarshal(w.Body.Bytes(), &apps); err != nil || w.Code != http.StatusOK || !slices.Equal(appNames(apps), c.apps) {
			t.Errorf("%s from %s apps: %d %s, want %v", c.name, c.from, w.Code, w.Body, c.apps)
		}
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
	alice, bob, dave := accesstest.Ed25519(), accesstest.SKEd25519(), accesstest.Ed25519()
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

// TestRatePerPerson: past the burst a person is told 429, with
// Retry-After and no Motel- header, whatever the route; the journal
// says so once; and another person is not held to it.
func TestRatePerPerson(t *testing.T) {
	people := newTestPeople(t)
	h, _ := newTestHandler(t)
	core, logs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	h.people = people.reg
	now := time.Now()
	h.rate = access.NewRate(func() time.Time { return now })
	tok := token(people.alice)
	for i := range access.RateBurst {
		if w := sendTo(t, h, "GET", "/_motel/whoami", tok); w.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i+1, w.Code)
		}
	}
	for _, target := range []string{"/demo", "/_motel/apps", "/ghost"} {
		w := sendTo(t, h, "GET", target, tok)
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" ||
			w.Body.String() != "{\"error\":\"too many requests; retry after 1 s\"}\n" {
			t.Errorf("past the burst, %s: %d %v %q", target, w.Code, w.Header(), w.Body)
		}
		wantNoMotelHeader(t, w)
	}
	if got := logs.FilterMessage("access: person throttled").All(); len(got) != 1 || got[0].ContextMap()["person"] != "alice" {
		t.Errorf("journal %v", logs.All())
	}
	if w := sendTo(t, h, "GET", "/demo", token(people.carol)); w.Code != http.StatusOK {
		t.Errorf("carol, beside a throttled alice: %d", w.Code)
	}
}

// TestHostRefusalNamesTheHost: a token for this box sent to another of
// its names (or through a proxy that rewrites Host) is the flat 401,
// and the journal names the host that arrived.
func TestHostRefusalNamesTheHost(t *testing.T) {
	people := newTestPeople(t)
	h, _ := newTestHandler(t)
	core, logs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	h.people = people.reg
	req := httptest.NewRequest(http.MethodGet, "http://www.example.test/demo", nil)
	req.Header.Set("Authorization", token(people.alice))
	w := httptest.NewRecorder()
	if err := h.ServeHTTP(w, req, nil); err != nil {
		t.Fatal(err)
	}
	lines := logs.FilterMessage("webhook auth failed").All()
	if w.Code != http.StatusUnauthorized || len(lines) != 1 || lines[0].ContextMap()["host"] != "www.example.test" || lines[0].ContextMap()["person"] != "alice" {
		t.Errorf("%d; journal %v", w.Code, logs.All())
	}
}

// TestSignerKeysNoPersonMayUse: once a block declares people, a signer
// line's ed25519 key is a person's, but an ssh-rsa signer key is not,
// and the registry does not list it.
func TestSignerKeysNoPersonMayUse(t *testing.T) {
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaPub, err := ssh.NewPublicKey(&rsaPriv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	alice := accesstest.Ed25519()
	aliceKey, err := access.ParsePerson([]string{"alice", alice.Type(), alice.B64()})
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString
	l := access.Lines{Keys: []access.Key{
		access.SignerKey("carol", ssh.KeyAlgoRSA, b64(rsaPub.Marshal()), rsaPub.Marshal()),
		aliceKey,
	}}
	if _, err := access.CheckLines(l, nil); err != nil {
		t.Fatal(err)
	}
	reg := access.NewRegistry(personBox, l)
	if _, listed := reg.Lookup(ssh.FingerprintSHA256(rsaPub)); listed {
		t.Error("the ssh-rsa key is listed")
	}
	if _, listed := reg.Lookup(alice.Fingerprint()); !listed || reg.Keys() != 1 {
		t.Fatalf("alice listed %v, %d keys", listed, reg.Keys())
	}
}

// TestSmallOrderKeyForgery is why no line takes an Ed25519 key of small
// order: under the identity point, R the base point and S = 1 verify
// for any claims, with no private key. The test shows that forgery
// verifies, then that a person line with the key is refused (a signer
// line is refused alike, by box/proof's ParseSigner).
func TestSmallOrderKeyForgery(t *testing.T) {
	identity := make([]byte, ed25519.PublicKeySize)
	identity[0] = 1 // the identity point's encoding
	smallWire := ssh.Marshal(struct {
		Type string
		Key  []byte
	}{ssh.KeyAlgoED25519, identity})
	small, err := ssh.ParsePublicKey(smallWire)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := fmt.Sprintf(`{"aud":"%s","iat":%d,"exp":%d}`, personBox, now.Unix(), now.Add(time.Minute).Unix())
	base := append([]byte{0x58}, bytes.Repeat([]byte{0x66}, 31)...)
	one := make([]byte, 32)
	one[0] = 1
	sig := ssh.Marshal(ssh.Signature{Format: ssh.KeyAlgoED25519, Blob: append(base, one...)})
	blob := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Version   uint32
		PublicKey []byte
		Namespace string
		Reserved  []byte
		HashAlg   string
		Signature []byte
	}{1, smallWire, access.TokenNamespace, nil, "sha512", sig})...)
	forged := access.TokenScheme + " " + access.TokenEnvelope + base64.RawURLEncoding.EncodeToString([]byte(claims)) + "." + base64.RawURLEncoding.EncodeToString(blob)
	if _, err := access.CheckToken(access.TokenRequest{Authorization: []string{forged}, Method: "GET", Host: personBox}, personBox, now,
		func(string) (ssh.PublicKey, bool) { return small, true }, nil); err != nil {
		t.Fatalf("the forgery does not verify under the small-order key, so this test shows nothing: %v", err)
	}
	if _, err := access.ParsePerson([]string{"mallory", ssh.KeyAlgoED25519, base64.StdEncoding.EncodeToString(smallWire)}); err == nil || !strings.Contains(err.Error(), "its point has small order") {
		t.Errorf("a person line with the key: %v", err)
	}
}

// TestRateIsThePersons: the rate is one bucket per person, whichever of
// their keys signs: a burst spent with one key leaves none for the
// other.
func TestRateIsThePersons(t *testing.T) {
	one, two := accesstest.Ed25519(), accesstest.SKEd25519()
	var l access.Lines
	for _, k := range []accesstest.Key{one, two} {
		key, err := access.ParsePerson([]string{"alice", k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	h, _ := newTestHandler(t)
	h.people = access.NewRegistry(personBox, l)
	now := time.Now()
	h.rate = access.NewRate(func() time.Time { return now })
	for i := range access.RateBurst {
		if w := sendTo(t, h, "GET", "/_motel/whoami", token(one)); w.Code != http.StatusOK {
			t.Fatalf("request %d with the first key: %d", i+1, w.Code)
		}
	}
	if w := sendTo(t, h, "GET", "/_motel/whoami", token(two)); w.Code != http.StatusTooManyRequests {
		t.Errorf("the second key after the first spent the burst: %d", w.Code)
	}
}

// TestPersonClearsTheAddressFailures: a person's token that verifies
// forgets its address's failures, as a deploy token's does, so the next
// refusals from that address are journaled again from a fresh budget.
func TestPersonClearsTheAddressFailures(t *testing.T) {
	people := newTestPeople(t)
	h, rig := newTestHandler(t)
	core, logs := observer.New(zap.InfoLevel)
	h.logger, h.people, h.limiter = zap.New(core), people.reg, deploytrust.NewLimiter(rig.clock)
	bad := token(people.stranger)
	for range deploytrust.FailBudget - 1 {
		sendTo(t, h, "GET", "/demo", bad)
	}
	if w := sendTo(t, h, "GET", "/demo", token(people.alice)); w.Code != http.StatusOK {
		t.Fatalf("alice: %d", w.Code)
	}
	for range deploytrust.FailBudget {
		sendTo(t, h, "GET", "/demo", bad)
	}
	if got, want := logs.FilterMessage("webhook auth failed").Len(), 2*deploytrust.FailBudget-1; got != want {
		t.Errorf("%d refusals journaled, want %d: the verified token did not clear the address", got, want)
	}
}

// TestForwardedNotStrictFailsClosed: a request a trusted proxy
// forwarded, on a server without trusted_proxies_strict, matches no
// from list whatever client_ip says (the client wrote it), and the
// journal says why; on a strict server client_ip is judged; a request
// no proxy forwarded is judged by client_ip either way. A grant with
// no list still applies.
func TestForwardedNotStrictFailsClosed(t *testing.T) {
	alice, bob := accesstest.Ed25519(), accesstest.SKEd25519()
	var l access.Lines
	for name, k := range map[string]accesstest.Key{"alice": alice, "bob": bob} {
		key, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, args := range [][]string{
		{"alice", "viewer", "app", "demo", "from", "198.51.100.0/24"},
		{"bob", "viewer", "app", "demo", "from", "198.51.100.0/24"},
		{"bob", "viewer", "box"},
	} {
		g, err := access.ParseGrant(args)
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	reg := access.NewRegistry(personBox, l)
	for _, c := range []struct {
		name      string
		who       accesstest.Key
		forwarded bool
		strict    int
		code      int
		reason    string // the journal's address, for a refusal
	}{
		{"alice, forwarded, not strict", alice, true, 0, http.StatusUnauthorized, "forwarded by a trusted proxy on a server without trusted_proxies_strict"},
		{"alice, forwarded, strict", alice, true, 1, http.StatusOK, ""},
		{"alice, forwarded, strict as 2 (Caddy: any value > 0)", alice, true, 2, http.StatusOK, ""},
		{"alice, forwarded, strict as -1 (Caddy: not strict)", alice, true, -1, http.StatusUnauthorized, "forwarded by a trusted proxy on a server without trusted_proxies_strict"},
		{"alice, not forwarded, not strict", alice, false, 0, http.StatusOK, ""},
		{"bob, forwarded, not strict: his box grant has no list", bob, true, 0, http.StatusOK, ""},
	} {
		h, _ := newTestHandler(t)
		core, logs := observer.New(zap.InfoLevel)
		h.logger, h.people = zap.New(core), reg
		req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/demo", nil)
		req.RemoteAddr = "192.0.2.1:4321"
		ctx := context.WithValue(req.Context(), caddyhttp.VarsCtxKey, map[string]any{
			caddyhttp.ClientIPVarKey:     "198.51.100.7",
			caddyhttp.TrustedProxyVarKey: c.forwarded,
		})
		ctx = context.WithValue(ctx, caddyhttp.ServerCtxKey, &caddyhttp.Server{TrustedProxiesStrict: c.strict})
		req = req.WithContext(ctx)
		req.Header.Set("Authorization", token(c.who))
		w := httptest.NewRecorder()
		if err := h.ServeHTTP(w, req, nil); err != nil {
			t.Fatal(err)
		}
		if w.Code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body, c.code)
		}
		lines := logs.FilterMessage("webhook auth failed").All()
		switch {
		case c.reason != "" && (len(lines) != 1 || lines[0].ContextMap()["address"] != c.reason ||
			!strings.Contains(lines[0].ContextMap()["refused"].(string), "outside-every-address-list")):
			t.Errorf("%s: journal %v", c.name, logs.All())
		case c.reason == "" && len(lines) != 0:
			t.Errorf("%s: journal %v", c.name, logs.All())
		}
	}
}

// TestHandlerDummyCheckByListedType: through the handler, with only an
// ed25519 key listed, an unlisted ed25519 key's token costs one dummy
// verification, which fails, and an unlisted P-521 key's none: the
// handler hands CheckToken the registry's own listed types.
func TestHandlerDummyCheckByListedType(t *testing.T) {
	alice := accesstest.Ed25519()
	key, err := access.ParsePerson([]string{"alice", alice.Type(), alice.B64()})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := newTestHandler(t)
	h.people = access.NewRegistry(personBox, access.Lines{Keys: []access.Key{key}})
	var seen []string
	t.Cleanup(access.SetDummyVerifyHook(func(keyType string, err error) {
		if err == nil {
			t.Errorf("the dummy %s key verified", keyType)
		}
		seen = append(seen, keyType)
	}))
	for _, c := range []struct {
		name string
		auth string
		want []string
	}{
		{"an unlisted ed25519 key", token(accesstest.Ed25519()), []string{ssh.KeyAlgoED25519}},
		{"a P-521 key, a type a person may not hold yet", token(accesstest.P521()), nil},
	} {
		seen = nil
		if w := sendTo(t, h, "GET", "/_motel/whoami", c.auth); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", c.name, w.Code)
		}
		if !slices.Equal(seen, c.want) {
			t.Errorf("%s: dummy verifications %v, want %v", c.name, seen, c.want)
		}
	}
}

// appNames is the names in an app list's body, in its order.
func appNames(a wire.Apps) []string {
	names := make([]string, 0, len(a.Apps))
	for _, e := range a.Apps {
		names = append(names, e.Name)
	}
	return names
}
