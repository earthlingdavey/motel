package notify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/liveswap"
)

func TestLifecycleSinkMapsLiveswapsEvents(t *testing.T) {
	te := newTestEngine(t)
	s := lifecycleSink{e: te.engine}
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

// TestBackupPollerFollowsMotelBackupsVerdicts: a run is failed unless
// every app is ok or pending (motel-backup run's own exit), and a drill
// that did not prove an app is drill_failed, as status reads both.
func TestBackupPollerFollowsMotelBackupsVerdicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	fresh := &record.Snapshot{ID: "a", Time: now.Add(-time.Hour)}
	for _, class := range []record.Class{record.Incomplete, record.DataMissing, record.NotAttempted, record.Failed} {
		var b backupPoller
		writeRecord(t, path, &record.Status{Apps: map[string]*record.App{"blog": {Class: class, LastOK: fresh}}})
		if got := names(b.poll(now, path)); !got["backup_run_failed"] {
			t.Errorf("class %q: %v", class, got)
		}
	}
	for _, class := range []record.Class{record.OK, record.Pending} {
		var b backupPoller
		writeRecord(t, path, &record.Status{Apps: map[string]*record.App{"blog": {Class: class, LastOK: fresh}}})
		if got := names(b.poll(now, path)); got["backup_run_failed"] {
			t.Errorf("class %q is no failure: %v", class, got)
		}
	}
	var b backupPoller
	writeRecord(t, path, &record.Status{Apps: map[string]*record.App{
		"blog": {Class: record.OK, LastOK: fresh, RestoreDrill: &record.Drill{Time: now, Detail: "the restored database fails its integrity check"}},
		"shop": {Class: record.OK, LastOK: fresh, RestoreProven: &record.Drill{Time: now}},
	}})
	evs := b.poll(now, path)
	if got := names(evs); !got["drill_failed"] {
		t.Fatalf("a per-app drill that proved nothing: %v", got)
	}
	for _, e := range evs {
		if e.Name == "drill_failed" && (!strings.Contains(e.Summary, "blog") || strings.Contains(e.Summary, "shop") || strings.Contains(e.Summary, "integrity")) {
			t.Fatalf("summary = %q", e.Summary)
		}
	}
}

