package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// phaseGate holds a deploy at each phase it enters, after the phase is
// set, until the test lets it go: a status read "while a phase runs".
type phaseGate struct {
	entered chan string
	next    chan struct{}
}

func newPhaseGate() *phaseGate {
	return &phaseGate{entered: make(chan string), next: make(chan struct{})}
}

func (g *phaseGate) onPhase(p string) {
	g.entered <- p
	<-g.next
}

func phasesOf(ps []phaseTiming) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return strings.Join(names, ",")
}

// TestDeployingShowsEachPhaseWhileItRuns: while a deploy runs, the
// status's deploying names its version, who started it and when, and
// every phase entered so far, the current one last; the last phase's
// seconds run with the clock and an ended phase's stay put; once it is
// over, deploying is gone and last_deploy holds the same phases with
// the same times.
func TestDeployingShowsEachPhaseWhileItRuns(t *testing.T) {
	rig := newTestRig(t)
	g := newPhaseGate()
	done := make(chan error, 1)
	go func() {
		done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1", by: "local:/k.pub", onPhase: g.onPhase})
	}()
	var seen []string
	var last *deployInFlight
	for {
		select {
		case p := <-g.entered:
			seen = append(seen, p)
			s := rig.ma.status()
			d := s.Deploying
			if d == nil || d.Version != "v1" || d.By != "local:/k.pub" || s.Phase != p || phasesOf(d.Phases) != strings.Join(seen, ",") {
				t.Fatalf("in %s: phase %q, deploying %+v; want v1 with %v", p, s.Phase, d, seen)
			}
			for i, ph := range d.Phases {
				if ph.StartedAt.Before(d.StartedAt) || (i > 0 && ph.StartedAt.Before(d.Phases[i-1].StartedAt)) || ph.Seconds < 0 {
					t.Fatalf("in %s: the timings run backwards: %+v", p, d)
				}
				if last != nil && i < len(last.Phases)-1 && ph != last.Phases[i] {
					t.Fatalf("in %s: phase %s, which ended, changed from %+v to %+v", p, ph.Name, last.Phases[i], ph)
				}
			}
			rig.clock.Advance(1500 * time.Millisecond)
			later := rig.ma.status().Deploying
			n := len(d.Phases) - 1
			if later == nil || math.Abs(later.Phases[n].Seconds-d.Phases[n].Seconds-1.5) > 1e-9 || later.Phases[n].StartedAt != d.Phases[n].StartedAt {
				t.Fatalf("in %s: the running phase's seconds did not follow the clock: %+v then %+v", p, d.Phases[n], later)
			}
			last = later
			g.next <- struct{}{}
		case err := <-done:
			must(t, err)
			s := rig.ma.status()
			if s.Deploying != nil || s.Phase != "idle" || s.LastDeploy == nil {
				t.Fatalf("after the deploy: %+v", s)
			}
			if got := phasesOf(s.LastDeploy.Phases); got != strings.Join(seen, ",") || len(seen) < 5 {
				t.Fatalf("last_deploy's phases %s, seen while it ran %v", got, seen)
			}
			if !s.LastDeploy.StartedAt.Equal(last.StartedAt) {
				t.Errorf("deploying began at %v, last_deploy at %v", last.StartedAt, s.LastDeploy.StartedAt)
			}
			for i := range len(last.Phases) - 1 {
				if s.LastDeploy.Phases[i] != last.Phases[i] {
					t.Errorf("phase %d: %+v while it ran, %+v in the record", i, last.Phases[i], s.LastDeploy.Phases[i])
				}
			}
			return
		}
	}
}

