package notify

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/status"
	"github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
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

// TestPeopleSinkRaisesBoxEvents: liveswap's two announcements about a
// person's key are one-shot box events at warning, from source access,
// keyed by the key's fingerprint, with the summary liveswap wrote, the
// name and the fingerprint's start in it as written; a thief cycling networks is
// held to the limit per key (four, then a journal line each), and
// another key is not held by it.
func TestPeopleSinkRaisesBoxEvents(t *testing.T) {
	te := newTestEngine(t)
	s := peopleSink{e: te.engine}
	alice, bob := accesstest.Ed25519().Fingerprint(), accesstest.Ed25519().Fingerprint()
	first := liveswap.Announcement{Name: liveswap.EventPersonNewNetwork, Person: "alice@example.com", Key: alice, From: "203.0.113.0/24", First: true}
	s.Announce(first)
	got := te.drain()
	if len(got) != 1 {
		t.Fatalf("posts %v", got)
	}
	want := map[string]string{"alertname": "person_new_network", "kind": "event", "severity": "warning", "subject": "box", "source": "access",
		"key": "access/box/person_new_network/" + alice, "instance": alice}
	for k, v := range want {
		if got[0].Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, got[0].Labels[k], v)
		}
	}
	if _, ok := got[0].Labels["app"]; ok {
		t.Errorf("a box event with an app label: %v", got[0].Labels)
	}
	if sum := got[0].Annotations["summary"]; sum != first.Summary() || !strings.Contains(sum, alice[:19]) {
		t.Errorf("summary %q, want %q", sum, first.Summary())
	}
	// A long name that looks generated, cut in the summary, is sent as
	// written.
	long := liveswap.Announcement{Name: liveswap.EventPersonNewNetwork, Person: "svc-" + strings.Repeat("7f3e9d2a", 10), Key: bob, From: "203.0.113.0/24"}
	s.Announce(long)
	if got := te.drain(); len(got) != 1 || got[0].Annotations["summary"] != long.Summary() {
		t.Errorf("a long name: %v, want %q", got, long.Summary())
	}
	for i := range 6 {
		s.Announce(liveswap.Announcement{Name: liveswap.EventPersonNewNetwork, Person: "alice@example.com", Key: alice, From: fmt.Sprintf("198.51.%d.0/24", i)})
	}
	s.Announce(liveswap.Announcement{Name: liveswap.EventPersonOutsideList, Person: "bob@example.com", Key: bob, From: "203.0.113.9"})
	got = te.drain()
	if len(got) != 4 || got[3].Labels["alertname"] != "person_outside_list" || got[3].Labels["key"] != "access/box/person_outside_list/"+bob {
		t.Fatalf("posts %v", got)
	}
	if l := te.lines("monitor: event"); len(l) != 9 || l[4].ContextMap()["suppressed"] != nil || l[5].ContextMap()["suppressed"] == nil || l[8].ContextMap()["suppressed"] != nil {
		t.Errorf("journal %v", l)
	}
	s.Announce(liveswap.Announcement{Name: "something_else", Person: "x", Key: alice})
	if got := te.drain(); len(got) != 0 {
		t.Errorf("an unknown announcement posted %v", got)
	}
}

