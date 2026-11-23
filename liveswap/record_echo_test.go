package liveswap

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestRecordNotFoundIsThePlainSentence: the 404 is the handler's own
// sentence and the version, whatever else is so: an env_file value that
// is one of the sentence's own words does not stand as a marker in it,
// a version that is a commit id or is shaped like a token is the
// caller's own and reads as sent, and neither it nor the status (moteld's
// own) is withheld while the app's env_file does not read; nor is the
// 422 of a query that does not parse.
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
	if code, body := get("/" + app); code != http.StatusOK || !strings.HasPrefix(body, `{"app":"`+app+`"`) {
		t.Errorf("the status while the env_file does not read: %d %s, want it sent: it is moteld's own", code, body)
	}
	if code, body := get("/" + app + "?deploy=v9"); code != http.StatusNotFound || body != `{"error":"no deploy recorded for version v9"}`+"\n" {
		t.Errorf("?deploy=v9 while the env_file does not read: %d %s", code, body)
	}
	// Nor is the 422 of a query that does not parse, which holds the
	// parser's words and three bytes of the request at most.
	if code, body := get("/" + app + "?x=%ZZ"); code != http.StatusUnprocessableEntity || body != `{"error":"malformed query: invalid URL escape \"%ZZ\""}`+"\n" {
		t.Errorf("a query that does not parse while the env_file does not read: %d %s", code, body)
	}
}

// TestRecordNotReadTellsNothingOfTheEnv: when GET /<app>?deploy=<version>
// reads no record, its answer is the handler's words and the version
// as it was asked, whoever asks and whatever the app's env_file holds
// (no filter meets it): the same bytes for an app that holds the values
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
	if got := string(ma.door().text("correcthorse s3cretpw9")); !strings.Contains(got, "[redacted:DB_PASSWORD]") || !strings.Contains(got, "[redacted:DATABASE_URL]") {
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

	// The 500 of the same read: a record's name that cannot be read as
	// one, here a directory in its place. Its error names the file, and
	// so the version asked for and the app's directory; the answer is
	// one fixed sentence, the same bytes whatever is asked and whatever
	// the app holds, and the error is in the journal.
	core, logs := observer.New(zap.ErrorLevel)
	for _, name := range []string{secret, plain} {
		dir, err := recordsDir(r.apps[name].spec.dirs, true)
		must(t, err)
		must(t, os.Mkdir(filepath.Join(dir, "correcthorse.json"), 0o700))
		must(t, os.Mkdir(filepath.Join(dir, "wrongguess12.json"), 0o700))
		r.apps[name].logger = zap.New(core)
	}
	const unread = `{"error":"this deploy's record could not be read; the box's journal says why (journalctl -u moteld)"}` + "\n"
	for who, ask := range callers {
		for _, name := range []string{secret, plain} {
			for _, guess := range []string{"correcthorse", "wrongguess12"} {
				if code, body := ask(http.MethodGet, "/"+name+"?deploy="+guess, ""); code != http.StatusInternalServerError || body != unread {
					t.Errorf("%s, %s's record %s that does not read: %d %s, want 500 %s", who, name, guess, code, body, unread)
				}
			}
		}
	}
	if lines := logs.FilterMessage("reading a deploy record").All(); len(lines) != 8 || lines[0].ContextMap()["version"] == nil || !strings.Contains(fmt.Sprint(lines[0].ContextMap()["error"]), ".json is not a regular file") {
		t.Errorf("the journal for a record that does not read: %v", logs.All())
	}
	for _, name := range []string{secret, plain} {
		dir, err := recordsDir(r.apps[name].spec.dirs, true)
		must(t, err)
		must(t, os.Remove(filepath.Join(dir, "correcthorse.json")))
		must(t, os.Remove(filepath.Join(dir, "wrongguess12.json")))
	}

	// A query that does not parse: the parser's words, quoting the
	// request's own bad escape.
	for who, ask := range callers {
		code, body := ask(http.MethodGet, "/"+secret+"?x=%ZZ", "")
		if code != http.StatusUnprocessableEntity || !strings.HasPrefix(body, `{"error":"malformed query: `) || strings.Contains(body, "redacted") {
			t.Errorf("%s, a query that does not parse: %d %s", who, code, body)
		}
	}
}