// TestDeployingARollbackAndARelaunch: a rollback, and a rollback naming
// the running version (a relaunch), are deploys: deploying is there
// while each runs, [] for phases before its first, and gone with its
// outcome, a failure before any phase included.
func TestDeployingARollbackAndARelaunch(t *testing.T) {
	rig := newTestRig(t)
	for _, v := range []string{"v1", "v2"} {
		must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/" + v + ".tgz", version: v}))
	}
	held, release := rig.holdFetch(t)
	for _, tc := range []struct {
		name, version string
		fail          error
	}{
		{"a rollback that fails before any phase", "v1", errors.New("the release went")},
		{"a relaunch of the running version", "v2", nil},
	} {
		done := make(chan error, 1)
		go func() {
			done <- rig.ma.Deploy(context.Background(), deployRequest{version: tc.version, rollback: true})
		}()
		held()
		s := rig.ma.status()
		if d := s.Deploying; d == nil || d.Version != tc.version || d.Phases == nil || len(d.Phases) != 0 || s.Phase != "idle" {
			t.Fatalf("%s, while it runs: phase %q, deploying %+v", tc.name, s.Phase, d)
		}
		raw, err := json.Marshal(s)
		must(t, err)
		if !strings.Contains(string(raw), `"phases":[]},"deploys":`) {
			t.Errorf("%s: deploying's phases are not [] before deploys: %s", tc.name, raw)
		}
		release(tc.fail)
		err = <-done
		if (err != nil) != (tc.fail != nil) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		s = rig.ma.status()
		if s.Deploying != nil || s.LastDeploy == nil || s.LastDeploy.Version != tc.version {
			t.Fatalf("%s, after: %+v", tc.name, s)
		}
		if tc.fail != nil && (s.LastDeploy.Status != "failed" || s.LastDeploy.Phase != "") {
			t.Errorf("%s: last_deploy %+v, want failed in no phase", tc.name, s.LastDeploy)
		}
		if tc.fail == nil && s.LastDeploy.Note == "" {
			t.Errorf("%s: last_deploy %+v is not a relaunch", tc.name, s.LastDeploy)
		}
	}
}

// TestDeployingASecondDeployRefused: a deploy refused because one runs
// leaves the running one's deploying as it was, through the handler
// too, whose 409 carries no status.
func TestDeployingASecondDeployRefused(t *testing.T) {
	h, rig := newTestHandler(t)
	held, release := rig.holdFetch(t)
	done := make(chan error, 1)
	go func() {
		done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"})
	}()
	held()
	before := rig.ma.status().Deploying
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2.tgz", version: "v2"}); !errors.Is(err, errDeployInProgress) {
		t.Fatalf("a second deploy: %v", err)
	}
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/2.tgz","version":"v2"}`); w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "deploying") {
		t.Fatalf("a second deploy through the handler: %d %s", w.Code, w.Body)
	}
	w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	var s wire.Status
	must(t, json.Unmarshal(w.Body.Bytes(), &s))
	after := rig.ma.status().Deploying
	if before == nil || s.Deploying == nil || s.Deploying.Version != "v1" || phasesOf(after.Phases) != phasesOf(before.Phases) || !after.StartedAt.Equal(before.StartedAt) {
		t.Fatalf("the running deploy's deploying changed: %+v, then %+v; GET %s", before, after, w.Body)
	}
	release(nil)
	must(t, <-done)
}

