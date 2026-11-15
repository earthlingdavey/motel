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
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
)

func TestRetryAfterBounds(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		v    string
		want time.Duration
		ok   bool
	}{
		{"0", time.Second, true},
		{"7", 7 * time.Second, true},
		{"60", 60 * time.Second, true},
		{"61", 0, false},
		{"18446744083", 0, false}, // overflowed to a 9 s wait before the cap came first
		{"9223372036854775807", 0, false},
		{"-1", 0, false},
		{"", 0, false},
		{"soon", 0, false},
		{"1.5", 0, false},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{now.Add(-time.Hour).Format(http.TimeFormat), time.Second, true},
		{now.Add(time.Hour).Format(http.TimeFormat), 0, false},
	}
	for _, c := range cases {
		d, ok := retryAfter(http.Header{"Retry-After": {c.v}}, now)
		if d != c.want || ok != c.ok {
			t.Errorf("Retry-After %q = %s, %v; want %s, %v", c.v, d, ok, c.want, c.ok)
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
	if _, err := access.CheckToken(req, "deploy.example.test", now, lookup); err != nil {
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
		"unreachable":    &url.Error{Op: "Get", URL: "https://deploy.example.com/", Err: errors.New(evil)},
	} {
		f := failure(t, c.unreachable(ctx, err))
		if strings.ContainsAny(f.Message, "\x1b\x07") || !strings.Contains(f.Message, `evil\x1b]0;pwned\x07`) {
			t.Errorf("%s: %q", name, f.Message)
		}
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
