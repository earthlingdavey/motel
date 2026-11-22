package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/render"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// One fixture through the views and the renderer, in colour: what each
// field becomes. Pinned by a golden.
func TestViewsAndRender(t *testing.T) {
	var out bytes.Buffer
	color := render.Style{Color: true}
	start := time.Date(2026, 11, 13, 17, 39, 2, 0, time.UTC)
	section := func(name string) { out.WriteString("== " + name + "\n") }

	section("in backoff, its current version not first: no mark on it")
	if err := render.WriteStatus(&out, color, statusView(wire.Status{
		App: "blog", Phase: "idle", CurrentVersion: "v2", Running: true,
		Watchdog:          &wire.Watchdog{State: "backoff", Failures: 2, RestartsInWindow: 3, LastFailure: "health check timed out"},
		LastDeploy:        &wire.DeployResult{Version: "v2", Status: "succeeded", Note: "v2 was running: relaunched from the release on disk", FinishedAt: start},
		AvailableVersions: []string{"v3", "v2"},
	}, "blog")); err != nil {
		t.Fatal(err)
	}
	section("running, first: (running) on it alone; a deploy in flight")
	if err := render.WriteStatus(&out, color, statusView(wire.Status{App: "blog", Phase: "soaking", CurrentVersion: "v2", Running: true, AvailableVersions: []string{"v2", "v1"},
		Deploying: &wire.DeployInFlight{Version: "v3", By: "ci", StartedAt: start}}, "blog")); err != nil {
		t.Fatal(err)
	}
	section("deploys: a zone dropped to UTC, anything but a time as (unreadable)")
	if err := render.WriteDeploys(&out, summaryViews([]wire.DeploySummary{
		{Version: "v3", Status: "failed", Phase: "soaking", FinishedAt: json.RawMessage(`"2026-11-14T09:12:30+02:00"`)},
		{Version: "v2", Status: "succeeded", FinishedAt: json.RawMessage(`null`)},
		{Version: "v1", Status: "succeeded", FinishedAt: json.RawMessage(`5`)},
	})); err != nil {
		t.Fatal(err)
	}
	section("a deploy with no finish: no duration; a cut log tail")
	if err := render.WriteDeploy(&out, color, termtext.From("blog"), deployView(wire.DeployResult{
		Version: "v3", Status: "failed", Phase: "starting", StartedAt: start,
		Detail: &wire.DeployDetail{LogTail: []wire.AppText{"last line"}, LogTailTruncated: true},
	})); err != nil {
		t.Fatal(err)
	}
	golden(t, "views-and-render", out.String())
}

// TestStatusSaysTheState: motel status says the app's state first, as
// the TUI's row does (running, throttled, stopped, backoff, not
// deployed), the versions line marks the current version with that
// same state, and a deploy in flight is on a line of its own: its
// version, its phase, when it started and by whom. The trial saw
// "example  idle 537749a7608c" for an app that ran, and "preparing
// 537749a7608c" while another version was being prepared; a stopped
// app's versions line then said its version was running.
func TestStatusSaysTheState(t *testing.T) {
	started := time.Date(2026, 11, 19, 4, 32, 18, 0, time.UTC)
	running := wire.Status{App: "blog", Phase: "idle", CurrentVersion: "537749a7608c", Running: true, PID: 483463, AvailableVersions: []string{"537749a7608c"}}
	deploying := running
	deploying.Phase = "preparing"
	deploying.Deploying = &wire.DeployInFlight{Version: "tui1-fail-1", By: "local:/etc/motel/tui1-example.pub sub=tui1-trial", StartedAt: started,
		Phases: []wire.PhaseTiming{{Name: "extracting", StartedAt: started, Seconds: 0.001}, {Name: "preparing", StartedAt: started, Seconds: 6.8}}}
	stopped := running
	stopped.Running = false
	throttled := stopped
	throttled.Watchdog = &wire.Watchdog{State: "throttled", RestartsInWindow: 5}
	backoff := running
	backoff.Watchdog = &wire.Watchdog{State: "backoff", Failures: 2}
	none := wire.Status{App: "blog", Phase: "idle", AvailableVersions: []string{"537749a7608c"}}
	for _, c := range []struct {
		name string
		st   wire.Status
		want string
	}{
		{"running", running, "blog  running 537749a7608c  pid 483463\nversions     537749a7608c (running)\n"},
		{"a deploy in flight", deploying, "blog  running 537749a7608c  pid 483463\n" +
			"deploying    tui1-fail-1 preparing, started 2026-11-19 04:32 UTC  by local:/etc/motel/tui1-example.pub sub=tui1-trial\n" +
			"versions     537749a7608c (running)\n"},
		{"stopped", stopped, "blog  stopped 537749a7608c  pid 483463\nversions     537749a7608c (stopped)\n"},
		{"throttled", throttled, "blog  throttled 537749a7608c  pid 483463\nwatchdog     throttled, 5 restarts in its window\nversions     537749a7608c (throttled)\n"},
		{"backoff", backoff, "blog  backoff 537749a7608c  pid 483463\nwatchdog     backoff, 2 failed checks in a row\nversions     537749a7608c (backoff)\n"},
		{"not deployed", none, "blog  not deployed\nversions     537749a7608c\n"},
	} {
		fb := newFakeBox(t)
		fb.status = c.st
		if r := run(t, fb, deployToken, "status", appURL(fb, "blog")); r.code != 0 || r.stdout != c.want {
			t.Errorf("%s: exit %d, stdout\n%s\nwant\n%s(stderr %q)", c.name, r.code, r.stdout, c.want, r.stderr)
		}
	}
}
