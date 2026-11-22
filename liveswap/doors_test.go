package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// strs is xs as plain strings: a test's view of typed text.
func strs[T ~string](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
}

// doorBox is the handler's one app, demo, with the test people (alice
// views demo, carol views the box, erin reads its logs), an env_file, a
// 40-line tail, and v1 deployed over HTTP.
type doorBox struct {
	h      *Handler
	rig    *testRig
	people *testPeople
}

func newDoorBox(t *testing.T, env string) *doorBox {
	t.Helper()
	h, rig := newTestHandler(t)
	b := &doorBox{h: h, rig: rig, people: newTestPeople(t)}
	h.people = b.people.reg
	rig.spec.deployLogLines = 40
	rig.spec.envFile = filepath.Join(t.TempDir(), "demo.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte(env), 0o600))
	if code, body := b.post(t, "v1", false); code != http.StatusOK {
		t.Fatalf("deploying v1: %d %s", code, body)
	}
	return b
}

// post is POST /demo with the app's deploy token, its phases streamed
// as lines when stream: the code and the bytes.
func (b *doorBox) post(t *testing.T, version string, stream bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/demo", strings.NewReader(`{"url":"https://x/`+version+`.tgz","version":"`+version+`"}`))
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	if stream {
		req.Header.Set("Accept", ndjson)
	}
	return b.serve(t, req)
}

func (b *doorBox) serve(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	if err := b.h.ServeHTTP(w, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return w.Code, w.Body.String()
}

// sinks is every answer a reader can get of demo now, and every record
// on disk, by where it was read.
func (b *doorBox) sinks(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{
		"GET /demo":                  do(t, b.h, http.MethodGet, "/demo", appToken(t), "").Body.String(),
		"GET /demo, a viewer":        sendTo(t, b.h, http.MethodGet, "/demo", token(b.people.alice)).Body.String(),
		"GET /_motel/apps":           sendTo(t, b.h, http.MethodGet, "/_motel/apps", token(b.people.carol)).Body.String(),
		"GET /_motel/apps/demo/logs": sendTo(t, b.h, http.MethodGet, "/_motel/apps/demo/logs", token(b.people.erin)).Body.String(),
	}
	entries, err := os.ReadDir(b.rig.spec.dirs.deploys)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		version := strings.TrimSuffix(e.Name(), ".json")
		raw, err := os.ReadFile(filepath.Join(b.rig.spec.dirs.deploys, e.Name()))
		must(t, err)
		out["record of "+version] = string(raw)
		out["GET /demo?deploy="+version] = do(t, b.h, http.MethodGet, "/demo?deploy="+version, appToken(t), "").Body.String()
	}
	return out
}

// readable is whether a reader of body can read value: one of the forms
// the filter knows, in its bytes, in a string the JSON decodes to, or in
// such a string with Go's quoting of a run undone (an error that quotes
// the app's bytes).
func readable(body, value string) bool {
	texts := []string{body}
	for _, line := range strings.Split(body, "\n") {
		var v any
		if json.Unmarshal([]byte(line), &v) != nil {
			continue
		}
		for _, s := range jsonStrings(v, nil) {
			texts = append(texts, s)
			for _, q := range goQuotedRe.FindAllString(s, -1) {
				if u, err := strconv.Unquote(q); err == nil {
					texts = append(texts, u)
				}
			}
		}
	}
	for _, text := range texts {
		for _, f := range secretForms(value) {
			if len(f) >= secretMinLen && strings.Contains(text, f) {
				return true
			}
		}
	}
	return false
}

// jsonStrings is every string, keys included, in the decoded JSON v.
func jsonStrings(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = jsonStrings(e, out)
		}
	case map[string]any:
		for k, e := range x {
			out = jsonStrings(e, append(out, k))
		}
	}
	return out
}

// noneReadable fails the test for each sink a value is readable in.
func noneReadable(t *testing.T, sinks map[string]string, values ...string) {
	t.Helper()
	names := make([]string, 0, len(sinks))
	for n := range sinks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range values {
			if readable(sinks[n], v) {
				t.Errorf("%s holds %q: %.600s", n, v, sinks[n])
			}
		}
	}
}