// TestDeployingUnderConcurrentReads, run under -race: status reads,
// the handler's among them, while deploys of every kind run, pushes
// through the handler included, each read consistent: a phase other
// than idle is deploying's last; no deploying means idle; the times
// never run backwards; and last_deploy, when deploying is there, is an
// earlier deploy's, begun strictly before it (each deploy begins a
// second after the last), never the same deploy's outcome beside it.
func TestDeployingUnderConcurrentReads(t *testing.T) {
	h, rig := newTestHandler(t)
	tok := appToken(t)
	var mu sync.Mutex
	var reads, inFlight int
	check := func(s wire.Status) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		d := s.Deploying
		if d == nil {
			if s.Phase != "idle" {
				t.Errorf("phase %q with no deploying", s.Phase)
			}
			return
		}
		inFlight++
		if s.Phase != "idle" && (len(d.Phases) == 0 || d.Phases[len(d.Phases)-1].Name != s.Phase) {
			t.Errorf("phase %q, deploying %+v", s.Phase, d)
		}
		for i, p := range d.Phases {
			if p.StartedAt.Before(d.StartedAt) || (i > 0 && p.StartedAt.Before(d.Phases[i-1].StartedAt)) || p.Seconds < 0 {
				t.Errorf("times run backwards: %+v", d)
			}
		}
		if s.LastDeploy != nil && !s.LastDeploy.StartedAt.Before(d.StartedAt) {
			t.Errorf("last_deploy %+v is not earlier than the deploy in flight %+v", s.LastDeploy, d)
		}
	}
	read := func() {
		check(rig.ma.status())
		w := do(t, h, http.MethodGet, "/demo", tok, "")
		var s wire.Status
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Errorf("GET: %v: %s", err, w.Body)
		}
		check(s)
	}
	// One reader reads all the time, and at each phase the deploy also
	// waits for one read of its own, so some reads are sure to land
	// inside a phase; a second reads the status alone, as fast as it
	// can, for the steps no hook reaches.
	readNow := make(chan chan struct{})
	onPhase := func(string) {
		ack := make(chan struct{})
		readNow <- ack
		<-ack
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case ack := <-readNow:
				read()
				close(ack)
			default:
				read()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				check(rig.ma.status())
			}
		}
	}()
	ctx := context.Background()
	push := func(version string) error {
		req := httptest.NewRequest(http.MethodPost, "/demo?version="+version, strings.NewReader("the artifact"))
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		must(t, h.ServeHTTP(w, req, nil))
		if w.Code != http.StatusOK {
			return errors.New(w.Body.String())
		}
		return nil
	}
	for round := range 5 {
		v := func(n int) string { return fmt.Sprintf("r%d-v%d", round, n) }
		for i, step := range []func() error{
			func() error {
				return rig.ma.Deploy(ctx, deployRequest{url: "https://x/1.tgz", version: v(1), onPhase: onPhase})
			},
			func() error {
				return rig.ma.Deploy(ctx, deployRequest{url: "https://x/2.tgz", version: v(2), onPhase: onPhase})
			}, // fails to start
			func() error { return push(v(3)) },
			func() error {
				return rig.ma.Deploy(ctx, deployRequest{version: v(1), rollback: true, onPhase: onPhase})
			},
			func() error {
				return rig.ma.Deploy(ctx, deployRequest{version: v(1), rollback: true, onPhase: onPhase})
			}, // the running version: a relaunch
		} {
			rig.clock.Advance(time.Second) // each deploy begins after the last ended
			if i == 1 {
				rig.runner.setStartErr(errors.New("boom"))
			}
			err := step()
			rig.runner.setStartErr(nil)
			if (err != nil) != (i == 1) {
				t.Fatalf("round %d, deploy %d: %v", round, i, err)
			}
		}
	}
	close(stop)
	wg.Wait()
	if inFlight < 60 { // the phases of two deploys a round, two reads each, five rounds
		t.Errorf("%d reads, %d of them with a deploy in flight", reads, inFlight)
	}
}

// TestDeployingFiltered: deploying passes the app's filter like every
// field of the status: a version or deployer holding an env_file value
// comes out as its marker, and a version that looks generated (a git
// SHA) is a name the filter lets through, as it does the versions the
// status already names.
func TestDeployingFiltered(t *testing.T) {
	h, rig := newTestHandler(t)
	const secret = "s3cretvalue-41d07a"
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("SECRET="+secret+"\n"), 0o600))
	sha := "a41c09e2b7d1f3e5c7a9b1d3f5e7c9a1b3d5f7e9"
	held, release := rig.holdFetch(t)
	for _, tc := range []struct{ version, by, want string }{
		{secret, "local:" + secret, `"deploying":{"version":"[redacted:SECRET]","deployed_by":"local:[redacted:SECRET]",`},
		{sha, "local:/k.pub", `"deploying":{"version":"` + sha + `",`},
	} {
		done := make(chan error, 1)
		go func() {
			done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: tc.version, by: tc.by})
		}()
		held()
		w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
		if body := w.Body.String(); !strings.Contains(body, tc.want) || strings.Contains(body, secret) {
			t.Errorf("GET while %s deploys: %s", tc.version, body)
		}
		release(nil)
		must(t, <-done)
	}
}

