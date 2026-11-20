package liveswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// logsRig is alertsRig with demo keeping its output on the box, its
// journal src, and the handler's journal observed. erin reads demo's
// logs; alice views demo, carol the box, and dave holds no grant.
func logsRig(t *testing.T, journal []scriptEntry) (*Handler, *testPeople, *scriptedLogs, *observer.ObservedLogs) {
	t.Helper()
	h, people := alertsRig(t)
	core, logs := observer.New(zap.DebugLevel)
	h.logger = zap.New(core)
	ma := h.app.managed["demo"]
	ma.spec.deployLogLines = 200
	src := &scriptedLogs{journal: journal}
	ma.logs = src
	return h, people, src, logs
}

// demoJournal is n lines of demo's, each from its unit of release v1.
func demoJournal(n int, text func(i int) string) []scriptEntry {
	j := appJournal(n, text)
	for i := range j {
		j[i].userUnit = "motel-demo.v1.0a1b2c3d0a1b2c3d.service"
	}
	return j
}

// decodeLogs is the body as a client reads it: strictly, every field
// the wire type's.
func decodeLogs(t *testing.T, w *httptest.ResponseRecorder) wire.Logs {
	t.Helper()
	var got wire.Logs
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
	return got
}

const needsLogs = `{"error":"this needs the logs role on this app","needs":"logs","scope":"app"}` + "\n"