// The canary: an app prints two values of its env_file (one plain, one
// with a quote and a backslash, which JSON and Go escape) at every place
// the app's bytes reach an answer or a record: its journal while a
// deploy fails, its health probe's body and Location, an answer that is
// no HTTP, which Go quotes into the probe's error, the watchdog's failed
// probe, journalctl's own failure, and its journal at the logs read.
// Neither is readable in any answer of any reader, streamed or not, nor
// in any record on disk; each place's keys are named.
func TestCanaryAtEveryDoor(t *testing.T) {
	const plain, quoted = "canary-printed-by-the-app-7Hq2", `canary"quoted\by-the-app-9Lm4`
	b := newDoorBox(t, "PLAIN="+plain+"\nQUOTED="+quoted+"\n")
	esc, _ := json.Marshal(quoted)
	tail := []string{"boot " + plain, `{"msg":` + string(esc) + `}`, "then " + quoted}
	// A failed health gate: the journal and the probe's answer.
	b.rig.ma.journal = &fakeJournal{lines: tail}
	b.rig.prober.err = fmt.Errorf("not healthy within deadline 5m0s: %w", &probeError{status: 503, location: "https://example.test/" + plain + "?q=" + quoted, body: `{"err":` + string(esc) + `} ` + plain})
	answers := map[string]string{}
	_, answers["a failed deploy's answer"] = b.post(t, "v2", false)
	_, answers["a failed deploy, streamed"] = b.post(t, "v3", true)
	// An answer that is no HTTP: Go's client quotes the app's bytes.
	b.rig.prober.err = fmt.Errorf("not healthy within deadline 5m0s: %w", fmt.Errorf("net/http: malformed HTTP response %q", plain+" "+string(esc)))
	_, answers["an answer that is no HTTP"] = b.post(t, "v4", false)
	// journalctl's own failure, in log_tail_error.
	b.rig.ma.journal = &fakeJournal{err: errors.New("journalctl: " + plain + " " + quoted)}
	b.rig.prober.err = fmt.Errorf("not healthy within deadline 5m0s: %w", &probeError{status: 500})
	_, answers["a failed deploy with no tail"] = b.post(t, "v5", false)
	// The watchdog's failed probe of the running v1.
	b.rig.ma.wd.mu.Lock()
	b.rig.ma.wd.state = wdStateWatching
	b.rig.ma.wd.mu.Unlock()
	_, _ = b.rig.ma.probeFailed("v1", fmt.Errorf("net/http: malformed HTTP status code %q", quoted+" "+plain))
	// The logs read.
	b.rig.ma.logs = &scriptedLogs{journal: demoJournal(3, func(i int) string { return tail[i] })}
	sinks := b.sinks(t)
	for k, v := range answers {
		sinks[k] = v
	}
	noneReadable(t, sinks, plain, quoted)
	// Each place still says something, and names the keys it replaced.
	var st wire.Status
	must(t, json.Unmarshal([]byte(sinks["GET /demo"]), &st))
	if st.Watchdog == nil || !strings.Contains(string(st.Watchdog.LastFailure), "[redacted:") || strings.Join(st.RedactedEnv, ",") != "PLAIN,QUOTED" {
		t.Errorf("GET /demo: %s", sinks["GET /demo"])
	}
	var logs wire.Logs
	must(t, json.Unmarshal([]byte(sinks["GET /_motel/apps/demo/logs"]), &logs))
	if len(logs.Lines) != 3 || strings.Join(logs.RedactedEnv, ",") != "PLAIN,QUOTED" {
		t.Errorf("the logs read: %s", sinks["GET /_motel/apps/demo/logs"])
	}
	failed := 0
	for k, body := range sinks {
		var rec wire.DeployResult
		if !strings.HasPrefix(k, "record of ") || json.Unmarshal([]byte(body), &rec) != nil || rec.Status != "failed" {
			continue
		}
		failed++
		if !rec.FilteredAtEntry || len(rec.RedactedEnv) == 0 {
			t.Errorf("%s: %s", k, body)
		}
	}
	if failed == 0 {
		t.Error("no failed deploy's record was read")
	}
}

// failRemoval makes the release removals fail as they fail for an
// account that is not root when the app has made a read-only directory
// in its release whose name holds value: the system's error names the
// path (removeAll).
func failRemoval(t *testing.T, value string) {
	t.Helper()
	saved := removeAll
	t.Cleanup(func() { removeAll = saved })
	removeAll = func(p string) error {
		return &os.PathError{Op: "unlinkat", Path: filepath.Join(p, "cache-"+value, "keep"), Err: syscall.EACCES}
	}
}

// L1 of the spike's attack: a deploy fails its health gate, and the
// cleanup of its release cannot remove a directory the app made, whose
// name holds an env value; the system's words name the path, and the
// error net hides it in the deploy's answer, the status, the record and
// the record read back.
func TestErrorNetCleanupOfAFailedRelease(t *testing.T) {
	const value = "canary-yellow-bird-sings-0642"
	b := newDoorBox(t, "TENANT="+value+"\n")
	failRemoval(t, value)
	b.rig.prober.err = fmt.Errorf("not healthy within deadline 5m0s: %w", &probeError{status: 503, body: "starting"})
	code, body := b.post(t, "v2", false)
	if code != http.StatusInternalServerError || !strings.Contains(body, "cleanup of failed release v2: unlinkat ") || !strings.Contains(body, "/cache-[redacted:TENANT]/keep: permission denied") {
		t.Fatalf("the premise, a cleanup that names the app's path: %d %s", code, body)
	}
	sinks := b.sinks(t)
	sinks["the deploy's answer"] = body
	noneReadable(t, sinks, value)
}

