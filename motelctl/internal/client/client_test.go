package client

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
)

// box is a test box: a TLS server whose answers the test scripts, and
// a client that trusts it, sleeping on a clock that only records.
type box struct {
	srv    *httptest.Server
	hits   atomic.Int32
	auths  []string
	slept  []time.Duration
	client *Client
}

func newBox(t *testing.T, cred Credential, answer func(n int, w http.ResponseWriter, r *http.Request)) *box {
	t.Helper()
	b := &box{}
	b.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(b.hits.Add(1))
		b.auths = append(b.auths, r.Header.Get("Authorization"))
		answer(n, w, r)
	}))
	// The refused handshake of the certificate test is the point, not
	// noise for the log.
	b.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	b.srv.StartTLS()
	t.Cleanup(b.srv.Close)
	u, _ := url.Parse(b.srv.URL)
	b.client = &Client{
		Box:  Box{Host: u.Hostname(), Port: u.Port()},
		Cred: cred,
		HTTP: NewHTTP(b.srv.Client().Transport),
		Now:  time.Now,
		Sleep: func(_ context.Context, d time.Duration) error {
			b.slept = append(b.slept, d)
			return nil
		},
	}
	return b
}

func motelJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body + "\n"))
}

const flat401 = `{"error":"invalid or missing deploy token (Authorization: Bearer <jwt>)"}`

func failure(t *testing.T, err error) *Failure {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("err %v (%T), want a *Failure", err, err)
	}
	return f
}

// countingSigner stands in for ssh-keygen; each signature is new.
type countingSigner struct{ n int }

func (s *countingSigner) Sign(context.Context, sshkey.Key, string, []byte) ([]byte, error) {
	s.n++
	return []byte("-----BEGIN SSH SIGNATURE-----\nU1NIU0lH" + strings.Repeat("A", s.n*4) + "\n-----END SSH SIGNATURE-----\n"), nil
}

func personCred(t *testing.T) (*PersonToken, *countingSigner) {
	t.Helper()
	// A made-up ed25519 public key line: only its shape is read here.
	pk, err := sshkey.ParsePublic("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn") // gitleaks:allow — made up for this test
	if err != nil {
		t.Fatal(err)
	}
	s := &countingSigner{}
	return &PersonToken{Key: sshkey.Key{Public: pk, Shown: "~/.ssh/id_ed25519.pub"}, Signer: s, Box: Box{Host: "127.0.0.1"}, Now: time.Now}, s
}

func TestGetAnswersAtOnce(t *testing.T) {
	b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blog" || r.URL.RawQuery != "range=24h" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("asked %s %q, Accept %q", r.URL.Path, r.URL.RawQuery, r.Header.Get("Accept"))
		}
		motelJSON(w, 200, `{"app":"blog"}`)
	})
	ans, err := b.client.Get(context.Background(), "/blog", url.Values{"range": {"24h"}})
	if err != nil || ans.Code != 200 || len(b.slept) != 0 || b.auths[0] != "Bearer tok" {
		t.Fatalf("answer %+v, err %v, slept %v, auth %q", ans, err, b.slept, b.auths)
	}
}

// A 401 is sent once more after the wait its credential has, with a
// fresh token; a second 401 stands.
// A 401 is sent once more after the wait its credential has; a
// second 401 stands.
func TestGetRetriesA401Once(t *testing.T) {
	person, _ := personCred(t)
	cases := []struct {
		name string
		cred Credential
		wait time.Duration
	}{
		{"deploy token", DeployToken("tok"), 3 * time.Second},
		{"person token", person, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name+" then 200", func(t *testing.T) {
			b := newBox(t, c.cred, func(n int, w http.ResponseWriter, _ *http.Request) {
				if n == 1 {
					motelJSON(w, 401, flat401)
					return
				}
				motelJSON(w, 200, `{}`)
			})
			ans, err := b.client.Get(context.Background(), "/blog", nil)
			if err != nil || ans.Code != 200 {
				t.Fatalf("answer %+v, err %v", ans, err)
			}
			if len(b.slept) != 1 || b.slept[0] != c.wait {
				t.Errorf("slept %v, want once %s", b.slept, c.wait)
			}
		})
	}
}

