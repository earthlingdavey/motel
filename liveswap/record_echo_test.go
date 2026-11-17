package liveswap

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestRecordNotFoundTellsNothingOfTheEnv: GET /<app>?deploy=<version>
// for a version with no record answers with the handler's words and
// the version as it was asked, whoever asks and whatever the app's
// env_file holds (redact.go, rule 6). Through the app's filter the
// version came back as a marker naming the key whenever it held one
// of the app's values: as written, inside a longer string, as base64
// or hex, or a URL value's password alone. A person who may only read
// could try values until one did.
func TestRecordNotFoundTellsNothingOfTheEnv(t *testing.T) {
	const app = "billingapp"
	r := newAppsRig(t, app)
	ma := r.apps[app]
	must(t, os.WriteFile(ma.spec.envFile, []byte("DB_PASSWORD=correcthorse\nDATABASE_URL=postgres://app:s3cretpw9@db.internal/app\nMODE_WORD=recorded\n"), 0o600)) // gitleaks:allow — made up for this test
	must(t, ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1", by: "ci"}))
	// The app's filter does know the values: its status would mask them.
	if got := ma.redactorFor(ma.status()).redactJSON([]byte(`{"x":"correcthorse s3cretpw9"}`)); !strings.Contains(got, "[redacted:DB_PASSWORD]") || !strings.Contains(got, "[redacted:DATABASE_URL]") {
		t.Fatalf("the app's filter does not know the test's values: %s", got)
	}
	guesses := []string{
		"wrongguess12",
		"correcthorse",                // a value as written
		"xcorrecthorsex",              // inside a longer string
		"nope.correcthorse.s3cretpw9", // several guesses in one request
		base64.RawURLEncoding.EncodeToString([]byte("correcthorse")),
		hex.EncodeToString([]byte("correcthorse")),
		"s3cretpw9",                   // a URL value's password alone
		"recorded",                    // a value that is a word of the sentence itself
		strings.Repeat("0a1b2c3d", 5), // a commit id: not masked as a generated token
		"ghp_" + strings.Repeat("a1B2c3D4e5", 4)[:36], // gitleaks:allow — made up for this test: a version shaped like a token is the caller's own
	}
	callers := map[string]func(target string) (int, string){
		"a viewer": func(target string) (int, string) {
			w := sendTo(t, r.h, http.MethodGet, target, token(r.people.carol))
			return w.Code, w.Body.String()
		},
		"a deploy token": func(target string) (int, string) {
			w := do(t, r.h, http.MethodGet, target, appToken(t), "")
			return w.Code, w.Body.String()
		},
	}
	for who, get := range callers {
		for _, guess := range guesses {
			code, body := get("/" + app + "?deploy=" + guess)
			if want := `{"error":"no deploy recorded for version ` + guess + `"}` + "\n"; code != http.StatusNotFound || body != want {
				t.Errorf("%s, ?deploy=%s: %d %s, want 404 %s", who, guess, code, body, want)
			}
		}
	}

	// The exemption is that body's, not the string's. A version an
	// earlier request stored, here one named after a value, is text the
	// app's filter still replaces: in the status, in the record, and in
	// a refusal that names it.
	must(t, ma.Deploy(context.Background(), deployRequest{url: "https://x/2.tgz", version: "correcthorse", by: "ci"}))
	for who, get := range callers {
		for _, target := range []string{"/" + app, "/" + app + "?deploy=correcthorse"} {
			code, body := get(target)
			if code != http.StatusOK || strings.Contains(body, "correcthorse") || !strings.Contains(body, "[redacted:DB_PASSWORD]") {
				t.Errorf("%s, GET %s after a deploy named for a value: %d %s, want the value replaced", who, target, code, body)
			}
		}
	}

	// It holds nothing of the app's, so it is not withheld while the
	// app's env_file does not read, as every body of the app's is.
	must(t, os.WriteFile(ma.spec.envFile, []byte("this line is no assignment\n"), 0o600))
	ma.secretsMu.Lock()
	ma.secretsLoaded = false
	ma.secretsMu.Unlock()
	for who, get := range callers {
		if code, body := get("/" + app); code != http.StatusOK || !strings.Contains(body, "response withheld") {
			t.Errorf("%s, the status while the env_file does not read: %d %s, want it withheld", who, code, body)
		}
		if code, body := get("/" + app + "?deploy=v9"); code != http.StatusNotFound || body != `{"error":"no deploy recorded for version v9"}`+"\n" {
			t.Errorf("%s, ?deploy=v9 while the env_file does not read: %d %s", who, code, body)
		}
	}
}
