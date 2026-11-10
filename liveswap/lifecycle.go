package liveswap

import "time"

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
	if c.notifier == nil {
		return
	}
	ev.App = ma.name
	if ev.Summary != "" {
		ev.Summary, _ = ma.redactorFor(statusSnapshot{}).Redact(ev.Summary)
	}
	c.notifier.Lifecycle(ev)
}
