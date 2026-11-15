package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/render"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// One fixture through the views and the renderer, in colour: what each
// field becomes. Pinned by a golden.
func TestViewsAndRender(t *testing.T) {
	var out bytes.Buffer
	color := render.Style{Color: true}
	start := time.Date(2026, 11, 13, 17, 39, 2, 0, time.UTC)
	section := func(name string) { out.WriteString("== " + name + "\n") }

	section("crashed, its running version not first: no (running)")
	if err := render.WriteStatus(&out, color, statusView(wire.Status{
		App: "blog", Phase: "crashed", CurrentVersion: "v2",
		Watchdog:          &wire.Watchdog{State: "unhealthy", Failures: 2, RestartsInWindow: 3, LastFailure: "health check timed out"},
		LastDeploy:        &wire.DeployResult{Version: "v2", Status: "succeeded", Note: "v2 was running: relaunched from the release on disk", FinishedAt: start},
		AvailableVersions: []string{"v3", "v2"},
	})); err != nil {
		t.Fatal(err)
	}
	section("running, first: (running) on it alone")
	if err := render.WriteStatus(&out, color, statusView(wire.Status{App: "blog", Phase: "running", CurrentVersion: "v2", AvailableVersions: []string{"v2", "v1"}})); err != nil {
		t.Fatal(err)
	}
	section("deploys: a zone dropped to UTC, null as nothing, a number as itself")
	if err := render.WriteDeploys(&out, summaryViews([]wire.DeploySummary{
		{Version: "v3", Status: "failed", Phase: "health_gate", FinishedAt: json.RawMessage(`"2026-11-14T09:12:30+02:00"`)},
		{Version: "v2", Status: "succeeded", FinishedAt: json.RawMessage(`null`)},
		{Version: "v1", Status: "succeeded", FinishedAt: json.RawMessage(`5`)},
	})); err != nil {
		t.Fatal(err)
	}
	section("a deploy with no finish: no duration; a cut log tail")
	if err := render.WriteDeploy(&out, color, termtext.From("blog"), deployView(wire.DeployResult{
		Version: "v3", Status: "failed", Phase: "start", StartedAt: start,
		Detail: &wire.DeployDetail{LogTail: []string{"last line"}, LogTailTruncated: true},
	})); err != nil {
		t.Fatal(err)
	}
	golden(t, "views-and-render", out.String())
}
