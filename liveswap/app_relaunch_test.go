package liveswap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A deploy or rollback naming the version that is running relaunches
// it from the release on disk — the normal pipeline minus fetch and
// extract — when it is for the same artifact. These tests pin that
// rule and its edges: what relaunches, what is refused and how it
// says so, and that the version's history is untouched either way.

// pinA and pinB are two digests of the shape a deployer asserts.
var (
	pinA = strings.Repeat("ab", 32)
	pinB = strings.Repeat("cd", 32)
)

// deployV1Pinned deploys v1 from a URL with the given pin.
func deployV1Pinned(t *testing.T, rig *testRig, pin string) error {
	t.Helper()
	return rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/v1.tgz", version: "v1", sha256: pin, by: "test"})
}

// phaseNames is the names of a result's phases, in order.
func phaseNames(r *deployResult) []string {
	var names []string
	for _, p := range r.Phases {
		names = append(names, p.Name)
	}
	return names
}

// stoppedAt is the i-th handle the runner was asked to stop.
func (r *fakeRunner) stoppedAt(i int) handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped[i]
}

// A URL deploy of the running version with its recorded pin relaunches
// it: a second instance of the same release starts, soaks, is
// promoted, and the first is drained and stopped — nothing is fetched,
// no pre_start runs, and the version's record is the deploy that put
// it there.
func TestRelaunchOnMatchingPin(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}
	must(t, deployV1Pinned(t, rig, pinA))
	first := rig.runner.handleAt(0)
	sockV1 := rig.ma.activeSocket.Load()
	record, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	before := rig.clock.Now()

	must(t, deployV1Pinned(t, rig, pinA))

	// Taken from disk as a rollback is: the fetcher saw a rollback of
	// v1, so it downloaded and extracted nothing.
	if got := rig.fetch.lastReq; !got.rollback || got.version != "v1" {
		t.Fatalf("the relaunch must take the release as a rollback does, got %+v", got)
	}
	if rig.runner.startCount() != 3 { // pre_start, v1, then the relaunch — no second pre_start
		t.Fatalf("starts = %d, want the first deploy's pre_start and app and one relaunch", rig.runner.startCount())
	}
	if rig.runner.runOnceCount != 1 {
		t.Fatalf("pre_start ran %d times; a relaunch must not re-run it", rig.runner.runOnceCount)
	}
	if rig.ma.activeSocket.Load() == sockV1 {
		t.Fatal("the cutover did not move to the new instance")
	}
	if rig.runner.stopCount() != 1 || rig.runner.stoppedAt(0) != first {
		t.Fatalf("the old instance must be stopped exactly once after promote: stops=%d", rig.runner.stopCount())
	}
	if rig.clock.Now().Sub(before) < rig.spec.drain {
		t.Fatal("drain was not observed before stopping the old instance")
	}
	if first.isAlive() {
		t.Fatal("the old instance is still alive")
	}
	st := rig.ma.status()
	if st.CurrentVersion != "v1" || !st.Running || st.Phase != "idle" {
		t.Fatalf("status after relaunch: %+v", st)
	}
	// The release dir is the one that was serving: the same path, not
	// a copy, and still there.
	if got := rig.runner.started[2].dir; got != rig.spec.dirs.release("v1") {
		t.Fatalf("relaunched from %s, want the running release dir", got)
	}
	last := st.LastDeploy
	if last.Status != "succeeded" || last.Note != "relaunched v1 (same artifact)" || last.SHA256 != pinA {
		t.Fatalf("last_deploy = %+v", last)
	}
	if got := phaseNames(last); len(got) == 0 || got[0] != "preparing" || strings.Join(got, ",") != "preparing,starting,soaking,promoting,draining,stopping_old" {
		t.Fatalf("phases = %v; a relaunch neither downloads nor extracts", got)
	}
	// History: one record, byte for byte the first deploy's.
	after, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	if string(after) != string(record) {
		t.Fatalf("the relaunch rewrote v1's record:\n%s\n%s", record, after)
	}
	if len(st.Deploys) != 1 || st.Deploys[0].Version != "v1" || summaryPin(st.Deploys[0]) != pinA {
		t.Fatalf("deploys = %+v, want v1's one record with its pin", st.Deploys)
	}
}

// A different pin under the running version is a different artifact:
// refused, nothing launched, the message naming immutability and the
// way a new value reaches the app.
func TestRelaunchRefusedOnADifferentPin(t *testing.T) {
	rig := newTestRig(t)
	must(t, deployV1Pinned(t, rig, pinA))
	record, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)

	err = deployV1Pinned(t, rig, pinB)
	var ve validationError
	if !errors.As(err, &ve) {
		t.Fatalf("want a 422 refusal, got %v", err)
	}
	for _, want := range []string{"version v1 is already running", "different artifact", "versions are immutable", "a rollback to v1 relaunches", "new secret reaches the app"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err.Error(), want)
		}
	}
	if rig.runner.startCount() != 1 || rig.runner.stopCount() != 0 {
		t.Fatalf("a refusal must launch and stop nothing: starts=%d stops=%d", rig.runner.startCount(), rig.runner.stopCount())
	}
	if after, _ := readDeployRecord(rig.spec.dirs, "v1"); string(after) != string(record) {
		t.Fatalf("the refusal rewrote v1's record:\n%s\n%s", record, after)
	}
	if last := rig.ma.status().LastDeploy; last.Status != "failed" || last.Note != "" || last.Error != err.Error() {
		t.Fatalf("last_deploy must be the refusal, with no relaunch note: %+v", last)
	}
}

