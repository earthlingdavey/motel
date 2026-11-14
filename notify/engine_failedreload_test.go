package notify

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/backups/record"
)

// notStarted keeps configure from starting the engine's goroutine: the
// test drives the engine's minute and its posts itself.
func notStarted(te *testEngine) {
	done := make(chan struct{})
	close(done)
	te.running, te.cancel, te.done = true, func() {}, done
}

// ends is the alertnames posted as ended (endsAt now or before).
func ends(te *testEngine, got []amAlert) map[string]bool {
	out := map[string]bool{}
	for _, a := range got {
		if !a.EndsAt.IsZero() && !a.EndsAt.After(te.at) {
			out[a.Labels["alertname"]] = true
		}
	}
	return out
}

// TestFailedReloadEndsNothing: a reload's candidate drops the app blog,
// the backup record and the filesystem measured, while each has an alert
// firing (liveswap's, a producer's, the backup's, the disk's) and blog's
// deploy is muted. Another app's Start fails after notify's, Caddy stops
// the candidate, and the old config serves on: nothing it runs was
// resolved, the table and external.json are as before, and the alerts
// are re-posted; what was told in the gap for wiki, an app only the
// candidate runs, is ended. The reload that then succeeds ends the
// rest at the old config's Stop. A boot settles at configure.
func TestFailedReloadEndsNothing(t *testing.T) {
	dir := t.TempDir()
	useStateDir(t, dir)
	file := filepath.Join(dir, externalName)
	te := newTestEngine(t)
	notStarted(te)
	old := &App{cfg: &resolved{apps: []string{"blog", "shop"}}, diskPaths: []string{"/var/lib/motel"}, logger: te.logger}
	te.configure(old, te.p, te.known, te.logger)
	// A boot settles at once: the pollers read the config's sources.
	if te.backupPath != DefaultBackupRecord || !slices.Equal(te.diskPaths, old.diskPaths) {
		t.Fatalf("after the boot's configure: backup %q, disks %v", te.backupPath, te.diskPaths)
	}
	producer := external(true)
	producer.App = "blog"
	for _, ev := range []Event{
		watchdog(true), producer,
		{Name: "drill_failed", Kind: kindState, Severity: SeverityCritical, Source: "backup", Summary: "x", Firing: true},
		{Name: alertDiskLow, Kind: kindState, Severity: SeverityWarning, Source: "disk", Instance: "/var/lib/motel", Summary: "x", Firing: true},
	} {
		te.report(ev)
	}
	te.mute("blog", true)
	te.drain()
	kept, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	candidate := func() *App {
		return &App{BackupRecord: "off", cfg: &resolved{apps: []string{"shop", "wiki"}}, logger: te.logger}
	}
	resolvedLines := 0 // the journal's resolved lines so far
	same := func(when string) {
		t.Helper()
		if got := ends(te, te.drain()); len(got) != 0 {
			t.Fatalf("%s: ended %v", when, got)
		}
		if l := te.logs.FilterMessage("monitor: event").FilterField(zap.String("state", "resolved")).All(); len(l) != resolvedLines {
			t.Fatalf("%s: journal %+v", when, l)
		}
		if len(te.states) != 4 || len(te.mutes) != 1 {
			t.Fatalf("%s: states %v, mutes %v", when, te.states, te.mutes)
		}
		if b, _ := os.ReadFile(file); string(b) != string(kept) {
			t.Fatalf("%s: external.json %s, was %s", when, b, kept)
		}
	}

	nu := candidate()
	te.configure(nu, te.p, te.known, te.logger)
	same("the candidate's Start")
	// In the gap, the candidate's liveswap reports for wiki, an app only
	// the candidate runs: the old config, serving on, can never clear it.
	wiki := Event{Name: "relaunch_refused", Kind: kindState, Severity: SeverityCritical, App: "wiki", Source: "liveswap", Summary: "x", Firing: true}
	te.report(wiki)
	te.drain()
	te.release(nu) // another app's Start failed
	if te.active != old {
		t.Fatal("the engine did not go back to the serving config")
	}
	if got := ends(te, te.drain()); len(got) != 1 || !got["relaunch_refused"] || te.states[wiki.key()] != nil {
		t.Fatalf("the fallback did not end wiki's alert: ended %v, state %v", got, te.states[wiki.key()])
	}
	resolvedLines++
	same("the fallback")
	te.advance(time.Minute)
	te.tick()
	if got := te.drain(); countAlert(got, "watchdog_throttled") != 1 || countAlert(got, alertDiskLow) != 1 ||
		countAlert(got, "disk_failing") != 1 || countAlert(got, "drill_failed") != 1 || countAlert(got, alertDeployMute) != 1 {
		t.Fatalf("not re-posted after the fallback: %+v", got)
	}
	same("the next minute")

	// The same candidate again, and this time the reload succeeds.
	nu = candidate()
	te.configure(nu, te.p, te.known, te.logger)
	same("the second candidate's Start")
	te.release(old)
	got := ends(te, te.drain())
	for _, n := range []string{"watchdog_throttled", "disk_failing", "drill_failed", alertDiskLow, alertDeployMute} {
		if !got[n] {
			t.Errorf("%s not ended at the old config's Stop: %v", n, got)
		}
	}
	if len(te.states) != 0 || len(te.mutes) != 0 {
		t.Fatalf("left: states %v, mutes %v", te.states, te.mutes)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("external.json after the reload: %v", err)
	}
}

