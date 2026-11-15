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
func TestGetRetriesA401Once(t *testing.T) {
	person, signer := personCred(t)
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
	if signer.n != 2 {
		t.Errorf("the person's retry signed %d tokens, want a fresh one: 2", signer.n)
	}
}

func TestGetRefused(t *testing.T) {
	person, _ := personCred(t)
	cases := []struct {
		name string
		cred Credential
		skew time.Duration
		want []string
	}{
		{"person, clock right", person, 0, []string{
			"did not accept your key SHA256:", "(~/.ssh/id_ed25519.pub). Either no person line on the box lists it, your address is outside the address list on your grants, or 127.0.0.1 is not the box's main name. Your clock is within 1 s of the box's. Send the operator the line `motelctl key` prints."}},
		{"deploy, clock right", DeployToken("tok"), 0, []string{
			"refused the token in MOTEL_TOKEN (sent as Bearer). Most often this app's deploy_trust", "Your clock is within 1 s"}},
		{"box clock behind", person, -2 * time.Minute, []string{"this machine's clock is 120 s ahead of the box's", "only within 60 s of its own clock"}},
		{"box clock ahead", DeployToken("tok"), 51 * time.Second, []string{"this machine's clock is 51 s behind the box's"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			b := newBox(t, c.cred, func(_ int, w http.ResponseWriter, _ *http.Request) {
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
		{"too long to wait", []string{"120"}, nil, ExitUnknown, "try again later."},
		{"a wait that would overflow", []string{"18446744083"}, nil, ExitUnknown, "try again later."},
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
	if f.Exit != ExitCertificate || !strings.Contains(f.Message, "did not verify (x509: certificate signed by unknown authority). motelctl never sends a token over an unverified connection. As root on the box: journalctl -u motel -g certificate") {
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
	want := "127.0.0.1 did not answer on " + port + " (connection refused). If motel is down, as root on the box: systemctl status motel; journalctl -u motel -n 50. The box's off-box heartbeat also reports a stopped motel."
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
		{"status", 200, "application/json", `{"app":"blog","phase":"running","available_versions":[]}`, ExitOK, "", false},
		{"not found", 404, "application/json", `{"error":"no deploy recorded for version v9"}`, ExitNotFound, "127.0.0.1: no deploy recorded for version v9", true},
		{"forbidden", 403, "application/json", `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}`, ExitForbidden, "127.0.0.1 does not allow this: this needs the viewer role on this app", true},
		{"no baseline", 409, "application/json", `{"error":"this box has no baseline"}`, ExitFailed, "127.0.0.1 answered 409: this box has no baseline", true},
		{"motel's 500, a hostile sentence", 500, "application/json; charset=utf-8", "{\"error\":\"boom \\u001b]52;c;eA==\\u0007\"}", ExitFailed, `127.0.0.1 answered 500: boom \x1b]52;c;eA==\x07`, true},
		{"a gateway's JSON 502", 502, "application/json", "{\"error\":\"upstream \\u001b[2J down\"}", ExitUnknown, `Something at 127.0.0.1 answered 502 without Motel-Version, so not known to be motel's own answer: upstream \x1b[2J down`, false},
		{"a 500 without Motel-Version", 500, "application/json", `{"error":"boom"}`, ExitUnknown, "answered 500 without Motel-Version", false},
		{"a gateway's 502", 502, "text/html", "<html>bad gateway</html>", ExitUnknown, "Something at 127.0.0.1 answered (HTTP 502, not motel's answer). Is this the box's deploy host?", false},
		{"a 200 that is not motel's", 200, "text/html", "<html>hi</html>", ExitUnknown, "Something at 127.0.0.1 answered (HTTP 200, not motel's answer: not JSON). Is this the box's deploy host?", false},
		{"a 200 of the wrong shape", 200, "application/json", `{"app":7}`, ExitUnknown, "not motel's answer: json: cannot unmarshal", false},
	}
	c := &Client{Box: Box{Host: "127.0.0.1"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ans := &Answer{Code: tc.code, Header: http.Header{"Content-Type": {tc.ctype}}, Body: []byte(tc.body + "\n")}
			// Motel stamps its release on every answer to a verified
			// token; the gateway's do not carry it.
			if strings.HasPrefix(tc.name, "motel's") {
				ans.Header.Set("Motel-Version", "v0.7.0")
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