// Without a digest on both sides there is nothing to match, and
// nothing is fetched to find out: a request with no pin, and a request
// with a pin against a version whose record has none, are both refused
// with the remedy named. The second is what a push deployed before
// pushes were hashed, or an unpinned pull, comes to.
func TestRelaunchRefusedWithoutADigest(t *testing.T) {
	cases := []struct {
		name          string
		recorded, pin string
		want          string
	}{
		{"request carries no pin", pinA, "", "carries no sha256"},
		{"version has no recorded pin", "", pinA, "digest is not on record"},
		{"neither", "", "", "carries no sha256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			must(t, deployV1Pinned(t, rig, tc.recorded))
			rig.fetch.err = errors.New("a refusal must not fetch to compare")
			err := deployV1Pinned(t, rig, tc.pin)
			var ve validationError
			if !errors.As(err, &ve) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "already running") {
				t.Fatalf("want a 422 saying %q, got %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "a rollback to v1 relaunches it") || !strings.Contains(err.Error(), "new secret reaches the app") {
				t.Fatalf("the refusal must say how a new value reaches the app: %v", err)
			}
			if rig.runner.startCount() != 1 {
				t.Fatalf("starts = %d; a refusal launches nothing", rig.runner.startCount())
			}
		})
	}
}

// A URL deploy of the running version whose digest matches gets the
// url checks a fetch would give it: a host the allowlist refuses, or
// plain http, is the fetch's own 422, with nothing launched — a URL
// that could not deploy anything cannot relaunch either.
func TestRelaunchChecksTheURLLikeAFetch(t *testing.T) {
	rig := newTestRig(t)
	must(t, deployV1Pinned(t, rig, pinA))
	for _, tc := range []struct{ url, want string }{
		{"https://elsewhere.test/v1.tgz", "refused"},
		{"http://x/v1.tgz", "plain http"},
	} {
		err := rig.ma.Deploy(context.Background(), deployRequest{url: tc.url, version: "v1", sha256: pinA})
		var ve validationError
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want a 422 saying %q, got %v", tc.url, tc.want, err)
		}
	}
	if rig.runner.startCount() != 1 {
		t.Fatalf("starts = %d; a refused url launches nothing", rig.runner.startCount())
	}
}

// A rollback to the running version is the same release by definition:
// it relaunches without a digest, and its last_deploy carries the
// record's pin like any rollback.
func TestRollbackToRunningVersionRelaunches(t *testing.T) {
	rig := newTestRig(t)
	must(t, deployV1Pinned(t, rig, pinA))
	first := rig.runner.handleAt(0)
	record, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)

	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	if rig.runner.startCount() != 2 || rig.runner.stopCount() != 1 || rig.runner.stoppedAt(0) != first {
		t.Fatalf("starts=%d stops=%d; want one relaunch and the old instance stopped", rig.runner.startCount(), rig.runner.stopCount())
	}
	last := rig.ma.status().LastDeploy
	if last.Status != "succeeded" || last.Note != "relaunched v1 (same artifact)" || last.SHA256 != pinA {
		t.Fatalf("last_deploy = %+v", last)
	}
	if after, _ := readDeployRecord(rig.spec.dirs, "v1"); string(after) != string(record) {
		t.Fatalf("a rollback onto the running version rewrote its record:\n%s\n%s", record, after)
	}
	// An unpinned version rolls back onto itself just the same.
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v2", rollback: true}))
	if last := rig.ma.status().LastDeploy; last.Note != "relaunched v2 (same artifact)" || last.SHA256 != "" {
		t.Fatalf("unpinned relaunch: %+v", last)
	}
}

