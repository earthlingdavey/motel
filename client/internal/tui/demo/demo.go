// Package demo is a box made of fixtures: the TUI's --demo, which
// shows sample data with no box, and the fake client its tests run
// against. Its fixtures are bodies as a box writes them, with times
// written as "@-12m" (twelve minutes before the fake's clock at its
// first read, or, in a body a test set, when it was set) so that ages
// read as they would, and grow as a box's do.
package demo

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

//go:embed fixtures
var fixtures embed.FS

// Host is the box the fixtures are of.
const Host = "deploy.example.com"

// Fake is model.Client over the fixtures. A test sets Errs to make a
// read fail, and reads Calls to see what was asked and when.
type Fake struct {
	Now func() time.Time
	// Delay is how long each read takes.
	Delay time.Duration

	mu sync.Mutex
	// Errs makes a read fail: keyed "whoami", "apps", "box", "backups", "box-alerts", an app's
	// name, "<app>@<version>", "alerts:<app>", or "series:<app>".
	Errs map[string]error
	// Bodies replaces a fixture's body, keyed by its path in fixtures/.
	Bodies map[string]string
	Calls  []Call
	// anchor is the fake's clock at its first read, which the fixtures'
	// times are read against, and set when each body set was set, which
	// its times are: a box's times stay put from one read to the next,
	// so the deploy in flight is the same deploy at each.
	anchor time.Time
	set    map[string]time.Time
}

// Call is one read the fake answered.
type Call struct {
	Job model.Job
	At  time.Time
}

// Set makes the read keyed k fail with err, or answer again with nil.
func (f *Fake) Set(k string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Errs == nil {
		f.Errs = map[string]error{}
	}
	if err == nil {
		delete(f.Errs, k)
		return
	}
	f.Errs[k] = err
}

// Body replaces the fixture at path with body.
func (f *Fake) Body(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Bodies == nil {
		f.Bodies = map[string]string{}
	}
	f.Bodies[path] = body
	if f.set == nil {
		f.set = map[string]time.Time{}
	}
	if f.Now != nil {
		f.set[path] = f.Now()
	}
}

var token = regexp.MustCompile(`"@(-?[0-9]+)([smhd])"`)

var units = map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}

