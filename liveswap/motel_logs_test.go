package liveswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
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
		if len(l.Text) > logsLineMax || !strings.HasSuffix(string(l.Text), "…") || !utf8.ValidString(string(l.Text)) {
			t.Errorf("line %d: %d bytes, ends %q", i+1, len(l.Text), l.Text[max(0, len(l.Text)-20):])
		}
	}
	if !strings.HasSuffix(string(got.Lines[1].Text), " sess=[re…") {
		t.Errorf("the value across the cut, replaced whole, then the marker cut: …%q", got.Lines[1].Text[len(got.Lines[1].Text)-20:])
	}
	if strings.Join(got.RedactedEnv, ",") != "SESSION_SECRET,SMTP_PASSWORD" {
		t.Errorf("redacted_env: %q", got.RedactedEnv)
	}
	// The order this replaces: the cut first, then the filter, sends a
	// piece of the value no filter knows.
	f := h.app.managed["demo"].door().r
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
	// Each line's text passes the filter on its own, and nothing passes
	// over the body: a value of the punctuation between two lines meets
	// nothing to replace, and both lines are sent, each filtered.
	j = demoJournal(2, func(i int) string { return []string{"made-up-value-0001 end of one", "start of two"}[i] })
	h, people, _, logs := logsRig(t, j)
	value := `one"},{"at":"2026-11-20T09:00:00.001Z","release":"v1","text":"start` // the text, a time and the punctuation between two lines
	must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("K="+value+"\nS=made-up-value-0001\n"), 0o600))
	w = sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	got = decodeLogs(t, w)
	if len(got.Lines) != 2 || got.Lines[0].Text != "[redacted:S] end of one" || got.Lines[1].Text != "start of two" || got.Cursor != cursorOf(1) || strings.Join(got.RedactedEnv, ",") != "S" {
		t.Errorf("a value across two lines: %d %s", w.Code, w.Body)
	}
	if n := logs.FilterMessage("access: person read logs").Len(); n != 1 {
		t.Errorf("a read wrote %d read's lines", n)
	}
}

// TestLogsUnreadableEnvFile: with the app's env_file unreadable the
// filter knows no value, so no line's text is sent: each is the
// filter's sentence, which names why the file did not read (an empty
// line empty), and the rest of the answer
// (each line's time and release, the cursor) is moteld's own and sent;
// the journal gets no read's line, since no line's text was sent. A
// command reading it exits 0.
func TestLogsUnreadableEnvFile(t *testing.T) {
	j := demoJournal(3, func(i int) string { return []string{"made-up-value-0001 printed", "", "plain"}[i] })
	h, people, _, logs := logsRig(t, j)
	ma := h.app.managed["demo"]
	must(t, os.Remove(ma.spec.envFile))
	must(t, os.Mkdir(ma.spec.envFile, 0o700)) // there, and not a file that reads
	ma.secretsLoaded = false
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	got := decodeLogs(t, w)
	if w.Code != http.StatusOK || len(got.Lines) != 3 || got.Cursor != cursorOf(2) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	withheld := wire.AppText("[withheld: the app's env_file could not be read, so its values are unknown to the filter (read " + ma.spec.envFile + ": is a directory)]")
	for i, want := range []wire.AppText{withheld, "", withheld} {
		if got.Lines[i].Text != want || !got.Lines[i].At.Equal(j[i].at) || got.Lines[i].Release != "v1" {
			t.Errorf("line %d: %+v", i, got.Lines[i])
		}
	}
	if strings.Contains(w.Body.String(), "made-up-value-0001") || strings.Contains(w.Body.String(), "plain") {
		t.Errorf("a line's text is sent: %s", w.Body)
	}
	if n := logs.FilterMessage("access: person read logs").Len(); n != 0 {
		t.Errorf("a read that sent no line's text wrote %d read's lines", n)
	}
}

