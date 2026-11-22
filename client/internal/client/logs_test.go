package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// testCursor is a journal cursor of journalctl's form, made up.
const testCursor = "s=0123456789abcdef0123456789abcdef;i=1a2b;b=fedcba9876543210fedcba9876543210;m=3c4d5e;t=5f6e7d8c9b0a1;x=9f8e7d6c5b4a3210"

// TestLogsAsks: the count and the cursor are sent as the box reads
// them, the cursor's ; and = escaped (a raw ; is the box's 422); none
// is sent when none is named; a count outside wire.LogsLines and a
// cursor outside wire.LogsCursor are refused before anything is sent.
func TestLogsAsks(t *testing.T) {
	var got []string
	b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		motelJSON(w, 200, `{"kept":"on","lines":[],"more":false,"gap":false}`)
	})
	app := App{Name: "blog"}
	if _, _, err := b.client.Logs(context.Background(), app, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.client.Logs(context.Background(), app, "1000", testCursor); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/_motel/apps/blog/logs?",
		"/_motel/apps/blog/logs?after=" + strings.NewReplacer(";", "%3B", "=", "%3D").Replace(testCursor) + "&lines=1000",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("asked\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range []struct{ lines, after string }{
		{"100", ""},
		{"", "s=1;i=2"},
		{"", testCursor + "\n"},
		{"", "-" + testCursor},
	} {
		_, _, err := b.client.Logs(context.Background(), app, c.lines, c.after)
		if f := failure(t, err); f.Exit != ExitUsage {
			t.Errorf("lines %q after %q: exit %d %q", c.lines, c.after, f.Exit, f.Message)
		}
	}
	if b.hits.Load() != 2 {
		t.Errorf("%d requests, want 2: a refused argument sends nothing", b.hits.Load())
	}
}

// TestReadLogs: GET /_motel/apps/<app>/logs's answer is taken only with
// kept a word (one the client does not know is taken, for the command
// to say so), its lines, a cursor of the box's grammar or none, and each
// line's release a version or "" (one the box's filter replaced part of
// is the client's words); a line's text is taken whatever it holds.
func TestReadLogs(t *testing.T) {
	cl := &Client{Box: Box{Host: "127.0.0.1"}}
	whole := `{"kept":"on","lines":[{"at":"2026-11-20T09:30:00.123456Z","release":"v42","text":"GET /cart 200 \u001b[31m12ms"},` +
		`{"at":"2026-11-20T09:30:01Z","release":"","text":"fatal: cannot start"}],"more":false,"gap":false,"cursor":"` + testCursor + `","redacted_env":["DATABASE_URL"]}`
	read := func(body string) (wire.Logs, error) {
		var l wire.Logs
		ans := &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
		return l, cl.Read(ans, &l)
	}
	if l, err := read(whole); err != nil || len(l.Lines) != 2 || l.Cursor != testCursor || l.Lines[0].Text != "GET /cart 200 \x1b[31m12ms" {
		t.Fatalf("the whole answer: %v, %+v", err, l)
	}
	for _, c := range []struct{ name, body string }{
		{"kept off", `{"kept":"off","lines":[],"more":false,"gap":false}`},
		{"a word this client does not know", `{"kept":"later","lines":[]}`},
		{"no cursor", strings.Replace(whole, `"cursor":"`+testCursor+`",`, "", 1)},
	} {
		if _, err := read(c.body); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if l, err := read(strings.Replace(whole, `"release":"v42"`, `"release":"[redacted:RELEASE]"`, 1)); err != nil || l.Lines[0].Release != Filtered {
		t.Errorf("a marker in a release: %v, %+v", err, l.Lines)
	}
	for _, c := range []struct{ name, body, want string }{
		{"no kept", strings.Replace(whole, `"kept":"on",`, "", 1), "it has no kept)"},
		{"no lines", `{"kept":"on","more":false}`, "it has no lines)"},
		{"lines null", `{"kept":"on","lines":null}`, "it has no lines)"},
		{"kept not a word", strings.Replace(whole, `"kept":"on"`, `"kept":"on\u001b[2J"`, 1), "its kept is not a word)"},
		{"a cursor not a cursor", strings.Replace(whole, testCursor, "s=1;i=2", 1), "its cursor is not a journal cursor)"},
		{"a cursor the filter changed", strings.Replace(whole, testCursor, "[redacted:CURSOR]", 1), "its cursor is not a journal cursor)"},
		{"a release not a version", strings.Replace(whole, `"release":"v42"`, `"release":"v42 \u001b[2J"`, 1), "a line's release in it is not a version)"},
	} {
		_, err := read(c.body)
		if err == nil {
			t.Errorf("%s: taken", c.name)
			continue
		}
		f := failure(t, err)
		if f.Exit != ExitUnknown || !strings.Contains(f.Message, "not moteld's answer: "+c.want) {
			t.Errorf("%s: exit %d %q", c.name, f.Exit, f.Message)
		}
	}
	// The box's filter withheld the body: said as that, exit 1, with the
	// body for --json; its last resort, [#0], the same.
	for _, body := range []string{`{"error":"response withheld: the app's env_file could not be read"}`, `{"error":"[#0]"}`} {
		_, err := read(body)
		if f := failure(t, err); f.Exit != ExitFailed || !strings.Contains(f.Message, "withheld its answer") || string(f.Body) != body {
			t.Errorf("%s: exit %d %q", body, f.Exit, f.Message)
		}
	}
}

// TestLogsBusy: a 503 with Retry-After, which the box answers while it
// runs as many logs reads as it runs at once, is not waited out by the
// read: it fails at once, exit 3, with the wait on the failure for a
// follower; a 503 with none, one asking for more than a minute, or one
// whose body is not a JSON error, carries no wait.
func TestLogsBusy(t *testing.T) {
	busy := `{"error":"the box is running as many logs reads as it runs at once; retry after 1 s"}`
	for _, c := range []struct {
		after, body string
		want        time.Duration
	}{{"1", busy, time.Second}, {"0", busy, time.Second}, {"", busy, 0}, {"60", busy, time.Minute}, {"61", busy, 0}, {"7200", busy, 0},
		// Another server's page, or a 503 that says nothing: not waited.
		{"120", "<html>down for maintenance</html>", 0}, {"1", `{}`, 0}} {
		b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
			if c.after != "" {
				w.Header().Set("Retry-After", c.after)
			}
			motelJSON(w, http.StatusServiceUnavailable, c.body)
		})
		_, _, err := b.client.Logs(context.Background(), App{Name: "blog"}, "", "")
		f := failure(t, err)
		if f.Exit != ExitUnknown || f.Status != http.StatusServiceUnavailable || f.RetryAfter != c.want {
			t.Errorf("Retry-After %q: exit %d, status %d, wait %s; want %s", c.after, f.Exit, f.Status, f.RetryAfter, c.want)
		}
		if len(b.slept) != 0 || b.hits.Load() != 1 {
			t.Errorf("Retry-After %q: slept %v over %d requests, want none over one", c.after, b.slept, b.hits.Load())
		}
	}
}