// deployShField is the examples' deploy.sh reading one field out of a
// body, by its own function: the last occurrence of the key.
func deployShField(t *testing.T, body, key string) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "examples", "node", "scripts", "deploy.sh"))
	must(t, err)
	def := regexp.MustCompile(`(?m)^field\(\) \{\n.*\n\}\n`).Find(script)
	if def == nil {
		t.Fatal("deploy.sh has no field function of the shape this test reads")
	}
	out, err := exec.Command("sh", "-c", string(def)+`field "$1" "$2"`, "sh", body, key).Output()
	must(t, err)
	return strings.TrimSuffix(string(out), "\n")
}

// TestFailureBodyPhaseWithADeployInFlight: deploy.sh names the phase a
// deploy failed in by the last "phase" in the failure body. deploying
// is gone from the status by the time a failed deploy's own body is
// written, and a body that holds one (the status read once another
// deploy began) still ends with last_deploy's phase, deploying being
// before deploys and last_deploy and holding no "phase" key.
func TestFailureBodyPhaseWithADeployInFlight(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.runner.startErr = errors.New("boom")
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), `"deploying"`) || deployShField(t, w.Body.String(), "phase") != "starting" {
		t.Fatalf("the failure body: %d %s", w.Code, w.Body)
	}
	rig.runner.startErr = nil

	held, release := rig.holdFetch(t)
	done := make(chan error, 1)
	go func() {
		done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2.tgz", version: "v2"})
	}()
	held()
	// The failure body of v1 as it would read with v2 begun: the
	// status, through the filter, as deployOutcome writes it.
	code, body, rd := deployOutcome(rig.ma, errors.New("start failed: boom"))
	raw, err := json.Marshal(body)
	must(t, err)
	got := rd.redactJSON(raw)
	release(nil)
	must(t, <-done)
	if code != http.StatusInternalServerError {
		t.Fatalf("code %d", code)
	}
	d, deploys, last := strings.Index(got, `"deploying":`), strings.Index(got, `"deploys":`), strings.Index(got, `"last_deploy":`)
	if d < 0 || deploys < d || last < deploys || !strings.Contains(got[d:deploys], `"name":"extracting"`) {
		t.Fatalf("deploying is not before deploys and last_deploy, with v2's phases: %s", got)
	}
	if phase := deployShField(t, got, "phase"); phase != "starting" {
		t.Fatalf("deploy.sh reads the failing phase as %q: %s", phase, got)
	}
}

// TestAppsSummaryNamesTheDeployInFlight: the app list's summary names
// the version of a deploy in flight, as GET /<app>'s deploying does at
// the same moment, for that app alone, and not once it is over.
func TestAppsSummaryNamesTheDeployInFlight(t *testing.T) {
	r := newAppsRig(t, "busy", "calm")
	f := r.fetch["busy"]
	f.gate, f.inFetch = make(chan error), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.apps["busy"].Deploy(context.Background(), deployRequest{url: "https://x/busy.tgz", version: "v1"})
	}()
	<-f.inFetch
	summary := func(name string) *wire.AppSummary {
		t.Helper()
		var e wire.AppEntry
		must(t, json.Unmarshal(r.list(t)[name], &e))
		if e.Summary == nil {
			t.Fatalf("%s: no summary", name)
		}
		return e.Summary
	}
	w := sendTo(t, r.h, http.MethodGet, "/busy", token(r.people.carol))
	var s wire.Status
	must(t, json.Unmarshal(w.Body.Bytes(), &s))
	busy := summary("busy")
	if s.Deploying == nil || busy.Deploying != s.Deploying.Version || busy.Deploying != "v1" || busy.Phase != s.Phase {
		t.Errorf("the summary %+v, the status's deploying %+v", busy, s.Deploying)
	}
	if calm := summary("calm"); calm.Deploying != "" {
		t.Errorf("an app with no deploy in flight: %+v", calm)
	}
	f.gate <- nil
	must(t, <-done)
	if busy := summary("busy"); busy.Deploying != "" || busy.CurrentVersion != "v1" {
		t.Errorf("after the deploy: %+v", busy)
	}
}