// The retry sends the same person token while it has a minute of life
// left, so a hardware key is touched once; with less left it signs
// again.
func TestPersonRetryReusesTheToken(t *testing.T) {
	for _, c := range []struct {
		name      string
		advance   time.Duration // the clock's step during the wait
		wantSigns int
	}{
		{"a second later", time.Second, 1},
		{"with 61 s of life left", 209 * time.Second, 1},
		{"with 59 s of life left", 211 * time.Second, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			person, signer := personCred(t)
			now := time.Now().Truncate(time.Second)
			person.Now = func() time.Time { return now }
			b := newBox(t, person, func(n int, w http.ResponseWriter, _ *http.Request) {
				if n == 1 {
					motelJSON(w, 401, flat401)
					return
				}
				motelJSON(w, 200, `{}`)
			})
			b.client.Sleep = func(context.Context, time.Duration) error { now = now.Add(c.advance); return nil }
			if _, err := b.client.Get(context.Background(), "/blog", nil); err != nil {
				t.Fatal(err)
			}
			if signer.n != c.wantSigns || (c.wantSigns == 1) != (b.auths[0] == b.auths[1]) {
				t.Errorf("%d signatures, the same token twice: %v; want %d", signer.n, b.auths[0] == b.auths[1], c.wantSigns)
			}
		})
	}
}

// The clock is named only for a person's token, only when the box's
// Date is outside what the box takes for the token sent (iat = now-30,
// exp = now+270, 60 s either way), first, and with the other causes
// kept.
func TestGetRefused(t *testing.T) {
	others := "no person or signer line on the box lists it; your address is outside the from list on every grant you hold; 127.0.0.1 is not the box's main name; or the box's moteld is too old to take a key's token at all. Send the operator the line `motelctl key` prints."
	either := "The box says no more to a key it does not accept, so it is one of these: " + others
	or := ". Or, as the box says no more, one of these: " + others
	cases := []struct {
		name   string
		person bool
		skew   time.Duration // the box's Date, from this machine's clock
		want   []string
		not    string
	}{
		{"person, clock right", true, 0, []string{"did not accept your key SHA256:", "(~/.ssh/id_ed25519.pub). " + either}, "clock"},
		{"person, box 89 s behind: still taken", true, -89 * time.Second, []string{either}, "clock"},
		{"person, box 91 s behind", true, -91 * time.Second, []string{"This machine's clock differs from the box's by 91 s (ahead of it; the box's Date is ", "more than a token made here can bridge", or}, ""},
		{"person, box 329 s ahead: still taken", true, 329 * time.Second, []string{either}, "clock"},
		{"person, box 331 s ahead", true, 331 * time.Second, []string{"differs from the box's by 331 s (behind it;", or}, ""},
		{"deploy, clock right", false, 0, []string{"refused the token in MOTEL_TOKEN (sent as Bearer). Most often this app's deploy_trust"}, "clock"},
		{"deploy, box 10 min behind: the clock plays no part", false, -10 * time.Minute, []string{"refused the token in MOTEL_TOKEN"}, "clock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			var cred Credential = DeployToken("tok")
			if c.person {
				p, _ := personCred(t)
				p.Now = func() time.Time { return now }
				cred = p
			}
			b := newBox(t, cred, func(_ int, w http.ResponseWriter, _ *http.Request) {
				// Go's server keeps a Date the handler set.
				w.Header().Set("Date", now.Add(c.skew).UTC().Format(http.TimeFormat))
				motelJSON(w, 401, flat401)
			})
			b.client.Now = func() time.Time { return now }
			_, err := b.client.Get(context.Background(), "/blog", nil)
			f := failure(t, err)
			if f.Exit != ExitRefused || b.hits.Load() != 2 {
				t.Fatalf("exit %d after %d requests, want 4 after 2", f.Exit, b.hits.Load())
			}
			for _, w := range c.want {
				if !strings.Contains(f.Message, w) {
					t.Errorf("message\n%s\nlacks %q", f.Message, w)
				}
			}
			if c.not != "" && strings.Contains(f.Message, c.not) {
				t.Errorf("message\n%s\nnames the %s", f.Message, c.not)
			}
		})
	}
}

