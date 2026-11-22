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
	r := NewRedactor([]string{"DATABASE_URL=postgres://app:" + pw + "@db/app", "SHORT=abc", "TOKEN=" + pw}, nil)
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

func TestRedactorSafeList(t *testing.T) {
	sha := "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c" // a 40-char git SHA as a version
	r := NewRedactor(nil, []string{sha})
	if out, _ := r.Redact("current " + sha + " ok"); !strings.Contains(out, sha) {
		t.Errorf("a safe-listed version was masked: %q", out)
	}
	if out, _ := (*Redactor)(nil).Redact("current " + sha + " ok"); strings.Contains(out, sha) {
		t.Errorf("without the safe list the same SHA must be masked: %q", out)
	}
	// A dotted version is tokenised the way layer 4 tokenises, so its
	// generated segment is safe wherever it appears on its own.
	dotted := "2026.09.14." + sha
	r = NewRedactor(nil, []string{dotted})
	if out, _ := r.Redact(`"current_version":"` + dotted + `","unit":"motel-x.` + dotted + `.0a1b2c3d0a1b2c3d.service"`); strings.Contains(out, "[masked") {
		t.Errorf("a dotted safe version was masked: %q", out)
	}
	// A safe string equal to an env_file value exempts nothing (rule
	// 1): the value is still redacted, whatever named the string.
	r = NewRedactor([]string{"RELEASE=" + sha, "ROOT=/var/lib/liveswap/example"}, []string{sha, "/var/lib/liveswap/example"})
	if out, keys := r.Redact(sha + " at /var/lib/liveswap/example/run"); strings.Contains(out, sha) || strings.Contains(out, "/var/lib/liveswap/example") || strings.Join(keys, ",") != "RELEASE,ROOT" {
		t.Errorf("a safe string exempted an env_file value: %q %v", out, keys)
	}
}

// Rule 1 from the filter's callers' side: every kind of safe string —
// the app's name, a version a request named, one read off disk, an
// app dir — is dropped when it equals a known value, and the rest of
// the safe list still holds out of the heuristics.
func TestNoSafeStringEqualsAKnownValue(t *testing.T) {
	sha := "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c"
	env := []string{"NAME=blog-production", "VERSION=" + sha, "DIR=/var/lib/liveswap/blog/shared"}
	r := NewRedactor(env, []string{"blog-production", sha, "/var/lib/liveswap/blog/shared", "/var/lib/liveswap/blog", "e7f8091a2b3c4d5e6f708192a3b4c5d63f9a1c2b"})
	out, keys := r.Redact("blog-production " + sha + " /var/lib/liveswap/blog/shared /var/lib/liveswap/blog e7f8091a2b3c4d5e6f708192a3b4c5d63f9a1c2b")
	for _, v := range []string{"blog-production", sha, "/var/lib/liveswap/blog/shared"} {
		if strings.Contains(out, v) {
			t.Errorf("safe string equal to a known value survived: %q in %q", v, out)
		}
	}
	if strings.Join(keys, ",") != "DIR,NAME,VERSION" {
		t.Errorf("keys = %v", keys)
	}
	if !strings.Contains(out, "/var/lib/liveswap/blog ") || !strings.Contains(out, "e7f8091a2b3c4d5e6f708192a3b4c5d63f9a1c2b") {
		t.Errorf("a safe string that is no known value was not held out of the heuristics: %q", out)
	}
}