// TestDeployingAPushFromItsUpload: a push is in flight from the moment
// the box takes it, before its upload is staged, as the 409 a second
// deploy gets then says; an upload that is not staged ends it with no
// outcome, and one that is ends it with its pipeline's.
func TestDeployingAPushFromItsUpload(t *testing.T) {
	h, rig := newTestHandler(t)
	inStage, letGo := make(chan struct{}), make(chan error)
	h.stage = func(body io.Reader, tmp string, limit int64) (string, string, error) {
		inStage <- struct{}{}
		if err := <-letGo; err != nil {
			return "", "", &stagingError{err}
		}
		return stageUpload(body, tmp, limit)
	}
	push := func(version string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/demo?version="+version, strings.NewReader("the artifact"))
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("Authorization", "Bearer "+appToken(t))
		w := httptest.NewRecorder()
		if err := h.ServeHTTP(w, req, nil); err != nil {
			t.Error(err)
		}
		return w
	}
	held, release := rig.holdFetch(t)
	for _, tc := range []struct {
		version string
		staged  error
		code    int
	}{
		{"v8", errors.New("no space left on device"), http.StatusInternalServerError},
		{"v9", nil, http.StatusOK},
	} {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- push(tc.version) }()
		<-inStage
		s := rig.ma.status()
		d := s.Deploying
		if d == nil || d.Version != tc.version || d.By == "" || len(d.Phases) != 0 || s.Phase != "idle" {
			t.Fatalf("%s while its upload is staged: phase %q, deploying %+v", tc.version, s.Phase, d)
		}
		if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/7.tgz","version":"v7"}`); w.Code != http.StatusConflict {
			t.Fatalf("a deploy while %s uploads: %d %s", tc.version, w.Code, w.Body)
		}
		if again := rig.ma.status().Deploying; again == nil || again.Version != tc.version || !again.StartedAt.Equal(d.StartedAt) {
			t.Fatalf("the refused deploy changed deploying: %+v", again)
		}
		// The upload takes a while: the pipeline that follows keeps the
		// time the push was taken, and its record has its own.
		rig.clock.Advance(5 * time.Second)
		letGo <- tc.staged
		if tc.staged == nil {
			held()
			if in := rig.ma.status().Deploying; in == nil || !in.StartedAt.Equal(d.StartedAt) || len(in.Phases) == 0 {
				t.Errorf("%s in its pipeline: deploying %+v, taken at %v", tc.version, in, d.StartedAt)
			}
			release(nil)
		}
		w := <-done
		s = rig.ma.status()
		if w.Code != tc.code || s.Deploying != nil || s.Phase != "idle" {
			t.Fatalf("%s: %d %s, then deploying %+v", tc.version, w.Code, w.Body, s.Deploying)
		}
		if tc.staged != nil && s.LastDeploy != nil {
			t.Errorf("an upload not staged has an outcome: %+v", s.LastDeploy)
		}
		if tc.staged == nil && (s.LastDeploy == nil || s.LastDeploy.Version != tc.version || !s.LastDeploy.StartedAt.After(d.StartedAt)) {
			t.Errorf("the push's outcome: %+v, in flight from %v", s.LastDeploy, d.StartedAt)
		}
	}
}