// TestFailedReloadKeepsTheHeartbeatHold: a candidate without the channel
// whose failure holds the heartbeat does not resume it, unless it serves.
func TestFailedReloadKeepsTheHeartbeatHold(t *testing.T) {
	useStateDir(t, t.TempDir())
	te, a, body, read := holdEngine(t, oneChannelBlock)
	notStarted(te)
	te.configure(a, te.p, te.known, te.logger)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	te.scrapeFailed(context.Background(), read)
	te.advance(heartbeatHoldAfter)
	te.scrapeFailed(context.Background(), read)
	if !te.heartbeatHeld() {
		t.Fatal("not held")
	}
	candidate := func() *App {
		b := mustParse(t, `monitor {
	channel bob json {secret.NOTIFY_BOB_URL}
	operator bob
	heartbeat {secret.NOTIFY_HC_URL}
}`)
		b.cfg = mustResolve(t, b)
		_, _, b.receivers = render(b.cfg, testStateDir)
		return b
	}
	nu := candidate()
	te.configure(nu, te.p, te.known, te.logger)
	te.release(nu)
	if !te.heartbeatHeld() || heartbeats(te) != 0 {
		t.Fatal("a failed reload that dropped the channel resumed the heartbeat")
	}
	if l := te.lines("monitor: heartbeat resumed"); len(l) != 0 {
		t.Fatalf("resumed lines = %+v", l)
	}
	nu = candidate()
	te.configure(nu, te.p, te.known, te.logger)
	te.release(a)
	if te.heartbeatHeld() {
		t.Fatal("the reload that serves keeps the hold")
	}
	if l := te.lines("monitor: heartbeat resumed"); len(l) != 1 {
		t.Fatalf("resumed lines = %+v", l)
	}
}

// TestPollSettledMidReadTellsNothingDropped: a poll reads the sources
// outside the lock, and a reload that drops them serves (the old
// config's Stop settles the new one) while it reads: what it read of
// them is neither told nor listed. Told, the backup's drill_failed would
// fire again with nothing to clear it (backup_record off reads nothing),
// and the disk's would fire, then recover a poll later.
func TestPollSettledMidReadTellsNothingDropped(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "status.json")
	disk := filepath.Join(dir, "data") // its own path: the stats in flight are the process's
	te := newTestEngine(t)
	writeRecord(t, rec, &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK, LastOK: &record.Snapshot{ID: "a", Time: te.at}}}, LastDrill: &record.Drill{Time: te.at, Detail: "could not begin"}})
	old := &App{BackupRecord: rec, diskPaths: []string{disk}, cfg: &resolved{}, logger: te.logger}
	te.setConfig(old, te.p, te.known, te.logger)
	nu := &App{BackupRecord: "off", cfg: &resolved{}, logger: te.logger}
	var midRead atomic.Bool
	var once sync.Once
	te.disk.stat = func(string) (diskNumbers, error) {
		if midRead.Load() {
			once.Do(func() {
				te.mu.Lock()
				te.settle(nu)
				te.mu.Unlock()
			})
		}
		return diskNumbers{usedPct: 95, dev: 1}, nil
	}
	drill := Event{Name: "drill_failed", Source: "backup"}.key()
	full := Event{Name: alertDiskLow, Source: "disk", Instance: disk}.key()
	te.poll(context.Background())
	te.drain()
	if te.states[drill] == nil || te.states[full] == nil {
		t.Fatalf("the first poll fired %v", te.states)
	}
	if len(te.seen.backups) != 1 {
		t.Fatalf("the first poll listed backups %v", te.seen.backups)
	}

	te.mu.Lock()
	te.point(nu, te.p, te.known, te.logger) // the candidate's Start
	te.mu.Unlock()
	te.advance(backupEvery)
	midRead.Store(true)
	te.poll(context.Background())
	for _, a := range te.drain() {
		if n := a.Labels["alertname"]; (n == "drill_failed" || n == alertDiskLow) && a.EndsAt.IsZero() {
			t.Errorf("%s fired again after the reload dropped it", n)
		}
	}
	if st := te.states[drill]; st != nil {
		t.Fatalf("drill_failed is back in the table: %+v", st)
	}
	if st := te.states[full]; st != nil {
		t.Fatalf("disk_low is back in the table: %+v", st)
	}
	if len(te.seen.backups) != 0 || len(te.seen.disks) != 0 {
		t.Fatalf("listed: backups %v, disks %v", te.seen.backups, te.seen.disks)
	}
	te.advance(time.Minute)
	te.tick()
	if got := te.drain(); countAlert(got, "drill_failed") != 0 || countAlert(got, alertDiskLow) != 0 {
		t.Fatalf("posted a minute on: %+v", got)
	}
}