// load reads a fixture, makes its times real, and decodes it into v.
func (f *Fake) load(ctx context.Context, j model.Job, key, path string, v any) ([]byte, error) {
	if f.Delay > 0 {
		t := time.NewTimer(f.Delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	f.mu.Lock()
	now := f.Now()
	f.Calls = append(f.Calls, Call{Job: j, At: now})
	if f.anchor.IsZero() {
		f.anchor = now
	}
	err := f.Errs[key]
	body, replaced := f.Bodies[path]
	anchor := f.anchor
	if at, ok := f.set[path]; ok && replaced {
		anchor = at
	}
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !replaced {
		b, err := fs.ReadFile(fixtures, "fixtures/"+path)
		if err != nil {
			return nil, &model.Failure{Kind: model.FailNotFound, Message: Host + ` answered 404; the box says: "no such record"`}
		}
		body = string(b)
	}
	body = token.ReplaceAllStringFunc(body, func(m string) string {
		p := token.FindStringSubmatch(m)
		n, _ := strconv.Atoi(p[1])
		return `"` + anchor.Add(time.Duration(n)*units[p[2]]).UTC().Format(time.RFC3339) + `"`
	})
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return nil, fmt.Errorf("fixture %s: %w", path, err)
	}
	return []byte(body), nil
}

// WhoAmI answers whoami.json.
func (f *Fake) WhoAmI(ctx context.Context) (model.Got[wire.WhoAmI], error) {
	var g model.Got[wire.WhoAmI]
	b, err := f.load(ctx, model.Job{Kind: model.KWhoAmI}, "whoami", "whoami.json", &g.V)
	g.Body = b
	return g, err
}

// Apps answers apps.json, whose names it takes as the TUI does,
// as client.AppNames does: the fake stands in for the client package,
// whose import the TUI's packages may not have.
func (f *Fake) Apps(ctx context.Context) (model.Got[[]string], error) {
	var list wire.Apps
	b, err := f.load(ctx, model.Job{Kind: model.KApps}, "apps", "apps.json", &list)
	names := make([]string, len(list.Apps))
	for i, e := range list.Apps {
		names[i] = e.Name
	}
	return model.Got[[]string]{V: names, Body: b}, err
}

// Status answers status/<app>.json.
func (f *Fake) Status(ctx context.Context, app string) (model.Got[wire.Status], error) {
	var g model.Got[wire.Status]
	b, err := f.load(ctx, model.Job{Kind: model.KStatus, App: app}, app, "status/"+app+".json", &g.V)
	g.Body = b
	return g, err
}

// Deploy answers deploys/<app>/<version>.json, else the status's last
// deploy when it is that version.
func (f *Fake) Deploy(ctx context.Context, app, version string) (model.Got[wire.DeployResult], error) {
	var g model.Got[wire.DeployResult]
	j := model.Job{Kind: model.KDeploy, App: app, Version: version}
	b, err := f.load(ctx, j, app+"@"+version, "deploys/"+app+"/"+version+".json", &g.V)
	if model.NotFound(err) {
		var st wire.Status
		if _, serr := f.load(ctx, j, app, "status/"+app+".json", &st); serr == nil && st.LastDeploy != nil && st.LastDeploy.Version == version {
			b, _ = json.Marshal(st.LastDeploy)
			return model.Got[wire.DeployResult]{V: *st.LastDeploy, Body: b}, nil
		}
	}
	g.Body = b
	return g, err
}

// BoxStatus answers box.json.
func (f *Fake) BoxStatus(ctx context.Context) (model.Got[wire.BoxStatus], error) {
	var g model.Got[wire.BoxStatus]
	b, err := f.load(ctx, model.Job{Kind: model.KBox}, "box", "box.json", &g.V)
	g.Body = b
	return g, err
}

// Backups answers backups.json.
func (f *Fake) Backups(ctx context.Context) (model.Got[wire.Backups], error) {
	var g model.Got[wire.Backups]
	b, err := f.load(ctx, model.Job{Kind: model.KBackups}, "backups", "backups.json", &g.V)
	g.Body = b
	return g, err
}

// Alerts answers alerts/<app>.json, or no alert held for an app with
// none: every app of the fixtures is on a box with a monitor.
func (f *Fake) Alerts(ctx context.Context, app string) (model.Got[wire.Alerts], error) {
	var g model.Got[wire.Alerts]
	j := model.Job{Kind: model.KAlerts, App: app}
	path := "alerts/" + app + ".json"
	f.mu.Lock()
	_, replaced := f.Bodies[path]
	f.mu.Unlock()
	if _, err := fs.Stat(fixtures, "fixtures/"+path); err != nil && !replaced {
		f.Body(path, `{"monitor":"on","alerts":[]}`)
	}
	b, err := f.load(ctx, j, "alerts:"+app, path, &g.V)
	g.Body = b
	return g, err
}

// BoxAlerts answers box-alerts.json.
func (f *Fake) BoxAlerts(ctx context.Context) (model.Got[wire.BoxAlerts], error) {
	var g model.Got[wire.BoxAlerts]
	b, err := f.load(ctx, model.Job{Kind: model.KBoxAlerts}, "box-alerts", "box-alerts.json", &g.V)
	g.Body = b
	return g, err
}

// Series answers series/<app>/<range>.json when a test gave one
// (Body), else a series it makes over the window ending at its clock's
// last step: every app's memory and CPU, and for blog and shop their
// requests too, with a gap of an hour and a half (a stop) six hours
// back, so a screen shows a window whole, a gap and the request panels.
// For an app the fixtures do not have, the store holds nothing.
func (f *Fake) Series(ctx context.Context, app, rng string) (model.Got[wire.Series], error) {
	var g model.Got[wire.Series]
	j := model.Job{Kind: model.KSeries, App: app, Range: rng}
	path := "series/" + app + "/" + rng + ".json"
	f.mu.Lock()
	_, replaced := f.Bodies[path]
	now := f.Now()
	f.mu.Unlock()
	if !replaced {
		b, err := json.Marshal(made(app, rng, now))
		if err != nil {
			return g, err
		}
		f.Body(path, string(b))
	}
	b, err := f.load(ctx, j, "series:"+app, path, &g.V)
	g.Body = b
	if !replaced {
		f.mu.Lock()
		delete(f.Bodies, path)
		f.mu.Unlock()
	}
	return g, err
}

// made is the series Series makes of app over rng ending at now's last
// step: numbers from a sine and the app's name, so a screen draws the
// same plots at every run.
func made(app, rng string, now time.Time) wire.Series {
	spans := map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "14d": 14 * 24 * time.Hour}
	steps := map[string]time.Duration{"1h": time.Minute, "24h": 5 * time.Minute, "7d": 30 * time.Minute, "14d": time.Hour}
	span, step := spans[rng], steps[rng]
	s := wire.Series{Store: wire.StoreOn, Window: rng, Panels: []wire.Panel{}}
	if span == 0 {
		return s
	}
	end := now.Truncate(step).UTC()
	s.StepSeconds, s.End = int(step/time.Second), &end
	if _, err := fs.Stat(fixtures, "fixtures/status/"+app+".json"); err != nil {
		return s
	}
	seed := 0.0
	for _, c := range app {
		seed += float64(c)
	}
	observed := app == "blog" || app == "shop"
	panels := map[string][][2]float64{}
	for t := end.Add(-span); !t.After(end); t = t.Add(step) {
		back := end.Sub(t)
		if back > 6*time.Hour && back < 7*time.Hour+30*time.Minute {
			continue // stopped: no point
		}
		x := float64(t.Unix())/3600 + seed
		ts := float64(t.Unix())
		day := (math.Sin(x*2*math.Pi/24) + 1) / 2
		// The CPU and the requests follow the day hours apart from the
		// memory, so the plots are not one shape.
		busy := (math.Sin((x-7)*2*math.Pi/24) + 1) / 2
		panels[wire.PanelMemory] = append(panels[wire.PanelMemory], [2]float64{ts, math.Round((120 + 60*day + float64(int(seed)%40)) * (1 << 20))})
		panels[wire.PanelCPU] = append(panels[wire.PanelCPU], [2]float64{ts, math.Round((0.02+0.05*busy)*1e6) / 1e6})
		if observed {
			panels[wire.PanelRequests] = append(panels[wire.PanelRequests], [2]float64{ts, math.Round((2+6*busy)*1e6) / 1e6})
			panels[wire.Panel5xx] = append(panels[wire.Panel5xx], [2]float64{ts, math.Round(0.004*busy*busy*1e4) / 1e4})
			panels[wire.PanelP95] = append(panels[wire.PanelP95], [2]float64{ts, math.Round((40+80*busy*busy)*10) / 10})
		}
	}
	for _, p := range []struct{ name, unit string }{{wire.PanelMemory, wire.UnitBytes}, {wire.PanelCPU, wire.UnitCores},
		{wire.PanelRequests, wire.UnitPerSecond}, {wire.Panel5xx, wire.UnitShare}, {wire.PanelP95, wire.UnitMs}} {
		if pts := panels[p.name]; len(pts) > 0 {
			s.Panels = append(s.Panels, wire.Panel{Name: p.name, Unit: p.unit, Points: pts})
		}
	}
	return s
}

// Key is the key the demo says it signs with: a made-up one.
var Key = model.Key{
	Fingerprint: "SHA256:SAMPLE0000000000000000000000000000000000000",
	Shown:       "~/.ssh/id_ed25519.pub",
	Source:      "the default file",
	Line:        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn", // gitleaks:allow — made up for this test
}
