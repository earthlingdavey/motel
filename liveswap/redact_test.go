package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// Rule 3: the outcome vocabulary stands outside the filter wherever
// it is a status, a phase or a phase's name — the status GET's
// last_deploy and deploys, a record, a phase timing — and nowhere
// else: the same word in error text is a known value like any other.
func TestOutcomeWordsSurviveEverywhere(t *testing.T) {
	r := NewRedactor([]string{"WORD=succeeded", "STEP=preparing", "END=stopping_old"}, nil)
	body := `{"deploys":[{"version":"v2","status":"failed","phase":"preparing"},{"version":"v1","status":"succeeded"}],` +
		`"last_deploy":{"version":"v2","status":"failed","error":"migrate: preparing succeeded then stopping_old","phase":"preparing",` +
		`"phases":[{"name":"preparing","seconds":1.5},{"name":"stopping_old","seconds":0.1}],"detail":{"log_tail":["status: succeeded"]}}}`
	out := r.redactJSON([]byte(body))
	for _, want := range []string{`"status":"failed"`, `"status":"succeeded"`, `"phase":"preparing"`, `"name":"preparing"`, `"name":"stopping_old"`,
		`"error":"migrate: [redacted:STEP] [redacted:WORD] then [redacted:END]"`, `"log_tail":["status: [redacted:WORD]"]`, `"redacted_env":["END","STEP","WORD"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	// In place: the body's order is as marshalled, so the last "phase"
	// in a failure body is still last_deploy's.
	if i := strings.LastIndex(out, `"phase":"`); !strings.HasPrefix(out[i:], `"phase":"preparing","phases"`) {
		t.Errorf("field order changed: %s", out)
	}
	// A value that overlaps a pair — its quotes, its key, more than the
	// word — is not a word: layer 1 sees it before the pair is held,
	// and the body it breaks is withheld, the key reported.
	for _, v := range []string{`"status":"failed","error"`, `succeeded"`, `:"preparing"`, `"name":"stopping_old"`} {
		out := NewRedactor([]string{"WEIRD=" + v}, nil).redactJSON([]byte(body))
		if !strings.HasPrefix(out, `{"error":"response withheld: a redacted value overlapped the response's own structure"`) || !strings.Contains(out, `"redacted_env":["WEIRD"]`) || strings.Contains(out, v) {
			t.Errorf("value %q overlapping a pair: %s", v, out)
		}
	}
	// A value that is part of a word is replaced inside it, which
	// leaves an outcome that is not a word: withheld too, rather than
	// served with a status nothing can read.
	for _, v := range []string{"ucceeded", "stopping", "reparing"} {
		out := NewRedactor([]string{"WEIRD=" + v}, nil).redactJSON([]byte(body))
		if !strings.HasPrefix(out, `{"error":"response withheld: a redacted value overlapped the response's own outcome"`) || !strings.Contains(out, `"redacted_env":["WEIRD"]`) || strings.Contains(out, v) {
			t.Errorf("value %q inside a word: %s", v, out)
		}
	}
	// A pair a record planted where motel writes none is a pair in
	// the count before and after alike, so it cannot mask a damaged
	// one; and with no value to redact in it, it is left as it is.
	planted := `{"version":"v1","status":"succeeded","x":{"name":"succeeded"}}`
	if out := NewRedactor([]string{"WEIRD=ucceeded"}, nil).redactJSON([]byte(planted)); !strings.HasPrefix(out, `{"error":"response withheld: a redacted value overlapped the response's own outcome"`) {
		t.Errorf("a planted pair masked a damaged outcome: %s", out)
	}
	if out := NewRedactor([]string{"WORD=succeeded"}, nil).redactJSON([]byte(planted)); out != planted {
		t.Errorf("a planted pair is a pair: %s", out)
	}
	// A URL value's password spelled as a word is a bare word form,
	// and rides through at an outcome key like the value would.
	if out := NewRedactor([]string{"DATABASE_URL=postgres://u:succeeded@h/db"}, nil).redactJSON([]byte(body)); !strings.Contains(out, `"status":"succeeded"`) || strings.Contains(out, "preparing succeeded then") {
		t.Errorf("a password spelled as a word: %s", out)
	}
	// A withheld body carries no outcome, and the words are
	// not smuggled onto it.
	r.withhold = "the app's env_file could not be read"
	if out := r.redactJSON([]byte(body)); strings.Contains(out, "succeeded") || strings.Contains(out, "preparing") {
		t.Errorf("outcome words on a withheld body: %s", out)
	}
	// Without a vocabulary secret the body is untouched.
	if out := NewRedactor(nil, nil).redactJSON([]byte(body)); out != body {
		t.Errorf("the filter changed a body with nothing to redact:\n%s\n%s", body, out)
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

func TestRedactJSONWithholdsAnUnparsableBody(t *testing.T) {
	// A value made of JSON's own punctuation can match the body's
	// structure rather than a string inside it. The body is withheld,
	// the keys still reported, and the result is always valid JSON.
	r := NewRedactor([]string{`ALLOWED=["a","b"]`}, nil)
	out := r.redactJSON([]byte(`{"app":"x","available_versions":["a","b"]}`))
	if !json.Valid([]byte(out)) {
		t.Fatalf("not JSON: %s", out)
	}
	if !strings.Contains(out, "withheld") || !strings.Contains(out, `"redacted_env":["ALLOWED"]`) {
		t.Errorf("withheld body = %s", out)
	}
	// An array body has nowhere to put redacted_env and stays an array.
	if out := r.redactJSON([]byte(`["x"]`)); out != `["x"]` {
		t.Errorf("array body = %s", out)
	}
	// The fallback's own text and the redacted_env field's key names go
	// through the last pass too: a second value equal to a word in the
	// fallback, or a key named for its value, cannot come back.
	r = NewRedactor([]string{`ALLOWED=["a","b"]`, "SECRET=redacted", "withheld=withheld"}, nil)
	out = r.redactJSON([]byte(`{"app":"x","available_versions":["a","b"],"e":"withheld"}`))
	rest := strings.Replace(out, `"redacted_env":`, "", 1)
	if !json.Valid([]byte(out)) || strings.Contains(rest, "redacted") || strings.Contains(rest, "withheld") || !strings.Contains(out, `"redacted_env":[`) {
		t.Errorf("fallback or field leaked a value, or the report is missing: %s", out)
	}
	// A value equal to the report field's own name: the field keeps its
	// name (fixed public text) and still names the key.
	r = NewRedactor([]string{"WEIRD=redacted_env"}, nil)
	out = r.redactJSON([]byte(`{"error":"saw redacted_env here"}`))
	if !json.Valid([]byte(out)) || !strings.Contains(out, `"redacted_env":["WEIRD"]`) || strings.Contains(out, "saw redacted_env") {
		t.Errorf("value equal to the field name: %s", out)
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

func TestRedactJSONWithholdsWhenTheEnvFileIsUnread(t *testing.T) {
	r := NewRedactor(nil, nil)
	r.withhold = `the app's env_file could not be read (open /etc/motel/we"ird\path.env: no such file)`
	out := r.redactJSON([]byte(`{"app":"x","error":"anything at all"}`))
	if !json.Valid([]byte(out)) || strings.Contains(out, "anything at all") || !strings.Contains(out, "withheld") {
		t.Errorf("body not withheld: %s", out)
	}
	// The reason carries a path; a quote or backslash in it must not
	// turn the diagnostic into the opaque fallback.
	var obj map[string]string
	if err := json.Unmarshal([]byte(out), &obj); err != nil || !strings.Contains(obj["error"], `we"ird\path.env`) {
		t.Errorf("reason lost: %s", out)
	}
	// And the diagnostic is filtered like any body: a token-shaped
	// segment in the path, or a line the parse error quotes, is masked.
	r.withhold = `/etc/motel/app.env:3: "TOKEN ghp_` + strings.Repeat("a1B2", 9) + `" is not KEY=VALUE`
	out = r.redactJSON([]byte(`{}`))
	if !json.Valid([]byte(out)) || strings.Contains(out, "ghp_") || !strings.Contains(out, "[redacted:") {
		t.Errorf("withhold diagnostic not filtered: %s", out)
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

// A redacted_env the body already carries below its top level — a
// deployResult's, nested in a status — is hoisted into the body's own,
// with the keys the filter found, each once.
func TestRedactJSONHoistsANestedRedactedEnv(t *testing.T) {
	r := NewRedactor([]string{"TOP=hunter2hunter2hunter2"}, nil) // gitleaks:allow
	body := `{"error":"failed","status":{"last_deploy":{"redacted_env":["API_TOKEN"],"detail":{"log_tail":["[redacted:API_TOKEN]"]}},"deploys":[{"redacted_env":["DB_URL","API_TOKEN"]}]},"note":"hunter2hunter2hunter2"}`
	out := r.redactJSON([]byte(body))
	var got struct {
		RedactedEnv []string `json:"redacted_env"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.RedactedEnv, ",") != "API_TOKEN,DB_URL,TOP" {
		t.Fatalf("redacted_env = %v in %s", got.RedactedEnv, out)
	}
}

// The hoist also runs when the filter itself found nothing — the
// detail's early replacement left only markers — and adds no field
// when the name is a string's text rather than a field.
func TestRedactJSONHoistsWithNothingOfItsOwnToReport(t *testing.T) {
	r := NewRedactor(nil, nil)
	for _, tc := range []struct{ name, body, want string }{
		{"a nested field alone", `{"error":"failed","status":{"last_deploy":{"redacted_env":["API_TOKEN"],"detail":{"probe":{"body":"[redacted:API_TOKEN]"}}}}}`, `"redacted_env":["API_TOKEN"]`},
		{"the name as text", `{"error":"the field is called \"redacted_env\""}`, ""},
	} {
		out := r.redactJSON([]byte(tc.body))
		var got struct {
			RedactedEnv *[]string `json:"redacted_env"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if tc.want == "" && got.RedactedEnv != nil {
			t.Errorf("%s: gained a field: %s", tc.name, out)
		}
		if tc.want != "" && (got.RedactedEnv == nil || !strings.HasPrefix(out, `{"error":"failed","redacted_env":["API_TOKEN"]`)) {
			t.Errorf("%s: no top-level field in %s", tc.name, out)
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

func TestRespondJSONRedacts(t *testing.T) {
	r := NewRedactor([]string{"SECRET=hunter2hunter2"}, nil)
	rec := httptest.NewRecorder()
	body := map[string]any{"error": "start failed: hunter2hunter2 rejected", "status": map[string]any{"app": "x"}}
	if err := RespondJSON(rec, http.StatusInternalServerError, body, r); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hunter2hunter2") {
		t.Errorf("value in body: %s", rec.Body.String())
	}
	if string(got["redacted_env"]) != `["SECRET"]` {
		t.Errorf("redacted_env = %s", got["redacted_env"])
	}
	if string(got["error"]) != `"start failed: [redacted:SECRET] rejected"` {
		t.Errorf("error = %s", got["error"])
	}
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("code %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// Nothing to redact: the body is the encoder's, byte for byte, with
	// no redacted_env field.
	rec = httptest.NewRecorder()
	if err := RespondJSON(rec, http.StatusOK, map[string]string{"error": "plain"}, r); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "{\"error\":\"plain\"}\n" {
		t.Errorf("untouched body = %q", rec.Body.String())
	}
}

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b[1 : len(b)-1]
}

// A body that already reports redacted keys — a stored record read
// back — keeps them through a later pass, whether or not that pass
// redacts more: withField merges with what the filtered body holds.
// An entry equal to a known value is body text like any other, and
// comes out as its marker with the key it belongs to reported.
func TestReportedKeysMergeWithTheBodys(t *testing.T) {
	r := NewRedactor([]string{"NEW=newvaluenewvalue1234"}, nil)
	if got := r.redactJSON([]byte(`{"error":"got newvaluenewvalue1234","redacted_env":["OLD"]}`)); !strings.Contains(got, `"redacted_env":["NEW","OLD"]`) || strings.Contains(got, "newvaluenewvalue1234") {
		t.Fatalf("got %s", got)
	}
	if got := r.redactJSON([]byte(`{"error":"nothing here","redacted_env":["OLD"]}`)); !strings.Contains(got, `"redacted_env":["OLD"]`) {
		t.Fatalf("a pass that redacts nothing must keep the keys: %s", got)
	}
	if got := r.redactJSON([]byte(`{"version":"v1","redacted_env":["newvaluenewvalue1234"]}`)); strings.Contains(got, "newvaluenewvalue1234") || !strings.Contains(got, `"redacted_env":["NEW","[redacted:NEW]"]`) {
		t.Fatalf("a planted reported entry leaked or was not accounted for: %s", got)
	}
	if got := r.redactJSON([]byte(`{"error":"got newvaluenewvalue1234","redacted_env":"not-an-array"}`)); !strings.Contains(got, `"redacted_env":["NEW"]`) {
		t.Fatalf("a field that is not an array is replaced: %s", got)
	}
	if got := r.redactJSON([]byte(`{"error":"got newvaluenewvalue1234","redacted_env":["OLD",5,null]}`)); !strings.Contains(got, `"redacted_env":["NEW","OLD"]`) {
		t.Fatalf("only the strings of a mixed array are kept: %s", got)
	}
}