// TestLogsRoute: GET /_motel/apps/<app>/logs answers a person whose
// grants give the app's logs, and nobody else: a viewer of the app or of
// the box, a person with no grant, and a logs reader of demo on another
// app or a name that is no app, the same 403 naming logs; a global
// deploy source the 403 naming a key; every other caller the flat 401.
// The journal is read only for the reader.
func TestLogsRoute(t *testing.T) {
	h, people, src, _ := logsRig(t, demoJournal(3, lineN))
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"app"}` + "\n"
	flat := sendTo(t, h, "GET", "/_motel/apps/demo/logs")
	for _, c := range []struct {
		name, target, auth string
		code               int
		body               string
	}{
		{"a viewer of demo", "/_motel/apps/demo/logs", token(people.alice), http.StatusForbidden, needsLogs},
		{"a viewer of the box", "/_motel/apps/demo/logs", token(people.carol), http.StatusForbidden, needsLogs},
		{"a viewer of the box, on a name that is no app", "/_motel/apps/ghost/logs", token(people.carol), http.StatusForbidden, needsLogs},
		{"a person with no grant", "/_motel/apps/demo/logs", token(people.dave), http.StatusForbidden, needsLogs},
		{"demo's logs reader, on blog", "/_motel/apps/blog/logs", token(people.erin), http.StatusForbidden, needsLogs},
		{"demo's logs reader, on a name that is no app", "/_motel/apps/ghost/logs", token(people.erin), http.StatusForbidden, needsLogs},
		{"a viewer of demo, a query that is none", "/_motel/apps/demo/logs?after=junk", token(people.alice), http.StatusForbidden, needsLogs},
		{"a global deploy source", "/_motel/apps/demo/logs", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"demo's deploy source", "/_motel/apps/demo/logs", "Bearer " + appToken(t), http.StatusUnauthorized, ""},
		{"a key no line lists", "/_motel/apps/demo/logs", token(people.stranger), http.StatusUnauthorized, ""},
	} {
		w := sendTo(t, h, "GET", c.target, c.auth)
		if c.body == "" {
			sameAnswer(t, c.name, w, flat)
		} else if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.body)
		}
		wantNoMotelHeader(t, w)
	}
	if len(src.calls) != 0 {
		t.Errorf("the refusals read the journal %d times", len(src.calls))
	}
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	if got := decodeLogs(t, w); w.Code != http.StatusOK || got.Kept != wire.KeptOn || len(got.Lines) != 3 {
		t.Errorf("demo's logs reader: %d %s", w.Code, w.Body)
	}
	wantNoMotelHeader(t, w)
	if w := sendTo(t, h, "POST", "/_motel/apps/demo/logs", token(people.erin)); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: %d %s", w.Code, w.Body)
	}
}

// logs422 is the logs row's 422, naming its keys in its own words.
const logs422 = `{"error":"GET /_motel/apps/{app}/logs takes only lines (one of 200, 50, 1000) and after (a journal cursor, as an answer gave it), each once"}` + "\n"

// TestLogsRouteQuery: the row takes lines, of its set, and after, a
// cursor of journalCursor's form, each once; anything else is its 422,
// which echoes nothing of the request, and the journal is not read. A
// cursor's ; must be escaped: a raw one is no separator url.ParseQuery
// takes. lines and after reach journalctl as the row's word and the
// cursor whole.
func TestLogsRouteQuery(t *testing.T) {
	h, people, src, _ := logsRig(t, demoJournal(3, lineN))
	erin := token(people.erin)
	for _, q := range []string{
		"?lines=7", "?lines=", "?lines=50&lines=50", "?since=1h", "?after=", "?after=" + testCursor,
		"?after=t%3D5f0000000000", "?after=hunter2-of-demo", "?%ZZ", "?after=" + url.QueryEscape(testCursor) + "&after=" + url.QueryEscape(testCursor),
		"?after=" + url.QueryEscape(strings.ToUpper(testCursor)), "?Lines=50", "?a;b",
	} {
		w := sendTo(t, h, "GET", "/_motel/apps/demo/logs"+q, erin)
		if w.Code != http.StatusUnprocessableEntity || w.Body.String() != logs422 {
			t.Errorf("%.60s: %d %q, want 422 %q", q, w.Code, w.Body, logs422)
		}
	}
	if len(src.calls) != 0 {
		t.Fatalf("a query the row does not take read the journal %d times", len(src.calls))
	}
	for q, want := range map[string]string{
		"":            "-r -n 201",
		"?lines=50":   "-r -n 51",
		"?lines=1000": "-r -n 1001",
		"?" + url.Values{"after": {cursorOf(1)}, "lines": {"50"}}.Encode(): "--cursor=" + cursorOf(1) + " -n +52",
	} {
		src.calls = nil
		if w := sendTo(t, h, "GET", "/_motel/apps/demo/logs"+q, erin); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
		if got := strings.Join(src.calls[0], " "); !strings.Contains(got, " "+want+" SYSLOG_IDENTIFIER=motel-demo _UID="+strconv.Itoa(os.Getuid())+" _TRANSPORT=stdout") {
			t.Errorf("%s: journalctl %s, want %s", q, got, want)
		}
	}
}

// TestLogsBody: the body as a client reads it: the lines oldest first,
// each its journal time, its release from its unit ("" for a line that
// lost it) and its text; more; gap; the cursor of the last line; and in
// the wire type's order, so a client decoding it loses nothing.
func TestLogsBody(t *testing.T) {
	j := demoJournal(4, lineN)
	j[2].unit, j[2].userUnit = "", ""
	h, people, _, _ := logsRig(t, j)
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs?lines=50", token(people.erin))
	want := `{"kept":"on","lines":[` +
		`{"at":"2026-11-20T09:00:00Z","release":"v1","text":"line 0"},` +
		`{"at":"2026-11-20T09:00:00.001Z","release":"v1","text":"line 1"},` +
		`{"at":"2026-11-20T09:00:00.002Z","release":"","text":"line 2"},` +
		`{"at":"2026-11-20T09:00:00.003Z","release":"v1","text":"line 3"}],` +
		`"more":false,"gap":false,"cursor":"` + cursorOf(3) + `"}` + "\n"
	if w.Code != http.StatusOK || w.Body.String() != want || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("got %d %s\nwant %s", w.Code, w.Body, want)
	}
	decodesAsWire(t, "GET /_motel/apps/demo/logs", w)
	// A follow from the last: nothing, no cursor, the client keeps its own.
	w = sendTo(t, h, "GET", "/_motel/apps/demo/logs?after="+url.QueryEscape(cursorOf(3)), token(people.erin))
	if w.Body.String() != `{"kept":"on","lines":[],"more":false,"gap":false}`+"\n" {
		t.Errorf("at the end: %s", w.Body)
	}
	// A cursor the box does not hold: a fresh tail, gap.
	w = sendTo(t, h, "GET", "/_motel/apps/demo/logs?lines=50&after="+url.QueryEscape(testCursor), token(people.erin))
	if got := decodeLogs(t, w); !got.Gap || len(got.Lines) != 4 || got.Cursor != cursorOf(3) {
		t.Errorf("a cursor the box does not hold: %s", w.Body)
	}
}

// TestLogsKeptOff: an app whose deploy_log_lines is 0 keeps its output
// on the box: "off", no line, the journal not read, and no journal line.
func TestLogsKeptOff(t *testing.T) {
	h, people, src, logs := logsRig(t, demoJournal(3, lineN))
	h.app.managed["demo"].spec.deployLogLines = 0
	for _, q := range []string{"", "?lines=1000", "?after=" + url.QueryEscape(cursorOf(0))} {
		w := sendTo(t, h, "GET", "/_motel/apps/demo/logs"+q, token(people.erin))
		if w.Code != http.StatusOK || w.Body.String() != `{"kept":"off","lines":[],"more":false,"gap":false}`+"\n" {
			t.Errorf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	if len(src.calls) != 0 || logs.FilterMessage("access: person read logs").Len() != 0 {
		t.Errorf("kept off: %d reads, %d journal lines", len(src.calls), logs.FilterMessage("access: person read logs").Len())
	}
}

// TestLogsLineFilteredBeforeItIsCut: each line passes the app's filter
// on its own, before it is cut to 4 KiB, and the body the final pass:
// a value the app logged JSON-escaped (a quote in it) is replaced, which
// the whole body's filter misses; a value the 4 KiB cut falls inside is
// replaced whole, never sent as a piece, which a cut before the filter
// sends; a value past the cut is not sent; a line over the cut ends in
// "…" within 4 KiB; and redacted_env names every key any line replaced.
func TestLogsLineFilteredBeforeItIsCut(t *testing.T) {
	session := "made-up-session-secret-0001"
	across := strings.Repeat("x", logsLineMax-12) + " sess=" + session + " tail"
	beyond := strings.Repeat("y ", 60000) + session
	h, people, _, _ := logsRig(t, demoJournal(4, func(i int) string {
		return []string{`{"level":"info","smtp":"hunter\"2spike"}`, across, beyond, "running in production mode"}[i] // gitleaks:allow — made up for this test
	}))
	must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("SMTP_PASSWORD=hunter\"2spike\nSESSION_SECRET="+session+"\n"), 0o600)) // gitleaks:allow — made up for this test
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	got := decodeLogs(t, w)
	if w.Code != http.StatusOK || len(got.Lines) != 4 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, frag := range []string{"hunter", "2spike", "made-u", session[:12], "secret-0001"} {
		if strings.Contains(w.Body.String(), frag) {
			t.Errorf("the body holds %q", frag)
		}
	}
	if got.Lines[0].Text != `{"level":"info","smtp":"[redacted:SMTP_PASSWORD]"}` {
		t.Errorf("the JSON-escaped value: %q", got.Lines[0].Text)
	}
	for i, l := range got.Lines[1:3] {
		if len(l.Text) > logsLineMax || !strings.HasSuffix(l.Text, "…") || !utf8.ValidString(l.Text) {
			t.Errorf("line %d: %d bytes, ends %q", i+1, len(l.Text), l.Text[max(0, len(l.Text)-20):])
		}
	}
	if !strings.HasSuffix(got.Lines[1].Text, "x …") {
		t.Errorf("the value across the cut, replaced, then the marker the cut fell in dropped: …%q", got.Lines[1].Text[len(got.Lines[1].Text)-20:])
	}
	if strings.Join(got.RedactedEnv, ",") != "SESSION_SECRET,SMTP_PASSWORD" {
		t.Errorf("redacted_env: %q", got.RedactedEnv)
	}
	// The order this replaces: the cut first, then the filter, sends a
	// piece of the value no filter knows.
	f := h.app.managed["demo"].redactorFor(statusSnapshot{})
	if naive, _ := f.Redact(cutText(across, logsLineMax)); !strings.HasSuffix(naive, "sess=made-u") {
		t.Errorf("the cut before the filter did not send the piece the test is about: %q", naive[len(naive)-40:])
	}
}

// TestLogsBodyTimesAndCursorPassNoFilter: an env_file value that is the
// lines' date is replaced in their text and leaves every at whole; the
// cursor, whose ids layer 4 masks, comes back as the journal gave it;
// and a body whose final pass changed its shape (a value of JSON's
// punctuation across two lines) goes whole through the filter, the
// value not sent, the times and the cursor kept.
func TestLogsBodyTimesAndCursorPassNoFilter(t *testing.T) {
	j := demoJournal(2, func(i int) string { return fmt.Sprintf("2026-11-20 entry %d", i) })
	h, people, _, _ := logsRig(t, j)
	must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("API_VERSION=2026-11-20\n"), 0o600))
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	got := decodeLogs(t, w)
	if len(got.Lines) != 2 || !got.Lines[0].At.Equal(j[0].at) || !got.Lines[1].At.Equal(j[1].at) || got.Lines[0].Text != "[redacted:API_VERSION] entry 0" ||
		got.Cursor != cursorOf(1) || strings.Join(got.RedactedEnv, ",") != "API_VERSION" {
		t.Errorf("%s", w.Body)
	}
	// Shape: two lines made one by a value of the punctuation between them.
	// The body goes whole through the filter, and names the key a line's
	// own filtering replaced too.
	j = demoJournal(2, func(i int) string { return []string{"made-up-value-0001 end of one", "start of two"}[i] })
	h, people, _, logs := logsRig(t, j)
	value := `one"},{"at":"2026-11-20T09:00:00.001Z","release":"v1","text":"start` // the text, a time and the punctuation between two lines
	must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("K="+value+"\nS=made-up-value-0001\n"), 0o600))
	w = sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	if strings.Contains(w.Body.String(), value) || strings.Contains(w.Body.String(), "made-up-value") ||
		!strings.Contains(w.Body.String(), `"cursor":"`+cursorOf(1)+`"`) || !strings.Contains(w.Body.String(), `"redacted_env":["K","S"]`) {
		t.Errorf("a value across two lines: %d %s", w.Code, w.Body)
	}
	if got := logs.FilterMessage("access: person read logs").All(); len(got) != 1 || fmt.Sprint(got[0].ContextMap()["redacted_env"]) != "[K S]" {
		t.Errorf("the fallback's journal line: %v", got)
	}
	// A withheld body (the env_file unreadable) is the filter's sentence,
	// no cursor, and, sending no line, no journal line.
	must(t, os.Remove(h.app.managed["demo"].spec.envFile))
	h.app.managed["demo"].secretsLoaded = false
	w = sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), `{"error":"response withheld`) || strings.Contains(w.Body.String(), "cursor") {
		t.Errorf("withheld: %d %s", w.Code, w.Body)
	}
	if n := logs.FilterMessage("access: person read logs").Len(); n != 1 {
		t.Errorf("a withheld body wrote a read's line: %d lines", n)
	}
}