// TestLogsLongLineWithNoSpace: a line over 4 KiB with no whitespace
// (minified JSON) keeps its first 4 KiB, less the last longest-form
// bytes of what the filter saw when the first cut fell inside the line,
// where a value that cut split could be; and no piece of a value either
// cut fell in is sent. A known value longer than a line (its hex over
// 4 KiB) empties no line shorter than the first cut.
func TestLogsLongLineWithNoSpace(t *testing.T) {
	value := "made-up-value-across-the-cut-0001" // 33 bytes; its hex form is the longest, 66
	f := NewRedactor([]string{"V=" + value}, nil)
	for _, at := range []int{logsLineMax - 40, logsLineMax - 3, logsLineMax + 20, logsLineMax + 60} {
		text := strings.Repeat("{", at) + value + strings.Repeat("}", 3000)
		seen := map[string]bool{}
		out := filterLogLine(f, text, seen)
		if len(out) != logsLineMax || !strings.HasSuffix(out, "…") || !utf8.ValidString(out) {
			t.Errorf("value at %d: %d bytes, ends %q", at, len(out), out[max(0, len(out)-20):])
		}
		for n := 8; n <= len(value); n++ {
			if strings.Contains(out, value[:n]) {
				t.Errorf("value at %d: a piece of %d bytes is sent", at, n)
				break
			}
		}
	}
	// A credential only the shape layer knows, split by the first cut, by
	// an app with no env_file: no piece of it is sent.
	token := "ghp_" + strings.Repeat("aB3dE5", 6) // gitleaks:allow — made up for this test
	for _, at := range []int{logsLineMax - 20, logsLineMax + logsCutMargin - 10} {
		out := filterLogLine(NewRedactor(nil, nil), strings.Repeat("x", at)+" "+token+" after", map[string]bool{})
		if strings.Contains(out, "ghp_") || strings.Contains(out, "aB3dE5") || len(out) > logsLineMax {
			t.Errorf("a token at %d: …%q", at, out[max(0, len(out)-40):])
		}
	}
	// A generated token longer than the margin, split by the first cut:
	// no piece of its start is sent either.
	long := strings.Repeat("Zq9", 3000) // gitleaks:allow — made up for this test
	if out := filterLogLine(NewRedactor(nil, nil), strings.Repeat("x", 4000)+" "+long+" after", map[string]bool{}); strings.Contains(out, "Zq9") {
		t.Errorf("a token longer than the margin: …%q", out[max(0, len(out)-40):])
	}
	// A private key on one line (an app's config logged as JSON), its
	// block's end within the margin past the cut: replaced whole.
	key := "-----BEGIN " + "PRIVATE KEY-----\\n" + strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASC\\n", 70) + "-----END " + "PRIVATE KEY-----\\n" // gitleaks:allow — made up for this test
	for _, total := range []int{6000, 20000} {
		line := strings.Repeat("c", 3000) + `"private_key":"` + key + `"` + strings.Repeat("c", total)
		if out := filterLogLine(NewRedactor(nil, nil), line, map[string]bool{}); strings.Contains(out, "MIIE") || total == 6000 && !strings.Contains(out, "[redacted:private-key]") {
			t.Errorf("a key on one line of %d: …%q", total, out[max(0, len(out)-60):])
		}
	}
	// Bytes that are not UTF-8: the filter sees them as written, so a
	// value holding them is replaced; what is sent is valid UTF-8, within
	// 4 KiB however many runs there are.
	bad := NewRedactor([]string{"K=made\xffup-value-01"}, nil)
	if out := filterLogLine(bad, "key made\xffup-value-01 here \xfe", map[string]bool{}); out != "key [redacted:K] here \uFFFD" {
		t.Errorf("a value with a byte that is not UTF-8: %q", out)
	}
	if out := filterLogLine(nil, strings.Repeat("\xffa", 3000), map[string]bool{}); !utf8.ValidString(out) || len(out) > logsLineMax {
		t.Errorf("runs of bad bytes: %d bytes, valid %v", len(out), utf8.ValidString(out))
	}
	// Whitespace changes nothing: the cut is by bytes.
	out := filterLogLine(f, strings.Repeat("x", logsLineMax-10)+" "+strings.Repeat("y", 100), map[string]bool{})
	if out != strings.Repeat("x", logsLineMax-10)+" yyyyyy…" {
		t.Errorf("a space near the cut: …%q", out[len(out)-20:])
	}
	// A value whose hex form is over 4 KiB: a 6 KB line with no space and
	// no value keeps its first 4 KiB.
	big := NewRedactor([]string{"G=" + strings.Repeat("{\"k\":\"made-up\"}", 160)}, nil)
	if out := filterLogLine(big, strings.Repeat("u", 6000), map[string]bool{}); out != strings.Repeat("u", logsLineMax-3)+"…" {
		t.Errorf("a long value's filter: %d bytes, %.20q", len(out), out)
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

// TestLogsFinalPassLengthensNoText: a value that is a word of the
// outcome vocabulary stands in a text's own "status" pair through the
// line's filter (rule 3) and is replaced by the body's final pass, which
// lengthens the text; a text over 4 KiB is not sent, but cut.
func TestLogsFinalPassLengthensNoText(t *testing.T) {
	text := strings.Repeat("{", logsLineMax-len(`"status":"succeeded"`)) + `"status":"succeeded"`
	h, people, _, _ := logsRig(t, demoJournal(2, func(i int) string { return []string{text, "short"}[i] }))
	must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("K=succeeded\n"), 0o600))
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	got := decodeLogs(t, w)
	if len(got.Lines) != 2 || strings.Contains(w.Body.String(), "succeeded") || got.Cursor != cursorOf(1) {
		t.Fatalf("%d %.300s", w.Code, w.Body)
	}
	for i, l := range got.Lines {
		if len(l.Text) > logsLineMax || !utf8.ValidString(string(l.Text)) {
			t.Errorf("line %d: %d bytes", i, len(l.Text))
		}
	}
}