// L2: moteld stopped during a deploy whose units ran; at the next deploy
// its marker is settled, the release cannot be removed for the same
// directory, and the record motel writes for the interrupted deploy
// names the path: the net hides it there and in every answer.
func TestErrorNetInterruptedDeployRecord(t *testing.T) {
	const value = "canary-yellow-bird-sings-0642"
	b := newDoorBox(t, "TENANT="+value+"\n")
	d := b.rig.spec.dirs
	must(t, os.MkdirAll(d.release("v9"), 0o755))
	must(t, saveDeployMarker(d, deployMarker{Version: "v9", StartedAt: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}))
	failRemoval(t, value)
	_, body := b.post(t, "v10", false)
	sinks := b.sinks(t)
	sinks["the next deploy's answer"] = body
	if rec := sinks["record of v9"]; !strings.Contains(rec, "the deploy was cut short") || !strings.Contains(rec, "/cache-[redacted:TENANT]/keep") || !strings.Contains(rec, `"redacted_env":["TENANT"]`) {
		t.Fatalf("the premise, the interrupted deploy's record names the path: %s", rec)
	}
	noneReadable(t, sinks, value)
}

// linkedCommand has the running v1 replace its own command, in its
// release dir, with a link to a path outside its view whose name holds
// an env value, and returns the value and the real pre-flight's refusal
// of that release (resolveInView), which names the link's target.
func linkedCommand(t *testing.T) (b *doorBox, value string, refusal error) {
	t.Helper()
	outside := filepath.Join(t.TempDir(), "opt", "runtime-canary-yellow-bird-0642", "bin")
	must(t, os.MkdirAll(outside, 0o755))
	value = filepath.Join(outside, "node")
	must(t, os.WriteFile(value, []byte("#!/bin/sh\n"), 0o755))
	b = newDoorBox(t, "RUNTIME_BIN="+value+"\n")
	rel := b.rig.spec.dirs.release("v1")
	_ = os.Remove(filepath.Join(rel, "server"))
	must(t, os.Symlink(value, filepath.Join(rel, "server")))
	pre, _, n := b.rig.runner.lastPreflightAndStart()
	if n == 0 {
		t.Fatal("no pre-flight was recorded")
	}
	pre.dir, pre.version = rel, "v1"
	pre.command = append([]string{"./server"}, pre.command[1:]...)
	pre.sandbox = b.rig.spec.sandboxSpecFor(rel, pre.nonce)
	if _, _, refusal = resolveInView(pre); refusal == nil || !strings.Contains(refusal.Error(), value) {
		t.Fatalf("the premise, the real pre-flight names the link's target: %v", refusal)
	}
	return b, value, refusal
}

// L3: a rollback to a release the app rewrote is refused at its
// pre-flight, whose refusal names the link's target, a path holding an
// env value: the net hides it in the 422 and in last_deploy. The same
// refusal on the watchdog's relaunch, and on a recovery after a restart,
// is hidden in watchdog.last_failure.
func TestErrorNetPreflightRefusal(t *testing.T) {
	t.Run("a rollback", func(t *testing.T) {
		b, value, refusal := linkedCommand(t)
		if code, body := b.post(t, "v2", false); code != http.StatusOK {
			t.Fatalf("deploying v2: %d %s", code, body)
		}
		b.rig.runner.setPreflightErr(refusal)
		req := httptest.NewRequest(http.MethodPost, "/demo?rollback=v1", nil)
		req.Header.Set("Authorization", "Bearer "+appToken(t))
		code, body := b.serve(t, req)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "[redacted:RUNTIME_BIN]") || !strings.Contains(body, `"redacted_env":["RUNTIME_BIN"]`) {
			t.Fatalf("the rollback: %d %s", code, body)
		}
		sinks := b.sinks(t)
		sinks["the rollback's answer"] = body
		noneReadable(t, sinks, value)
	})
	t.Run("the watchdog's relaunch", func(t *testing.T) {
		b, value, refusal := linkedCommand(t)
		rig := b.rig
		rig.startWatchdogT(t)
		waitUntil(t, "watchdog to arm", func() bool {
			s := rig.ma.wd.currentState()
			return s == wdStateGrace || s == wdStateWatching
		})
		rig.runner.setPreflightErr(refusal)
		rig.runner.handleAt(0).kill()
		advanceUntil(t, rig, time.Second, "the relaunch refused", func() bool {
			wd := rig.ma.status().Watchdog
			return wd != nil && wd.LastFailure != ""
		})
		sinks := b.sinks(t)
		if !strings.Contains(sinks["GET /demo"], `"last_failure":`) || !strings.Contains(sinks["GET /demo"], "[redacted:RUNTIME_BIN]") {
			t.Fatalf("the premise, last_failure names the refusal: %s", sinks["GET /demo"])
		}
		noneReadable(t, sinks, value)
	})
	t.Run("a recovery after a restart", func(t *testing.T) {
		b, value, refusal := linkedCommand(t)
		rig := b.rig
		rig.runner.handleAt(0).kill()
		rig.ma.mu.Lock()
		rig.ma.current = nil
		rig.ma.mu.Unlock()
		rig.ma.activeSocket.Store(nil)
		rig.runner.setPreflightErr(refusal)
		rig.startWatchdogT(t)
		done := make(chan struct{})
		go func() { rig.ma.recover(context.Background(), zap.NewNop()); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("recover did not give up on a refused relaunch")
		}
		sinks := b.sinks(t)
		if !strings.Contains(sinks["GET /demo"], "[redacted:RUNTIME_BIN]") {
			t.Fatalf("the premise, last_failure names the refusal: %s", sinks["GET /demo"])
		}
		noneReadable(t, sinks, value)
	})
}

