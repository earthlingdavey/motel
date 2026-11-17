package liveswap

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordNotReadTellsNothingOfTheEnv: when GET /<app>?deploy=<version>
// reads no record, its answer is the handler's words and the version
// as it was asked, whoever asks and whatever the app's env_file holds
// (redact.go, rule 6): the same bytes for an app that holds the values
// guessed and for one that holds none. Through the app's filter the
// version came back as a marker naming the key whenever it held one of
// the app's values, as written, inside a longer string, as base64 or
// hex, or a URL value's password alone, and a person who may only read
// could try values until one did.
func TestRecordNotReadTellsNothingOfTheEnv(t *testing.T) {
	const secret, plain = "billingapp", "plainapp"
	r := newAppsRig(t, secret, plain)
	ma := r.apps[secret]
	must(t, os.WriteFile(ma.spec.envFile, []byte("DB_PASSWORD=correcthorse\nDATABASE_URL=postgres://app:s3cretpw9@db.internal/app\n"), 0o600)) // gitleaks:allow — made up for this test
	must(t, os.WriteFile(r.apps[plain].spec.envFile, nil, 0o600))
	for _, name := range []string{secret, plain} {
		must(t, r.apps[name].Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1", by: "ci"}))
	}
	// The app's filter does know the values: any body of the app's
	// would have them replaced.
	if got := ma.redactorFor(ma.status()).redactJSON([]byte(`{"x":"correcthorse s3cretpw9"}`)); !strings.Contains(got, "[redacted:DB_PASSWORD]") || !strings.Contains(got, "[redacted:DATABASE_URL]") {
		t.Fatalf("the app's filter does not know the test's values: %s", got)
	}
	callers := map[string]func(method, target, body string) (int, string){
		"a viewer": func(method, target, _ string) (int, string) {
			w := sendTo(t, r.h, method, target, token(r.people.carol))
			return w.Code, w.Body.String()
		},
		"a deploy token": func(method, target, body string) (int, string) {
			w := do(t, r.h, method, target, appToken(t), body)
			return w.Code, w.Body.String()
		},
	}
	guesses := []string{
		"wrongguess12",
		"correcthorse",                // a value as written
		"xcorrecthorsex",              // inside a longer string
		"nope.correcthorse.s3cretpw9", // several guesses in one request
		base64.RawURLEncoding.EncodeToString([]byte("correcthorse")),
		hex.EncodeToString([]byte("correcthorse")),
		"s3cretpw9", // a URL value's password alone
	}
	for who, ask := range callers {
		for _, guess := range guesses {
			code, body := ask(http.MethodGet, "/"+secret+"?deploy="+guess, "")
			if want := `{"error":"no deploy recorded for version ` + guess + `"}` + "\n"; code != http.StatusNotFound || body != want {
				t.Errorf("%s, ?deploy=%s: %d %s, want 404 %s", who, guess, code, body, want)
			}
			if c, b := ask(http.MethodGet, "/"+plain+"?deploy="+guess, ""); c != code || b != body {
				t.Errorf("%s, ?deploy=%s: %d %s of the app that holds the values, %d %s of the one that holds none", who, guess, code, body, c, b)
			}
		}
	}

	// The 500 of the same read names the record's file, and so the
	// version asked for: a record's name that cannot be read as one,
	// here a directory in its place.
	for _, name := range []string{secret, plain} {
		dir, err := recordsDir(r.apps[name].spec.dirs, true)
		must(t, err)
		must(t, os.Mkdir(filepath.Join(dir, "correcthorse.json"), 0o700))
	}
	for who, ask := range callers {
		code, body := ask(http.MethodGet, "/"+secret+"?deploy=correcthorse", "")
		if code != http.StatusInternalServerError || !strings.Contains(body, "reading deploy record: ") || !strings.Contains(body, "correcthorse.json") || strings.Contains(body, "redacted") {
			t.Errorf("%s, a record that does not read: %d %s, want a 500 naming the file as it was asked", who, code, body)
		}
		// The same but for each app's own directory.
		c, b := ask(http.MethodGet, "/"+plain+"?deploy=correcthorse", "")
		if c != code || strings.ReplaceAll(b, r.apps[plain].spec.dirs.deploys, ma.spec.dirs.deploys) != body {
			t.Errorf("%s, a record that does not read: %d %s of the app that holds the values, %d %s of the one that holds none", who, code, body, c, b)
		}
	}
	for _, name := range []string{secret, plain} {
		dir, err := recordsDir(r.apps[name].spec.dirs, true)
		must(t, err)
		must(t, os.Remove(filepath.Join(dir, "correcthorse.json")))
	}

	// A query that does not parse: the parser's words, quoting the
	// request's own bad escape.
	for who, ask := range callers {
		code, body := ask(http.MethodGet, "/"+secret+"?x=%ZZ", "")
		if code != http.StatusUnprocessableEntity || !strings.HasPrefix(body, `{"error":"malformed query: `) || strings.Contains(body, "redacted") {
			t.Errorf("%s, a query that does not parse: %d %s", who, code, body)
		}
	}

	// The exemption is those bodies', not the string's. A version an
	// earlier request stored, here one named after a value, is text the
	// app's filter still replaces: in the status and in the record, for
	// both callers, and in a deploy's refusal that names it.
	must(t, ma.Deploy(context.Background(), deployRequest{url: "https://x/2.tgz", version: "correcthorse", by: "ci"}))
	for who, ask := range callers {
		for _, target := range []string{"/" + secret, "/" + secret + "?deploy=correcthorse"} {
			code, body := ask(http.MethodGet, target, "")
			if code != http.StatusOK || strings.Contains(body, "correcthorse") || !strings.Contains(body, "[redacted:DB_PASSWORD]") {
				t.Errorf("%s, GET %s after a deploy named for a value: %d %s, want the value replaced", who, target, code, body)
			}
		}
	}
	if code, body := callers["a deploy token"](http.MethodPost, "/"+secret, `{"url":"https://x/3.tgz","version":"correcthorse"}`); code != http.StatusUnprocessableEntity || strings.Contains(body, "correcthorse") || !strings.Contains(body, "[redacted:DB_PASSWORD]") {
		t.Errorf("a deploy of that version again: %d %s, want a 422 with the value replaced", code, body)
	}
}

// TestRecordNotFoundIsThePlainSentence: the 404 is the handler's own
// sentence and the version, whatever else is so: an env_file value that
// is one of the sentence's own words does not stand as a marker in it,
// a version that is a commit id or is shaped like a token is the
// caller's own and reads as sent, and the answer is not withheld while
// the app's env_file does not read, as every body of the app's is.
func TestRecordNotFoundIsThePlainSentence(t *testing.T) {
	const app = "billingapp"
	r := newAppsRig(t, app)
	ma := r.apps[app]
	must(t, os.WriteFile(ma.spec.envFile, []byte("MODE_WORD=recorded\n"), 0o600))
	get := func(target string) (int, string) {
		w := sendTo(t, r.h, http.MethodGet, target, token(r.people.carol))
		return w.Code, w.Body.String()
	}
	for _, version := range []string{
		"v9",
		"recorded",
		strings.Repeat("0a1b2c3d", 5),
		"ghp_" + strings.Repeat("a1B2c3D4e5", 4)[:36], // gitleaks:allow — made up for this test: a version shaped like a token
	} {
		if code, body := get("/" + app + "?deploy=" + version); code != http.StatusNotFound || body != `{"error":"no deploy recorded for version `+version+`"}`+"\n" {
			t.Errorf("?deploy=%s: %d %s", version, code, body)
		}
	}
	must(t, os.WriteFile(ma.spec.envFile, []byte("this line is no assignment\n"), 0o600))
	ma.secretsMu.Lock()
	ma.secretsLoaded = false
	ma.secretsMu.Unlock()
	if code, body := get("/" + app); code != http.StatusOK || !strings.Contains(body, "response withheld") {
		t.Errorf("the status while the env_file does not read: %d %s, want it withheld", code, body)
	}
	if code, body := get("/" + app + "?deploy=v9"); code != http.StatusNotFound || body != `{"error":"no deploy recorded for version v9"}`+"\n" {
		t.Errorf("?deploy=v9 while the env_file does not read: %d %s", code, body)
	}
}
