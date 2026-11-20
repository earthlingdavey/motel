package liveswap

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// heldRig is the handler with demo's env_file holding env, and v1
// deployed at the rig's clock (2023-11-14T22:13:20Z).
func heldRig(t *testing.T, env string) (*Handler, *testRig) {
	t.Helper()
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "demo.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte(env), 0o600))
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`); w.Code != http.StatusOK {
		t.Fatalf("deploying v1: %d %s", w.Code, w.Body)
	}
	return h, rig
}

// TestStatusTimesPassNoFilter: an env_file value that is a piece of a
// time's text (the deploy's date, its clock time) leaves every time in
// GET /<app> whole, so the status decodes, and the body reports no key:
// the value is nowhere else in it.
func TestStatusTimesPassNoFilter(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	for _, value := range []string{"2023-11-14", "T22:13:20Z", "2023-11-14T22:13:20Z"} {
		h, _ := heldRig(t, "API_VERSION="+value+"\n")
		w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
		var s wire.Status
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != http.StatusOK || s.LastDeploy == nil {
			t.Fatalf("API_VERSION=%s: %v: %d %s", value, err, w.Code, w.Body)
		}
		if !s.LastDeploy.StartedAt.Equal(at) || !s.LastDeploy.FinishedAt.Equal(at) || len(s.LastDeploy.Phases) == 0 || !s.LastDeploy.Phases[0].StartedAt.Equal(at) {
			t.Errorf("API_VERSION=%s: the times: %s", value, w.Body)
		}
		if len(s.Deploys) != 1 || string(s.Deploys[0].StartedAt) != `"2023-11-14T22:13:20Z"` {
			t.Errorf("API_VERSION=%s: the record's times in the list: %s", value, w.Body)
		}
		if strings.Contains(w.Body.String(), "redacted") {
			t.Errorf("API_VERSION=%s: a marker in a body that holds the value only in its times: %s", value, w.Body)
		}
	}
}

// TestDeployRecordTimesPassNoFilter: the record is written with its
// times whole, so GET /<app>?deploy=<v> decodes, and the file on disk
// holds the time.
func TestDeployRecordTimesPassNoFilter(t *testing.T) {
	h, rig := heldRig(t, "API_VERSION=2023-11-14\n")
	b, err := os.ReadFile(deployRecordPath(rig.spec.dirs.deploys, "v1"))
	if err != nil || !strings.Contains(string(b), `"started_at":"2023-11-14T22:13:20Z"`) {
		t.Fatalf("the record on disk: %v %s", err, b)
	}
	w := do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), "")
	var rec wire.DeployResult
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil || w.Code != http.StatusOK || !rec.StartedAt.Equal(time.Unix(1_700_000_000, 0)) {
		t.Fatalf("%v: %d %s", err, w.Code, w.Body)
	}
}

// TestDeployAnswerTimesPassNoFilter: a deploy's own answer, single and
// streamed, keeps its times whole.
func TestDeployAnswerTimesPassNoFilter(t *testing.T) {
	h, _ := heldRig(t, "API_VERSION=2023-11-14\n")
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/2.tgz","version":"v2"}`)
	var s wire.Status
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != http.StatusOK || s.LastDeploy == nil || s.LastDeploy.Version != "v2" {
		t.Fatalf("%v: %d %s", err, w.Code, w.Body)
	}
}

// TestStatusNumbersWithheldNamed is the accepted row's behaviour
// (DESIGN-liveswap.md, "Accepted residuals"): an env_file value of
// eight or more digits that equals digits of a number in the status (a
// memory figure the metrics store answered) is replaced inside the
// number, so the body is withheld whole, saying why and naming the key,
// and the value is not in it.
func TestStatusNumbersWithheldNamed(t *testing.T) {
	h, _ := heldRig(t, "ACCOUNT_ID=12345678\n")
	now := 123456789.0
	h.app.SetMetricsSource(&fakeMetricsSource{reply: &wire.Metrics{Window: "1h", Memory: &wire.MemorySummary{Now: &now}}})
	w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	const want = `{"error":"response withheld: a redacted value overlapped the response's own structure","redacted_env":["ACCOUNT_ID"]}` + "\n"
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Errorf("got %d %s\nwant %s", w.Code, w.Body, want)
	}
}