// TestDeployingGoneFromAPushsResponses: no answer to a push holds its
// own deploying, whatever way it ends: refused before its upload (an
// app dir that is a link), failed in its pipeline, single or streamed.
// A push's pipeline returns into the handler, which writes before the
// push's deferred end runs, so this holds the pipeline's own end to
// clearing it.
func TestDeployingGoneFromAPushsResponses(t *testing.T) {
	h, rig := newTestHandler(t)
	push := func(version string, stream bool) string {
		req := httptest.NewRequest(http.MethodPost, "/demo?version="+version, strings.NewReader("the artifact"))
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("Authorization", "Bearer "+appToken(t))
		if stream {
			req.Header.Set("Accept", "application/x-ndjson")
		}
		w := &statusAtWrite{ResponseRecorder: httptest.NewRecorder(), ma: rig.ma}
		must(t, h.ServeHTTP(w, req, nil))
		// As its answer (a stream's last line) is written, nothing of
		// this push may still be shown, a body with no status included:
		// only its deferred end runs after.
		if w.shown != nil {
			t.Errorf("%s: %+v shown as its answer was written: %s", version, w.shown, w.Body)
		}
		lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
		return lines[len(lines)-1]
	}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
	d := rig.spec.dirs
	must(t, os.RemoveAll(d.tmp))
	must(t, os.Symlink(t.TempDir(), d.tmp))
	if body := push("v8", false); !strings.Contains(body, "refusing to deploy") || strings.Contains(body, `"deploying"`) {
		t.Errorf("a push refused before its upload: %s", body)
	}
	must(t, os.Remove(d.tmp))
	must(t, os.Mkdir(d.tmp, 0o750))
	rig.runner.setStartErr(errors.New("boom"))
	for _, stream := range []bool{false, true} {
		v := map[bool]string{false: "v9", true: "v10"}[stream]
		if body := push(v, stream); !strings.Contains(body, `"error":"start failed: boom"`) || strings.Contains(body, `"deploying"`) {
			t.Errorf("a push failed in its pipeline (stream %v): %s", stream, body)
		}
	}
	rig.runner.setStartErr(nil)
	// An upload that is not staged, each way: answered with no status
	// at all, so only the status at the write can show what it held.
	for i, err := range []error{errArtifactTooLarge, &stagingError{errors.New("no space left on device")}, errors.New("the body ended")} {
		h.stage = func(io.Reader, string, int64) (string, string, error) { return "", "", err }
		if body := push(fmt.Sprintf("v2%d", i), false); !strings.Contains(body, `"error":`) {
			t.Errorf("an upload not staged: %s", body)
		}
	}
	h.stage = nil
	if s := rig.ma.status(); s.Deploying != nil {
		t.Errorf("after: %+v", s.Deploying)
	}
}

// statusAtWrite is a recorder that reads the app's deploying as each
// part of the answer is written; shown is what the last write saw (a
// stream's last line, or the single answer).
type statusAtWrite struct {
	*httptest.ResponseRecorder
	ma    *managedApp
	shown *deployInFlight
}

func (w *statusAtWrite) WriteHeader(code int) {
	w.shown = w.ma.status().Deploying
	w.ResponseRecorder.WriteHeader(code)
}

func (w *statusAtWrite) Write(b []byte) (int, error) {
	w.shown = w.ma.status().Deploying
	return w.ResponseRecorder.Write(b)
}

// TestDeployingAPanicInStaging: the deferred end is the backstop for a
// way out that clears nothing itself; a panic in the upload's staging
// leaves no deploy shown and the lock free.
func TestDeployingAPanicInStaging(t *testing.T) {
	h, rig := newTestHandler(t)
	h.stage = func(io.Reader, string, int64) (string, string, error) { panic("staging") }
	req := httptest.NewRequest(http.MethodPost, "/demo?version=v1", strings.NewReader("the artifact"))
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the stage did not panic")
			}
		}()
		_ = h.ServeHTTP(httptest.NewRecorder(), req, nil)
	}()
	if d := rig.ma.status().Deploying; d != nil {
		t.Errorf("after a panic in staging: %+v", d)
	}
	if !rig.ma.deployMu.TryLock() {
		t.Fatal("the deploy lock is still held")
	}
	rig.ma.deployMu.Unlock()
}