// TestPeopleSinkBeforeNotifyRuns: an announcement made while no config
// runs notify (a start's first seconds) is kept, and told when one does;
// another event then is not, as before.
func TestPeopleSinkBeforeNotifyRuns(t *testing.T) {
	te := newTestEngine(t)
	te.active = nil
	s := peopleSink{e: te.engine}
	fp := accesstest.Ed25519().Fingerprint()
	for i := range earlyMax + 3 {
		s.Announce(liveswap.Announcement{Name: liveswap.EventPersonNewNetwork, Person: "alice@example.com", Key: fmt.Sprint(fp, i), From: "203.0.113.0/24", First: true})
	}
	te.report(Event{Name: "deploy_failed", Kind: kindEvent, App: "blog", Source: "liveswap", Summary: "x"})
	if got := te.drain(); len(got) != 0 {
		t.Fatalf("posted while no config runs notify: %v", got)
	}
	te.point(&App{}, te.p, te.known, te.logger)
	got := te.drain()
	if len(got) != earlyMax || got[0].Labels["alertname"] != "person_new_network" || got[0].Labels["instance"] != fmt.Sprint(fp, 0) {
		t.Fatalf("told %d once a config runs it: %v", len(got), got[:min(len(got), 1)])
	}
	te.point(&App{}, te.p, te.known, te.logger)
	if got := te.drain(); len(got) != 0 {
		t.Errorf("told again: %v", got)
	}
	// One key cycling networks keeps a bucket's worth, and leaves room
	// for another key's first use.
	te.active = nil
	for i := range earlyMax {
		s.Announce(liveswap.Announcement{Name: liveswap.EventPersonNewNetwork, Person: "mallory@example.com", Key: fp + "cycling", From: fmt.Sprintf("198.51.%d.0/24", i)})
	}
	s.Announce(liveswap.Announcement{Name: liveswap.EventPersonNewNetwork, Person: "bob@example.com", Key: fp + "bob", From: "203.0.113.0/24", First: true})
	te.point(&App{}, te.p, te.known, te.logger)
	if got := te.drain(); len(got) != bucketSize+1 || got[bucketSize].Labels["instance"] != fp+"bob" {
		t.Errorf("told %d, want %d of the cycling key and bob's: %v", len(got), bucketSize, got)
	}
	// Kept past earlyFor (a box with no monitor block for a while), it is
	// not told late as if it were now; and a full buffer of such old ones
	// leaves room for a fresh one, which is told.
	te.active = nil
	for i := range earlyMax {
		s.Announce(liveswap.Announcement{Name: liveswap.EventPersonNewNetwork, Person: "alice@example.com", Key: fmt.Sprint(fp, "old", i), From: "198.51.100.0/24"})
	}
	te.advance(earlyFor + time.Second)
	s.Announce(liveswap.Announcement{Name: liveswap.EventPersonNewNetwork, Person: "alice@example.com", Key: fp + "fresh", From: "198.51.100.0/24"})
	te.point(&App{}, te.p, te.known, te.logger)
	if got := te.drain(); len(got) != 1 || got[0].Labels["instance"] != fp+"fresh" {
		t.Errorf("told %d after %s, want the fresh one alone: %v", len(got), earlyFor, got)
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
	te.report(Event{Name: "backup_drill_failed", Kind: kindState, Severity: SeverityCritical, Source: "backup", Summary: "x", Firing: true})
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
	for _, n := range []string{"watchdog_throttled", "backup_drill_failed", alertDiskLow, alertDeployMute} {
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

// setUpBackups is a credential file in dir, set up at: whether backups
// are set up, and since when, is all the poller looks at of it.
func setUpBackups(t *testing.T, dir string, at time.Time) string {
	t.Helper()
	p := filepath.Join(dir, "repository.env")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

// backupRig is a backup poller on a box whose backups were set up
// setUpAgo before now (0: not set up), with a record beside it.
type backupRig struct {
	b    backupPoller
	path string
	now  time.Time
}

const day = 24 * time.Hour

func newBackupRig(t *testing.T, setUpAgo time.Duration) *backupRig {
	t.Helper()
	dir := t.TempDir()
	r := &backupRig{path: filepath.Join(dir, "status.json"), now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	r.b.envFile = filepath.Join(dir, "repository.env")
	if setUpAgo > 0 {
		setUpBackups(t, dir, r.now.Add(-setUpAgo))
	}
	return r
}

// next is when the next poll is: what a record written for it is aged
// against.
func (r *backupRig) next() time.Time { return r.now.Add(backupEvery) }

// poll writes st (nil: no record) and polls five minutes on.
func (r *backupRig) poll(t *testing.T, st *record.Status) []Event {
	t.Helper()
	if st == nil {
		_ = os.Remove(r.path)
	} else {
		writeRecord(t, r.path, st)
	}
	r.now = r.now.Add(backupEvery)
	return r.b.poll(r.now, r.path)
}

// soundRecord is a box status calls healthy at now: a run ten minutes
// ago that backed blog up, a listing since, a restore proven two days
// ago and a clean check of the repository three days ago.
func soundRecord(now time.Time) *record.Status {
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	s := &record.Snapshot{ID: "aaaaaaaa1111", Time: now.Add(-10 * time.Minute), Seen: at(9 * time.Minute)}
	return &record.Status{
		Started: now.Add(-11 * time.Minute), Finished: now.Add(-9 * time.Minute), Listed: at(9 * time.Minute),
		Apps: map[string]*record.App{"blog": {Class: record.OK, Looked: "/var/lib/liveswap/blog/shared", Snapshot: s, LastOK: s, LastSnapshot: s,
			RestoreProven: &record.Drill{Snapshot: *s, Time: now.Add(-48 * time.Hour)}}},
		LastDrill:  &record.Drill{Time: now.Add(-72 * time.Hour)},
		LastCheck:  &record.Check{Time: now.Add(-72 * time.Hour), Group: "38/52", Class: record.CheckClean},
		CheckSince: at(30 * day),
	}
}

// firingKeys is names' keys, sorted, as one string.
func firingKeys(evs []Event) string {
	return strings.Join(slices.Sorted(maps.Keys(names(evs))), " ")
}

// TestBackupEachProblemIsItsAlert: each kind of problem status judges is
// the alert its rule names, alone, at its listed level, the box's, with
// the app as its instance where the line is an app's, and the pointer
// then status's own line as its summary. Every name status lists is
// raised by one case.
func TestBackupEachProblemIsItsAlert(t *testing.T) {
	cases := []struct {
		key    string
		setUp  time.Duration
		record func(now time.Time) *record.Status
		says   string
	}{
		{"backup_never_ran", 4 * time.Hour, func(time.Time) *record.Status { return nil }, "no run yet, and backups were set up"},
		{"backup_not_running", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Started, st.Finished, st.Apps = now.Add(-5*time.Hour), now.Add(-4*time.Hour), map[string]*record.App{}
			return st
		}, "no run has finished since"},
		{"backup_stale/blog", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Apps["blog"].LastOK.Time = now.Add(-4 * time.Hour)
			return st
		}, "blog: stale: the last complete backup is from"},
		{"backup_never_complete/blog", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Apps["blog"].LastOK = nil
			return st
		}, "blog: no complete backup yet"},
		{"backup_no_result/shop", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Apps["shop"] = nil
			return st
		}, "shop: no result on record"},
		{"backup_run_failed", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Error = "the repository refused"
			return st
		}, "the last run ended early: the repository refused"},
		{"backup_run_failed/blog", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Apps["blog"].Class, st.Apps["blog"].Detail = record.Failed, "restic exited 1"
			return st
		}, "blog: failed: restic exited 1"},
		{"backup_data_missing/blog", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			a := st.Apps["blog"]
			a.Class, a.Detail, a.Snapshot = record.DataMissing, "the data is gone", nil
			return st
		}, "blog: data missing: the data is gone"},
		{"backup_snapshot_gone/blog", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			seen := now.Add(-69 * time.Minute)
			st.Apps["blog"].LastOK = &record.Snapshot{ID: "cccccccc3333", Time: now.Add(-70 * time.Minute), Seen: &seen}
			return st
		}, "blog: snapshot cccccccc is no longer in the repository"},
		{"backup_repository_unchecked", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.LastCheck.Time = now.Add(-9 * day)
			return st
		}, ": clean: old, more than 8 days ago"},
		{"backup_repository_check_failed", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.LastCheck.Class, st.LastCheck.Detail = record.CheckUnreachable, "the storage did not answer"
			return st
		}, "(data group 38/52): unreachable: the storage did not answer"},
		{"backup_repository_unlisted", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			u := now.Add(-4 * time.Hour)
			st.Unlisted = &u
			return st
		}, "the repository has not been listed since"},
		{"backup_drill_failed", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.LastDrill.Detail = "the plan could not be made"
			return st
		}, "could not begin: the plan could not be made"},
		{"backup_drill_failed/blog", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Apps["blog"].RestoreDrill = &record.Drill{Time: now, Detail: "the restored database fails its integrity check"}
			return st
		}, "blog: restore not proven: the restored database fails its integrity check"},
		{"backup_restore_proof_old/blog", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Apps["blog"].RestoreProven.Time = now.Add(-9 * day)
			return st
		}, "blog: restore last proven: "},
		{"backup_restore_unproven/blog", 0, func(now time.Time) *record.Status {
			st := soundRecord(now)
			st.Apps["blog"].RestoreProven = nil
			return st
		}, "blog: restore not proven: no drill of it has run"},
	}
	raised := map[string]bool{}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			r := newBackupRig(t, cmp.Or(c.setUp, 60*day))
			evs := r.poll(t, c.record(r.next()))
			if got := firingKeys(evs); got != c.key {
				t.Fatalf("raised %q, want %q alone", got, c.key)
			}
			e := evs[0]
			i := slices.IndexFunc(status.Alerts, func(a status.Alert) bool { return a.Name == e.Name })
			if i < 0 || e.Severity != status.Alerts[i].Severity || e.Kind != kindState || e.Source != "backup" || e.App != "" || e.subject() != "box" {
				t.Fatalf("not as listed, or not the box's: %+v", e)
			}
			if line, ok := strings.CutPrefix(e.Summary, backupPointer); !ok || !strings.Contains(line, c.says) {
				t.Fatalf("summary %q, want the pointer then a line that says %q", e.Summary, c.says)
			}
			raised[e.Name] = true
		})
	}
	for _, a := range status.Alerts {
		if !raised[a.Name] {
			t.Errorf("no case raises %s", a.Name)
		}
	}
}

