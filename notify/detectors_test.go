package notify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/liveswap"
)

func TestLifecycleSinkMapsLiveswapsEvents(t *testing.T) {
	te := newTestEngine(t)
	s := lifecycleSink{te.engine}
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventDeployStarted, App: "blog", Release: "v2"})
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventWatchdogThrottled, App: "blog", Release: "v1", Summary: "restart loop"})
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventDeployFailed, App: "blog", Release: "v2", DeployedBy: "local:/k", Summary: "deploy of v2 failed"})
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventDeployEnded, App: "blog", Release: "v2"})
	got := te.drain()
	byName := map[string]amAlert{}
	var order []string
	for _, a := range got {
		order = append(order, a.Labels["alertname"])
		byName[a.Labels["alertname"]] = a
	}
	want := []string{"deploy_in_progress", "watchdog_throttled", "deploy_failed", "deploy_in_progress"}
	if len(order) != len(want) {
		t.Fatalf("posts = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("posts = %v, want %v", order, want)
		}
	}
	if w := byName["watchdog_throttled"]; w.Labels["kind"] != "state" || w.Labels["severity"] != "critical" || w.Labels["app"] != "blog" {
		t.Errorf("watchdog = %v", w.Labels)
	}
	if f := byName["deploy_failed"]; f.Labels["kind"] != "event" || f.Labels["severity"] != "warning" || f.Labels["deployed_by"] != "local:/k" {
		t.Errorf("deploy_failed = %v", f.Labels)
	}
	// The recovery carries the watchdog's hold to the engine.
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventWatchdogRecovered, App: "blog", Hold: 2 * time.Minute})
	if st := te.states["liveswap/app:blog/watchdog_throttled"]; st == nil || st.firing || st.hold != 2*time.Minute {
		t.Fatalf("state = %+v", st)
	}
}