// The error net runs whatever failed and whether or not a unit ran: a
// download refused before any launch, with a current value in the
// deployer's URL, is hidden in the answer and the record. A deploy whose
// own launch rendered the env_file reads no file at its net, so a reload
// to an unreadable file while it ran withholds nothing of its error: the
// values it launched with are known.
func TestErrorNetBeforeALaunchAndAfterAReload(t *testing.T) {
	const value = "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — made up for this test
	b := newDoorBox(t, "SIG="+value+"\n")
	b.rig.fetch.err = fmt.Errorf("downloading https://example.test/v2.tgz?sig=%s: HTTP 403", value)
	code, body := b.post(t, "v2", false)
	if code != http.StatusInternalServerError || !strings.Contains(body, "[redacted:") {
		t.Fatalf("a refused download: %d %s", code, body)
	}
	b.rig.fetch.err = nil
	noneReadable(t, b.sinks(t), value)

	ma := b.rig.ma
	ma.rememberSecrets(b.rig.spec.envFile, []string{"SIG=" + value})
	live := *b.rig.spec
	live.envFile = filepath.Join(t.TempDir(), "missing.env")
	ma.spec = &live
	text, keys := ma.errText("start failed: "+value, false)
	if text != "start failed: [redacted:SIG]" || strings.Join(keys, ",") != "SIG" {
		t.Errorf("the net of a deploy that launched, the env_file since unreadable: %q %v", text, keys)
	}
	if text, _ := ma.errText("start failed: "+value, true); !strings.HasPrefix(string(text), "[withheld:") {
		t.Errorf("the net where the file is read: %q", text)
	}
}

// A safe name exempts nothing from the known values (rule 1 of the
// filter's header): the app's text that holds a version, a pin or a
// release on disk equal to an env value has it replaced, though such a
// name is left alone by the heuristics.
func TestADoorsNamesExemptNoValue(t *testing.T) {
	const value = "release-named-like-a-value-01"
	b := newDoorBox(t, "TOKEN="+value+"\n")
	must(t, os.MkdirAll(b.rig.spec.dirs.release(value), 0o755))
	d := b.rig.ma.door(true, value)
	if got := d.text("serving " + value); got != "serving [redacted:TOKEN]" {
		t.Errorf("the app's text: %q", got)
	}
	sha := strings.Repeat("4c2d9e7b1a05", 3) + "0f9e"
	if got := b.rig.ma.door(true, sha).text("built " + sha); got != wire.AppText("built "+sha) {
		t.Errorf("a version that looks generated, named: %q", got)
	}
}

// The logs door reads no more of a long line than the logs read always
// has: a value past the first 4 KiB and its margin is in no part of the
// filter's input, so it is named in no redacted_env; one inside what is
// read is replaced and named; what is sent is at most 4 KiB, ending in
// "…".
func TestLogsDoorReadsABoundedPieceOfALongLine(t *testing.T) {
	const near, far = "made-up-value-in-the-margin-01", "made-up-value-far-down-the-line-02"
	line := strings.Repeat("a", logsLineMax+100) + " " + near + " " + strings.Repeat("b", 20000) + " " + far + " " + strings.Repeat("c", 20000)
	if len(line) < 40000 {
		t.Fatal("the premise: a line longer than the filter reads")
	}
	seen := map[string]bool{}
	out := filterLogLine(NewRedactor([]string{"NEAR=" + near, "FAR=" + far}, nil), line, seen)
	if len(out) > logsLineMax || !strings.HasSuffix(out, "…") || strings.Contains(out, near) || strings.Contains(out, far) {
		t.Errorf("sent %d bytes, ending %q", len(out), out[max(0, len(out)-20):])
	}
	if !seen["NEAR"] || seen["FAR"] {
		t.Errorf("the keys named %v: want NEAR, read in the margin, and not FAR, past what is read", seen)
	}
}