// TestBackupProblemsStartAndEndEachByItself: a second problem that
// starts while a first is open is raised then, beside it, and each
// clears when it is put right, the other going on.
func TestBackupProblemsStartAndEndEachByItself(t *testing.T) {
	r := newBackupRig(t, 60*day)
	unlisted := func(st *record.Status) *record.Status {
		u := r.next().Add(-4 * time.Hour)
		st.Unlisted = &u
		return st
	}
	stale := func(st *record.Status) *record.Status {
		st.Apps["blog"].LastOK.Time = r.next().Add(-4 * time.Hour)
		return st
	}
	failed := func(st *record.Status) *record.Status { st.Error = "the repository refused"; return st }
	for i, step := range []struct {
		st   func() *record.Status
		want string
	}{
		{func() *record.Status { return soundRecord(r.next()) }, ""},
		{func() *record.Status { return unlisted(soundRecord(r.next())) }, "backup_repository_unlisted"},
		{func() *record.Status { return stale(unlisted(soundRecord(r.next()))) }, "backup_repository_unlisted backup_stale/blog"},
		// A warning beside two criticals clears neither.
		{func() *record.Status { return failed(stale(unlisted(soundRecord(r.next())))) }, "backup_repository_unlisted backup_run_failed backup_stale/blog"},
		{func() *record.Status { return stale(unlisted(soundRecord(r.next()))) }, "-backup_run_failed backup_repository_unlisted backup_stale/blog"},
		{func() *record.Status { return stale(soundRecord(r.next())) }, "-backup_repository_unlisted backup_stale/blog"},
		{func() *record.Status { return soundRecord(r.next()) }, "-backup_stale/blog"},
		{func() *record.Status { return soundRecord(r.next()) }, ""},
	} {
		if got := firingKeys(r.poll(t, step.st())); got != step.want {
			t.Fatalf("step %d: %q, want %q", i, got, step.want)
		}
	}
}

