package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactorKnownValues(t *testing.T) {
	const pw = `p@ss"w\ord-12345`
	r := NewRedactor([]string{"DATABASE_URL=postgres://app:" + pw + "@db/app", "SHORT=abc", "TOKEN=" + pw})
	full := "postgres://app:" + pw + "@db/app"
	for name, in := range map[string]string{
		"as written":   "dial " + full + " failed",
		"json escaped": string(mustJSON(t, "dial "+full+" failed")),
		"base64":       "x " + base64.StdEncoding.EncodeToString([]byte(pw)) + " y",
		"base64 raw":   "x " + base64.RawURLEncoding.EncodeToString([]byte(pw)) + " y",
		"hex":          "x " + hex.EncodeToString([]byte(pw)) + " y",
		"url escaped":  "GET /?p=" + url.QueryEscape(pw) + " " + url.PathEscape(pw),
	} {
		out, keys := r.Redact(in)
		if strings.Contains(out, pw) || strings.Contains(out, full) {
			t.Errorf("%s: value survived: %q", name, out)
		}
		if !strings.Contains(out, "[redacted:") {
			t.Errorf("%s: no marker in %q", name, out)
		}
		if len(keys) == 0 {
			t.Errorf("%s: no keys reported", name)
		}
	}
	out, keys := r.Redact("SHORT is abc and that is fine")
	if out != "SHORT is abc and that is fine" || len(keys) != 0 {
		t.Errorf("a value under %d chars must not be redacted: %q %v", secretMinLen, out, keys)
	}
	// The longer form (DATABASE_URL) wins over the value it contains
	// (TOKEN) — a value of both, since the URL's password is a known
	// value on its own — and both keys are reported when both appear.
	out, keys = r.Redact(full + " and " + pw)
	if out != "[redacted:DATABASE_URL] and [redacted:DATABASE_URL,TOKEN]" {
		t.Errorf("longest-first: %q", out)
	}
	if strings.Join(keys, ",") != "DATABASE_URL,TOKEN" {
		t.Errorf("keys = %v", keys)
	}
}

func TestRedactorSharedValuesAndMarkers(t *testing.T) {
	// Two keys with one value: both are reported, and the marker names
	// both.
	r := NewRedactor([]string{"A=abcdefghij", "B=abcdefghij"})
	out, keys := r.Redact("x abcdefghij y")
	if out != "x [redacted:A,B] y" || strings.Join(keys, ",") != "A,B" {
		t.Errorf("shared value: %q %v", out, keys)
	}
	// A value that is a substring of the usual marker — any key's
	// value, not only the one being replaced — moves every marker to
	// one that does not contain it.
	r = NewRedactor([]string{"A=abcdefghij", "SECRET=redacted", "OTHER=REDACTED:OTHER"})
	for _, in := range []string{"value redacted here", "REDACTED:OTHER", "x abcdefghij y", "abcdefghij redacted REDACTED:OTHER"} {
		out, _ := r.Redact(in)
		if strings.Contains(out, "redacted") || strings.Contains(out, "REDACTED:OTHER") || strings.Contains(out, "abcdefghij") {
			t.Errorf("a marker reintroduced a value: %q -> %q", in, out)
		}
	}
	// A key named for its own value defeats every named candidate; the
	// marker is then a number in brackets.
	r = NewRedactor([]string{"withheld=withheld"})
	if out, _ := r.Redact("it was withheld"); strings.Contains(out, "withheld") || !strings.Contains(out, "[#1]") {
		t.Errorf("key named for its value: %q", out)
	}
	// A shape rule's own marker text can contain a known value; the
	// final pass removes it.
	r = NewRedactor([]string{"SECRET=private-key"})
	if out, _ := r.Redact("-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----"); strings.Contains(out, "private-key") {
		t.Errorf("a shape marker reintroduced a value: %q", out)
	}
}

