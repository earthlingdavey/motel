package notify

import (
	"slices"
	"strings"

	"github.com/smallhoursorg/motel/liveswap"
)

// lifecycleAlerts are liveswap's, about one app; releaseAlerts those
// among them, with post_deploy_regressed, whose release label names a
// release motel knows when it fires: on disk (a rollback's target, a
// deploy's success, a regression after it) or in the app's deploy
// records (a failed deploy, whose directory is removed at once but
// whose outcome is recorded once the deploy was accepted, and if the
// record write did not fail: liveswap/deploys.go). deploy_refused
// carries the release too, but a refusal writes no record and its
// release may be any name a deployer sent, so it is kept out.
var (
	lifecycleAlerts = []string{
		liveswap.EventWatchdogThrottled, liveswap.EventRelaunchRefused,
		liveswap.EventDeployFailed, liveswap.EventDeployRefused, liveswap.EventRollback, liveswap.EventDeploySucceeded,
	}
	releaseAlerts = []string{liveswap.EventRollback, liveswap.EventDeploySucceeded, liveswap.EventDeployFailed, eventPostDeployRegressed}
)

// raisedAlerts are the alert names motel's own detectors raise on any
// box with a monitor block: liveswap's lifecycle (detectors.go,
// lifecycleSink: a recovery resolves its firing's name), the tls,
// backup and disk pollers, and channel_failed.
// TestSilenceableAlertsAreWhatMotelRaises holds this list to the names
// the package's emitters spell out, both ways.
var raisedAlerts = append(slices.Clone(lifecycleAlerts),
	"cert_failed", alertDiskLow,
	"backup_unreadable", "backup_stale", "backup_run_failed", "drill_failed",
	alertChannelFailed,
)

// storeAlerts are what a block with a metrics line adds: the store's
// chain (metricspush.go), beside the vmalert rules rendered for this
// config (fixedRules, operatorRules) and, with a post_deploy rule, its
// event.
var storeAlerts = []string{eventStoreDown, eventRulesDown, eventStoreReadOnly}

// notSilenceable are names motel posts that no silence may take:
// Watchdog is the heartbeat, whose silence would make the check off the
// box report the box down; NotifyTest is `motel monitor test`'s own,
// and deploy_in_progress the deploy mute, which reaches no channel.
var notSilenceable = []string{alertWatchdog, alertNotifyTest, alertDeployMute}

// silenceableAlerts are the alert names `motel monitor silence add
// --alert` takes on the config a: sorted, each once.
func silenceableAlerts(a *App) []string {
	names := slices.Clone(raisedAlerts)
	if a != nil && a.Metrics != nil {
		names = append(names, storeAlerts...)
		for _, g := range []vmGroup{fixedRules(), operatorRules(a)} {
			for _, r := range g.Rules {
				names = append(names, r.Alert)
			}
		}
		if slices.ContainsFunc(a.Metrics.Rules, func(r Rule) bool { return r.Kind == rulePostDeploy }) {
			names = append(names, eventPostDeployRegressed)
		}
	}
	slices.Sort(names)
	return slices.DeleteFunc(slices.Compact(names), func(n string) bool { return slices.Contains(notSilenceable, n) })
}

// alertScope is what an alert name's labels can be narrowed by on the
// config a: app reports an alert about an app (an app label:
// liveswap's, post_deploy_regressed, and the vmalert rules whose
// subject is an app's), pinned the one app a rule of one app's is
// about, and release one whose release label names a release motel
// knows (releaseAlerts).
func alertScope(a *App, name string) (app bool, pinned string, release bool) {
	if slices.Contains(releaseAlerts, name) {
		return true, "", true
	}
	if slices.Contains(lifecycleAlerts, name) {
		return true, "", false
	}
	if a != nil && a.Metrics != nil {
		for _, g := range []vmGroup{fixedRules(), operatorRules(a)} {
			for _, r := range g.Rules {
				subject, ok := strings.CutPrefix(r.Labels["subject"], "app:")
				if r.Alert != name || !ok {
					continue
				}
				if strings.Contains(subject, "{{") {
					subject = ""
				}
				return true, subject, false
			}
		}
	}
	return false, "", false
}