// TestLogsCursorPassesNoFilterInTheCheck: the check that one more pass
// changes nothing is of the body without its cursor, which passes no
// filter: an env_file value inside a cursor's id withholds nothing.
func TestLogsCursorPassesNoFilterInTheCheck(t *testing.T) {
	h, people, _, _ := logsRig(t, demoJournal(2, lineN))
	must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("K=0123456789abcdef\n"), 0o600))
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	if got := decodeLogs(t, w); len(got.Lines) != 2 || got.Cursor != cursorOf(1) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// pieces is whether out holds any 8-byte piece of v.
func pieces(out, v string) bool {
	for i := 0; i+8 <= len(v); i++ {
		if strings.Contains(out, v[i:i+8]) {
			return true
		}
	}
	return false
}

// words is prose of n bytes or more: words of letters between spaces,
// none of which a layer replaces.
func words(n int) string {
	return strings.Repeat("the app said ", n/13+1)
}

// TestLogsCutShapes: a line the first cut falls in keeps its readable
// start, and sends no piece of a value or a token either cut split: a
// value across logsLineMax, one across the first cut, one in the margin
// between them, a token longer than the margin, a long token the filter
// masks (its marker far shorter than it), and markers longer than their
// values before the cut.
func TestLogsCutShapes(t *testing.T) {
	value := "planted.va/lue:0123 4567.89ab/cdef" // gitleaks:allow — made up for this test
	f := NewRedactor([]string{"V=" + value}, nil)
	pre := logsLineMax + logsCutMargin
	base64ish := func(n int) string { // a token layer 4 masks
		var b strings.Builder
		for b.Len() < n {
			b.WriteString("Qm9vdHN0cmFwX3Rva2VuOjE3Mz0aR8")
		}
		return b.String()[:n]
	}
	for _, c := range []struct {
		name, text, start, planted string
	}{
		{"a value across logsLineMax", words(logsLineMax - 10)[:logsLineMax-10] + value + words(pre), "the app said", value},
		{"a value across the first cut", words(pre - 10)[:pre-10] + value + words(100), "the app said", value},
		{"a value in the margin", words(logsLineMax + 100)[:logsLineMax+100] + " " + value + " " + words(pre), "the app said", value},
		{"a token longer than the margin", words(4000)[:4000] + " " + strings.Repeat("Zq9", 3000) + " end", "the app said", strings.Repeat("Zq9", 3000)},
		{"a long token the filter masks", "ERROR decode failed for upload 17: " + base64ish(16000), "ERROR decode failed for upload 17: ", base64ish(16000)},
		// The filter shrinks the start (a 9,000-byte token masked), so the
		// piece of the value the first cut split comes to stand inside the
		// first logsLineMax bytes of what it returns.
		{"a value across the first cut, after a masked token", "upload " + base64ish(9000) + " " + words(pre)[:pre-9008-12] + value + words(100), "upload [masked, ", value},
		{"markers longer than their values", strings.Repeat(value+", ", 400) + value + words(pre), "[redacted:V], [redacted:V]", value},
	} {
		out := filterLogLine(f, c.text, map[string]bool{})
		if !strings.HasPrefix(out, c.start) || !strings.HasSuffix(out, "…") || len(out) > logsLineMax || !utf8.ValidString(out) {
			t.Errorf("%s: %d bytes, %.60q … %q", c.name, len(out), out, out[max(0, len(out)-30):])
		}
		if pieces(out, c.planted) {
			t.Errorf("%s: a piece of what was planted is sent: …%q", c.name, out[max(0, len(out)-60):])
		}
		// What is kept is at most the first logsLineMax bytes of the line
		// as filtered: shorter where the filter shrank them.
		if want := map[string]int{"a long token the filter masks": len("ERROR decode failed for upload 17: [masked, "), "markers longer than their values": 4096 / len(value+", ") * len("[redacted:V], "), "a value across the first cut, after a masked token": len("upload [masked, ")}[c.name]; want == 0 && len(out) < logsLineMax-200 || len(out) < want {
			t.Errorf("%s: only %d bytes of the start kept", c.name, len(out))
		}
	}
}

