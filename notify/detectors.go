package notify

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sort"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/status"
	"github.com/smallhoursorg/motel/liveswap"
)

// lifecycleSink is liveswap's Notifier: #418's first events, as
// liveswap reports them (liveswap/lifecycle.go), into the engine; and a
// deploy's or a rollback's cutover, told at the swap, into the
// post-deploy comparison (c, nil for none). The cutover is no event of
// the engine's: deploy_succeeded, which it drops unless
// `deploy_succeeded on`, comes after the drain.
type lifecycleSink struct {
	e *engine
	c *comparator
}

func (s lifecycleSink) Lifecycle(ev liveswap.LifecycleEvent) {
	if ev.Name == liveswap.EventCutover {
		if s.c != nil {
			s.c.deployed(ev)
		}
		return
	}
	base := Event{App: ev.App, Source: "liveswap", Summary: ev.Summary, Release: ev.Release, DeployedBy: ev.DeployedBy, Standing: ev.Standing}
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

// sourceAccess is the source of the two events about a person's key.
const sourceAccess = "access"

// peopleSink is liveswap's Announcer: a person's key used from a
// network it was never accepted from, or a valid signature from outside
// every address list on their grants (liveswap/people_networks.go), as
// a one-shot box event at warning to the operator's channels. Its key
// is the key's fingerprint (access/box/<name>/<fingerprint>), so the
// limit per key bounds a thief who cycles networks: four at once, then
// one a quarter-hour, each refused one a journal line.
type peopleSink struct{ e *engine }

func (s peopleSink) Announce(a liveswap.Announcement) {
	oneShot := func(name string) {
		s.e.report(Event{Name: name, Kind: kindEvent, Severity: SeverityWarning, Source: sourceAccess,
			Instance: a.Key, Summary: a.Summary(), Safe: a.Safe()})
	}
	switch a.Name {
	case liveswap.EventPersonNewNetwork, liveswap.EventPersonOutsideList:
		oneShot(a.Name)
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

// DefaultBackupEnvFile is the backup credential file, whose presence
// says backups are set up and whose time says since when. Only its name
// is looked at (Lstat), as every account's look does (backups/look): its
// directory is root's 0755, the file root's 0600.
const DefaultBackupEnvFile = "/etc/motel-backup/repository.env"

// backupPoller is #418's backup source. status is the only judge: each
// problem line of status.Report, on the look any account on the box
// takes (backups/look, which GET /_motel/backups and `motel-backup
// status` read) but for the units the manager holds, which status says
// and never judges, is an alert of the name and level written at its
// rule (status.Alerts), with the app as its instance where the line is
// an app's. A box with no credential file has no backups set up, and
// nothing to say; one whose file cannot be looked at, or whose record
// cannot be read, has no verdict: backup_unreadable, beside what fired.
// It is the engine's, and outlives a config load, so what it reported
// firing is cleared by whichever config is serving when it clears.
type backupPoller struct {
	last time.Time
	set  stateSet
	// read is each app's last complete backup as the last record read
	// that could be read held it, and readAt when: what `moteld monitor
	// rules` lists beside backup_stale. A new map at each read.
	read   map[string]time.Time
	readAt time.Time
	// readPath is the record read is from: another path's is dropped.
	readPath string
	// envFile is the credential file looked at: theEngine's is
	// DefaultBackupEnvFile; a test's none, so a test never judges the
	// host's backups.
	envFile string
}

func (b *backupPoller) poll(now time.Time, path string) []Event {
	if path != b.readPath {
		b.read, b.readPath = nil, path
	}
	if path == "" || (!b.last.IsZero() && now.Sub(b.last) < backupEvery) {
		return nil
	}
	b.last = now
	// No verdict puts nothing right: what fired stays fired, beside
	// backup_unreadable, which says why there is none.
	unknown := func(why string) []Event {
		kept := slices.DeleteFunc(slices.Collect(maps.Values(b.set.prev)), func(e Event) bool { return e.Name == alertBackupUnreadable })
		return b.set.diff(append(kept, Event{Name: alertBackupUnreadable, Kind: kindState, Severity: SeverityWarning, Source: "backup", Summary: why}))
	}
	// In look's order: whether backups are set up, then the record.
	env, err := os.Lstat(b.envFile)
	if errors.Is(err, fs.ErrNotExist) {
		b.read = nil
		return b.set.diff(nil)
	} else if err != nil {
		// The client's read is moteld's look, so it says why; root's
		// look on the box can see the file, and says the backups' health.
		return unknown(b.envFile + " cannot be looked at by moteld, so the backups' health is not known; " +
			"motel backup status says why (on the box, as root, motel-backup status says their health)")
	}
	st, err := record.Read(path)
	if err != nil {
		return unknown("the backup record cannot be read, so whether the backups are fresh is not known")
	}
	b.read, b.readAt = map[string]time.Time{}, now
	for name, a := range st.Apps {
		if a != nil && a.LastOK != nil {
			b.read[name] = a.LastOK.Time
		}
	}
	var fire []Event
	for _, l := range status.Report(status.Input{Record: st, Now: now, SetUp: env.ModTime()}).Lines {
		if l.Level != status.Problem {
			continue
		}
		// A box event about an app: the app is the instance, never the
		// app label, so it reaches the operator alone. The pointer first:
		// a summary is cut at maxSummary, and the line's tail is what goes.
		fire = append(fire, Event{Name: l.Alert, Kind: kindState, Severity: l.Severity, Source: "backup", Instance: l.App,
			Summary: backupPointer + l.Text})
	}
	return b.set.diff(fire)
}

// alertBackupUnreadable is the one backup alert that is no rule of
// status's: the record does not parse, or the credential file cannot be
// looked at, so status could not tell.
const alertBackupUnreadable = "backup_unreadable"

// backupPointer begins each backup alert's summary: where the rest is
// said, the laptop client's read first (a person with the viewer role
// on the box), then the box's own command.
const backupPointer = "motel backup status (on the box: motel-backup status) says: "

// diskPoller is #418's disk source: statfs on the filesystems under
// motel's directories, one per device, every minute.
type diskPoller struct {
	set   stateSet
	level map[uint64]string // device → the severity it fires at: the
	// filesystem's, whichever path stands for it as the path list changes
	stat func(path string) (diskNumbers, error)
	// read is the last poll's measure of each filesystem, under the
	// path that stood for it: what `moteld monitor rules` lists beside
	// disk_low. A new slice at each poll.
	read []diskReading
}

// diskReading is one filesystem's used share and the severity it fires
// at (none, warning or critical), and when it was measured: the
// engine stamps a poll's own (zero here) with the poll's time, and one
// carried over from an earlier poll keeps its own.
type diskReading struct {
	path    string
	usedPct float64
	level   string
	at      time.Time
	dev     uint64
}

// The thresholds: warning at 80 % used, critical at 90; each clears 5
// points under (#418).
const (
	diskWarn = 80
	diskCrit = 90
	diskGap  = 5
)

// diskNumbers are a filesystem's, as df reads them: the disk source's
// used share, and the disk series' available bytes (metricscollector.go).
type diskNumbers struct {
	usedPct float64
	avail   uint64
	dev     uint64
}

// statDisk is statfs and the device of the filesystem holding path.
func statDisk(path string) (diskNumbers, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return diskNumbers{}, err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return diskNumbers{}, err
	}
	return fsNumbers(fs, st.Dev), nil
}

// fsNumbers reads statfs's counts as df does: blocks of f_frsize (the
// fragment size, which f_bsize, the preferred I/O size, need not be),
// f_bsize where a filesystem leaves f_frsize 0.
func fsNumbers(fs syscall.Statfs_t, dev uint64) diskNumbers {
	unit := uint64(fs.Frsize) //nolint:gosec // a block size is positive
	if unit == 0 {
		unit = uint64(fs.Bsize) //nolint:gosec // a block size is positive
	}
	n := diskNumbers{avail: fs.Bavail * unit, dev: dev}
	// Used over what a user can have.
	if used := fs.Blocks - fs.Bfree; used+fs.Bavail > 0 {
		n.usedPct = 100 * float64(used) / float64(used+fs.Bavail)
	}
	return n
}

func (d *diskPoller) poll(paths []string) []Event {
	if d.level == nil {
		d.level = map[uint64]string{}
	}
	stat := d.stat
	if stat == nil {
		stat = statDisk
	}
	// On their own goroutines, as the disk series' are (statAll): a
	// statfs that hangs (a network mount whose server went away) must
	// not hold the engine's goroutine, which posts every event and the
	// heartbeat.
	results := diskPollStats.statAll(paths, stat)
	seen := map[uint64]bool{}
	var fire []Event
	read := []diskReading{}
	for i, p := range paths {
		n, err := results[i].n, results[i].err
		if err != nil {
			// Not measured is not cleared: a path that fired keeps
			// firing, and one that did not fires nothing; a statfs not
			// back in time is not measured.
			k := Event{Name: alertDiskLow, Source: "disk", Instance: p}.key()
			if prev, ok := d.set.prev[k]; ok {
				fire = append(fire, prev)
			}
			// And its last reading stays listed, with its own time,
			// unless another path measures its filesystem now (below).
			if j := slices.IndexFunc(d.read, func(r diskReading) bool { return r.path == p }); j >= 0 {
				read = append(read, d.read[j])
			}
			continue
		}
		used, dev := n.usedPct, n.dev
		if seen[dev] {
			continue // a filesystem already measured
		}
		seen[dev] = true
		level := diskLevel(d.level[dev], used)
		d.level[dev] = level
		read = append(read, diskReading{path: p, usedPct: used, level: level, dev: dev})
		if level == "" {
			continue
		}
		fire = append(fire, Event{Name: alertDiskLow, Kind: kindState, Severity: level, Source: "disk", Instance: p,
			Summary: fmt.Sprintf("the filesystem holding %s is %.0f%% full", p, used)})
	}
	d.read = slices.DeleteFunc(read, func(r diskReading) bool { return !r.at.IsZero() && seen[r.dev] })
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
	_ liveswap.Notifier  = lifecycleSink{}
	_ liveswap.Announcer = peopleSink{}
)