// A record an older moteld wrote bears no mark that its app text was
// filtered where it entered: it was filtered whole when written, with
// the values known then, and is filtered again when read, its error and
// detail, with the values known now. A record that bears the mark is
// sent as it stands.
func TestOldRecordIsFilteredWhenRead(t *testing.T) {
	const learnt = "value-joined-env-file-later-77"
	b := newDoorBox(t, "TOKEN="+learnt+"\n")
	d := b.rig.spec.dirs
	old := `{"finished_at":"2026-11-01T10:00:05Z","detail":{"exit":"exit status 3","probe":{"status":503,"location":"https://x/` + learnt + `","body":"probe ` + learnt + `"},"log_tail":["one ` + learnt + `","[redacted:OLD] two"],"log_tail_error":"journalctl ` + learnt + `"},` +
		`"error":"start failed: ` + learnt + `","redacted_env":["OLD"],"sha256":"` + strings.Repeat("ab", 32) + `","started_at":"2026-11-01T10:00:00Z","status":"failed","version":"v8"}`
	must(t, writeDeployRecord(d, "v8", []byte(old)))
	w := do(t, b.h, http.MethodGet, "/demo?deploy=v8", appToken(t), "")
	got := w.Body.String()
	if w.Code != http.StatusOK || readable(got, learnt) {
		t.Fatalf("an unmarked record: %d %s", w.Code, got)
	}
	var rec wire.DeployResult
	must(t, json.Unmarshal(w.Body.Bytes(), &rec))
	if rec.Error != "start failed: [redacted:TOKEN]" || rec.Detail == nil || rec.Detail.Exit != "exit status 3" || rec.Detail.Probe.Status != 503 ||
		strings.Join(strs(rec.Detail.LogTail), "|") != "one [redacted:TOKEN]|[redacted:OLD] two" || rec.Detail.LogTailError != "journalctl [redacted:TOKEN]" ||
		strings.Join(rec.RedactedEnv, ",") != "OLD,TOKEN" || rec.SHA256 != strings.Repeat("ab", 32) || rec.Version != "v8" || rec.FilteredAtEntry {
		t.Errorf("an unmarked record, filtered: %s", got)
	}
	// Marked, the same bytes are sent as they stand.
	marked := strings.Replace(old, `"error":`, `"filtered_at_entry":true,"error":`, 1)
	must(t, writeDeployRecord(d, "v8", []byte(marked)))
	if w := do(t, b.h, http.MethodGet, "/demo?deploy=v8", appToken(t), ""); w.Body.String() != marked+"\n" {
		t.Errorf("a marked record: %s", w.Body)
	}
	// An error or a detail not of its type is not sent: it could not be
	// filtered as one.
	must(t, writeDeployRecord(d, "v8", []byte(`{"version":"v8","status":"failed","error":["`+learnt+`"],"detail":"`+learnt+`"}`)))
	if w := do(t, b.h, http.MethodGet, "/demo?deploy=v8", appToken(t), ""); w.Body.String() != `{"status":"failed","version":"v8"}`+"\n" {
		t.Errorf("an unmarked record of other types: %s", w.Body)
	}
}

// wirePath is the wire package's import path.
const wirePath = "github.com/smallhoursorg/motel/liveswap/wire"

