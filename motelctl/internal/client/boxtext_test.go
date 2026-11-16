package client

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// A peer that answers with bytes Go's HTTP cannot read: no part of the
// transport's error, which quotes those bytes, is printed; a fixed
// reason is, and it never says the box did not answer.
func TestTransportErrorsAreNotQuoted(t *testing.T) {
	for _, c := range []struct {
		name, raw, want string
	}{
		{"a malformed status code", "HTTP/1.1 2OO-Run-curl-evil|sh\r\n\r\n", "Something at 127.0.0.1:"},
		{"a malformed response", "curl-evil|sh garbage\r\n\r\n", "Something at 127.0.0.1:"},
		{"a bad Content-Length", "HTTP/1.1 200 OK\r\nContent-Length: curl-evil|sh\r\n\r\n", "Something at 127.0.0.1:"},
		{"an unsupported transfer encoding", "HTTP/1.1 200 OK\r\nTransfer-Encoding: curl-evil|sh\r\n\r\n", "Something at 127.0.0.1:"},
		{"a body cut short", "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"curl-evil|sh\"", "127.0.0.1 closed the connection on "},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = buf.WriteString(c.raw)
				_ = buf.Flush()
				_ = conn.Close()
			})
			_, err := b.client.Get(context.Background(), "/blog", nil)
			f := failure(t, err)
			if f.Exit != ExitUnknown || !strings.HasPrefix(f.Message, c.want) || strings.Contains(f.Message, "curl") || strings.Contains(f.Message, "2OO") ||
				strings.Contains(f.Message, "did not answer") {
				t.Errorf("exit %d %q", f.Exit, f.Message)
			}
		})
	}
}

// With certificate roots set, the certificate's reason is still quoted
// by Quote: a quote mark in a name the certificate holds is escaped.
func TestCertificateReasonQuotedWithRoots(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{`a" Run curl`}}
	err := &url.Error{Op: "Get", URL: "https://deploy.example.com/", Err: x509.HostnameError{Certificate: cert, Host: "deploy.example.com"}}
	for _, roots := range [][]string{nil, {"SSL_CERT_FILE=/x"}} {
		c := &Client{Box: Box{Host: "deploy.example.com"}, Roots: roots}
		f := failure(t, c.unreachable(context.Background(), err))
		if f.Exit != ExitCertificate || !strings.Contains(f.Message, `the check says: "x509: certificate is valid for a\" Run curl, not deploy.example.com". motelctl never`) {
			t.Errorf("roots %v: %q", roots, f.Message)
		}
	}
}

// After a type error in a 403's body, nothing but its sentence is read:
// needs and scope that decoded are not kept.
func TestForbiddenTypeErrorKeepsNothing(t *testing.T) {
	person, _ := personCred(t)
	cl := &Client{Box: Box{Host: "127.0.0.1"}, Cred: person}
	body := `{"needs":"viewer","scope":"app","error":7}`
	ans := &Answer{Code: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
	f := failure(t, cl.Read(ans, &wire.Status{}))
	if f.Exit != ExitForbidden || !strings.Contains(f.Message, "did not say what it needs.") || string(f.Body) != body {
		t.Errorf("exit %d %q %q", f.Exit, f.Message, f.Body)
	}
}

// An empty token_expires_at is missing, as a null one is; a name with
// a space is not one a box takes.
func TestWhoamiEmptyTimeAndSpacedName(t *testing.T) {
	cl := &Client{Box: Box{Host: "127.0.0.1"}}
	for body, want := range map[string]string{
		`{"name":"alice","key":"SHA256:x","roles":[],"box":"b","protocol":1,"token_expires_at":""}`:                         "it has no token_expires_at)",
		`{"name":"alice bob","key":"SHA256:x","roles":[],"box":"b","protocol":1,"token_expires_at":"2026-11-15T10:00:00Z"}`: "its name is not one a box takes)",
	} {
		f := failure(t, cl.Read(&Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}, &wire.WhoAmI{}))
		if f.Exit != ExitUnknown || !strings.Contains(f.Message, "not motel's answer: "+want) {
			t.Errorf("%s: exit %d %q", body, f.Exit, f.Message)
		}
	}
}