// TestLogsLongLineWithNoSpace: a line over 4 KiB with no whitespace
// (minified JSON) keeps all of the cut but the last longest-form bytes,
// where a value the cut split could be; and no piece of a value the
// cut fell in is sent.
func TestLogsLongLineWithNoSpace(t *testing.T) {
	value := "made-up-value-across-the-cut-0001" // 33 bytes; its hex form is the longest, 66
	f := NewRedactor([]string{"V=" + value}, nil)
	for _, at := range []int{logsLineMax - 40, logsLineMax - 3, logsLineMax + 20, logsLineMax + 60} {
		text := strings.Repeat("{", at) + value + strings.Repeat("}", 3000)
		seen := map[string]bool{}
		out := filterLogLine(f, text, seen)
		if len(out) > logsLineMax || len(out) < logsLineMax-3-66-len("[redacted:V]") || !strings.HasSuffix(out, "…") || !utf8.ValidString(out) {
			t.Errorf("value at %d: %d bytes, ends %q", at, len(out), out[max(0, len(out)-20):])
		}
		for n := 8; n <= len(value); n++ {
			if strings.Contains(out, value[:n]) {
				t.Errorf("value at %d: a piece of %d bytes is sent", at, n)
				break
			}
		}
	}
	// With whitespace near the end, the cut is there, as before.
	out := filterLogLine(f, strings.Repeat("x", logsLineMax-10)+" "+strings.Repeat("y", 100), map[string]bool{})
	if out != strings.Repeat("x", logsLineMax-10)+" …" {
		t.Errorf("a space near the cut: …%q", out[len(out)-20:])
	}
}