// A relaunch whose new instance never becomes healthy fails like any
// deploy: the old instance keeps serving on the same socket, the new
// one is stopped, the release stays (it is the one serving), and the
// version's record is untouched; last_deploy carries the failure.
func TestFailedRelaunchKeepsOldServing(t *testing.T) {
	rig := newTestRig(t)
	must(t, deployV1Pinned(t, rig, pinA))
	first := rig.runner.handleAt(0)
	sockV1 := rig.ma.activeSocket.Load()
	record, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)

	rig.prober.err = errors.New("never became healthy")
	err = deployV1Pinned(t, rig, pinA)
	if err == nil {
		t.Fatal("want the health gate's failure")
	}
	if rig.ma.activeSocket.Load() != sockV1 {
		t.Fatal("a failed relaunch must not move traffic")
	}
	if !first.isAlive() || rig.runner.stopCount() != 1 || rig.runner.stoppedAt(0) != rig.runner.handleAt(1) {
		t.Fatalf("the new instance alone must be stopped: old alive=%v stops=%d", first.isAlive(), rig.runner.stopCount())
	}
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err != nil {
		t.Fatalf("the serving release must never be removed by a failed relaunch: %v", err)
	}
	st := rig.ma.status()
	if st.CurrentVersion != "v1" || !st.Running {
		t.Fatalf("old version must keep serving: %+v", st)
	}
	if st.LastDeploy.Status != "failed" || st.LastDeploy.Phase != "soaking" || st.LastDeploy.Note != "" {
		t.Fatalf("last_deploy = %+v", st.LastDeploy)
	}
	if after, _ := readDeployRecord(rig.spec.dirs, "v1"); string(after) != string(record) {
		t.Fatalf("a failed relaunch rewrote v1's record (the version is still serving):\n%s\n%s", record, after)
	}
	if len(st.Deploys) != 1 || st.Deploys[0].Status != "succeeded" {
		t.Fatalf("deploys = %+v", st.Deploys)
	}
}

// A push is hashed as it lands, and the digest is recorded as the
// version's pin; a second push of the same bytes relaunches, and one
// of other bytes under the same version is refused.
func TestWebhookPushSameBytesRelaunches(t *testing.T) {
	h, rig := newTestHandler(t)
	sum := sha256.Sum256([]byte("tarball-bytes"))
	digest := hex.EncodeToString(sum[:])
	lastDeploy := func(body []byte) deployResult {
		var m struct {
			LastDeploy deployResult `json:"last_deploy"`
		}
		must(t, json.Unmarshal(body, &m))
		return m.LastDeploy
	}

	w := send(t, h, gzipDeploy(t, "/demo?version=v9", appToken(t), "tarball-bytes"))
	if ld := lastDeploy(w.Body.Bytes()); w.Code != http.StatusOK || ld.SHA256 != digest || ld.Note != "" {
		t.Fatalf("push: %d last_deploy=%+v; want the upload's digest recorded", w.Code, ld)
	}
	if got := recordedPin(rig.spec.dirs, "v9"); got != digest {
		t.Fatalf("record pin = %q, want the upload's digest", got)
	}

	w = send(t, h, gzipDeploy(t, "/demo?version=v9", appToken(t), "tarball-bytes"))
	if ld := lastDeploy(w.Body.Bytes()); w.Code != http.StatusOK || ld.Note != "relaunched v9 (same artifact)" || ld.SHA256 != digest {
		t.Fatalf("same bytes: %d %s", w.Code, w.Body.String())
	}
	if got := rig.fetch.lastReq; !got.rollback {
		t.Fatalf("the relaunch must take the release as a rollback does, not extract the upload: %+v", got)
	}
	if rig.runner.startCount() != 2 || rig.runner.stopCount() != 1 {
		t.Fatalf("starts=%d stops=%d", rig.runner.startCount(), rig.runner.stopCount())
	}

	w = send(t, h, gzipDeploy(t, "/demo?version=v9", appToken(t), "other-bytes"))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "different artifact") {
		t.Fatalf("other bytes: %d %s", w.Code, w.Body.String())
	}
	if rig.runner.startCount() != 2 {
		t.Fatal("a refused push launched something")
	}
}

// The response names the relaunch — with the env_file, when the app
// has one, since that is what a rerun is reaching for — and the note
// passes the filter whole, in the single response and the stream's
// done line alike.
func TestWebhookRelaunchResponseNamesIt(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("TOKEN=hunter2hunter2hunter2\n"), 0o600))
	body := `{"url":"https://x/a.tgz","version":"v1","sha256":"` + pinA + `"}`
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), body); w.Code != http.StatusOK {
		t.Fatalf("deploy: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, http.MethodPost, "/demo", appToken(t), body)
	want := `"note":"relaunched v1 (same artifact); it reads its env file now"`
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
		t.Fatalf("relaunch: %d %s; want %s", w.Code, w.Body.String(), want)
	}
	_, lines := streamLinesOf(t, h, body)
	last := lines[len(lines)-1]
	ld, _ := last["last_deploy"].(map[string]any)
	if last["event"] != "done" || last["http_status"] != float64(200) || ld == nil || ld["note"] != "relaunched v1 (same artifact); it reads its env file now" {
		t.Fatalf("done line = %v", last)
	}
	for i, l := range lines[:len(lines)-1] {
		if p := l["phase"]; i == 0 && p != "preparing" || p == "downloading" || p == "extracting" {
			t.Fatalf("streamed phases = %v; a relaunch starts at preparing", lines)
		}
	}
	// The status carries it until the next deploy; the record does not.
	if w := do(t, h, http.MethodGet, "/demo", appToken(t), ""); !strings.Contains(w.Body.String(), want) {
		t.Fatalf("status: %s", w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), ""); strings.Contains(w.Body.String(), `"note"`) {
		t.Fatalf("the record must not carry a relaunch: %s", w.Body.String())
	}
}
