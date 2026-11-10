package notify

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/status"
	"github.com/smallhoursorg/motel/liveswap"
)

// lifecycleSink is liveswap's Notifier: #418's first events, as
// liveswap reports them (liveswap/lifecycle.go), into the engine.
type lifecycleSink struct{ e *engine }

func (s lifecycleSink) Lifecycle(ev liveswap.LifecycleEvent) {
	base := Event{App: ev.App, Source: "liveswap", Summary: ev.Summary, Release: ev.Release, DeployedBy: ev.DeployedBy}
	state := func(name, sev string, firing bool) {
		base.Name, base.Kind, base.Severity, base.Firing, base.Hold = name, kindState, sev, firing, ev.Hold
		s.e.report(base)
	}
	oneShot := func(sev string) {
		base.Name, base.Kind, base.Severity = ev.Name, kindEvent, sev
		s.e.report(base)
	}
	switch ev.Name {
	case liveswap.EventWatchdogThrottled:
		state(liveswap.EventWatchdogThrottled, SeverityCritical, true)
	case liveswap.EventWatchdogRecovered:
		state(liveswap.EventWatchdogThrottled, SeverityCritical, false)
	case liveswap.EventRelaunchRefused:
		state(liveswap.EventRelaunchRefused, SeverityCritical, true)
	case liveswap.EventRelaunchResolved:
		state(liveswap.EventRelaunchRefused, SeverityCritical, false)
	case liveswap.EventDeployFailed, liveswap.EventDeployRefused, liveswap.EventRollback:
		oneShot(SeverityWarning)
	case liveswap.EventDeploySucceeded:
		oneShot(SeverityInfo)
	case liveswap.EventDeployStarted:
		s.e.mute(ev.App, true)
	case liveswap.EventDeployEnded:
		s.e.mute(ev.App, false)
	}
}

// tlsHandler is subscribed by name to CertMagic's cert_failed and
// cert_obtained, which Caddy's tls app emits through the events app.
type tlsHandler struct{ e *engine }

// certCriticalUnder is #418's: a failed renewal is critical with under
// a week left.
const certCriticalUnder = 7 * 24 * time.Hour

func (h tlsHandler) Handle(_ context.Context, e caddy.Event) error {
	id, _ := e.Data["identifier"].(string)
	if id == "" {
		return nil
	}
	ev := Event{Name: "cert_failed", Kind: kindState, Severity: SeverityWarning, Source: "tls", Instance: id}
	switch e.Name() {
	case "cert_failed":
		ev.Firing = true
		renewal, _ := e.Data["renewal"].(bool)
		left, hasLeft := e.Data["remaining"].(time.Duration)
		switch {
		case renewal && hasLeft:
			ev.Summary = fmt.Sprintf("the certificate for %s could not be renewed; it expires in %s", id, left.Round(time.Hour))
			if left < certCriticalUnder {
				ev.Severity = SeverityCritical
			}
		case renewal:
			ev.Summary = "the certificate for " + id + " could not be renewed"
		default:
			ev.Summary = "no certificate could be obtained for " + id
		}
	case "cert_obtained":
		ev.Firing = false
	default:
		return nil
	}
	h.e.report(ev)
	return nil
}

// stateSet turns "these keys fire now" into the reports that move the
// engine there: each firing one, and a clear for each that fired last
// time and does not now.
type stateSet struct{ prev map[string]Event }

func (s *stateSet) diff(now []Event) []Event {
	cur := map[string]Event{}
	out := make([]Event, 0, len(now))
	for _, ev := range now {
		ev.Firing = true
		cur[ev.key()] = ev
		out = append(out, ev)
	}
	keys := make([]string, 0, len(s.prev))
	for k := range s.prev {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := cur[k]; !ok {
			ev := s.prev[k]
			ev.Firing = false
			out = append(out, ev)
		}
	}
	s.prev = cur
	return out
}

// DefaultBackupRecord is the record motel-backup writes, which is for
// everyone to read (backups/record): the serving process reading it is
// the permitted direction of trust (#418, its corrections to #137).
const DefaultBackupRecord = "/var/lib/motel-backup/status.json"

// backupEvery is how often the record is read (#418).
const backupEvery = 5 * time.Minute

// backupPoller is #418's backup source. A box with no record has no
// backups set up, and nothing to say. It is the engine's, and outlives
// a config load, so what it reported firing is cleared by whichever
// config is serving when it clears.
type backupPoller struct {
	last time.Time
	set  stateSet
}