// TestLogsFourAtOnce: four reads run at once in the process; a fifth
// waits a second for one to end, then is told to come back, in fixed
// words with Retry-After, and the journal is not read for it.
func TestLogsFourAtOnce(t *testing.T) {
	h, people, src, _ := logsRig(t, demoJournal(3, lineN))
	src.hold = make(chan struct{})
	var wg sync.WaitGroup
	codes := make(chan int, logsReadsAtOnce)
	for range logsReadsAtOnce {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin)).Code
		}()
	}
	for len(logsSlots) < logsReadsAtOnce {
		time.Sleep(5 * time.Millisecond)
	}
	began := time.Now()
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" || w.Body.String() != `{"error":"`+logsBusyMessage+`"}`+"\n" || time.Since(began) < logsSlotWait {
		t.Errorf("the fifth: %d %v %s after %v", w.Code, w.Header(), w.Body, time.Since(began))
	}
	close(src.hold)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Errorf("one of the four: %d", c)
		}
	}
	if len(logsSlots) != 0 {
		t.Errorf("%d slots still taken", len(logsSlots))
	}
	if n := len(src.calls); n != logsReadsAtOnce {
		t.Errorf("%d reads of the journal", n)
	}
}

// TestLogsJournalLine: a read that sends lines is one line in the box's
// journal, naming the person, the app, how many, the cursors of the
// first and the last, the gap when there was one, and the keys the
// filter replaced; a read that sends none writes none.
func TestLogsJournalLine(t *testing.T) {
	h, people, _, logs := logsRig(t, demoJournal(5, func(i int) string { return "line hunter2-of-demo " + strconv.Itoa(i) }))
	sendTo(t, h, "GET", "/_motel/apps/demo/logs?lines=50&after="+url.QueryEscape(cursorOf(1)), token(people.erin))
	sendTo(t, h, "GET", "/_motel/apps/demo/logs?after="+url.QueryEscape(cursorOf(4)), token(people.erin))
	sendTo(t, h, "GET", "/_motel/apps/demo/logs?lines=50&after="+url.QueryEscape(testCursor), token(people.erin))
	got := logs.FilterMessage("access: person read logs").All()
	if len(got) != 2 {
		t.Fatalf("%d lines: %v", len(got), got)
	}
	for i, want := range []map[string]any{
		{"person": "erin", "app": "demo", "lines": int64(3), "first": cursorOf(2), "last": cursorOf(4), "redacted_env": []any{"DB_PASSWORD"}},
		{"person": "erin", "app": "demo", "lines": int64(5), "first": cursorOf(0), "last": cursorOf(4), "gap": true, "redacted_env": []any{"DB_PASSWORD"}},
	} {
		if got[i].Level != zap.WarnLevel || fmt.Sprint(got[i].ContextMap()) != fmt.Sprint(want) {
			t.Errorf("line %d: %v %v, want %v", i, got[i].Level, got[i].ContextMap(), want)
		}
	}
}