// madeIn is every place in the Go files under dirs (none of them tests),
// each named relative to base, where an AppText or an ErrText is made
// from something that is not a constant: a conversion (written with the
// package's name or any name it is imported under, in parentheses or
// not, a generic function instantiated with the type, a type declared
// as or over it), by the file it is in; and a call of OwnText, by the
// function it is in. What a walk of the source cannot see is a decode:
// json.Unmarshal into such a field makes one with nothing written
// (servedRecord's decode of an older record's detail is the one, and
// passes the door again).
func madeIn(t *testing.T, base string, dirs ...string) (conversions, ownText map[string][]string) {
	t.Helper()
	conversions, ownText = map[string][]string{}, map[string][]string{}
	fset := gotoken.NewFileSet()
	base, err := filepath.Abs(base)
	must(t, err)
	for _, root := range dirs {
		root, err := filepath.Abs(root)
		must(t, err)
		err = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(base, path)
			wireNames := map[string]bool{}
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == wirePath {
					if imp.Name != nil {
						wireNames[imp.Name.Name] = true
					} else {
						wireNames["wire"] = true
					}
				}
			}
			// textType is whether e names AppText or ErrText (and which).
			textType := func(e ast.Expr) string {
				for {
					p, ok := e.(*ast.ParenExpr)
					if !ok {
						break
					}
					e = p.X
				}
				switch x := e.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := x.X.(*ast.Ident); ok && wireNames[pkg.Name] && (x.Sel.Name == "AppText" || x.Sel.Name == "ErrText") {
						return x.Sel.Name
					}
				case *ast.Ident:
					if f.Name.Name == "wire" && (x.Name == "AppText" || x.Name == "ErrText") {
						return x.Name
					}
				case *ast.ArrayType: // []wire.AppText(…)
					if s, ok := x.Elt.(*ast.SelectorExpr); ok && wireNames[fmt.Sprint(s.X)] && (s.Sel.Name == "AppText" || s.Sel.Name == "ErrText") {
						return s.Sel.Name
					}
				}
				return ""
			}
			isWire := func(e ast.Expr, name string) bool {
				if s, ok := e.(*ast.SelectorExpr); ok {
					pkg, ok := s.X.(*ast.Ident)
					return ok && wireNames[pkg.Name] && s.Sel.Name == name
				}
				id, ok := e.(*ast.Ident)
				return ok && f.Name.Name == "wire" && id.Name == name
			}
			for _, decl := range f.Decls {
				fn, _ := decl.(*ast.FuncDecl)
				ast.Inspect(decl, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.TypeSpec: // type x = wire.AppText, type x wire.ErrText
						if name := textType(x.Type); name != "" && f.Name.Name != "wire" {
							conversions[name] = append(conversions[name], rel)
						}
					case *ast.IndexExpr: // conv[wire.AppText](s)
						if name := textType(x.Index); name != "" {
							conversions[name] = append(conversions[name], rel)
						}
					case *ast.IndexListExpr:
						for _, ix := range x.Indices {
							if name := textType(ix); name != "" {
								conversions[name] = append(conversions[name], rel)
							}
						}
					case *ast.CallExpr:
						if name := textType(x.Fun); name != "" {
							if len(x.Args) == 1 {
								if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == gotoken.STRING {
									return true // a constant: needs no filter
								}
							}
							conversions[name] = append(conversions[name], rel)
						} else if isWire(x.Fun, "OwnText") && fn != nil {
							where := fn.Name.Name
							if fn.Recv != nil && len(fn.Recv.List) == 1 {
								recv := fn.Recv.List[0].Type
								if s, ok := recv.(*ast.StarExpr); ok {
									recv = s.X
								}
								if id, ok := recv.(*ast.Ident); ok {
									where = id.Name + "." + where
								}
							}
							ownText[rel] = append(ownText[rel], where)
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return conversions, ownText
}

// The modules that fill an answer's text fields: liveswap, the box
// webhook, and notify (wire.Metrics.Error, an app's alerts).
var guardDirs = []string{".", filepath.Join("..", "box"), filepath.Join("..", "notify")}

// The app's text has a type only the filter makes: no conversion to
// wire.AppText is written anywhere on the box but in redact.go, so a
// field of that type cannot be given the app's bytes as they came. A
// door added elsewhere fails here.
func TestAppTextOnlyFromTheDoor(t *testing.T) {
	conv, _ := madeIn(t, "..", guardDirs...)
	if !allAre(conv["AppText"], filepath.Join("liveswap", "redact.go")) || len(conv["AppText"]) == 0 {
		t.Errorf("wire.AppText is converted to in %v; want liveswap/redact.go alone (the door)", conv["AppText"])
	}
}

// An error's text has a type of its own, made from a string that is not
// a constant in two ways only: through the app's filter (errText,
// redact.go), or by wire.OwnText for a sentence that holds nothing an
// app wrote. Every caller of OwnText is listed here: a new one is a
// decision written down here, and an error's text from under an app's
// directories goes through errText instead.
func TestErrTextMakers(t *testing.T) {
	conv, own := madeIn(t, "..", guardDirs...)
	if c := conv["ErrText"]; len(c) == 0 || !allAre(c, filepath.Join("liveswap", "redact.go"), filepath.Join("liveswap", "wire", "wire.go")) {
		t.Errorf("wire.ErrText is converted to in %v; want liveswap/redact.go (the net) and liveswap/wire/wire.go (OwnText) alone", c)
	}
	want := map[string][]string{
		// The request's own name, query, body, version and the box's
		// figures; never an error from under an app's directories.
		filepath.Join("liveswap", "handler.go"):         {"Handler.ServeHTTP", "Handler.servePerson", "ForbiddenBody", "Handler.getStatus", "Handler.getStatus", "Handler.deployURL", "Handler.deployURL", "Handler.deployURL", "Handler.deployPush", "Handler.deployPush", "Handler.deployPush", "Handler.deployRollback", "Handler.deployRecord", "Handler.deployRecord"},
		filepath.Join("liveswap", "motel_approutes.go"): {"Handler.serveMotelApp", "Handler.serveMotelApp"},
		filepath.Join("liveswap", "people.go"):          {"AdmitPerson"},
		// The box's catalogue, its state's paths and the push's id.
		filepath.Join("box", "handler.go"): {"errorBody"},
	}
	for f := range own {
		sort.Strings(own[f])
	}
	for f := range want {
		sort.Strings(want[f])
	}
	if fmt.Sprint(own) != fmt.Sprint(want) {
		t.Errorf("wire.OwnText is called in\n%v\nwant\n%v", own, want)
	}
}

func allAre(got []string, allowed ...string) bool {
	for _, g := range got {
		ok := false
		for _, a := range allowed {
			ok = ok || g == a
		}
		if !ok {
			return false
		}
	}
	return true
}

// The guards see what they are for, in the forms a walk can see: a
// conversion with the package's name, in parentheses, under a renamed
// import, through a declared type or alias and through a generic
// function, and a new caller of OwnText.
func TestGuardsSeeANewMaker(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "door.go"), []byte(`package liveswap
import (
	"github.com/smallhoursorg/motel/liveswap/wire"
	w "github.com/smallhoursorg/motel/liveswap/wire"
)
type mine = wire.AppText
type also w.ErrText
func conv[T ~string](s string) T { return T(s) }
func f(s string, err error) (wire.AppText, wire.ErrText, wire.Error, wire.AppText, w.ErrText, wire.AppText) {
	return wire.AppText(s), wire.ErrText(err.Error()), wire.Error{Error: wire.OwnText(err.Error())}, (wire.AppText)(s), w.ErrText(s), conv[wire.AppText](s)
}
func g() wire.AppText { return wire.AppText("a constant") }
`), 0o600))
	conv, own := madeIn(t, dir, dir)
	if fmt.Sprint(conv) != "map[AppText:[door.go door.go door.go door.go] ErrText:[door.go door.go door.go]]" || fmt.Sprint(own) != "map[door.go:[f]]" {
		t.Errorf("found %v and %v", conv, own)
	}
}