// statusAtEvents reads the app's status at each event a deploy's end
// tells the notifier, which happen after its outcome is set: deploying
// must be gone by then, not left for a later step to clear.
type statusAtEvents struct {
	ma   *managedApp
	t    *testing.T
	mu   sync.Mutex
	seen int
}

func (n *statusAtEvents) Lifecycle(ev LifecycleEvent) {
	switch ev.Name {
	case EventDeploySucceeded, EventDeployFailed, EventDeployRefused, EventRollback, EventDeployEnded:
	default:
		return
	}
	s := n.ma.status()
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seen++
	if s.Deploying != nil || s.Phase != "idle" {
		n.t.Errorf("at %s of %s: phase %q, deploying %+v", ev.Name, ev.Release, s.Phase, s.Deploying)
	}
}

// TestDeployingGoneWhenTheEndIsTold: at every event of a deploy's end,
// for a success, a failure, a refusal, a rollback and a relaunch, the
// status shows its outcome and no deploy in flight.
func TestDeployingGoneWhenTheEndIsTold(t *testing.T) {
	rig := newTestRig(t)
	n := &statusAtEvents{ma: rig.ma, t: t}
	rig.ma.setNotifier(n)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1.tgz", version: "v1"}))
	rig.runner.setStartErr(errors.New("boom"))
	if err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2.tgz", version: "v2"}); err == nil {
		t.Fatal("v2 started")
	}
	rig.runner.setStartErr(nil)
	if err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/1.tgz", version: "v1"}); err == nil {
		t.Fatal("v1 again was not refused")
	}
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/3.tgz", version: "v3"}))
	must(t, rig.ma.Deploy(ctx, deployRequest{version: "v1", rollback: true}))
	must(t, rig.ma.Deploy(ctx, deployRequest{version: "v1", rollback: true}))
	if n.seen < 12 {
		t.Errorf("%d events seen, want two for each of six deploys", n.seen)
	}
}