// TestLogsCutRandom: lines of prose with a planted value, a long token
// and a short one at random places, of random lengths past the first
// cut: no 8-byte piece of the value or the token is ever sent, and the
// prose before the first of them is.
func TestLogsCutRandom(t *testing.T) {
	value := "planted.va/lue:0123 4567.89ab/cdef" // gitleaks:allow — made up for this test
	f := NewRedactor([]string{"V=" + value}, nil)
	rnd := rand.New(rand.NewPCG(7, 11))
	tok := func(n int) string {
		const a = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		b := make([]byte, n)
		for i := range b {
			b[i] = a[rnd.IntN(len(a))]
		}
		return string(b)
	}
	for range 300 {
		total := logsLineMax + logsCutMargin + rnd.IntN(12000)
		long := tok(9000 + rnd.IntN(4000))
		// Near a cut most of the time: across logsLineMax or the first
		// cut, or anywhere; the token early half the time, so the filter
		// shrinks the start.
		near := func() int {
			switch rnd.IntN(3) {
			case 0:
				return logsLineMax - rnd.IntN(40)
			case 1:
				return logsLineMax + logsCutMargin - rnd.IntN(40)
			}
			return rnd.IntN(total)
		}
		at, at2 := near(), rnd.IntN(4000)
		if rnd.IntN(2) == 0 {
			at2 = near()
		}
		var b strings.Builder
		first := total
		for b.Len() < total {
			switch {
			case b.Len() >= at && at >= 0:
				first = min(first, b.Len())
				b.WriteString(value)
				at = -1
			case b.Len() >= at2 && at2 >= 0:
				first = min(first, b.Len())
				b.WriteString(long)
				at2 = -1
			default:
				b.WriteString("word" + strconv.Itoa(rnd.IntN(10)) + " ")
			}
		}
		text := b.String()
		out := filterLogLine(f, text, map[string]bool{})
		if pieces(out, value) || pieces(out, long) || len(out) > logsLineMax || !utf8.ValidString(out) {
			t.Fatalf("value at %d, token at %d, %d bytes: sent %d bytes, …%q", strings.Index(text, value), strings.Index(text, long), len(text), len(out), out[max(0, len(out)-60):])
		}
		// The prose before the first planted thing, up to where the cut
		// may take a word, is sent.
		if want := min(first, logsLineMax-3) - 20; want > 0 && !strings.HasPrefix(out, text[:want]) {
			t.Fatalf("the start was not kept: planted at %d, sent %d bytes", first, len(out))
		}
	}
}

