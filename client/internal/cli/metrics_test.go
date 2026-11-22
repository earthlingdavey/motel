package cli

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// daySeries is a day of blog's numbers: memory, CPU with a gap, the
// request panels, and a panel this client does not know.
func daySeries() wire.Series {
	end := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	t := func(back int) float64 { return float64(end.Add(-time.Duration(back) * 5 * time.Minute).Unix()) }
	return wire.Series{Store: wire.StoreOn, StepSeconds: 300, End: &end, Panels: []wire.Panel{
		{Name: wire.PanelMemory, Unit: wire.UnitBytes, Points: [][2]float64{{t(287), 150 << 20}, {t(100), 306 << 20}, {t(0), 190840832}}},
		{Name: wire.PanelCPU, Unit: wire.UnitCores, Points: [][2]float64{{t(10), 0.0004}, {t(0), 0.25}}},
		{Name: wire.PanelRequests, Unit: wire.UnitPerSecond, Points: [][2]float64{{t(0), 2.5}}},
		{Name: wire.Panel5xx, Unit: wire.UnitShare, Points: [][2]float64{{t(0), 0.0123}}},
		{Name: wire.PanelP95, Unit: wire.UnitMs, Points: [][2]float64{{t(0), 80.1}}},
		{Name: "disk_bytes", Unit: "bytes", Points: [][2]float64{{t(0), 1}}},
	}}
}