func TestGetWaitsOutA429Once(t *testing.T) {
	cases := []struct {
		name      string
		after     []string // Retry-After of each answer; "" ends with a 200
		wantSlept []time.Duration
		wantExit  int
		wantMsg   string
	}{
		{"waits, then answered", []string{"2", ""}, []time.Duration{2 * time.Second}, ExitOK, ""},
		{"zero is a second", []string{"0", ""}, []time.Duration{time.Second}, ExitOK, ""},
		{"stands after the wait", []string{"2", "7"}, []time.Duration{2 * time.Second}, ExitUnknown, "127.0.0.1 asked you to slow down; try again in 7 s."},
		{"too long to wait", []string{"120"}, nil, ExitUnknown, "127.0.0.1 asked you to slow down: the box asks you to wait 120 s, longer than motelctl waits; try again then."},
		{"a wait that would overflow", []string{"18446744083"}, nil, ExitUnknown, "the box asks you to wait 18446744083 s, longer than motelctl waits; try again then."},
		{"a negative wait", []string{"-5"}, nil, ExitUnknown, "try again later."},
		{"no Retry-After", []string{"none"}, nil, ExitUnknown, "try again later."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBox(t, DeployToken("tok"), func(n int, w http.ResponseWriter, _ *http.Request) {
				if n > len(c.after) || c.after[n-1] == "" {
					motelJSON(w, 200, `{}`)
					return
				}
				if c.after[n-1] != "none" {
					w.Header().Set("Retry-After", c.after[n-1])
				}
				motelJSON(w, 429, `{"error":"too many requests"}`)
			})
			_, err := b.client.Get(context.Background(), "/blog", nil)
			if c.wantExit == ExitOK {
				if err != nil {
					t.Fatal(err)
				}
			} else if f := failure(t, err); f.Exit != c.wantExit || !strings.HasSuffix(f.Message, c.wantMsg) {
				t.Errorf("exit %d %q, want %d ending %q", f.Exit, f.Message, c.wantExit, c.wantMsg)
			}
			if len(b.slept) != len(c.wantSlept) || (len(c.wantSlept) > 0 && b.slept[0] != c.wantSlept[0]) {
				t.Errorf("slept %v, want %v", b.slept, c.wantSlept)
			}
		})
	}
}

// A certificate that does not verify ends the command: no flag
// continues past it, and no token was read by whoever answered.
func TestGetCertificateFailure(t *testing.T) {
	b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
		motelJSON(w, 200, `{}`)
	})
	b.client.HTTP = NewHTTP(nil) // the system's roots, which never signed the test's certificate
	_, err := b.client.Get(context.Background(), "/blog", nil)
	f := failure(t, err)
	if f.Exit != ExitCertificate || !strings.Contains(f.Message, "did not verify; the check says: \"x509: certificate signed by unknown authority\". motelctl never sends a token over an unverified connection. As root on the box: journalctl -u moteld -g certificate") {
		t.Fatalf("exit %d: %s", f.Exit, f.Message)
	}
	if b.hits.Load() != 0 {
		t.Error("the request reached the handler")
	}
}

func TestGetNothingAnswers(t *testing.T) {
	// Port 1 (tcpmux): nothing in a test environment listens there,
	// and no test can take it, as a port just closed could be taken.
	port := "1"
	c := &Client{Box: Box{Host: "127.0.0.1", Port: port}, Cred: DeployToken("tok"), HTTP: NewHTTP(nil), Now: time.Now}
	_, err := c.Get(context.Background(), "/blog", nil)
	f := failure(t, err)
	want := "127.0.0.1 did not answer on " + port + " (connection refused). If moteld is down, as root on the box: systemctl status moteld; journalctl -u moteld -n 50. The box's off-box heartbeat also reports a stopped moteld."
	if f.Exit != ExitUnknown || f.Message != want {
		t.Fatalf("exit %d\n%s\nwant 3\n%s", f.Exit, f.Message, want)
	}
}