func certEvent(t *testing.T, name string, data map[string]any) caddy.Event {
	t.Helper()
	e, err := caddy.NewEvent(caddy.Context{Context: context.Background()}, name, data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestTLSHandler(t *testing.T) {
	te := newTestEngine(t)
	h := tlsHandler{te.engine}
	cases := []struct {
		data map[string]any
		sev  string
	}{
		{map[string]any{"identifier": "a.example.org", "renewal": true, "remaining": 3 * 24 * time.Hour}, SeverityCritical},
		{map[string]any{"identifier": "b.example.org", "renewal": true, "remaining": 20 * 24 * time.Hour}, SeverityWarning},
		{map[string]any{"identifier": "c.example.org", "renewal": false}, SeverityWarning},
	}
	for _, c := range cases {
		if err := h.Handle(context.Background(), certEvent(t, "cert_failed", c.data)); err != nil {
			t.Fatal(err)
		}
	}
	got := te.drain()
	if len(got) != 3 {
		t.Fatalf("posts = %+v", got)
	}
	for i, c := range cases {
		l := got[i].Labels
		if l["severity"] != c.sev || l["instance"] != c.data["identifier"] || l["subject"] != "box" || l["alertname"] != "cert_failed" {
			t.Errorf("case %d labels = %v", i, l)
		}
		if _, ok := l["app"]; ok {
			t.Errorf("a certificate event carries an app label: %v", l)
		}
	}
	if err := h.Handle(context.Background(), certEvent(t, "cert_obtained", map[string]any{"identifier": "a.example.org"})); err != nil {
		t.Fatal(err)
	}
	if st := te.states["tls/box/cert_failed/a.example.org"]; st == nil || st.firing {
		t.Fatal("cert_obtained did not clear its identifier")
	}
}

func writeRecord(t *testing.T, path string, st *record.Status) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(evs []Event) map[string]bool {
	out := map[string]bool{}
	for _, e := range evs {
		k := e.Name
		if e.Instance != "" {
			k += "/" + e.Instance
		}
		if !e.Firing {
			k = "-" + k
		}
		out[k] = true
	}
	return out
}

func TestBackupPoller(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	var b backupPoller
	if evs := b.poll(now, path); len(evs) != 0 {
		t.Fatalf("no record (backups not set up): %+v", evs)
	}
	old := &record.Snapshot{ID: "a", Time: now.Add(-4 * time.Hour)}
	fresh := &record.Snapshot{ID: "b", Time: now.Add(-time.Hour)}
	writeRecord(t, path, &record.Status{Apps: map[string]*record.App{
		"blog": {Class: record.OK, LastOK: old},
		"shop": {Class: record.Failed, LastOK: fresh},
	}, LastDrill: &record.Drill{Time: now, Detail: "could not begin"}})
	if evs := b.poll(now.Add(time.Minute), path); evs != nil {
		t.Fatal("polled inside five minutes")
	}
	now = now.Add(5 * time.Minute)
	evs := b.poll(now, path)
	got := names(evs)
	for _, want := range []string{"backup_stale/blog", "backup_run_failed", "drill_failed"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	for _, e := range evs {
		if e.App != "" || e.subject() != "box" {
			t.Errorf("a backup event is the box's: %+v", e)
		}
		if e.Name == "backup_stale" && e.Severity != SeverityCritical {
			t.Errorf("stale is critical: %+v", e)
		}
	}
	// Everything put right: each clears.
	writeRecord(t, path, &record.Status{Apps: map[string]*record.App{
		"blog": {Class: record.OK, LastOK: &record.Snapshot{ID: "c", Time: now}},
		"shop": {Class: record.OK, LastOK: &record.Snapshot{ID: "d", Time: now}},
	}})
	now = now.Add(5 * time.Minute)
	got = names(b.poll(now, path))
	for _, want := range []string{"-backup_stale/blog", "-backup_run_failed", "-drill_failed"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	if got := names(b.poll(now, path)); !got["backup_unreadable"] {
		t.Errorf("an unreadable record: %v", got)
	}
}

func TestDiskLevel(t *testing.T) {
	cases := []struct {
		prev string
		used float64
		want string
	}{
		{"", 50, ""}, {"", 79.9, ""}, {"", 80, SeverityWarning}, {"", 90, SeverityCritical},
		{SeverityWarning, 76, SeverityWarning}, {SeverityWarning, 74.9, ""},
		{SeverityCritical, 86, SeverityCritical}, {SeverityCritical, 84, SeverityWarning},
		{SeverityCritical, 70, ""},
	}
	for _, c := range cases {
		if got := diskLevel(c.prev, c.used); got != c.want {
			t.Errorf("diskLevel(%q, %v) = %q, want %q", c.prev, c.used, got, c.want)
		}
	}
}

func TestDiskPollerOncePerFilesystem(t *testing.T) {
	used := map[string]float64{"/var/lib/liveswap": 85, "/var/lib/motel": 85, "/var/lib/motel-notify": 40}
	dev := map[string]uint64{"/var/lib/liveswap": 1, "/var/lib/motel": 1, "/var/lib/motel-notify": 2}
	d := diskPoller{stat: func(p string) (float64, uint64, error) { return used[p], dev[p], nil }}
	paths := []string{"/var/lib/liveswap", "/var/lib/motel", "/var/lib/motel-notify"}
	evs := d.poll(paths)
	if len(evs) != 1 || evs[0].Instance != "/var/lib/liveswap" || evs[0].Severity != SeverityWarning || !evs[0].Firing {
		t.Fatalf("events = %+v", evs)
	}
	used["/var/lib/liveswap"] = 60
	evs = d.poll(paths)
	if len(evs) != 1 || evs[0].Firing {
		t.Fatalf("after the clean-up: %+v", evs)
	}
}

func TestSeverityChangeEndsTheOldAlert(t *testing.T) {
	te := newTestEngine(t)
	ev := Event{Name: alertDiskLow, Kind: kindState, Severity: SeverityWarning, Source: "disk", Instance: "/", Summary: "82%", Firing: true}
	te.report(ev)
	te.drain()
	ev.Severity, ev.Summary = SeverityCritical, "91%"
	te.report(ev)
	got := te.drain()
	if len(got) != 2 || got[0].Labels["severity"] != "warning" || got[0].EndsAt.IsZero() ||
		got[1].Labels["severity"] != "critical" || !got[1].EndsAt.IsZero() {
		t.Fatalf("posts = %+v", got)
	}
}
