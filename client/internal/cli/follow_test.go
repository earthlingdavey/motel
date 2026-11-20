package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// followBox answers GET /blog with each of its statuses in turn, the
// last again once they run out; a nil one is a 500.
type followBox struct {
	srv    *httptest.Server
	mu     sync.Mutex
	steps  []*wire.Status
	n      int
	bodies [][]byte
}

func newFollowBox(t *testing.T, steps ...*wire.Status) *followBox {
	t.Helper()
	fb := &followBox{steps: steps}
	fb.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		st := fb.steps[min(fb.n, len(fb.steps)-1)]
		fb.n++
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if st == nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"busy"}` + "\n"))
			return
		}
		b := encode(st)
		fb.mu.Lock()
		fb.bodies = append(fb.bodies, b)
		fb.mu.Unlock()
		_, _ = w.Write(b)
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func runFollow(t *testing.T, fb *followBox, sl *sleeps, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	env := map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_URL": fb.srv.URL + "/blog"}
	e := Env{
		Getenv:    func(k string) string { return env[k] },
		Stdout:    &out,
		Stderr:    &errOut,
		Dir:       t.TempDir(),
		Home:      t.TempDir(),
		Now:       time.Now,
		Sleep:     sl.sleep,
		Run:       noPrograms{},
		Version:   "v0.0.0-test",
		Transport: fb.srv.Client().Transport,
	}
	code := Main(context.Background(), append([]string{"status", fb.srv.URL + "/blog", "--follow"}, args...), e)
	return result{code, out.String(), errOut.String()}
}

var t1 = time.Date(2026, 11, 19, 14, 12, 0, 0, time.UTC)

func phasesAt(names ...string) []wire.PhaseTiming {
	var out []wire.PhaseTiming
	for i, n := range names {
		out = append(out, wire.PhaseTiming{Name: n, StartedAt: t1.Add(time.Duration(i) * time.Second), Seconds: 1})
	}
	return out
}

// flying is blog running v42 with v43's deploy in flight, its phases
// so far names.
func flying(names ...string) *wire.Status {
	return &wire.Status{App: "blog", Phase: names[len(names)-1], CurrentVersion: "v42", Running: true, AvailableVersions: []string{"v42"},
		Deploying:  &wire.DeployInFlight{Version: "v43", By: "ci", StartedAt: t1, Phases: phasesAt(names...)},
		LastDeploy: &wire.DeployResult{Version: "v42", Status: "succeeded", StartedAt: t1.Add(-time.Hour), FinishedAt: t1.Add(-time.Hour + 30*time.Second)}}
}

// landed is blog after v43's deploy ended as status says, its phases
// names; cur serves.
func landed(status, cur string, names ...string) *wire.Status {
	d := &wire.DeployResult{Version: "v43", Status: status, By: "ci", StartedAt: t1.Add(time.Second), FinishedAt: t1.Add(time.Minute), Phases: phasesAt(names...)}
	if status == "failed" {
		d.Phase, d.Error = names[len(names)-1], "the health check answered 500"
		d.Detail = &wire.DeployDetail{Exit: "exit status 3", LogTail: []string{"pg: connection refused"}}
	}
	return &wire.Status{App: "blog", Phase: "idle", CurrentVersion: cur, Running: true, AvailableVersions: []string{cur}, LastDeploy: d,
		Deploys: []wire.DeploySummary{{Version: "v43", Status: status, Phase: d.Phase}}}
}

// TestFollowADeploy (#580): status --follow reads every 5 s while the
// deploy it found in flight runs, prints each phase once, with the
// box's time, the last ones from the outcome's own list, then the
// outcome's record and what serves, and exits by the outcome.
func TestFollowADeploy(t *testing.T) {
	fb := newFollowBox(t, flying("downloading"), flying("downloading", "preparing", "soaking"),
		flying("downloading", "preparing", "soaking"), landed("failed", "v42", "downloading", "preparing", "soaking"))
	var sl sleeps
	r := runFollow(t, fb, &sl)
	if r.code != 9 {
		t.Fatalf("exit %d, want 9; stderr %q", r.code, r.stderr)
	}
	if want := "motel: the deploy of v43 failed in soaking; now v42 (running)\n"; r.stderr != want {
		t.Errorf("stderr %q, want %q", r.stderr, want)
	}
	for _, want := range []string{
		"deploying    v43 downloading, started 2026-11-19 14:12 UTC  by ci\n",
		"\nfollowing the deploy of v43, read every 5 s (ctrl-c stops following, not the deploy)\n" +
			"14:12:00 UTC  downloading\n14:12:01 UTC  preparing\n14:12:02 UTC  soaking\n\nblog v43  failed in soaking  2026-11-19 14:12:01 to 14:13:00 UTC (59s)\n",
		"error   the health check answered 500\n",
		"log tail (1 lines):\n  pg: connection refused\nnow     v42 (running)\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if len(sl.d) != 3 {
		t.Errorf("slept %d times, want 3 (four reads): %v", len(sl.d), sl.d)
	}
	for _, d := range sl.d {
		if d != FollowEvery {
			t.Errorf("slept %s between reads, want %s", d, FollowEvery)
		}
	}
}

// TestFollowEnds: each way a follow ends, and its exit.
func TestFollowEnds(t *testing.T) {
	for _, c := range []struct {
		name   string
		steps  []*wire.Status
		json   bool
		code   int
		stderr string
		stdout string
	}{
		{"succeeded", []*wire.Status{flying("soaking"), landed("succeeded", "v43", "soaking", "promoting", "draining", "stopping_old")}, false, 0, "",
			"14:12:00 UTC  soaking\n14:12:01 UTC  promoting\n14:12:02 UTC  draining\n14:12:03 UTC  stopping_old\n\nblog v43  succeeded"},
		{"none in flight", []*wire.Status{landed("succeeded", "v43", "soaking")}, false, 0, "motel: no deploy of blog is in flight: nothing to follow.\n", "blog  running v43\n"},
		// The box's last_deploy is still the deploy before: a push whose
		// upload was never staged.
		{"no outcome", []*wire.Status{flying("soaking"), {App: "blog", Phase: "idle", CurrentVersion: "v42", Running: true, AvailableVersions: []string{"v42"},
			LastDeploy: flying("soaking").LastDeploy}}, false, 3,
			"motel: the deploy of v43 ended, and the box gives no outcome of it: a push whose upload was never staged ends so, as does one whose outcome another deploy's replaced before this read\n", ""},
		// A second deploy of the same version, taken after the first.
		{"another deploy", []*wire.Status{flying("soaking"), func() *wire.Status {
			s := flying("soaking")
			s.Deploying.StartedAt = t1.Add(2 * time.Minute)
			return s
		}()}, false, 3, "motel: the deploy of v43 ended, and the box gives no outcome", ""},
		// A later deploy of v43, begun after the one followed ended and
		// ended before the read: its phases are not the ones followed.
		{"a later deploy's outcome", []*wire.Status{flying("soaking"), func() *wire.Status {
			s := landed("succeeded", "v43", "soaking")
			s.LastDeploy.Phases[0].StartedAt = t1.Add(30 * time.Second)
			return s
		}()}, false, 3, "motel: the deploy of v43 ended, and the box gives no outcome", ""},
		// Seen only in its upload, with no phase: matched by its end. The
		// box's last deploy is an earlier one of v43, ended before it.
		{"an upload's, with an earlier v43's outcome", []*wire.Status{func() *wire.Status {
			s := flying("soaking")
			s.Phase, s.Deploying.Phases = "idle", []wire.PhaseTiming{}
			s.LastDeploy = landed("failed", "v42", "soaking").LastDeploy
			s.LastDeploy.FinishedAt = t1.Add(-time.Second)
			return s
		}(), func() *wire.Status {
			s := landed("failed", "v42", "soaking")
			s.LastDeploy.FinishedAt = t1.Add(-time.Second)
			return s
		}()}, false, 3, "motel: the deploy of v43 ended, and the box gives no outcome", ""},
		// Seen first in its upload, then in a phase: a later deploy of
		// v43 that ended before the read does not begin with that phase.
		{"seen in a phase after its upload", []*wire.Status{func() *wire.Status {
			s := flying("soaking")
			s.Phase, s.Deploying.Phases = "idle", []wire.PhaseTiming{}
			return s
		}(), flying("soaking"), func() *wire.Status {
			s := landed("succeeded", "v43", "soaking")
			s.LastDeploy.Phases[0].StartedAt = t1.Add(30 * time.Second)
			return s
		}()}, false, 3, "motel: the deploy of v43 ended, and the box gives no outcome", ""},
		{"an older box", []*wire.Status{{App: "blog", Phase: "soaking", CurrentVersion: "v42", Running: true, AvailableVersions: []string{"v42"}}}, false, 3,
			"motel: blog is in soaking, and the box names no deploy in flight beside it (a moteld before v0.8.0): there is no deploy to follow\n", ""},
		{"a read fails", []*wire.Status{flying("soaking"), nil}, false, 3, "(following stopped before the deploy ended, which may still run)\n", ""},
		{"json, failed", []*wire.Status{flying("soaking"), landed("failed", "v42", "soaking")}, true, 9, "motel: the deploy of v43 failed in soaking; now v42 (running)\n", "last"},
		{"json, succeeded", []*wire.Status{flying("soaking"), landed("succeeded", "v43", "soaking")}, true, 0, "", "last"},
		{"json, none in flight", []*wire.Status{landed("succeeded", "v43", "soaking")}, true, 0, "", "last"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fb := newFollowBox(t, c.steps...)
			var args []string
			if c.json {
				args = []string{"--json"}
			}
			r := runFollow(t, fb, &sleeps{}, args...)
			if r.code != c.code {
				t.Fatalf("exit %d, want %d; stderr %q", r.code, c.code, r.stderr)
			}
			if !strings.Contains(r.stderr, c.stderr) || c.stderr == "" && r.stderr != "" {
				t.Errorf("stderr %q, want %q", r.stderr, c.stderr)
			}
			if c.stdout == "last" {
				// The last status read, alone: the box's own answer.
				sameJSON(t, r.stdout, string(fb.bodies[len(fb.bodies)-1]))
				return
			}
			if !strings.Contains(r.stdout, c.stdout) {
				t.Errorf("stdout lacks %q:\n%s", c.stdout, r.stdout)
			}
		})
	}
}