// TestLogsRefusedLine: a person Decide refuses an app's logs is a Warn
// line in the box's journal, naming the person, the app, the address
// and what was needed, at most once a minute for a person and an app.
func TestLogsRefusedLine(t *testing.T) {
	h, people, _, logs := logsRig(t, demoJournal(1, lineN))
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	saved := logsNow
	t.Cleanup(func() { logsNow = saved })
	logsNow = func() time.Time { return now }
	refusedLogs.Lock()
	clear(refusedLogs.last)
	refusedLogs.Unlock()
	refused := func() []observer.LoggedEntry { return logs.FilterMessage("access: person refused logs").All() }
	sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.alice))
	sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.alice))
	sendTo(t, h, "GET", "/_motel/apps/demo/alerts", token(people.dave)) // not a logs read
	if got := refused(); len(got) != 1 || got[0].Level != zap.WarnLevel ||
		fmt.Sprint(got[0].ContextMap()) != fmt.Sprint(map[string]any{"person": "alice", "app": "demo", "address": "192.0.2.1", "needs": "logs app"}) {
		t.Fatalf("%v", got)
	}
	sendTo(t, h, "GET", "/_motel/apps/ghost/logs", token(people.alice))
	now = now.Add(time.Minute)
	sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.alice))
	if got := refused(); len(got) != 3 || got[1].ContextMap()["app"] != "ghost" || got[2].ContextMap()["app"] != "demo" {
		t.Errorf("%v", got)
	}
	if logs.FilterMessage("access: person read logs").Len() != 0 {
		t.Error("a refusal wrote a read's line")
	}
}

