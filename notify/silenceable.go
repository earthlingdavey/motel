package notify

import (
	"slices"

	"github.com/smallhoursorg/motel/liveswap"
)

// raisedAlerts are the alert names motel's own detectors raise on any
// box with a monitor block: liveswap's lifecycle (detectors.go,
// lifecycleSink: a recovery resolves its firing's name), the tls,
// backup and disk pollers, and channel_failed.
// TestSilenceableAlertsAreWhatMotelRaises holds this list to the names
// the package's emitters spell out, both ways.
var raisedAlerts = []string{
	liveswap.EventWatchdogThrottled, liveswap.EventRelaunchRefused,
	liveswap.EventDeployFailed, liveswap.EventDeployRefused, liveswap.EventRollback, liveswap.EventDeploySucceeded,
	"cert_failed", alertDiskLow,
	"backup_unreadable", "backup_stale", "backup_run_failed", "drill_failed",
	alertChannelFailed,
}

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