func TestRead(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		ctype    string
		body     string
		wantExit int
		wantMsg  string
		wantBody bool
	}{
		{"status", 200, "application/json", `{"app":"blog","phase":"idle","available_versions":[]}`, ExitOK, "", false},
		{"not found", 404, "application/json", `{"error":"no deploy recorded for version v9"}`, ExitNotFound, `127.0.0.1 answered 404; the box says: "no deploy recorded for version v9"`, true},
		{"forbidden", 403, "application/json", `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}`, ExitForbidden, "127.0.0.1 does not allow the token in MOTEL_TOKEN this: it needs the viewer role on this app.", true},
		{"forbidden, a body of an older box", 403, "application/json", `{"error":"forbidden"}`, ExitForbidden, `127.0.0.1 refused this (HTTP 403) and did not say what it needs; the box says: "forbidden".`, true},
		{"a 403 that is not JSON", 403, "text/html", "<html>no</html>", ExitForbidden, "127.0.0.1 refused this (HTTP 403) and did not say what it needs.", false},
		{"a 500 in motel's words", 500, "application/json", `{"error":"boom"}`, ExitUnknown, `Something at 127.0.0.1 answered 500, not known to be motel's own answer; it says: "boom"`, false},
		{"a 503 of a box still starting", 503, "application/json", `{"error":"motel is starting and has not loaded this app yet; retry after 1 s"}`, ExitUnknown, `Something at 127.0.0.1 answered 503, not known to be motel's own answer; it says: "motel is starting and has not loaded this app yet; retry after 1 s"`, false},
		{"a 5xx with a Motel-Protocol", 502, "application/json", `{"error":"boom"}`, ExitUnknown, `Something at 127.0.0.1 answered 502, not known to be motel's own answer; it says: "boom"`, false},
		{"no baseline", 409, "application/json", `{"error":"this box has no baseline"}`, ExitFailed, `127.0.0.1 answered 409; the box says: "this box has no baseline"`, true},
		{"a 500, a hostile sentence", 500, "application/json; charset=utf-8", "{\"error\":\"boom \\u001b]52;c;eA==\\u0007\"}", ExitUnknown, `Something at 127.0.0.1 answered 500, not known to be motel's own answer; it says: "boom \x1b]52;c;eA==\x07"`, false},
		{"a gateway's JSON 502", 502, "application/json", "{\"error\":\"upstream \\u001b[2J down\"}", ExitUnknown, `Something at 127.0.0.1 answered 502, not known to be motel's own answer; it says: "upstream \x1b[2J down"`, false},
		{"a redirect with a JSON error", 302, "application/json", `{"error":"sign in at https://elsewhere.example"}`, ExitUnknown, "Something at 127.0.0.1 answered (HTTP 302, not motel's answer). Is this the box's deploy host?", false},
		{"a redirect with no body of its own", 301, "text/html", "<a href=\"https://x\">moved</a>", ExitUnknown, "Something at 127.0.0.1 answered (HTTP 301, not motel's answer). Is this the box's deploy host?", false},
		{"a gateway's 502", 502, "text/html", "<html>bad gateway</html>", ExitUnknown, "Something at 127.0.0.1 answered (HTTP 502, not motel's answer). Is this the box's deploy host?", false},
		{"a 200 that is not motel's", 200, "text/html", "<html>hi</html>", ExitUnknown, "Something at 127.0.0.1 answered (HTTP 200, not motel's answer: not JSON). Is this the box's deploy host?", false},
		{"a 200 of the wrong shape", 200, "application/json", `{"app":7}`, ExitUnknown, "not motel's answer: it has no app and phase)", false},
	}
	c := &Client{Box: Box{Host: "127.0.0.1"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ans := &Answer{Code: tc.code, Header: http.Header{"Content-Type": {tc.ctype}}, Body: []byte(tc.body + "\n")}
			// Motel sends no header of its own, so a header that looks
			// like one makes a 5xx no more motel's.
			if tc.name == "a 5xx with a Motel-Protocol" {
				ans.Header.Set("Motel-Protocol", "1")
			}
			var st wire.Status
			err := c.Read(ans, &st)
			if tc.wantExit == ExitOK {
				if err != nil || st.App != "blog" {
					t.Fatalf("Read = %v, %+v", err, st)
				}
				return
			}
			f := failure(t, err)
			if f.Exit != tc.wantExit || !strings.Contains(f.Message, tc.wantMsg) {
				t.Errorf("exit %d %q, want %d with %q", f.Exit, f.Message, tc.wantExit, tc.wantMsg)
			}
			if (f.Body != nil) != tc.wantBody {
				t.Errorf("body kept: %v, want %v", f.Body != nil, tc.wantBody)
			}
		})
	}
}

func TestParseApp(t *testing.T) {
	cases := []struct {
		in       string
		name     string
		path     string
		box      string
		wantFail bool
	}{
		{in: "blog", name: "blog", path: "/blog"},
		{in: "https://deploy.example.com/blog", name: "blog", path: "/blog", box: "deploy.example.com"},
		{in: "HTTPS://Deploy.Example.com/blog//", name: "blog", path: "/blog", box: "deploy.example.com"},
		{in: "https://deploy.example.com:8443/hooks/blog", name: "blog", path: "/hooks/blog", box: "deploy.example.com:8443"},
		{in: "https://deploy.example.com:443/blog", name: "blog", path: "/blog", box: "deploy.example.com"},
		{in: "Blog", wantFail: true},
		{in: "blog?x", wantFail: true},
		{in: "http://deploy.example.com/blog", wantFail: true},
		{in: "https://user@deploy.example.com/blog", wantFail: true},
		{in: "https://deploy.example.com/", wantFail: true},
		{in: "https://deploy_example.com/blog", wantFail: true},
		{in: "https://deploy.example.com:0/blog", wantFail: true},
	}
	for _, c := range cases {
		app, err := ParseApp(c.in)
		if c.wantFail {
			if f := failure(t, err); f.Exit != ExitUsage {
				t.Errorf("%q: exit %d, want 2", c.in, f.Exit)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		box := ""
		if app.Box != nil {
			box = app.Box.Addr()
		}
		if app.Name != c.name || app.Path != c.path || box != c.box {
			t.Errorf("%q = %+v (box %q), want %s %s %q", c.in, app, box, c.name, c.path, c.box)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, v := range []string{"v1", "1.2.3", "3f9c2a1e", "a_b-c"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"", ".v1", "v1#x", "v%31", "v 1", "../x"} {
		if CheckVersion(v) == nil {
			t.Errorf("%q taken", v)
		}
	}
}
