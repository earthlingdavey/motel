package liveswap

// What the examples' deploy.sh reads (its own functions, run by
// deployShReads) out of the failure bodies the handler writes when the
// failing deploy has no phase of its own (#533): the body's own cause,
// and no other deploy's phase.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lastLine sends req and returns the status code and the last line of
// the answer, which is what deploy.sh reads.
func lastLine(t *testing.T, h *Handler, req *http.Request, stream bool) (int, string) {
	t.Helper()
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	if stream {
		req.Header.Set("Accept", ndjson)
	}
	w := send(t, h, req)
	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	return w.Code, lines[len(lines)-1]
}

func pushVersion(t *testing.T, h *Handler, version string, stream bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/demo?version="+version, strings.NewReader("the artifact"))
	req.Header.Set("Content-Type", "application/gzip")
	return lastLine(t, h, req, stream)
}

func deployURLVersion(t *testing.T, h *Handler, version string, stream bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/demo", strings.NewReader(`{"url":"https://x/`+version+`.tgz","version":"`+version+`"}`))
	return lastLine(t, h, req, stream)
}

// linkTmp makes the app's tmp/ a link, which writeRefusal refuses, and
// returns what puts the directory back.
func linkTmp(t *testing.T, rig *testRig) (undo func()) {
	t.Helper()
	d := rig.spec.dirs
	must(t, d.ensure())
	must(t, os.RemoveAll(d.tmp))
	must(t, os.Symlink(t.TempDir(), d.tmp))
	return func() {
		must(t, os.Remove(d.tmp))
		must(t, os.Mkdir(d.tmp, 0o750))
	}
}

// failV2InStarting deploys v1, then v2, which fails in starting: the
// record and last_deploy a later failure body must not be read as.
func failV2InStarting(t *testing.T, h *Handler, rig *testRig) {
	t.Helper()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
	rig.runner.setStartErr(errors.New("boom"))
	if code, body := deployURLVersion(t, h, "v2", false); code != http.StatusInternalServerError {
		t.Fatalf("v2: %d %s", code, body)
	}
	rig.runner.setStartErr(nil)
}

// wantReading fails the test unless deploy.sh, having sent sent, reads
// phase and a cause beginning with whyPrefix out of body.
func wantReading(t *testing.T, name, body, sent, phase, whyPrefix string) {
	t.Helper()
	gotPhase, why := deployShReads(t, body, sent)
	if gotPhase != phase || !strings.HasPrefix(why, whyPrefix) {
		t.Errorf("%s: deploy.sh reads phase %q (want %q), cause %q (want %q…): %s", name, gotPhase, phase, why, whyPrefix, body)
	}
}

// refusalAlone fails the test unless body is a wire.Error: the cause
// and nothing about another deploy.
func refusalAlone(t *testing.T, name, body string) {
	t.Helper()
	var keys map[string]json.RawMessage
	must(t, json.Unmarshal([]byte(body), &keys))
	if len(keys) != 1 || keys["error"] == nil {
		t.Errorf("%s: not the refusal alone: %s", name, body)
	}
}

// TestPushRefusedBeforeStagingIsTheRefusalAlone: a push the app's
// directories refuse before its upload is staged is answered with the
// refusal alone, as a staging failure is, single or streamed (nothing
// has streamed yet), and deploy.sh names its cause and no phase: not
// those of the deploy before it, which failed, or succeeded, or was of
// the same version (its release removed, so it may be sent again).
func TestPushRefusedBeforeStagingIsTheRefusalAlone(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		failed        bool
		stream        bool
	}{
		{"after another version failed", "v3", true, false},
		{"after another version failed, streamed", "v3", true, true},
		{"after a success", "v2", false, false},
		{"the version that failed, sent again", "v2", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rig := newTestHandler(t)
			if tc.failed {
				failV2InStarting(t, h, rig)
				if _, err := os.Stat(rig.spec.dirs.release("v2")); !os.IsNotExist(err) {
					t.Fatalf("v2's release is still on disk: %v", err)
				}
			} else {
				must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
			}
			undo := linkTmp(t, rig)
			code, body := pushVersion(t, h, tc.version, tc.stream)
			undo()
			if code != http.StatusInternalServerError {
				t.Fatalf("%d %s", code, body)
			}
			refusalAlone(t, tc.name, body)
			wantReading(t, tc.name, body, tc.version, "", "refusing to deploy")
		})
	}
}