func TestRedactorBasicCredential(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Authorization: Basic dToxMjM0NTY3OA==", "Authorization: Basic [redacted:basic-credential]"},
		{"Authorization: Basic dToxMjM0NTY3OA", "Authorization: Basic [redacted:basic-credential]"},
		{"Authorization: Basic YTpi", "Authorization: Basic [redacted:basic-credential]"}, // a:b, the shortest
		{"Basic authentication is required", "Basic authentication is required"},
		{"basic YWJjZGVmZ2hpams=", "basic YWJjZGVmZ2hpams="}, // decodes, but to no user:pass
	} {
		if got, _ := (*Redactor)(nil).Redact(tc.in); got != tc.want {
			t.Errorf("%q:\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// A driver prints a URL's password on its own, so it is a known value
// by itself — one net/url does not read included: an unescaped `#`, a
// bare `%` or a space, which libpq takes as it stands.
func TestRedactorURLPasswordNetURLDoesNotRead(t *testing.T) {
	for _, pw := range []string{"pass-word99", "pa#ss-word99", "pa%ss-word99", "pa ss-word99", "p@ss-word99"} {
		r := NewRedactor([]string{"DATABASE_URL=postgres://app:" + pw + "@db:5432/app?sslmode=require"})
		out, keys := r.Redact("FATAL: password authentication failed with " + pw + " for app")
		if strings.Contains(out, pw) || strings.Join(keys, ",") != "DATABASE_URL" {
			t.Errorf("%q: %q %v", pw, out, keys)
		}
	}
	// A list of hosts holds an authority per `://`.
	if got := urlPasswords("http://a:9200,http://user:s3cretpassw0rd@b:9200/"); strings.Join(got, ",") != "s3cretpassw0rd" {
		t.Errorf("a list of hosts: %q", got)
	}
	// An escape is read both ways, on any authority of a list.
	if got := urlPasswords("postgres://app:p%40ss-word99@db/app"); strings.Join(got, ",") != "p%40ss-word99,p@ss-word99" {
		t.Errorf("an escaped password: %q", got)
	}
	if got := urlPasswords("http://a:9200,http://user:p%40ss-word99@b:9200/"); strings.Join(got, ",") != "p%40ss-word99,p@ss-word99" {
		t.Errorf("an escaped password on the second authority: %q", got)
	}
	// A password holding `/` or `?` is read short by net/url and by
	// hand alike, and what stands before the cut is not the secret:
	// nothing is read on its own rather than a mask over nothing.
	for _, v := range []string{"postgres://app:abcdefghij/rest@db", "postgres://app:abcdefghij?x@db", "https://example.com/a@b:cdefghij", "redis://db:6379/0", "redis://host:6379?user=x@y:eightchars", "https://user@host/x", "smtp://host:587\ncontact: ops@example.com", "no url here"} {
		if got := urlPasswords(v); len(got) != 0 {
			t.Errorf("%q: read a password %q", v, got)
		}
	}
}

// TestRedactorWhatPassesAndWhatIsHidden is the filter's contract in one
// place (the owner's decision of 2026-11-22): what an ordinary app logs
// passes as written, a request id, a commit, a digest, a URL's query, a
// password= assignment and a generated token among it; a secret is
// hidden when it is a known env_file value or has one of the ten
// shapes.
func TestRedactorWhatPassesAndWhatIsHidden(t *testing.T) {
	r := NewRedactor([]string{"DB_PASSWORD=made-up-db-pass-01"})
	sig := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	for _, in := range []string{
		"GET /orders 200 request_id=f3a9c1e2-7b4d-4e8a-9c2f-1d3e5a7b9c0d",
		"deployed 3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c",
		"pulled sha256:" + sig,
		"fetch https://s3.example/a.tgz?X-Amz-Expires=300&X-Amz-Signature=" + sig + " failed",
		"dsn: host=db user=app password=correct-horse-battery dbname=app", // gitleaks:allow — made up for this test
		"session k3J9xQ2mZ8pL0vT7wR4nY6bH1cD5fG7h expired",                // gitleaks:allow — made up for this test
		"invalid or missing deploy token (Authorization: Bearer <jwt>)",
	} {
		if got, keys := r.Redact(in); got != in || len(keys) != 0 {
			t.Errorf("changed:\n got %q\nwant %q", got, in)
		}
	}
	for _, tc := range []struct{ name, in, want string }{
		{"known value", "FATAL: password authentication failed: made-up-db-pass-01", "FATAL: password authentication failed: [redacted:DB_PASSWORD]"},
		{"pem", "-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----", "[redacted:private-key]"},
		{"jwt", "token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXBvIn0.c2lnbmF0dXJlX2hlcmVfMTIz here", "token [redacted:jwt] here"}, // gitleaks:allow
		{"bearer", "Authorization: Bearer AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", "Authorization: Bearer [redacted:token]"},
		{"userinfo", "dial postgres://app:hunter2@db:5432/app", "dial postgres://[redacted:userinfo]@db:5432/app"},
		{"aws", "key AKIAIOSFODNN7EXAMPLE used", "key [redacted:aws-key] used"},
		{"github", "ghp_" + strings.Repeat("a1B2", 9) + " leaked", "[redacted:github-token] leaked"},
		{"github pat", "github_pat_" + strings.Repeat("a1B2", 6) + " leaked", "[redacted:github-token] leaked"},
		{"slack", "xoxb-123456789012-abcdefghijkl", "[redacted:slack-token]"},
		{"stripe", "sk_live_" + strings.Repeat("Ab1", 6), "[redacted:stripe-key]"},
		{"basic", "Authorization: Basic dToxMjM0NTY3OA==", "Authorization: Basic [redacted:basic-credential]"},
	} {
		if got, _ := r.Redact(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// The corpus: a status body and a failure body in the shapes the
// webhook produces today, and app output in the shape a later change
// will carry in responses, each with what must survive (.keep) and
// what must not (.gone), so the shape rules can never drift toward
// blanking what a reader needs.
func TestRedactorCorpus(t *testing.T) {
	files, err := filepath.Glob("testdata/redact/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	r := NewRedactor([]string{"DATABASE_URL=postgres://app:s3cr3t-pw-value@db/app", "API_KEY=zK9dQ2mX8pL0vT7wR4nY6bH1"}) // gitleaks:allow
	for _, f := range files {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := r.Redact(string(in))
		base := strings.TrimSuffix(f, ".txt")
		for _, want := range lines(t, base+".keep") {
			if !strings.Contains(out, want) {
				t.Errorf("%s: %q did not survive:\n%s", f, want, out)
			}
		}
		for _, gone := range lines(t, base+".gone") {
			if strings.Contains(out, gone) {
				t.Errorf("%s: %q leaked:\n%s", f, gone, out)
			}
		}
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b[1 : len(b)-1]
}