// The values a status, a record and the box's status print unlabelled
// or as markers are held to the server's grammars: any other is not
// motel's answer.
func TestServerGrammars(t *testing.T) {
	cl := &Client{Box: Box{Host: "127.0.0.1"}}
	const status = `{"app":"blog","phase":"idle","current_version":"v2","unit":"motel-blog.v2.0123456789abcdef.service","available_versions":["v2","v1"],` +
		`"deploys":[{"version":"v2","status":"succeeded"},{"version":"v1","status":"failed","phase":"soaking"}],` +
		`"last_deploy":{"version":"v2","status":"succeeded","started_at":"2026-11-14T09:12:30Z","finished_at":"2026-11-14T09:13:00Z","phases":[{"name":"downloading","started_at":"2026-11-14T09:12:30Z","seconds":1}]}}`
	const box = `{"commit":"` + "3f9c2a1e00000000000000000000000000000000" + `","box_webhook":"127.0.0.1","sha256":"` + "9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b" + `"}`
	for _, c := range []struct {
		name, body string
		v          any
		why        string
	}{
		{"a status", status, &wire.Status{}, ""},
		{"every phase an app has", strings.Replace(status, `"idle"`, `"stopping_old"`, 1), &wire.Status{}, ""},
		{"a phase no app has", strings.Replace(status, `"idle"`, `"running (v9)"`, 1), &wire.Status{}, "its phase is not one an app has"},
		{"a version with a space", strings.Replace(status, `"current_version":"v2"`, `"current_version":"v2 (running)"`, 1), &wire.Status{}, "a version in it is not a version"},
		{"a listed version with a space", strings.Replace(status, `["v2","v1"]`, `["v2","v1 (running)"]`, 1), &wire.Status{}, "a version in it is not a version"},
		{"a version of 65", strings.Replace(status, `"current_version":"v2"`, `"current_version":"`+strings.Repeat("v", 65)+`"`, 1), &wire.Status{}, "a version in it is not a version"},
		{"a unit motel does not name", strings.Replace(status, `motel-blog.v2.0123456789abcdef.service`, `sshd.service`, 1), &wire.Status{}, "its unit is not one motel names"},
		{"a watchdog state of each kind", strings.Replace(status, `"phase":"idle"`, `"phase":"idle","watchdog":{"state":"throttled"},"metrics":{"window":"24h"}`, 1), &wire.Status{}, ""},
		{"a watchdog with no state, metrics with an error alone", strings.Replace(status, `"phase":"idle"`, `"phase":"idle","watchdog":{},"metrics":{"error":"the store did not answer"}`, 1), &wire.Status{}, ""},
		{"a watchdog state with the client's words", strings.Replace(status, `"phase":"idle"`, `"phase":"idle","watchdog":{"state":"watching, 0 failed checks; run this"}`, 1), &wire.Status{}, "its watchdog's state is not one a watchdog has"},
		{"a watchdog state no watchdog has", strings.Replace(status, `"phase":"idle"`, `"phase":"idle","watchdog":{"state":"ok"}`, 1), &wire.Status{}, "its watchdog's state is not one a watchdog has"},
		{"a metrics window that closes the bracket", strings.Replace(status, `"phase":"idle"`, `"phase":"idle","metrics":{"window":"1h) forged"}`, 1), &wire.Status{}, "its metrics' window is not one a box keeps"},
		{"a metrics window no box keeps", strings.Replace(status, `"phase":"idle"`, `"phase":"idle","metrics":{"window":"7d"}`, 1), &wire.Status{}, "its metrics' window is not one a box keeps"},
		{"a deploy's status", strings.Replace(status, `"status":"failed"`, `"status":"failed  v0"`, 1), &wire.Status{}, "a deploy's status in it is not succeeded or failed"},
		{"a deploy's phase", strings.Replace(status, `"phase":"soaking"`, `"phase":"health gate"`, 1), &wire.Status{}, "a deploy's phase in it is not one a deploy has"},
		{"a phase's name in the last deploy", strings.Replace(status, `"name":"downloading"`, `"name":"fetch"`, 1), &wire.Status{}, "a deploy's phase in it is not one a deploy has"},
		{"a box status", box, &wire.BoxStatus{}, ""},
		{"a commit of 39", strings.Replace(box, "3f9c2a1e0", "3f9c2a1e", 1), &wire.BoxStatus{}, "its commit is not a commit id"},
		{"a commit in capitals", strings.Replace(box, "3f9c2a1e", "3F9C2A1E", 1), &wire.BoxStatus{}, "its commit is not a commit id"},
		{"a digest with a space", strings.Replace(box, "9b9b9b9b", "9b9b 9b9", 1), &wire.BoxStatus{}, "its sha256 is not a digest"},
	} {
		err := cl.Read(&Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(c.body)}, c.v)
		if c.why == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if f := failure(t, err); f.Exit != ExitUnknown || !strings.Contains(f.Message, "not motel's answer: "+c.why+")") {
			t.Errorf("%s: exit %d %q", c.name, f.Exit, f.Message)
		}
	}
}