// TestBackupPollerUnreadableKeepsWhatFired: a record that cannot be read
// says nothing was put right.
func TestBackupPollerUnreadableKeepsWhatFired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	writeRecord(t, path, &record.Status{Apps: map[string]*record.App{}, LastDrill: &record.Drill{Time: now, Detail: "could not begin"}})
	var b backupPoller
	if got := names(b.poll(now, path)); !got["drill_failed"] {
		t.Fatalf("first poll: %v", got)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := names(b.poll(now.Add(5*time.Minute), path))
	if got["-drill_failed"] || !got["drill_failed"] || !got["backup_unreadable"] {
		t.Fatalf("unreadable: %v", got)
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
	d := diskPoller{stat: func(p string) (diskNumbers, error) { return diskNumbers{usedPct: used[p], dev: dev[p]}, nil }}
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

// TestSeverityChangeInsideTheHold: a key cleared and fired again at
// another severity before its hold ends is the same case.
func TestSeverityChangeInsideTheHold(t *testing.T) {
	te := newTestEngine(t)
	ev := Event{Name: alertDiskLow, Kind: kindState, Severity: SeverityWarning, Source: "disk", Instance: "/", Summary: "85%", Firing: true}
	te.report(ev)
	te.drain()
	ev.Firing = false
	te.report(ev)
	te.advance(10 * time.Second)
	ev.Firing, ev.Severity = true, SeverityCritical
	te.report(ev)
	got := te.drain()
	if len(got) != 2 || got[0].Labels["severity"] != "warning" || got[0].EndsAt.IsZero() || got[1].Labels["severity"] != "critical" {
		t.Fatalf("posts = %+v", got)
	}
	te.advance(time.Minute)
	te.tick()
	for _, a := range te.drain() {
		if a.Labels["alertname"] == alertDiskLow && !a.EndsAt.IsZero() {
			t.Fatalf("a resolve after the re-fire: %+v", a)
		}
	}
}

// TestPruneEndsWhatNoSourceClears: an app taken out of the config, or
// the backup record turned off, leaves no key re-posted for ever.
func TestPruneEndsWhatNoSourceClears(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	te.report(Event{Name: "drill_failed", Kind: kindState, Severity: SeverityCritical, Source: "backup", Summary: "x", Firing: true})
	te.report(Event{Name: alertDiskLow, Kind: kindState, Severity: SeverityWarning, Source: "disk", Instance: "/var/lib/liveswap", Summary: "x", Firing: true})
	te.mute("blog", true)
	te.drain()
	a := mustParse(t, "monitor {\nchannel ops slack {secret.NOTIFY_OPS_URL}\noperator ops\nbackup_record off\n}")
	r, err := a.resolve(map[string][]TrustSource{"shop": nil}, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg, a.diskPaths = r, []string{"/var/lib/motel"}
	te.setConfig(a, te.p, te.known, te.logger)
	got := te.drain()
	ended := map[string]bool{}
	for _, x := range got {
		if !x.EndsAt.IsZero() && !x.EndsAt.After(te.at) {
			ended[x.Labels["alertname"]] = true
		}
	}
	for _, n := range []string{"watchdog_throttled", "drill_failed", alertDiskLow, alertDeployMute} {
		if !ended[n] {
			t.Errorf("%s not ended: %+v", n, got)
		}
	}
	if len(te.states) != 0 || len(te.mutes) != 0 {
		t.Fatalf("left: %v %v", te.states, te.mutes)
	}
}

func TestSummaryKeepsAShaRelease(t *testing.T) {
	te := newTestEngine(t)
	sha := "3f2a9c1e8b7d6a5f4e3d2c1b0a9f8e7d6c5b4a39"
	te.report(Event{Name: "deploy_failed", Kind: kindEvent, App: "blog", Source: "liveswap", Release: sha, Summary: "deploy of " + sha + " failed"})
	got := te.drain()
	if len(got) != 1 || !strings.Contains(got[0].Annotations["summary"], sha) {
		t.Fatalf("summary = %q", got[0].Annotations["summary"])
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

func TestDiskPollerKeepsAPathItCannotMeasure(t *testing.T) {
	fail := false
	d := diskPoller{stat: func(string) (diskNumbers, error) {
		if fail {
			return diskNumbers{}, os.ErrPermission
		}
		return diskNumbers{usedPct: 92, dev: 1}, nil
	}}
	if evs := d.poll([]string{"/var/lib/liveswap"}); len(evs) != 1 || !evs[0].Firing {
		t.Fatalf("first: %+v", evs)
	}
	fail = true
	evs := d.poll([]string{"/var/lib/liveswap"})
	if len(evs) != 1 || !evs[0].Firing || evs[0].Severity != SeverityCritical {
		t.Fatalf("a failed statfs cleared the alert: %+v", evs)
	}
}

// A statfs that hangs (a network mount whose server went away) holds
// back no poll, which runs on the engine's goroutine: the path is not
// measured, so one that fired keeps firing and one that did not fires
// nothing, the other paths are, and the polls meanwhile start no other
// statfs of it (Copilot round 3 on #429).
func TestDiskPollerStatThatHangs(t *testing.T) {
	defer func(w time.Duration) { diskStatWait = w }(diskStatWait)
	diskStatWait = 50 * time.Millisecond
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var hang atomic.Bool
	var calls atomic.Int32
	used := map[string]float64{"/srv/hang-fired": 92, "/srv/hang-quiet": 40, "/var/lib/hang-other": 85}
	dev := map[string]uint64{"/srv/hang-fired": 1, "/srv/hang-quiet": 2, "/var/lib/hang-other": 3}
	d := diskPoller{stat: func(p string) (diskNumbers, error) {
		if hang.Load() && p != "/var/lib/hang-other" {
			calls.Add(1)
			<-release
		}
		return diskNumbers{usedPct: used[p], dev: dev[p]}, nil
	}}
	paths := []string{"/srv/hang-fired", "/srv/hang-quiet", "/var/lib/hang-other"}
	poll := func() []Event {
		t.Helper()
		got := make(chan []Event, 1)
		go func() { got <- d.poll(paths) }()
		select {
		case evs := <-got:
			return evs
		case <-time.After(5 * time.Second):
			t.Fatal("a poll waited on a statfs that hangs")
			return nil
		}
	}
	// Each event as instance=severity, or instance=resolved.
	state := func(evs []Event) string {
		var out []string
		for _, e := range evs {
			v := e.Severity
			if !e.Firing {
				v = "resolved"
			}
			out = append(out, e.Instance+"="+v)
		}
		slices.Sort(out)
		return strings.Join(out, " ")
	}
	firing := "/srv/hang-fired=critical /var/lib/hang-other=warning"
	if got := state(poll()); got != firing {
		t.Fatalf("first: %s", got)
	}
	// The fired path's filesystem cleans up and the quiet one fills,
	// while both hang: neither is measured, so what fires is as it was.
	hang.Store(true)
	used["/srv/hang-fired"], used["/srv/hang-quiet"] = 10, 95
	for range 3 {
		if got := state(poll()); got != firing {
			t.Fatalf("a poll over a hung statfs changed what fires: %s", got)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d statfs of the hung paths, want one each", n)
	}
	used["/var/lib/hang-other"] = 50
	if got := state(poll()); got != "/srv/hang-fired=critical /var/lib/hang-other=resolved" {
		t.Fatalf("the path that answers is measured: %s", got)
	}
	hang.Store(false)
	close(release)
	// The two released statfs come back each in its own time, and a poll
	// between them would see one path measured and the other not: wait
	// until neither is out, then one poll measures both.
	deadline := time.Now().Add(5 * time.Second)
	for {
		diskPollStats.mu.Lock()
		out := diskPollStats.busy["/srv/hang-fired"] || diskPollStats.busy["/srv/hang-quiet"]
		diskPollStats.mu.Unlock()
		if !out {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the released statfs did not come back")
		}
		time.Sleep(time.Millisecond)
	}
	if got := state(poll()); got != "/srv/hang-fired=resolved /srv/hang-quiet=critical" {
		t.Fatalf("after it came back: %s", got)
	}
}

// TestTLSKeyUnreportedIsCleared: CertMagic reports cert_failed on every
// failed attempt for a name it manages, at least every six hours; a
// name taken out of the config is never reported again, and its key is
// cleared once tlsStale has passed.
func TestTLSKeyUnreportedIsCleared(t *testing.T) {
	te := newTestEngine(t)
	h := tlsHandler{te.engine}
	fail := func() {
		if err := h.Handle(context.Background(), certEvent(t, "cert_failed", map[string]any{"identifier": "gone.example.org", "renewal": false})); err != nil {
			t.Fatal(err)
		}
	}
	fail()
	te.drain()
	key := "tls/box/cert_failed/gone.example.org"
	// Retried and failed again six hours on: still firing past 13 h
	// from the first report.
	te.advance(6 * time.Hour)
	fail()
	te.advance(7 * time.Hour)
	te.tick()
	if st := te.states[key]; st == nil || !st.firing {
		t.Fatal("cleared while CertMagic was still reporting it")
	}
	// No report for 13 h: taken as cleared, then resolved after the hold.
	te.advance(6 * time.Hour)
	te.tick()
	te.advance(31 * time.Second)
	te.tick()
	resolved := false
	for _, a := range te.drain() {
		if a.Labels["alertname"] == "cert_failed" && !a.EndsAt.IsZero() && !a.EndsAt.After(te.at) {
			resolved = true
		}
	}
	if !resolved || te.states[key] != nil {
		t.Fatalf("not resolved after %v unreported (state %+v)", tlsStale, te.states[key])
	}
}

// TestDiskLevelHeldByDevice: the path that stands for a filesystem can
// change with the config (liveswap added or removed); its hysteresis is
// the filesystem's, so a held critical stays critical above 85 %.
func TestDiskLevelHeldByDevice(t *testing.T) {
	used := map[string]float64{"/var/lib/liveswap": 92, "/var/lib/motel": 92}
	d := diskPoller{stat: func(p string) (diskNumbers, error) { return diskNumbers{usedPct: used[p], dev: 7}, nil }}
	if evs := d.poll([]string{"/var/lib/liveswap", "/var/lib/motel"}); len(evs) != 1 || evs[0].Severity != SeverityCritical {
		t.Fatalf("first: %+v", evs)
	}
	used["/var/lib/motel"] = 86
	for _, e := range d.poll([]string{"/var/lib/motel"}) {
		if e.Firing && e.Severity != SeverityCritical {
			t.Fatalf("a held critical became %s when its path changed: %+v", e.Severity, e)
		}
	}
}