// A probe that got no answer is said in motel's words without motel's
// own URL: Go's client names `Get "http://localhost/<health_path>"`,
// whose host and path are motel's (the path is the status's), and the
// error net would replace an env value equal to either (DB_HOST=localhost
// is common) in a sentence that holds nothing of the app's. Why it got
// no answer stays, and the chain for errors.Is.
func TestProbeWithNoAnswerLeavesMotelsURLOut(t *testing.T) {
	p := &httpProber{clock: newFakeClock()}
	sock := filepath.Join(shortTempDir(t), "nothing.sock")
	err := p.probeOnce(context.Background(), testSocketRef(t, sock), "/health?ready=1", time.Second)
	if err == nil {
		t.Fatal("a probe of a socket nothing listens on passed")
	}
	if s := err.Error(); !strings.HasPrefix(s, "health check got no answer: ") || strings.Contains(s, "localhost") || strings.Contains(s, "/health") {
		t.Errorf("the probe's sentence: %q", s)
	}
	var ue interface{ Timeout() bool }
	if !errors.As(err, &ue) {
		t.Errorf("the chain lost Go's client's error: %#v", err)
	}
	b := newDoorBox(t, "PGHOST=localhost\nHEALTH=/health?ready=1\n")
	if text, keys := b.rig.ma.errText("health gate: not healthy within deadline 5m0s: "+err.Error(), true); strings.Contains(string(text), "[redacted:") || keys != nil {
		t.Errorf("the net replaced motel's own words: %q %v", text, keys)
	}
}

// A failed deploy's tail passes the door line by line: a rule that
// reaches across white space (a bearer token, an assignment) never
// joins two lines or masks the next line's first word, and a line that
// holds a line break of its own stays one line, as the journal kept it.
func TestDoorTailKeepsItsLines(t *testing.T) {
	d := &door{r: NewRedactor(nil, nil), seen: map[string]bool{}}
	in := []string{"refreshing token", "ConnectionPoolManagerStarted ok", "db password:", "connection established", "one\ntwo"}
	got := strs(d.lines(in))
	if strings.Join(got, "|") != strings.Join(in, "|") {
		t.Errorf("the tail through the door: %q", got)
	}
}

// A version the records name is moteld's own name, left alone by the
// heuristics in an error's text as the versions on disk are: a
// generated-looking version that is no longer on disk (a pruned
// release, a recovery that finds its release gone) is not masked.
func TestErrorNetLeavesRecordedVersionsAlone(t *testing.T) {
	b := newDoorBox(t, "TOKEN=some-value-of-the-app-01\n")
	sha := strings.Repeat("9f8e7d6c5b4a", 3) + "3a2b"
	must(t, writeDeployRecord(b.rig.spec.dirs, sha, []byte(`{"version":"`+sha+`","status":"failed"}`)))
	msg := "state names version " + sha + " but its release dir is missing"
	if text, _ := b.rig.ma.errText(msg, true); string(text) != msg {
		t.Errorf("a recorded version in an error: %q", text)
	}
	if text, _ := b.rig.ma.errText(strings.Replace(msg, sha, strings.Repeat("7c6b5a4d3e2f", 3)+"1a0b", 1), true); !strings.Contains(string(text), "[masked, 40 chars]") {
		t.Errorf("the premise, a version no record names is masked: %q", text)
	}
}

// A failed deploy's answer names the keys the net replaced in its own
// error, whatever deploy the status beside it describes by then.
func TestFailureNamesItsOwnErrorsKeys(t *testing.T) {
	b := newDoorBox(t, "TOKEN=some-value-of-the-app-01\n")
	err := &doorError{err: errors.New("x"), msg: "start failed: [redacted:TOKEN]", keys: []string{"TOKEN"}}
	code, body := deployOutcome(b.rig.ma, err)
	f, ok := body.(wire.Failure)
	if code != http.StatusInternalServerError || !ok || strings.Join(f.RedactedEnv, ",") != "TOKEN" || f.Error != err.msg {
		t.Errorf("%d %+v", code, body)
	}
}

// With its env_file unreadable, a failed deploy's answer and record say
// the filter's sentence and that moteld's journal has the error, and
// the journal line names the error as it came and the file's own read
// error beside it.
func TestDeployFailedJournalNamesTheEnvFilesError(t *testing.T) {
	b := newDoorBox(t, "TOKEN=some-value-of-the-app-01\n")
	core, logs := observer.New(zap.ErrorLevel)
	b.rig.ma.logger = zap.New(core)
	must(t, os.WriteFile(b.rig.spec.envFile, []byte("not a line\n"), 0o600))
	b.rig.ma.secretsMu.Lock()
	b.rig.ma.secretsLoaded = false
	b.rig.ma.secretsMu.Unlock()
	if code, body := b.post(t, "v2", false); code != http.StatusInternalServerError || !strings.Contains(body, `"error":"[withheld: the app's env_file could not be read`) {
		t.Fatalf("%d %s", code, body)
	}
	lines := logs.FilterMessage("deploy failed").All()
	if len(lines) != 1 || !strings.Contains(fmt.Sprint(lines[0].ContextMap()["env_file"]), "not KEY=VALUE") ||
		!strings.Contains(fmt.Sprint(lines[0].ContextMap()["cause"]), "not KEY=VALUE") || !strings.Contains(fmt.Sprint(lines[0].ContextMap()["error"]), "moteld's journal has this error") {
		t.Errorf("the journal line: %v", lines)
	}
	// A deploy that fails for another reason meanwhile: its cause is in
	// the journal, the answer says where.
	logs.TakeAll()
	b.rig.fetch.err = errors.New("downloading https://example.test/v3.tgz: HTTP 404")
	if code, body := b.post(t, "v3", false); code != http.StatusInternalServerError || !strings.Contains(body, "moteld's journal has this error") {
		t.Fatalf("%d %s", code, body)
	}
	if lines := logs.FilterMessage("deploy failed").All(); len(lines) != 1 || !strings.Contains(fmt.Sprint(lines[0].ContextMap()["cause"]), "HTTP 404") {
		t.Errorf("the journal line: %v", lines)
	}
}