func (b *backupPoller) poll(now time.Time, path string) []Event {
	if path == "" || (!b.last.IsZero() && now.Sub(b.last) < backupEvery) {
		return nil
	}
	b.last = now
	ev := func(name, sev, instance, summary string) Event {
		return Event{Name: name, Kind: kindState, Severity: sev, Source: "backup", Instance: instance, Summary: summary}
	}
	var fire []Event
	st, err := record.Read(path)
	if err != nil {
		// What fired stays fired: an unreadable record says nothing has
		// been put right.
		for _, prev := range b.set.prev {
			if prev.Name != "backup_unreadable" {
				fire = append(fire, prev)
			}
		}
		fire = append(fire, ev("backup_unreadable", SeverityWarning, "", "the backup record cannot be read, so whether the backups are fresh is not known"))
		return b.set.diff(fire)
	}
	failed := st.Error != ""
	var unproven []string
	apps := make([]string, 0, len(st.Apps))
	for name := range st.Apps {
		apps = append(apps, name)
	}
	slices.Sort(apps)
	for _, name := range apps {
		a := st.Apps[name]
		if a == nil {
			continue
		}
		// motel-backup's own rule for a run (`run` exits failed): every
		// app ok, or pending its first data.
		if a.Class != record.OK && a.Class != record.Pending {
			failed = true
		}
		// The newest snapshot's drill that proved nothing, as `status`
		// reads it: not made good by an older proof.
		if a.RestoreDrill != nil {
			unproven = append(unproven, name)
		}
		// A box event about an app: the app is the instance, never
		// the app label, so it reaches the operator alone.
		if a.LastOK != nil && now.Sub(a.LastOK.Time) > status.StaleAfter {
			fire = append(fire, ev("backup_stale", SeverityCritical, name,
				fmt.Sprintf("the backup of %s is stale: the last complete one is from %s", name, record.When(a.LastOK.Time))))
		}
	}
	if failed {
		fire = append(fire, ev("backup_run_failed", SeverityWarning, "", "the last backup run failed or ended early; motel-backup status says what"))
	}
	if d := st.LastDrill; (d != nil && d.Detail != "") || (st.LastCheck != nil && st.LastCheck.Class == record.CheckDamaged) || len(unproven) > 0 {
		summary := "the last restore drill proved nothing or found the repository damaged; motel-backup status says what"
		if len(unproven) > 0 {
			summary = "the last restore drill did not prove " + strings.Join(unproven, ", ") + "; motel-backup status says why"
		}
		fire = append(fire, ev("drill_failed", SeverityCritical, "", summary))
	}
	return b.set.diff(fire)
}

// diskPoller is #418's disk source: statfs on the filesystems under
// motel's directories, one per device, every minute.
type diskPoller struct {
	set   stateSet
	level map[string]string // instance → the severity it fires at
	stat  func(path string) (used float64, dev uint64, err error)
}

// The thresholds: warning at 80 % used, critical at 90; each clears 5
// points under (#418).
const (
	diskWarn = 80
	diskCrit = 90
	diskGap  = 5
)

func statfs(path string) (float64, uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, 0, err
	}
	used := fs.Blocks - fs.Bfree
	if used+fs.Bavail == 0 {
		return 0, st.Dev, nil
	}
	// As df reports it: used over what a user can have.
	return 100 * float64(used) / float64(used+fs.Bavail), st.Dev, nil
}

func (d *diskPoller) poll(paths []string) []Event {
	if d.level == nil {
		d.level = map[string]string{}
	}
	stat := d.stat
	if stat == nil {
		stat = statfs
	}
	seen := map[uint64]bool{}
	var fire []Event
	for _, p := range paths {
		used, dev, err := stat(p)
		if err != nil {
			// Not measured is not cleared: a path that fired keeps firing.
			k := Event{Name: alertDiskLow, Source: "disk", Instance: p}.key()
			if prev, ok := d.set.prev[k]; ok {
				fire = append(fire, prev)
			}
			continue
		}
		if seen[dev] {
			continue // a filesystem already measured
		}
		seen[dev] = true
		level := diskLevel(d.level[p], used)
		d.level[p] = level
		if level == "" {
			continue
		}
		fire = append(fire, Event{Name: alertDiskLow, Kind: kindState, Severity: level, Source: "disk", Instance: p,
			Summary: fmt.Sprintf("the filesystem holding %s is %.0f%% full", p, used)})
	}
	return d.set.diff(fire)
}

// diskLevel is the severity a filesystem used% fires at, given the one
// it fired at before: each level clears diskGap points under its
// threshold.
func diskLevel(prev string, used float64) string {
	switch {
	case used >= diskCrit:
		return SeverityCritical
	case prev == SeverityCritical && used >= diskCrit-diskGap:
		return SeverityCritical
	case used >= diskWarn:
		return SeverityWarning
	case prev != "" && used >= diskWarn-diskGap:
		return SeverityWarning
	}
	return ""
}

var (
	_ liveswap.Notifier = lifecycleSink{}
)