func TestRedactorSharedValuesAndMarkers(t *testing.T) {
	// Two keys with one value: both are reported, and the marker names
	// both.
	r := NewRedactor([]string{"A=abcdefghij", "B=abcdefghij"}, nil)
	out, keys := r.Redact("x abcdefghij y")
	if out != "x [redacted:A,B] y" || strings.Join(keys, ",") != "A,B" {
		t.Errorf("shared value: %q %v", out, keys)
	}
	// A value that is a substring of the usual marker — any key's
	// value, not only the one being replaced — moves every marker to
	// one that does not contain it.
	r = NewRedactor([]string{"A=abcdefghij", "SECRET=redacted", "OTHER=REDACTED:OTHER"}, nil)
	for _, in := range []string{"value redacted here", "REDACTED:OTHER", "x abcdefghij y", "abcdefghij redacted REDACTED:OTHER"} {
		out, _ := r.Redact(in)
		if strings.Contains(out, "redacted") || strings.Contains(out, "REDACTED:OTHER") || strings.Contains(out, "abcdefghij") {
			t.Errorf("a marker reintroduced a value: %q -> %q", in, out)
		}
	}
	// A key named for its own value defeats every named candidate; the
	// marker is then a number in brackets.
	r = NewRedactor([]string{"withheld=withheld"}, nil)
	if out, _ := r.Redact("it was withheld"); strings.Contains(out, "withheld") || !strings.Contains(out, "[#1]") {
		t.Errorf("key named for its value: %q", out)
	}
	// A heuristic layer's own marker text can contain a known value;
	// the final pass removes it.
	r = NewRedactor([]string{"SECRET=private-key"}, nil)
	if out, _ := r.Redact("-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----"); strings.Contains(out, "private-key") {
		t.Errorf("a shape marker reintroduced a value: %q", out)
	}
	// The safe list exempts a value only when it equals a safe string
	// whole: a dotted version's segment is safe from the entropy
	// heuristic, not a non-secret when an env_file value equals it.
	sha := "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c"
	r = NewRedactor([]string{"TOKEN=" + sha}, []string{"2026.09.14." + sha})
	if out, keys := r.Redact("saw " + sha); strings.Contains(out, sha) || len(keys) != 1 {
		t.Errorf("an env_file value equal to a safe token survived: %q %v", out, keys)
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
		r := NewRedactor([]string{"DATABASE_URL=postgres://app:" + pw + "@db:5432/app?sslmode=require"}, nil)
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

func TestRedactorSafeSpansSurviveShapes(t *testing.T) {
	// A version shaped like a provider token is on the safe list and
	// comes through the shape rules untouched, everywhere it appears.
	v := "ghp_" + strings.Repeat("a1B2", 9)
	r := NewRedactor(nil, []string{v})
	in := `{"current_version":"` + v + `","unit":"motel-x.` + v + `.0a1b2c3d0a1b2c3d.service"}`
	if out, _ := r.Redact(in); out != in {
		t.Errorf("a safe version was rewritten by a shape rule:\n got %s\nwant %s", out, in)
	}
	if out, _ := (*Redactor)(nil).Redact(in); !strings.Contains(out, "[redacted:github-token]") {
		t.Errorf("without the safe list the same shape must be caught: %s", out)
	}
}

func TestRedactorShortSafeValuesDoNotSplitCredentials(t *testing.T) {
	// A one-character app name or version is safe, but protecting it as
	// a substring everywhere would carve a credential into pieces too
	// short for the entropy layer to see.
	cred := "k3J9xQ2mZ8pL0vT7wR4nY6bH1cD5fG"
	r := NewRedactor(nil, []string{"1", "a", "k3J9"})
	if out, _ := r.Redact("token " + cred + " end"); strings.Contains(out, cred) {
		t.Errorf("a short safe value shielded a credential: %q", out)
	}
}

func TestRedactorShapes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"jwt", "token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXBvIn0.c2lnbmF0dXJlX2hlcmVfMTIz here", "token [redacted:jwt] here"}, // gitleaks:allow
		{"bearer", "Authorization: Bearer AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", "Authorization: Bearer [redacted:token]"},
		{"401 placeholder untouched", "invalid or missing deploy token (Authorization: Bearer <jwt>)", "invalid or missing deploy token (Authorization: Bearer <jwt>)"},
		{"userinfo", "dial postgres://app:hunter2@db:5432/app", "dial postgres://[redacted:userinfo]@db:5432/app"},
		{"query", `fetch https://s3.example/a.tgz?X-Amz-Signature=abc&x=1 failed`, "fetch https://s3.example/a.tgz?[redacted:query] failed"},
		{"query stops at a quote", `{"url":"https://s3.example/a.tgz?sig=1","v":"x"}`, `{"url":"https://s3.example/a.tgz?[redacted:query]","v":"x"}`},
		{"aws", "key AKIAIOSFODNN7EXAMPLE used", "key [redacted:aws-key] used"},
		{"github", "ghp_" + strings.Repeat("a1B2", 9) + " leaked", "[redacted:github-token] leaked"},
		{"slack", "xoxb-123456789012-abcdefghijkl", "[redacted:slack-token]"},
		{"stripe", "sk_live_" + strings.Repeat("Ab1", 6), "[redacted:stripe-key]"},
		{"pem", "-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----", "[redacted:private-key]"},
		{"assignment", "PASSWORD=correct-horse-battery next", "PASSWORD=[redacted:credential] next"},
		{"assignment colon", "api_key: 0123456789abcdef", "api_key: [redacted:credential]"}, // gitleaks:allow
		{"prefixed key", "DB_PASSWORD=correct-horse-battery", "DB_PASSWORD=[redacted:credential]"},
		{"aws secret key", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "AWS_SECRET_ACCESS_KEY=[redacted:credential]"}, // gitleaks:allow
		{"json quoted", `{"password":"correct-horse-battery","x":1}`, `{"password":"[redacted:credential]","x":1}`},
		{"single quoted", `password='correct-horse-battery' next`, `password='[redacted:credential]' next`},
		{"a path is not a credential", "--api-key=/etc/motel/app.key", "--api-key=/etc/motel/app.key"},
		{"a placeholder is not a credential", "api_key={shared_dir}/key", "api_key={shared_dir}/key"},
		{"a marker is not re-redacted", "API_KEY=[redacted:API_KEY] next", "API_KEY=[redacted:API_KEY] next"},
		{"prose is not an assignment", "secrets: the examples do not", "secrets: the examples do not"},
		{"short assignment untouched", "password=short", "password=short"},
	} {
		if got, _ := (*Redactor)(nil).Redact(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestRedactorEntropy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		masked bool
	}{
		{"random base64", "k3J9xQ2mZ8pL0vT7wR4nY6bH1cD5fG", true},
		{"sha256 hex", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", true},
		{"identifier", "not_healthy_within_deadline_reached", false},
		{"upper identifier", "SOME_VERY_LONG_ENV_KEY_NAME_HERE", false},
		{"path", "/var/lib/liveswap/example/releases/dx1/server", false},
		{"unit name", "motel-example.dx1.0a1b2c3d0a1b2c3d.service", false},
		{"timestamp", "2026-09-14T07:15:33.123456789Z", false},
		{"deploy subject", "repo:smallhoursorg/example:ref:refs/heads/main", false},
		{"commit version", "0a1b2c3d4e5f", false},
		{"lowercase plus digits only", "abcdefghijklmnopqrst1234567890", false},
		// The bar follows the length: a 20- or 22-character token
		// (secrets.token_urlsafe(16) is 22) cannot reach 4.5 bits a
		// character, and was never masked; an identifier with a digit
		// and a unit setting of the same length stay readable.
		{"22-character url-safe token", "k3J9xQ2mZ8pL0vT7wR4nY6", true}, // gitleaks:allow — made up for this test
		{"20-character url-safe token", "k3J9xQ2mZ8pL0vT7wR4n", true},   // gitleaks:allow — made up for this test
		{"identifier with a digit", "loadEd25519PrivateKey", false},
		{"unit setting", "RandomizedDelaySec=40s", false},
		{"error name with a digit", "Sha256DigestMismatchAt0", false},
	} {
		out, _ := (*Redactor)(nil).Redact("x " + tc.in + " y")
		if masked := strings.Contains(out, "[masked, "); masked != tc.masked {
			t.Errorf("%s: masked=%v, want %v: %q", tc.name, masked, tc.masked, out)
		}
		if !tc.masked && !strings.Contains(out, tc.in) {
			t.Errorf("%s: changed: %q", tc.name, out)
		}
	}
}

// An assignment is judged by its parts: the key stays readable and the
// value is masked on its own, and a short value is not carried over the
// bar by its key's letters.
func TestRedactorEntropyJudgesAnAssignmentByItsParts(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// A key the shape rules know (`API_KEY=`) is theirs first; one
		// they do not is layer 4's, by its parts.
		{"a secret assigned", "BUILD_ID=zK9dQ2mX8pL0vT7wR4nY6bH1", "BUILD_ID=[masked, 24 chars]"},
		{"a short value", "SOME_SETTING_NAME=aB3dE5fG7h", "SOME_SETTING_NAME=aB3dE5fG7h"},
		{"base64 padding is no assignment", "k3J9xQ2mZ8pL0vT7wR4nY6bH1cD5fG==", "[masked, 32 chars]"},
		{"a dotted version's assignment", "APP_VERSION=2026.11.07-rc1", "APP_VERSION=2026.11.07-rc1"},
	} {
		if got, _ := (*Redactor)(nil).Redact(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// The corpus: a status body and a failure body in the shapes the
// webhook produces today, and app output in the shape a later change
// will carry in responses, each with what must survive (.keep) and
// what must not (.gone), so the heuristics can never drift toward
// blanking what a reader needs.
func TestRedactorCorpus(t *testing.T) {
	files, err := filepath.Glob("testdata/redact/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	r := NewRedactor([]string{"DATABASE_URL=postgres://app:s3cr3t-pw-value@db/app", "API_KEY=zK9dQ2mX8pL0vT7wR4nY6bH1"}, []string{"dx1", "dx2", "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c"}) // gitleaks:allow
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