// TestCutTextBound: a cut walks back at most a rune's length for a
// rune's start, so a run of bytes that are no UTF-8 (a binary dump) is
// cut at the bound, not back through the run.
func TestCutTextBound(t *testing.T) {
	if got := cutText(strings.Repeat("\x80", 6000), 4093); len(got) != 4093 {
		t.Errorf("a run of continuation bytes: %d bytes", len(got))
	}
	if got := cutText("😀abc", 3); got != "" {
		t.Errorf("a four-byte rune at the start: %q", got)
	}
	if got := cutText("ab"+strings.Repeat("é", 3000), 4093); len(got) != 4092 || !utf8.ValidString(got) {
		t.Errorf("a two-byte rune at the cut: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	out := filterLogLine(nil, strings.Repeat("ab\x80\x81\x82", 2000), map[string]bool{})
	if len(out) < logsLineMax-10 || !utf8.ValidString(out) {
		t.Errorf("a binary dump: %d bytes", len(out))
	}
}

// TestLogsGuessIsConfirmed: a logs reader who can send the app input
// can test a guess of a whole env_file value, the oracle of
// DESIGN-access.md row 26 by another way: a request line holding the
// value comes back with the marker that names its key, one holding a
// wrong guess as written, and the read's journal line names the key in
// redacted_env. It holds what the filter does and the docs name (the
// owner's decision is open); the filter cannot close it, since any
// visible replacement confirms a match.
func TestLogsGuessIsConfirmed(t *testing.T) {
	h, people, _, logs := logsRig(t, demoJournal(2, func(i int) string {
		return []string{`GET /hunter2-of-demo HTTP/1.1" 404`, `GET /hunter2-of-dumb HTTP/1.1" 404`}[i]
	}))
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	got := decodeLogs(t, w)
	if len(got.Lines) != 2 || got.Lines[0].Text != `GET /[redacted:DB_PASSWORD] HTTP/1.1" 404` || got.Lines[1].Text != `GET /hunter2-of-dumb HTTP/1.1" 404` ||
		strings.Join(got.RedactedEnv, ",") != "DB_PASSWORD" {
		t.Fatalf("%s", w.Body)
	}
	if lines := logs.FilterMessage("access: person read logs").All(); len(lines) != 1 ||
		fmt.Sprint(lines[0].ContextMap()["redacted_env"]) != "[DB_PASSWORD]" || lines[0].ContextMap()["person"] != "erin" {
		t.Errorf("the record: %v", lines)
	}
}

// TestLogsClientGone: a reader that goes away mid-read ends it, and that
// is no failure of the journal's for moteld to log.
func TestLogsClientGone(t *testing.T) {
	h, people, src, logs := logsRig(t, demoJournal(3, lineN))
	src.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/_motel/apps/demo/logs", nil).WithContext(ctx)
	req.Header.Set("Authorization", token(people.erin))
	if err := h.ServeHTTP(httptest.NewRecorder(), req, nil); err != nil {
		t.Fatal(err)
	}
	if n := logs.FilterMessage("logs: an app's journal could not be read").Len(); n != 0 {
		t.Errorf("a reader gone is logged as a journal not read: %d", n)
	}
}

// TestLogsRefusedLineByApp: the refusal's line is once a minute for each
// app of this box, and once a minute for every name that is no app here
// together: two apps give two lines, twenty made-up names one, and an
// app after them still its own. Each line names the name its request
// gave.
func TestLogsRefusedLineByApp(t *testing.T) {
	h, people, _, logs := logsRig(t, demoJournal(1, lineN))
	saved := logsNow
	t.Cleanup(func() { logsNow = saved })
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	logsNow = func() time.Time { return now }
	refusedLogs.Lock()
	clear(refusedLogs.last)
	refusedLogs.Unlock()
	apps := func() []string {
		var got []string
		for _, e := range logs.FilterMessage("access: person refused logs").All() {
			got = append(got, fmt.Sprint(e.ContextMap()["app"]))
		}
		return got
	}
	sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.alice))
	sendTo(t, h, "GET", "/_motel/apps/blog/logs", token(people.alice))
	for i := range 20 {
		sendTo(t, h, "GET", "/_motel/apps/ghost"+strconv.Itoa(i)+"/logs", token(people.alice))
	}
	sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.alice))
	if got := apps(); strings.Join(got, ",") != "demo,blog,ghost0" {
		t.Fatalf("within a minute: %q", got)
	}
	now = now.Add(time.Minute)
	sendTo(t, h, "GET", "/_motel/apps/ghost7/logs", token(people.alice))
	sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.alice))
	if got := apps(); strings.Join(got, ",") != "demo,blog,ghost0,ghost7,demo" {
		t.Errorf("a minute on: %q", got)
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

// filterLogLine is one line's text through a door over f (none: a
// filter that knows no value), as the logs read passes it (door.logLine).
func filterLogLine(f *Redactor, text string, seen map[string]bool) string {
	if f == nil {
		f = NewRedactor(nil, nil)
	}
	return string((&door{r: f, seen: seen}).logLine(text))
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
			rel, _ := unitRelease(l.userUnit, "demo")
			body.Lines = append(body.Lines, wire.LogLine{At: l.at, Release: rel})
		}
		d := &door{r: r, seen: map[string]bool{}}
		for i, l := range read.lines {
			body.Lines[i].Text = d.logLine(l.text)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
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
			if !utf8.ValidString(string(l.Text)) || len(l.Text) > logsLineMax {
				t.Fatalf("a text of %d bytes, UTF-8 %v", len(l.Text), utf8.ValidString(string(l.Text)))
			}
		}
	})
}