// TestDeployingGoneBeforeTheLockIsLetGo: a deploy that takes the lock
// the moment the last one lets go of it keeps its own record: the last
// one's end clears what it showed before it lets go, never after, when
// it would clear the next one's. The next is made holder inside the
// last one's unlock (the unlocked seam), before the last one returns,
// for each way a deploy ends: a pull, a failure, a push not staged.
func TestDeployingGoneBeforeTheLockIsLetGo(t *testing.T) {
	h, rig := newTestHandler(t)
	ctx := context.Background()
	var letGoWith *inFlight
	took := false
	rig.ma.unlocked = func() {
		if !rig.ma.deployMu.TryLock() {
			return
		}
		rig.ma.mu.Lock()
		letGoWith = rig.ma.inFlight
		rig.ma.mu.Unlock()
		took = true
		rig.ma.beginDeploy("next", "", rig.clock.Now())
	}
	h.stage = func(io.Reader, string, int64) (string, string, error) {
		return "", "", &stagingError{errors.New("no space left on device")}
	}
	for _, tc := range []struct {
		name string
		run  func()
	}{
		{"a pull", func() { must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1.tgz", version: "v1"})) }},
		{"a failure", func() {
			rig.runner.setStartErr(errors.New("boom"))
			defer rig.runner.setStartErr(nil)
			if rig.ma.Deploy(ctx, deployRequest{url: "https://x/2.tgz", version: "v2"}) == nil {
				t.Fatal("v2 started")
			}
		}},
		{"a push not staged", func() {
			req := httptest.NewRequest(http.MethodPost, "/demo?version=v3", strings.NewReader("the artifact"))
			req.Header.Set("Content-Type", "application/gzip")
			req.Header.Set("Authorization", "Bearer "+appToken(t))
			must(t, h.ServeHTTP(httptest.NewRecorder(), req, nil))
		}},
	} {
		took, letGoWith = false, nil
		tc.run()
		if !took {
			t.Fatalf("%s: the next did not take the lock", tc.name)
		}
		rig.ma.mu.Lock()
		f := rig.ma.inFlight
		rig.ma.mu.Unlock()
		if letGoWith != nil || f == nil || f.version != "next" {
			t.Errorf("%s: let go with %+v shown; the next deploy's record is %+v", tc.name, letGoWith, f)
		}
		rig.ma.endInFlight()
		rig.ma.deployMu.Unlock() // as the next holder
	}
}

// TestDeployingOneStepToItsOutcome: a deploy's end sets its outcome and
// takes its deploying away in one step, so a read never finds both (the
// outcome beside the deploy it is of) or neither (the deploy gone before
// its outcome is there). The stepped seam looks inside that step, for
// every kind of deploy: the deploy was still shown as it began, and is
// not as it ends, beside its own outcome.
func TestDeployingOneStepToItsOutcome(t *testing.T) {
	h, rig := newTestHandler(t)
	type step struct {
		began, ends *inFlight
		outcome     *deployResult
	}
	var steps []step
	rig.ma.stepped = func(began, ends *inFlight, outcome *deployResult) {
		steps = append(steps, step{began, ends, outcome})
	}
	ctx := context.Background()
	push := func(version string) {
		req := httptest.NewRequest(http.MethodPost, "/demo?version="+version, strings.NewReader("the artifact"))
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("Authorization", "Bearer "+appToken(t))
		must(t, h.ServeHTTP(httptest.NewRecorder(), req, nil))
	}
	for _, tc := range []struct {
		version string
		run     func()
	}{
		{"v1", func() { must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1.tgz", version: "v1"})) }},
		{"v2", func() {
			rig.runner.setStartErr(errors.New("boom"))
			defer rig.runner.setStartErr(nil)
			_ = rig.ma.Deploy(ctx, deployRequest{url: "https://x/2.tgz", version: "v2"})
		}},
		{"v1", func() { _ = rig.ma.Deploy(ctx, deployRequest{url: "https://x/1.tgz", version: "v1"}) }}, // refused: running, no pin
		{"v3", func() { push("v3") }},
		{"v1", func() { must(t, rig.ma.Deploy(ctx, deployRequest{version: "v1", rollback: true})) }},
		{"v1", func() { must(t, rig.ma.Deploy(ctx, deployRequest{version: "v1", rollback: true})) }}, // a relaunch
	} {
		steps = nil
		tc.run()
		if len(steps) != 1 {
			t.Fatalf("%s: %d steps to the outcome, want one", tc.version, len(steps))
		}
		st := steps[0]
		if st.began == nil || st.began.version != tc.version || st.ends != nil || st.outcome == nil || st.outcome.Version != tc.version {
			t.Errorf("%s: the step began with %+v shown and ends with %+v, outcome %+v", tc.version, st.began, st.ends, st.outcome)
		}
	}
}

// TestDeployingBeginIsUnderTheLock: beginDeploy writes the record under
// the lock status reads it under. The writer here syncs with the reader
// in no other way, so under -race a write outside the lock is reported,
// and the record read is whole: one call's version, deployer and time.
func TestDeployingBeginIsUnderTheLock(t *testing.T) {
	rig := newTestRig(t)
	if !rig.ma.deployMu.TryLock() {
		t.Fatal("the deploy lock is held")
	}
	defer rig.ma.deployMu.Unlock()
	at := time.Unix(1_800_000_000, 0)
	go rig.ma.beginDeploy("v1", "local:/k.pub", at)
	var d *deployInFlight
	for d == nil {
		d = rig.ma.status().Deploying
	}
	if d.Version != "v1" || d.By != "local:/k.pub" || !d.StartedAt.Equal(at) {
		t.Errorf("a torn record: %+v", d)
	}
	rig.ma.endInFlight()
}