// TestBackupAcrossARestart: a new process's first poll raises each
// problem again under the same key, and clears nothing: no memory is
// needed to tell what still holds.
func TestBackupAcrossARestart(t *testing.T) {
	r := newBackupRig(t, 60*day)
	st := soundRecord(r.next())
	u := r.next().Add(-4 * time.Hour)
	st.Unlisted, st.Error = &u, "the repository refused"
	before := firingKeys(r.poll(t, st))
	r.b = backupPoller{envFile: r.b.envFile}
	if after := firingKeys(r.poll(t, st)); before != "backup_repository_unlisted backup_run_failed" || after != before {
		t.Fatalf("before the restart %q, after %q", before, after)
	}
}

// TestBackupUnreadableKeepsWhatFired: a record that cannot be read puts
// nothing right: what fired stays fired, beside backup_unreadable; read
// again, what is put right clears. A process that starts on such a
// record knows nothing of what fired (an accepted row: Alertmanager then
// resolves it).
func TestBackupUnreadableKeepsWhatFired(t *testing.T) {
	r := newBackupRig(t, 60*day)
	st := soundRecord(r.next())
	st.Apps["blog"].LastOK.Time = r.next().Add(-4 * time.Hour)
	st.LastDrill.Detail = "could not begin"
	if got := firingKeys(r.poll(t, st)); got != "backup_drill_failed backup_stale/blog" {
		t.Fatalf("first: %q", got)
	}
	if err := os.WriteFile(r.path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(backupEvery)
	if got := firingKeys(r.b.poll(r.now, r.path)); got != "backup_drill_failed backup_stale/blog backup_unreadable" {
		t.Fatalf("unreadable: %q", got)
	}
	fresh := backupPoller{envFile: r.b.envFile}
	if got := firingKeys(fresh.poll(r.now, r.path)); got != "backup_unreadable" {
		t.Fatalf("a process that starts on it: %q", got)
	}
	if got := firingKeys(r.poll(t, soundRecord(r.next()))); got != "-backup_drill_failed -backup_stale/blog -backup_unreadable" {
		t.Fatalf("read again, healthy: %q", got)
	}
}

// TestBackupKeptWhenTheFileCannotBeLookedAt: an Lstat that fails other
// than "not there" (a directory the motel user may not search, an EIO)
// gives no verdict: backup_unreadable says so, naming the file and
// pointing to the client first, and what fired stays fired, never told
// resolved; looked at again, what is put right clears.
func TestBackupKeptWhenTheFileCannotBeLookedAt(t *testing.T) {
	r := newBackupRig(t, 60*day)
	st := soundRecord(r.next())
	u := r.next().Add(-4 * time.Hour)
	st.Unlisted = &u
	if got := firingKeys(r.poll(t, st)); got != "backup_repository_unlisted" {
		t.Fatalf("unlisted: %q", got)
	}
	// A path through a regular file fails with ENOTDIR, not ENOENT,
	// whoever runs the test (root searches any directory).
	file := r.b.envFile
	r.b.envFile = filepath.Join(file, "repository.env")
	st.Unlisted = nil
	evs := r.poll(t, st)
	if got := firingKeys(evs); got != "backup_repository_unlisted backup_unreadable" {
		t.Fatalf("cannot be looked at, the record healthy: %q", got)
	}
	s := summaryOf(evs, "backup_unreadable")
	want := r.b.envFile + " cannot be looked at by moteld, so the backups' health is not known; motel backup status says why (on the box, as root, motel-backup status says their health)"
	if s != want {
		t.Fatalf("summary:\n got %q\nwant %q", s, want)
	}
	// On the box the path is DefaultBackupEnvFile: the pointer is inside the cut.
	if n := utf8.RuneCountInString(strings.Replace(s, r.b.envFile, DefaultBackupEnvFile, 1)); n > maxSummary {
		t.Fatalf("%d characters: the cut takes the pointer", n)
	}
	r.b.envFile = file
	if got := firingKeys(r.poll(t, st)); got != "-backup_repository_unlisted -backup_unreadable" {
		t.Fatalf("looked at again: %q", got)
	}
	r.b.envFile = filepath.Join(file, "repository.env")
	st.Unlisted = &u
	if got := firingKeys(r.poll(t, st)); got != "backup_unreadable" {
		t.Fatalf("cannot be looked at, nothing fired: %q", got)
	}
	fresh := backupPoller{envFile: r.b.envFile}
	if got := firingKeys(fresh.poll(r.now, r.path)); got != "backup_unreadable" {
		t.Fatalf("a process that starts on it: %q", got)
	}
	// The record unreadable, then the file too: one backup_unreadable,
	// fired throughout, its summary the file's.
	r.b.envFile = file
	if err := os.WriteFile(r.path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(backupEvery)
	if got := firingKeys(r.b.poll(r.now, r.path)); got != "backup_unreadable" {
		t.Fatalf("unreadable: %q", got)
	}
	r.b.envFile = filepath.Join(file, "repository.env")
	r.now = r.now.Add(backupEvery)
	evs = r.b.poll(r.now, r.path)
	if got := firingKeys(evs); got != "backup_unreadable" || !strings.HasPrefix(summaryOf(evs, "backup_unreadable"), r.b.envFile) {
		t.Fatalf("unreadable, then the file cannot be looked at: %q %q", got, summaryOf(evs, "backup_unreadable"))
	}
}

// TestBackupNotSetUp: a box with no credential file has no backups set
// up, and nothing is raised whatever a record left behind says; one that
// had them and has the file removed clears what fired, as status then
// says backups are not set up; and nothing of the record is listed.
func TestBackupNotSetUp(t *testing.T) {
	r := newBackupRig(t, 0)
	st := soundRecord(r.next())
	st.Apps["blog"].LastOK.Time = r.next().Add(-4 * time.Hour)
	if got := firingKeys(r.poll(t, st)); got != "" || r.b.read != nil {
		t.Fatalf("not set up: %q, read %v", got, r.b.read)
	}
	if got := firingKeys(r.poll(t, nil)); got != "" {
		t.Fatalf("not set up, no record: %q", got)
	}
	setUpBackups(t, filepath.Dir(r.b.envFile), r.now.Add(-60*day))
	st = soundRecord(r.next())
	st.Apps["blog"].LastOK.Time = r.next().Add(-4 * time.Hour)
	if got := firingKeys(r.poll(t, st)); got != "backup_stale/blog" {
		t.Fatalf("set up: %q", got)
	}
	if err := os.Remove(r.b.envFile); err != nil {
		t.Fatal(err)
	}
	if got := firingKeys(r.poll(t, st)); got != "-backup_stale/blog" || r.b.read != nil {
		t.Fatalf("the file removed: %q, read %v", got, r.b.read)
	}
}

// TestBackupAnAppGoneFromTheRecord: an app the next run no longer
// writes clears its own alerts; another app's go on.
func TestBackupAnAppGoneFromTheRecord(t *testing.T) {
	r := newBackupRig(t, 60*day)
	both := func() *record.Status {
		st := soundRecord(r.next())
		shop := *st.Apps["blog"]
		shop.RestoreProven = nil
		st.Apps["shop"] = &shop
		st.Apps["blog"].RestoreProven = nil
		return st
	}
	if got := firingKeys(r.poll(t, both())); got != "backup_restore_unproven/blog backup_restore_unproven/shop" {
		t.Fatalf("both: %q", got)
	}
	st := both()
	delete(st.Apps, "shop")
	if got := firingKeys(r.poll(t, st)); got != "-backup_restore_unproven/shop backup_restore_unproven/blog" {
		t.Fatalf("shop gone: %q", got)
	}
}

// TestBackupManyAppsAtOnce: ten apps gone stale in one run are ten
// alerts of one name, each its own key, so the limit per key holds none
// back; all ten are posted at once (Alertmanager groups them by name:
// one message that lists them).
func TestBackupManyAppsAtOnce(t *testing.T) {
	te := newTestEngine(t)
	dir := t.TempDir()
	te.backup.envFile = setUpBackups(t, dir, te.at.Add(-60*day))
	te.backupPath = filepath.Join(dir, "status.json")
	st := soundRecord(te.at)
	blog := st.Apps["blog"]
	delete(st.Apps, "blog")
	for i := range 10 {
		a := *blog
		seen := te.at.Add(-9 * time.Minute)
		a.LastOK = &record.Snapshot{ID: fmt.Sprintf("%08d", i), Time: te.at.Add(-4 * time.Hour), Seen: &seen}
		st.Apps[fmt.Sprintf("app%d", i)] = &a
	}
	writeRecord(t, te.backupPath, st)
	te.poll(t.Context())
	keys := map[string]bool{}
	for _, a := range te.drain() {
		if a.Labels["alertname"] == "backup_stale" && a.Labels["severity"] == SeverityCritical && a.EndsAt.IsZero() {
			keys[a.Labels["key"]] = true
		}
	}
	if len(keys) != 10 || !keys["backup/box/backup_stale/app9"] {
		t.Fatalf("posted %d stale apps: %v", len(keys), keys)
	}
	for _, l := range te.lines("monitor: event") {
		if l.ContextMap()["suppressed"] != nil {
			t.Fatalf("held back: %v", l.ContextMap())
		}
	}
}

// TestBackupSummaries: the pointer comes first, the laptop client's read
// then the box's command, and survives the cut at maxSummary: the
// line's tail is what goes.
func TestBackupSummaries(t *testing.T) {
	if backupPointer != "motel backup status (on the box: motel-backup status) says: " {
		t.Errorf("pointer %q", backupPointer)
	}
	r := newBackupRig(t, 60*day)
	st := soundRecord(r.next())
	st.Error = "the repository refused"
	if s := summaryOf(r.poll(t, st), "backup_run_failed"); s != backupPointer+"the last run ended early: the repository refused" {
		t.Errorf("backup_run_failed: %q", s)
	}
	st = soundRecord(r.next())
	st.LastCheck.Class, st.LastCheck.Detail = record.CheckFailed, strings.Repeat("restic said something long ", 20)
	s := summaryOf(r.poll(t, st), "backup_repository_check_failed")
	if c := cleanText(nil, s); !strings.HasPrefix(c, backupPointer+"the repository check of ") || utf8.RuneCountInString(c) > maxSummary {
		t.Errorf("cut: %q", c)
	}
}

// summaryOf is the summary of the firing alert name, none if none.
func summaryOf(evs []Event, name string) string {
	for _, e := range evs {
		if e.Name == name && e.Firing {
			return e.Summary
		}
	}
	return ""
}

// startEngine is theEngine as the process starts, before any test
// replaces it.
var startEngine = theEngine

// TestTheEngineJudgesTheBoxsCredentialFile: the process's poller looks
// at the box's credential file; a test's never at the host's.
func TestTheEngineJudgesTheBoxsCredentialFile(t *testing.T) {
	if startEngine.backup.envFile != DefaultBackupEnvFile || newEngine().backup.envFile != "" {
		t.Fatalf("theEngine's %q, a test's %q", startEngine.backup.envFile, newEngine().backup.envFile)
	}
}

// TestStatusAlertsAreNotifysOwn: every alert status's rules name is one
// of motel's own here: raised at its level, listed, silenceable, and
// refused from an outside producer; its levels are notify's.
func TestStatusAlertsAreNotifysOwn(t *testing.T) {
	if status.AlertCritical != SeverityCritical || status.AlertWarning != SeverityWarning {
		t.Fatalf("status's levels %q %q", status.AlertCritical, status.AlertWarning)
	}
	// The prefix is motel's, a name status gives later included.
	if !reservedName("backup_offsite") || checkRuleName("backup_offsite") == nil {
		t.Errorf("backup_offsite is a producer's or a rule's to take")
	}
	silenceable := silenceableAlerts(&App{})
	for _, a := range status.Alerts {
		if m, ok := raisedBy(a.Name); !ok || m.severity != a.Severity {
			t.Errorf("%s: raised as %+v", a.Name, m)
		}
		if !slices.Contains(silenceable, a.Name) || !reservedName(a.Name) || checkRuleName(a.Name) == nil {
			t.Errorf("%s: not silenceable, or not refused from a producer or as a rule's name", a.Name)
		}
	}
}