// stalledWriter is a client that does not read its answer: every write
// waits until the test lets it go.
type stalledWriter struct {
	*httptest.ResponseRecorder
	writing chan struct{}
	release chan struct{}
}

func (s stalledWriter) Write(b []byte) (int, error) {
	s.writing <- struct{}{}
	<-s.release
	return s.ResponseRecorder.Write(b)
}

// TestLogsStalledReaderHoldsNoSlot: a slot bounds the journal's children
// and the filter, not a client's pace: four readers that stop reading
// their answers leave every slot free for the next.
func TestLogsStalledReaderHoldsNoSlot(t *testing.T) {
	h, people, _, _ := logsRig(t, demoJournal(3, lineN))
	release := make(chan struct{})
	writing := make(chan struct{}, logsReadsAtOnce)
	var wg sync.WaitGroup
	for range logsReadsAtOnce {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/_motel/apps/demo/logs", nil)
			req.Header.Set("Authorization", token(people.erin))
			_ = h.ServeHTTP(stalledWriter{httptest.NewRecorder(), writing, release}, req, nil)
		}()
	}
	for range logsReadsAtOnce {
		<-writing
	}
	if n := len(logsSlots); n != 0 {
		t.Errorf("four readers stalled in their writes hold %d slots", n)
	}
	if w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin)); w.Code != http.StatusOK {
		t.Errorf("the next read: %d %s", w.Code, w.Body)
	}
	close(release)
	wg.Wait()
}
