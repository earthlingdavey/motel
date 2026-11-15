package liveswap

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
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