// motel metrics: the box's answer is --json on a read; the human output
// is the client's words and numbers, a line a panel, the box's words
// only as checked words; a box with no metrics line says so and exits
// 0; one whose store did not answer, or that says a word for it the
// client does not know, exits 3 with the client's own sentence; a
// window not in the set is refused before anything is sent; the read is
// a person's key's alone.
func TestMetrics(t *testing.T) {
	fb := newFakeBox(t)
	fb.series = daySeries()
	body := func(target string) string { return string(withNewline(termtext.JSON(fb.bodies[target]))) }
	r := runKey(t, fb, nil, "metrics", "blog", "--range", "24h", "--json")
	if r.code != 0 || r.stdout != body("/_motel/apps/blog/series?range=24h") || r.stderr != "" {
		t.Fatalf("--json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(fb.auth[len(fb.auth)-1], "Motel m1.") {
		t.Errorf("Authorization %q, want a person's token", fb.auth[len(fb.auth)-1])
	}
	r = runKey(t, fb, nil, "metrics", "blog", "--range", "24h")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if bad := unsafeOutput.FindString(r.stdout); bad != "" || !utf8.ValidString(r.stdout) {
		t.Errorf("the output holds %q, or is not UTF-8:\n%s", bad, r.stdout)
	}
	const want = `127.0.0.1 holds blog's numbers over 24h, a point every 5 m, to 2026-11-19 14:00 UTC:
memory     min 150.0 MiB (11-18 14:05 UTC)  max 306.0 MiB (05:40 UTC)  newest 182.0 MiB (14:00 UTC)  3 of 289 steps
cpu        min 0.0004 cores (13:10 UTC)  max 0.2500 cores (14:00 UTC)  newest 0.2500 cores (14:00 UTC)  2 of 289 steps
requests   min 2.50/s (14:00 UTC)  max 2.50/s (14:00 UTC)  newest 2.50/s (14:00 UTC)  1 of 289 steps
5xx share  min 1.2% (14:00 UTC)  max 1.2% (14:00 UTC)  newest 1.2% (14:00 UTC)  1 of 289 steps had requests
p95        min 80.1 ms (14:00 UTC)  max 80.1 ms (14:00 UTC)  newest 80.1 ms (14:00 UTC)  1 of 289 steps had requests
other      the box's disk_bytes in bytes: not one this client draws
`
	if r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
	// The default window is the box's; and an app with no request
	// panel says why.
	end := daySeries().End
	fb.series = wire.Series{Store: wire.StoreOn, StepSeconds: 60, End: end, Panels: []wire.Panel{
		{Name: wire.PanelMemory, Unit: wire.UnitBytes, Points: [][2]float64{{float64(end.Unix()), 1 << 20}}}}}
	if r := runKey(t, fb, nil, "metrics", "blog"); r.code != 0 || !strings.Contains(r.stdout, "over 1h, a point every 1 m") ||
		!strings.Contains(r.stdout, "requests   none in the window: the box counts requests only for an app behind observe") {
		t.Errorf("1h, no requests: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// Without a requests panel, the 5xx share's steps are counted as any
	// panel's; a highest and lowest are written alike until they differ.
	t1 := func(back int) float64 { return float64(end.Add(-time.Duration(back) * time.Minute).Unix()) }
	fb.series.Panels = append(fb.series.Panels, wire.Panel{Name: wire.Panel5xx, Unit: wire.UnitShare, Points: [][2]float64{{t1(9), 0.0123}, {t1(1), 0.0121}}})
	if r := runKey(t, fb, nil, "metrics", "blog"); r.code != 0 || !strings.Contains(r.stdout, "5xx share  min 1.21% (13:59 UTC)  max 1.23% (13:51 UTC)  newest 1.21% (13:59 UTC)  2 of 61 steps\n") {
		t.Errorf("5xx, no requests: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// A requests panel with no point: the same.
	fb.series.Panels = append(fb.series.Panels, wire.Panel{Name: wire.PanelRequests, Unit: wire.UnitPerSecond, Points: [][2]float64{}})
	if r := runKey(t, fb, nil, "metrics", "blog"); r.code != 0 || !strings.Contains(r.stdout, "newest 1.21% (13:59 UTC)  2 of 61 steps\n") {
		t.Errorf("5xx beside no requests point: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// Beside a requests panel, the count is of the steps it has a rate
	// above 0 at, a 5xx point there or not (every request aborted), and
	// the 5xx share's points beside it when they differ.
	fb.series.Panels[len(fb.series.Panels)-1].Points = [][2]float64{{t1(9), 0.05}, {t1(5), 0.02}, {t1(1), 0.1}, {t1(0), 0}}
	if r := runKey(t, fb, nil, "metrics", "blog"); r.code != 0 || !strings.Contains(r.stdout, "newest 1.21% (13:59 UTC)  3 of 61 steps had requests, 2 with a point\n") {
		t.Errorf("5xx beside requests: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// No point of the app at all: no cause said for the request panels.
	fb.series.Panels = []wire.Panel{}
	if r := runKey(t, fb, nil, "metrics", "blog"); r.code != 0 || strings.Contains(r.stdout, "observe") || !strings.Contains(r.stdout, "none: the store holds no point of blog") {
		t.Errorf("no point: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	fb.series.Panels = []wire.Panel{{Name: wire.PanelMemory, Unit: wire.UnitBytes, Points: [][2]float64{{float64(end.Unix()), 1 << 20}}}}
	// Beside a panel this client does not know (one the box's filter may
	// have renamed), no cause is said for the request panels' absence.
	fb.series.Panels = append(fb.series.Panels, wire.Panel{Name: "requests_hidden", Unit: "per_second", Points: [][2]float64{}})
	if r := runKey(t, fb, nil, "metrics", "blog"); r.code != 0 || strings.Contains(r.stdout, "observe") {
		t.Errorf("an unknown panel: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	for _, c := range []struct {
		store, line string
		code        int
	}{
		{wire.StoreOff, "127.0.0.1 keeps no numbers of its apps: its monitor block has no metrics line\n", 0},
		{wire.StoreUnanswered, "", 3},
		{"later", "", 3},
	} {
		fb.series = wire.Series{Store: c.store, Panels: []wire.Panel{}}
		r := runKey(t, fb, nil, "metrics", "blog", "--range", "7d")
		if r.code != c.code || r.stdout != c.line || (c.code == 0) != (r.stderr == "") || strings.Count(r.stderr, "\n") > 1 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.store, r.code, r.stdout, r.stderr)
		}
	}
	// A window the box did not answer for: not moteld's answer.
	fb.series = daySeries()
	fb.series.Window = "14d"
	if r := runKey(t, fb, nil, "metrics", "blog", "--range", "24h"); r.code != 3 || !strings.Contains(r.stderr, "not moteld's answer") {
		t.Errorf("another window: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	n := len(fb.auth)
	if r := runKey(t, fb, nil, "metrics", "blog", "--range", "2h"); r.code != 2 || len(fb.auth) != n || !strings.Contains(r.stderr, `--range "2h" is not one of 1h, 24h, 7d, 14d`) {
		t.Errorf("--range 2h: exit %d, stderr %q, %d requests", r.code, r.stderr, len(fb.auth)-n)
	}
	if r := runKey(t, fb, map[string]string{"MOTEL_TOKEN": "x.y.z"}, "metrics", "blog"); r.code != 2 || len(fb.auth) != n {
		t.Errorf("MOTEL_TOKEN: exit %d, stderr %q", r.code, r.stderr)
	}
}