// TestHeldTimesAreTheBodysOwn: only a time the body holds as a time is
// held: one under a time's key, as its value. The same text in an
// error, in a list, or inside a string that spells a time's pair is
// the app's or the request's, and is filtered.
func TestHeldTimesAreTheBodysOwn(t *testing.T) {
	r := NewRedactor([]string{"API_VERSION=2023-11-14"}, nil)
	const at = `"2023-11-14T22:13:20.5Z"`
	for _, c := range []struct{ in, want string }{
		{`{"started_at":` + at + `,"phases":[{"name":"downloading","started_at":"2023-11-14T22:13:20+01:00","seconds":0}]}`,
			`{"started_at":` + at + `,"phases":[{"name":"downloading","started_at":"2023-11-14T22:13:20+01:00","seconds":0}]}`},
		{`{"at": ` + at + `}`, `{"at": ` + at + `}`},
		{`{"error":` + at + `,"at":` + at + `}`, `{"at":` + at + `,"error":"[redacted:API_VERSION]T22:13:20.5Z","redacted_env":["API_VERSION"]}`},
		{`{"log_tail":[` + at + `]}`, `{"log_tail":["[redacted:API_VERSION]T22:13:20.5Z"],"redacted_env":["API_VERSION"]}`},
		{`{"error":"\"started_at\":\"2023-11-14T22:13:20.5Z\""}`, `{"error":"\"started_at\":\"[redacted:API_VERSION]T22:13:20.5Z\"","redacted_env":["API_VERSION"]}`},
		// Not a time as encoding/json writes one: filtered.
		{`{"started_at":"2023-11-14"}`, `{"redacted_env":["API_VERSION"],"started_at":"[redacted:API_VERSION]"}`},
	} {
		if got := r.redactJSON([]byte(c.in)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

// TestHeldTimesFallBack: a body whose bytes, the times put back, would
// hold a known value across a time's edge, or whose placeholders the
// filter did not leave, or that holds the placeholder's text already,
// goes through the filter whole as before: the value is not in the
// bytes written.
func TestHeldTimesFallBack(t *testing.T) {
	const body = `{"started_at":"2023-11-14T22:13:20Z","error":"boom"}`
	for _, c := range []struct{ value, in, want string }{
		// A time's end and the JSON after it.
		{`20Z","error":"bo`, body, `{"redacted_env":["K"],"started_at":"2023-11-14T22:13:[redacted:K]om"}`},
		// The JSON before a time and its start, which holds the time's
		// opening quote; and the body's structure: withheld either way.
		{`at":"2023-11`, body, `{"error":"response withheld: a redacted value overlapped the response's own structure","redacted_env":["K"]}`},
		{`Z","error"`, body, `{"error":"response withheld: a redacted value overlapped the response's own structure","redacted_env":["K"]}`},
		// The placeholder's text in the body: nothing held.
		{`2023-11-14`, `{"started_at":"2023-11-14T22:13:20Z","error":"a\u0000"}`, `{"error":"a\u0000","redacted_env":["K"],"started_at":"[redacted:K]T22:13:20Z"}`},
	} {
		got := NewRedactor([]string{"K=" + c.value}, nil).redactJSON([]byte(c.in))
		if got != c.want || !json.Valid([]byte(got)) {
			t.Errorf("K=%s:\n got %s\nwant %s", c.value, got, c.want)
		}
	}
}

// TestHeldTimeKeysAreTheWireTimes: every time a body passing an app's
// filter holds is under one of heldTimeKeys, and no field of those
// bodies under one of them is anything but a time (or the deploy
// list's raw times, which are a record's, held as the record has them).
func TestHeldTimeKeysAreTheWireTimes(t *testing.T) {
	timeType, raw := reflect.TypeOf(time.Time{}), reflect.TypeOf(json.RawMessage{})
	seen := map[reflect.Type]bool{}
	used := map[string]bool{}
	var walk func(reflect.Type)
	walk = func(ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || ty == timeType || seen[ty] {
			return
		}
		seen[ty] = true
		for i := range ty.NumField() {
			f := ty.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			isTime := ft == timeType || (ft == raw && ty == reflect.TypeOf(wire.DeploySummary{}) && strings.HasSuffix(name, "_at"))
			if isTime {
				used[`"`+name+`"`] = true
			}
			if isTime != heldTimeKeys[`"`+name+`"`] {
				t.Errorf("%s.%s (%q): a time %v, held %v", ty.Name(), f.Name, name, isTime, heldTimeKeys[`"`+name+`"`])
			}
			walk(f.Type)
		}
	}
	for _, v := range []any{wire.Status{}, wire.Failure{}, wire.DeployResult{}, wire.PhaseEvent{}, wire.AppEntry{}, wire.Error{}} {
		walk(reflect.TypeOf(v))
	}
	for k := range heldTimeKeys {
		if !used[k] {
			t.Errorf("%s is held, and no time is under it", k)
		}
	}
}
