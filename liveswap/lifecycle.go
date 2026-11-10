package liveswap

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.uber.org/zap"
)

// The lifecycle events liveswap reports to a Notifier: #418's first
// events, and the deploy's start and end, which the notify module turns
// into the deploy mute (notify/DESIGN-notify.md, "Events").
const (
	EventWatchdogThrottled = "watchdog_throttled" // the restart budget is spent: a restart loop
	EventWatchdogRecovered = "watchdog_recovered" // healthy again after one; Hold says for how long it must stay so
	EventRelaunchRefused   = "relaunch_refused"   // a relaunch's pre-flight refused it: the app stays down
	EventRelaunchResolved  = "relaunch_resolved"  // an instance runs again after one
	EventDeployFailed      = "deploy_failed"
	EventDeployRefused     = "deploy_refused" // a 422: the request's own mistake
	EventDeploySucceeded   = "deploy_succeeded"
	EventRollback          = "rollback"
	EventDeployStarted     = "deploy_started"
	EventDeployEnded       = "deploy_ended"
)

// LifecycleEvent is one thing that happened to an app. Summary is one
// line in liveswap's own words, through the app's response filter: no
// secret and no log tail.
type LifecycleEvent struct {
	Name       string
	App        string
	Release    string
	DeployedBy string
	Summary    string
	// Hold, on EventWatchdogRecovered, is how long the app must stay
	// healthy before the recovery counts: max(30s, watchdog_grace), the
	// span after which the watchdog's own backoff resets.
	Hold time.Duration
	// Standing marks a report of what was told before and has not ended
	// (ReportStanding): a listener that holds the alert already ignores
	// it, and one that does not (a motel restarted since) takes it as
	// told — firing, or, for a recovery, held.
	Standing bool
}

// Notifier is told each lifecycle event, on the goroutine where it
// happened (the watchdog's, or the deploy's while it holds the app's
// deploy lock), so it must not block.
type Notifier interface {
	Lifecycle(LifecycleEvent)
}

// SetNotifier installs the notify module's listener on this config;
// Start hands it to every app it configures. Called from the notify
// module's Provision, after liveswap's own, so a config without a
// notify block reports to nobody.
func (a *App) SetNotifier(n Notifier) { a.notifier = n }

func (ma *managedApp) setNotifier(n Notifier) {
	ma.specMu.Lock()
	ma.notifier = n
	ma.specMu.Unlock()
}

// lifecycle reports ev for this app, with its summary through the
// app's filter.
func (ma *managedApp) lifecycle(c collaborators, ev LifecycleEvent) {
	ma.tell(c.notifier, ev)
}

func (ma *managedApp) tell(n Notifier, ev LifecycleEvent) {
	if n == nil {
		return
	}
	ev.App = ma.name
	if ev.Summary != "" {
		// The release is a name, which the entropy layer would mask when
		// it is a git sha (redact.go: a version is high-entropy by
		// design); it is a label beside the summary anyway.
		ev.Summary, _ = ma.redactorFor(statusSnapshot{}, ev.Release).Redact(ev.Summary)
	}
	n.Lifecycle(ev)
}

// ReportStanding tells n what each of this config's apps' watchdogs
// reported and has not ended: a restart loop (as its recovery, held, if
// the hold runs) and a refused relaunch, each a Standing report. The
// notify module calls it at its Start, after its engine is configured,
// so that the order in which Caddy starts the two apps does not matter.
// It is what keeps an alert across a motel restart: the flags are read
// back from each app's alerts.json, and nothing else would report a
// loop the watchdog was already in, nor a relaunch refused before the
// restart (recovery's own refusal says nothing). On a reload the
// listener holds each already, and ignores them.
func (a *App) ReportStanding(n Notifier) {
	for _, name := range slices.Sorted(maps.Keys(a.managed)) {
		ma, spec := a.managed[name], a.specs[name]
		if spec == nil {
			continue
		}
		ma.restoreAlerts(spec, a.logger.Named(name))
		ma.reportStanding(n, spec)
	}
}

func (ma *managedApp) reportStanding(n Notifier, spec *appSpec) {
	loop, recovering, refused := ma.wd.standing()
	release := ""
	if inst := ma.currentInstance(); inst != nil {
		release = inst.version
	}
	switch {
	case loop && recovering:
		ma.tell(n, LifecycleEvent{Name: EventWatchdogRecovered, Release: release, Hold: backoffResetAfter(spec), Standing: true,
			Summary: "healthy again after a restart loop"})
	case loop:
		ma.tell(n, LifecycleEvent{Name: EventWatchdogThrottled, Release: release, Standing: true,
			Summary: "the restart loop is still reported: it ends once the app is healthy for its hold"})
	}
	if refused {
		ma.tell(n, LifecycleEvent{Name: EventRelaunchRefused, Release: release, Standing: true,
			Summary: "the watchdog's relaunch was refused, so the app is down; still reported until an instance runs again"})
	}
}

// alertsMax bounds alerts.json as read: motel writes under 64 bytes.
const alertsMax = 4 << 10

// alertsTempPattern names persistAlerts' temp file (writeOwnFile).
const alertsTempPattern = ".alerts-*.tmp"

// restoreAlerts reads the app's alerts.json into its watchdog's flags,
// once per process, and points the writes of every later change at it
// (notify/DESIGN-notify.md, "Behaviour"). Read and written as
// state.json is (ownfile.go): never through a link.
func (ma *managedApp) restoreAlerts(spec *appSpec, logger *zap.Logger) {
	path := spec.dirs.alerts
	load := func() (reportedAlerts, error) {
		var r reportedAlerts
		data, _, err := readOwnFile(path, alertsMax, "an alerts file")
		if errors.Is(err, fs.ErrNotExist) {
			return r, nil
		}
		if err == nil {
			err = json.Unmarshal(data, &r)
		}
		return r, err
	}
	save := func(r reportedAlerts) error {
		removeOwnLeftovers(filepath.Dir(path), alertsTempPattern)
		var err error
		if r == (reportedAlerts{}) {
			if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		} else if data, merr := json.Marshal(r); merr != nil {
			err = merr
		} else if err = os.MkdirAll(filepath.Dir(path), 0o750); err == nil {
			err = writeOwnFile(path, alertsTempPattern, data)
		}
		if err != nil {
			logger.Warn("watchdog: what notify was told could not be kept for a restart; a restart now may let an alert lapse", zap.String("file", path), zap.Error(err))
		}
		return err
	}
	if err := ma.wd.restoreAlerts(load, save); err != nil {
		logger.Warn("watchdog: what notify was told before this start could not be read; taken as nothing", zap.String("file", path), zap.Error(err))
	}
}