// TestLogsReadFails: a journal that cannot be read is the 500 in fixed
// words, and the reason in the box's journal.
func TestLogsReadFails(t *testing.T) {
	h, people, src, logs := logsRig(t, nil)
	src.err = errors.New("journalctl: Failed to open journal")
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	if w.Code != http.StatusInternalServerError || w.Body.String() != `{"error":"`+logsUnreadMessage+`"}`+"\n" {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	if got := logs.FilterMessage("logs: an app's journal could not be read").All(); len(got) != 1 || !strings.Contains(fmt.Sprint(got[0].ContextMap()), "Failed to open journal") {
		t.Errorf("%v", got)
	}
}

// rawLogs hands each of its lines to the reader as journalctl's output,
// whatever the arguments.
type rawLogs [][]byte

func (r rawLogs) entries(_ context.Context, _ []string, each func([]byte) bool) error {
	for _, l := range r {
		if len(l) > 0 && !each(l) {
			break
		}
	}
	return nil
}

// FuzzLogsEntry: whatever journalctl prints, a read and its filter
// never panic; a read either fails or gives lines whose text is valid
// UTF-8, and a cursor journalCursor takes or none; and the body sent
// decodes as the wire type, each text valid UTF-8 of at most 4 KiB,
// holding no value of the app's.
func FuzzLogsEntry(f *testing.F) {
	for _, e := range []scriptEntry{
		{cursor: testCursor, at: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), message: "plain", userUnit: "motel-demo.v1.0a1b2c3d0a1b2c3d.service"},
		{cursor: testCursor, at: time.Unix(0, 0), message: []int{0xff, 'a', 0x1b}},
		{cursor: "s=0;i=1", at: time.Unix(1, 0), message: strings.Repeat("hunter2-of-demo ", 400)},
		{cursor: widestCursor, at: time.Unix(1, 0), message: nil},
	} {
		f.Add(e.json(), false)
		f.Add(append(append(e.json(), '\n'), e.json()...), true)
	}
	f.Add([]byte(`{"__CURSOR":"`+testCursor+`","__REALTIME_TIMESTAMP":"1","MESSAGE":"a\u0000b"}`), false)
	r := NewRedactor([]string{"DB_PASSWORD=hunter2-of-demo"}, nil)
	f.Fuzz(func(t *testing.T, out []byte, follow bool) {
		after := ""
		if follow {
			after = testCursor
		}
		read, err := readLogs(context.Background(), rawLogs(bytes.Split(out, []byte("\n"))), logsQuery{app: "demo", ident: "motel-demo", uid: logsUID, n: 50, after: after})
		if err != nil {
			return
		}
		if read.cursor != "" && !journalCursor.re.MatchString(read.cursor) {
			t.Fatalf("a cursor out of the grammar: %q", read.cursor)
		}
		body := wire.Logs{Kept: wire.KeptOn, Lines: []wire.LogLine{}, More: read.more, Gap: read.gap, Cursor: read.cursor}
		for _, l := range read.lines {
			if !utf8.ValidString(l.text) {
				t.Fatalf("not UTF-8: %q", l.text)
			}
			rel, _ := unitRelease(l.userUnit, "demo")
			body.Lines = append(body.Lines, wire.LogLine{At: l.at, Release: rel, Text: l.text})
		}
		s, _, ok := filterLogs(r, body)
		if !ok {
			s, _, _ = filterLogsWhole(r, body)
		}
		if strings.Contains(s, "hunter2-of-demo") {
			t.Fatalf("the value is sent: %s", s)
		}
		var got wire.Logs
		dec := json.NewDecoder(strings.NewReader(s))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&got); err != nil {
			if strings.HasPrefix(s, `{"error":`) {
				return
			}
			t.Fatalf("%v: %s", err, s)
		}
		if got.Cursor != read.cursor {
			t.Fatalf("the cursor %q came back %q", read.cursor, got.Cursor)
		}
		for _, l := range got.Lines {
			if !utf8.ValidString(l.Text) || len(l.Text) > logsLineMax {
				t.Fatalf("a text of %d bytes, UTF-8 %v", len(l.Text), utf8.ValidString(l.Text))
			}
		}
	})
}