// A deploy that fails before its launch, with the app's env_file not
// there (a mistyped path), says why: a file that is not there holds no
// values to know, so its error passes the filter with what the process
// remembers, as main's record filter let it. A failed probe of a
// running app with the file gone is withheld: its values are unknown.
func TestMissingEnvFileBeforeALaunch(t *testing.T) {
	b := newDoorBox(t, "TOKEN=some-value-of-the-app-01\n")
	ma := b.rig.ma
	must(t, os.Remove(b.rig.spec.envFile))
	ma.secretsMu.Lock()
	ma.secretsLoaded = false
	ma.secretsMu.Unlock()
	b.rig.fetch.err = errors.New("downloading https://example.test/v2.tgz: HTTP 404 some-value-of-the-app-01")
	code, body := b.post(t, "v2", false)
	if code != http.StatusInternalServerError || !strings.Contains(body, `"error":"downloading https://example.test/v2.tgz: HTTP 404 [redacted:TOKEN]"`) {
		t.Fatalf("%d %s", code, body)
	}
	if rec := b.sinks(t)["record of v2"]; !strings.Contains(rec, "HTTP 404 [redacted:TOKEN]") {
		t.Errorf("the record: %s", rec)
	}
	_, err := ma.probeFailed("v1", errors.New("net/http: malformed HTTP response \"x\""))
	if !strings.HasPrefix(err.Error(), "[withheld:") {
		t.Errorf("a probe's error with the file gone: %v", err)
	}
	// After a restart the process remembers no value, and may be
	// reattached to an instance the file's values launched: the same
	// deploy's error is withheld, the journal its to say.
	ma.secretsMu.Lock()
	ma.secrets, ma.secretsLoaded = nil, false
	ma.secretsMu.Unlock()
	if code, body := b.post(t, "v3", false); code != http.StatusInternalServerError || !strings.Contains(body, `"error":"[withheld: the app's env_file could not be read`) || strings.Contains(body, "some-value-of-the-app-01") {
		t.Errorf("after a restart, the file gone: %d %s", code, body)
	}
}

// A lifecycle event told while the app's env_file cannot be read says
// the filter's sentence for its summary (a refusal's reason can name a
// path the app wrote, and the alerts routes send what Alertmanager
// holds); with the file readable the summary is filtered as it always
// was.
func TestToldSummaryWhileTheEnvFileIsUnread(t *testing.T) {
	b := newDoorBox(t, "TOKEN=some-value-of-the-app-01\n")
	ma := b.rig.ma
	n := &recNotifier{}
	ma.tell(n, LifecycleEvent{Name: EventRelaunchRefused, Release: "v1", Summary: "relaunch of v1 refused: /x/some-value-of-the-app-01/bin"})
	must(t, os.Remove(b.rig.spec.envFile))
	ma.secretsMu.Lock()
	ma.secrets, ma.secretsLoaded = nil, false
	ma.secretsMu.Unlock()
	ma.tell(n, LifecycleEvent{Name: EventRelaunchRefused, Release: "v1", Summary: "relaunch of v1 refused: /x/other-value-unknown-02/bin"})
	if len(n.evs) != 2 || n.evs[0].Summary != "relaunch of v1 refused: /x/[redacted:TOKEN]/bin" || !strings.HasPrefix(n.evs[1].Summary, "[withheld: the app's env_file could not be read") {
		t.Errorf("%+v", n.evs)
	}
}

// The versions and the pins the app's records hold are names the
// heuristics leave alone in the app's text, as the status's were: an
// app that prints its artifact's digest keeps it readable.
func TestADoorsNamesAreTheRecordsPins(t *testing.T) {
	b := newDoorBox(t, "TOKEN=some-value-of-the-app-01\n")
	pin := strings.Repeat("3f9a1c7e", 8)
	must(t, writeDeployRecord(b.rig.spec.dirs, "v7", []byte(`{"version":"v7","status":"succeeded","sha256":"`+pin+`"}`)))
	if got := b.rig.ma.door(true).text("artifact sha256 " + pin); got != wire.AppText("artifact sha256 "+pin) {
		t.Errorf("the app's text: %q", got)
	}
	if got := b.rig.ma.door(true).text("artifact sha256 " + strings.Repeat("8b2d6e4f", 8)); !strings.Contains(string(got), "[masked, 64 chars]") {
		t.Errorf("the premise, a digest no record names is masked: %q", got)
	}
}
