package liveswap

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
// streamed, keeps its times whole: the status's, at the rig's clock,
// and each phase line's, at the wall clock (today's date the value).
func TestDeployAnswerTimesPassNoFilter(t *testing.T) {
	// Today's and tomorrow's: a run across midnight still has one.
	now := time.Now().UTC()
	h, _ := heldRig(t, "API_VERSION=2023-11-14\nTODAY="+now.Format(time.DateOnly)+"\nTOMORROW="+now.AddDate(0, 0, 1).Format(time.DateOnly)+"\n")
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/2.tgz","version":"v2"}`)
	var s wire.Status
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != http.StatusOK || s.LastDeploy == nil || s.LastDeploy.Version != "v2" {
		t.Fatalf("%v: %d %s", err, w.Code, w.Body)
	}
	w, _ = streamLinesOf(t, h, `{"url":"https://x/3.tgz","version":"v3"}`)
	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	for _, line := range lines[:len(lines)-1] {
		var ev wire.PhaseEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.At.IsZero() || ev.Event != wire.EventPhase {
			t.Errorf("a phase line: %v: %s", err, line)
		}
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &s); err != nil || s.LastDeploy == nil || s.LastDeploy.Version != "v3" || strings.Contains(w.Body.String(), "redacted") {
		t.Errorf("the last line: %v: %s", err, w.Body)
	}
}
