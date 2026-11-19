// Package demo is a box made of fixtures: the TUI's --demo, which
// shows sample data with no box, and the fake client its tests run
// against. Its fixtures are bodies as a box writes them, with times
// written as "@-12m" (twelve minutes before the fake's clock) so that
// ages read as they would.
package demo

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
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
	// name, "<app>@<version>", or "alerts:<app>".
	Errs map[string]error
	// Bodies replaces a fixture's body, keyed by its path in fixtures/.
	Bodies map[string]string
	Calls  []Call
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
	err := f.Errs[key]
	body, replaced := f.Bodies[path]
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
		return `"` + now.Add(time.Duration(n)*units[p[2]]).UTC().Format(time.RFC3339) + `"`
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

// Key is the key the demo says it signs with: a made-up one.
var Key = model.Key{
	Fingerprint: "SHA256:SAMPLE0000000000000000000000000000000000000",
	Shown:       "~/.ssh/id_ed25519.pub",
	Source:      "the default file",
	Line:        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn", // gitleaks:allow — made up for this test
}
