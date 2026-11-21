package demo

import (
	"context"
	"encoding/json"
	"io/fs"
	"strconv"
	"time"
)

// The demo's alerts arriving (#576): under --demo, from its start, a
// critical alert fires at 20 s, a warning one at 70 s, and the first
// resolves at 130 s, so the title bar's mark, its movement and the bell
// can be seen with no box. Each shows at the watch's next read of the
// box's alerts, as on a box.
//
// The box's alerts body is set whole at each step with every time
// pinned: a body a test or this script sets has its "@-6m" times read
// from when it was set (Body), which would give every alert in it a new
// start, and the watch would take each for a new alert.

// Script is the demo's steps, each at its time from the start.
var Script = []Step{
	{At: 20 * time.Second, Fire: &Arrival{Name: "relaunch_refused", Severity: "critical", App: "blog", Summary: "blog: the watchdog may not relaunch it: its restart budget is spent"}},
	{At: 70 * time.Second, Fire: &Arrival{Name: "app_memory_high", Severity: "warning", App: "shop", Summary: "shop: memory over 90% of its limit for 10m"}},
	{At: 130 * time.Second, Resolve: "relaunch_refused"},
}

// Step is one step of the script: an alert that fires, or the name of
// one that resolves.
type Step struct {
	At      time.Duration
	Fire    *Arrival
	Resolve string
}

// Arrival is an alert the script fires, active from the step's time.
type Arrival struct {
	Name, Severity, App, Summary string
	start                        time.Time
}

// Arrivals plays Script on f until ctx ends, waiting with after
// (time.After).
func Arrivals(ctx context.Context, f *Fake, after func(time.Duration) <-chan time.Time) {
	prev := time.Duration(0)
	for i, st := range Script {
		select {
		case <-ctx.Done():
			return
		case <-after(st.At - prev):
		}
		prev = st.At
		f.Arrive(i)
	}
}

// Arrive plays step i of Script on f: the box's alerts, and the alerts
// of each app the script fires on, are then the fixture's, their times
// pinned to the fake's first read, with the script's alerts firing at
// this step in front of them, newest first, each from the step it fired
// at.
func (f *Fake) Arrive(i int) {
	st := Script[i]
	f.mu.Lock()
	now := f.Now()
	if f.anchor.IsZero() {
		f.anchor = now
	}
	anchor := f.anchor
	if st.Fire != nil {
		a := *st.Fire
		a.start = now
		f.firing = append([]Arrival{a}, f.firing...)
	}
	var firing []Arrival
	for _, a := range f.firing {
		if a.Name != st.Resolve {
			firing = append(firing, a)
		}
	}
	f.firing = firing
	f.mu.Unlock()
	// The box's alerts, and each app's the script fires on, so its
	// screen shows them too.
	apps := map[string]bool{}
	for _, st := range Script {
		if st.Fire != nil {
			apps[st.Fire.App] = true
		}
	}
	f.arrived("box-alerts.json", anchor, firing, "")
	for app := range apps {
		f.arrived("alerts/"+app+".json", anchor, firing, app)
	}
}

// arrived sets the body at path: the fixture's (an app with none holds
// no alert), its times pinned to anchor, with the alerts of firing in
// front of its own, newest first; those of app alone when app is not ""
// (an app's alerts name no app).
func (f *Fake) arrived(path string, anchor time.Time, firing []Arrival, app string) {
	base, err := fs.ReadFile(fixtures, "fixtures/"+path)
	if err != nil {
		base = []byte(`{"monitor":"on","alerts":[]}`)
	}
	pinned := token.ReplaceAllStringFunc(string(base), func(m string) string {
		p := token.FindStringSubmatch(m)
		n, _ := strconv.Atoi(p[1])
		return `"` + anchor.Add(time.Duration(n)*units[p[2]]).UTC().Format(time.RFC3339) + `"`
	})
	var doc map[string]any
	if json.Unmarshal([]byte(pinned), &doc) != nil {
		return
	}
	alerts, _ := doc["alerts"].([]any)
	var fired []any
	for _, a := range firing {
		row := map[string]any{"name": a.Name, "severity": a.Severity, "summary": a.Summary, "state": "active", "starts_at": a.start.UTC().Format(time.RFC3339)}
		switch {
		case app == "":
			row["app"] = a.App
		case a.App != app:
			continue
		}
		fired = append(fired, row)
	}
	doc["alerts"] = append(fired, alerts...)
	b, err := json.Marshal(doc)
	if err != nil {
		return
	}
	f.Body(path, string(b))
}