// TestFailureBeforeTheFirstPhaseNamesNoPhase: a deploy that fails in
// the pipeline before its first phase is answered with its cause and
// the status, whose last_deploy is its own and holds no phase; deploy.sh
// names the cause and no phase, not the failed record's in deploys or
// the app's "idle".
func TestFailureBeforeTheFirstPhaseNamesNoPhase(t *testing.T) {
	marker := func(rig *testRig) func() {
		must(t, os.MkdirAll(filepath.Join(rig.spec.dirs.deploying, "x"), 0o750))
		return func() { must(t, os.RemoveAll(rig.spec.dirs.deploying)) }
	}
	for _, tc := range []struct {
		name     string
		failedV2 bool
		push     bool
		setup    func(*testRig) func()
		why      string
	}{
		{"URL, writeRefusal in the pipeline", true, false, func(rig *testRig) func() { return linkTmp(t, rig) }, "refusing to deploy"},
		{"URL, writeRefusal, no failed record", false, false, func(rig *testRig) func() { return linkTmp(t, rig) }, "refusing to deploy"},
		{"URL, the marker write fails", true, false, marker, "writing the deploy marker"},
		{"push, the marker write fails", true, true, marker, "writing the deploy marker"},
	} {
		for _, stream := range []bool{false, true} {
			name := tc.name
			if stream {
				name += ", streamed"
			}
			t.Run(name, func(t *testing.T) {
				h, rig := newTestHandler(t)
				if tc.failedV2 {
					failV2InStarting(t, h, rig)
				} else {
					must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
				}
				undo := tc.setup(rig)
				deploy := deployURLVersion
				if tc.push {
					deploy = pushVersion
				}
				code, body := deploy(t, h, "v3", stream)
				undo()
				if code != http.StatusInternalServerError || !strings.Contains(body, `"last_deploy":{"version":"v3",`) {
					t.Fatalf("%d %s", code, body)
				}
				wantReading(t, name, body, "v3", "", tc.why)
			})
		}
	}
}

// TestFailureInAPhaseNamesItsPhase: a deploy that fails in a phase is
// read with that phase and its cause, streamed or not.
func TestFailureInAPhaseNamesItsPhase(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h, rig := newTestHandler(t)
		must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
		rig.runner.setStartErr(errors.New("boom"))
		code, body := deployURLVersion(t, h, "v2", stream)
		if want := map[bool]int{false: http.StatusInternalServerError, true: http.StatusOK}[stream]; code != want {
			t.Fatalf("stream %v: %d %s", stream, code, body)
		}
		wantReading(t, "fails in starting", body, "v2", "starting", "start failed: boom")
		// A body about another version is not this one's phase.
		wantReading(t, "read as a deploy of v9", body, "v9", "", "start failed: boom")
	}
}

// TestFailureCauseReadsAsWritten: a cause holding what Go's JSON
// escapes (<, > and & as \u003c, \u003e and \u0026; a newline as \n)
// reads in deploy.sh as written, a newline as a space, and an escaped
// backslash before such a sequence stays a backslash.
func TestFailureCauseReadsAsWritten(t *testing.T) {
	h, rig := newTestHandler(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
	rig.runner.setStartErr(errors.New("probe answered 503: <html> & \"q\"\n\tnext \\u003c"))
	code, body := deployURLVersion(t, h, "v2", false)
	if code != http.StatusInternalServerError || !strings.Contains(body, `\u003chtml\u003e \u0026`) {
		t.Fatalf("%d %s", code, body)
	}
	if phase, why := deployShReads(t, body, "v2"); phase != "starting" || why != `start failed: probe answered 503: <html> & "q"  next \u003c` {
		t.Errorf("deploy.sh reads phase %q, cause %q: %s", phase, why, body)
	}
}
