package box

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

// FuzzHandlerRoute drives box_webhook with any method, path, query and
// Authorization: a path other than `/` is passed on untouched; on `/`
// a method other than GET or POST is 405 before authentication; and
// nothing without the bearer gets past the preamble — the flat 401,
// charged, and never a body that says more. With the bearer, `GET /`
// answers the baseline (409 here: none yet); a query is a result poll —
// 404 for an id in the grammar (none admitted here), 400 for anything
// else, `GET /?` with an empty query among it; and a push without its
// request id is 400, its body unread.
func FuzzHandlerRoute(f *testing.F) {
	f.Add("GET", "/", "", false, "", false)
	f.Add("GET", "/", "", true, "", true)
	f.Add("GET", "/", "result=0123456789abcdef0123456789abcdef", false, "Box-Poll "+strings.Repeat("A", 43)+"=", false)
	f.Add("POST", "/", "", false, "", true)
	f.Add("GET", "/x", "result=1", false, "", false)
	f.Add("PUT", "/", "", false, "Bearer x", false)
	f.Add("GET", "//", "", false, "", true)
	f.Add("GET", "/", "%ZZ", false, "", true)

	r := newRig(f)
	f.Fuzz(func(t *testing.T, method, path, query string, forceQuery bool, auth string, bearer bool) {
		if method == "" || strings.ContainsAny(method, " \t\r\n") {
			return // not a method net/http would hand a handler
		}
		r.h.limiter = deploytrust.NewLimiter(r.clock)
		r.logs.TakeAll()
		hr := httptest.NewRequest(http.MethodGet, "/", nil)
		hr.Method = method
		hr.URL.Path = path
		hr.URL.RawQuery = query
		hr.URL.ForceQuery = forceQuery
		hr.Header["Authorization"] = []string{auth}
		if bearer {
			// Minted per iteration: a token lives five minutes, a
			// fuzz run longer.
			hr.Header.Set("Authorization", "Bearer "+r.token(t))
		}
		passed := false
		w := httptest.NewRecorder()
		if err := r.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error {
			passed = true
			return nil
		})); err != nil {
			t.Fatal(err)
		}
		want, charged := 0, 0
		switch {
		case path != "/":
			if !passed || w.Body.Len() != 0 || r.h.limiter.Size() != 0 {
				t.Fatalf("%s %q not passed on untouched: %d %s", method, path, w.Code, w.Body)
			}
			return
		case method != http.MethodGet && method != http.MethodPost:
			want = http.StatusMethodNotAllowed
		case !bearer:
			want, charged = http.StatusUnauthorized, 1
		case method == http.MethodGet && query == "" && !forceQuery:
			want = http.StatusConflict
		case method == http.MethodPost:
			want = http.StatusBadRequest
		default:
			want = http.StatusBadRequest
			if q, err := url.ParseQuery(query); err == nil && len(q) == 1 && len(q["result"]) == 1 && isRequestID(q["result"][0]) {
				want = http.StatusNotFound
			}
		}
		if passed || w.Code != want || r.h.limiter.Size() != charged || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("%s %q ?%q: %d (%d charged), want %d (%d): %s", method, path, query, w.Code, r.h.limiter.Size(), want, charged, w.Body)
		}
		if want == http.StatusUnauthorized {
			var got map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != 1 || got["error"] != unauthorized {
				t.Fatalf("not the flat 401: %s", w.Body)
			}
		}
	})
}

// FuzzBoxOption: the `box` option's parser (what `moteld validate`
// and the running box read) and the walk (what root reads) agree on
// the signers of any box block both accept — so the list the box
// trusts is never other than the one the running config was checked
// against. The walk goes first: a file it refuses never reaches the
// adapter, so no fuzzed `import` is ever followed.
func FuzzBoxOption(f *testing.F) {
	trust := "\t\tdeploy_trust github {\n\t\t\taudience motel\n\t\t\tclaim repository o/r\n\t\t}\n"
	for _, body := range []string{
		trust + "\t\tsigner alice@example.com ssh-ed25519 K1\n\t\tsigner bob ssh-ed25519 K2\n",
		trust + "\t\tsigner \"alice\" ssh-ed25519 \"K1\"\n",
		trust + "\t\tsigner alice ssh-ed25519 <<EOF\n\t\tK1\n\t\tEOF\n",
		trust + "\t\tsigner {$P:alice} ssh-ed25519 K1\n",
		trust + "\t\tsigner alice ssh-ed25519 K1 # bob\n",
		trust + "\t\tsigner alice ssh-ed25519 K1 {\n\t\t}\n\t\tsigner bob ssh-ed25519 K2\n",
		trust + "\t\tsigner alice ssh-ed25519 K1\n\t}\n\tbox {\n" + trust + "\t\tsigner bob ssh-ed25519 K2\n",
	} {
		f.Add(string(file(body)))
	}
	f.Fuzz(func(t *testing.T, body string) {
		in := []byte("{\n\tbox {\n" + body + "\n\t}\n}\n\ndeploy.example.com {\n\tbox_webhook\n}\n")
		shape, err := Walk(in)
		if err != nil {
			return
		}
		cfg, _, err := caddyconfig.GetAdapter("caddyfile").Adapt(in, map[string]any{"filename": "Caddyfile"})
		if err != nil {
			return
		}
		var c struct {
			Apps struct {
				Box *App `json:"box"`
			} `json:"apps"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			t.Fatal(err)
		}
		if c.Apps.Box == nil {
			t.Fatalf("the walk read a box block the adapter did not: %s", cfg)
		}
		got := c.Apps.Box.Signers
		if len(got) != len(shape.Signers) {
			t.Fatalf("the option has %d signers, the walk %d: %+v vs %+v", len(got), len(shape.Signers), got, shape.Signers)
		}
		for i, s := range shape.Signers {
			if got[i] != (SignerConfig{Principal: s.Principal, Type: s.Type, Key: s.B64}) {
				t.Fatalf("signer %d: option %+v, walk %+v", i, got[i], s)
			}
		}
	})
}
