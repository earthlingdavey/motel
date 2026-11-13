package notify

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// RulesName is the rule file's name in the metrics state directory;
// motel-vmalert.service's -rule names it.
const RulesName = "rules.yml"

const rulesHeader = "# Rendered by motel (notify/DESIGN-metrics.md, \"Rules\").\n" +
	"# Do not edit: motel rewrites this file on every config load, and vmalert\n" +
	"# re-reads it within a minute (-configCheckInterval).\n"

// Label values the store's rules set, beside the kind and severity
// notify's route tree reads.
const (
	sourceVMAlert = "vmalert"

	ruleStoreDiskFill = "metrics_store_disk_fill"
	ruleAppMemoryHigh = "app_memory_high"
)

const storeDiskFillExpr = "min(predict_linear(vm_free_disk_space_bytes[6h], 86400)) < 0" +
	" and min(predict_linear(vm_free_disk_space_bytes[1h], 86400)) < 0"

type vmRuleFile struct {
	Groups []vmGroup `yaml:"groups"`
}

type vmGroup struct {
	Name     string   `yaml:"name"`
	Interval string   `yaml:"interval"`
	Rules    []vmRule `yaml:"rules"`
}

type vmRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for,omitempty"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// stateLabels are what notify's route tree routes a store rule by, as
// one of motel's own state alerts: route 6, the subject's receivers.
func stateLabels(name, severity, subject string) map[string]string {
	return map[string]string{
		"kind": kindState, "severity": severity, "subject": subject,
		"source": sourceVMAlert, "key": sourceVMAlert + "/" + subject + "/" + name,
	}
}

// fixedRules are the store's own: motel's text, nothing configurable.
// The store being read-only is not among them: no rule can see it
// (metricspush.go, checkReadOnly).
func fixedRules() vmGroup {
	app := stateLabels(ruleAppMemoryHigh, SeverityWarning, "app:{{ $labels.app }}")
	return vmGroup{Name: "motel-store", Interval: clockText("1m"), Rules: []vmRule{
		{
			// Both windows: free space is the whole filesystem's, and one
			// large write (a release, an upgrade) tilts the six-hour line
			// below zero for hours. The hour's line is flat again once the
			// write is an hour old.
			Alert: ruleStoreDiskFill, Expr: storeDiskFillExpr, For: clockText("30m"),
			Labels:      stateLabels(ruleStoreDiskFill, SeverityWarning, "box"),
			Annotations: map[string]string{"summary": "the metrics store's disk is predicted to fill within a day"},
		},
		{
			Alert: ruleAppMemoryHigh, Expr: "max by (app) (motel_app_memory_bytes / motel_app_memory_max_bytes) > 0.9", For: clockText("10m"),
			Labels:      app,
			Annotations: map[string]string{"summary": "{{ $labels.app }} has used over 90 % of its memory cap for ten minutes"},
		},
	}}
}

// channelFailedRules is vmalert's second source of ChannelFailed
// (DESIGN-metrics.md, "Rules"): for each receiver of the notify render,
// each integration among its channels, and each operator channel not
// among that receiver's channels of that integration (channel_failed's
// suspects), one rule whose labels are exactly those motel's own
// ChannelFailed carries, so that Alertmanager holds the two as one
// alert. Receivers sharing all three are one rule. The heartbeat's
// receiver has no channel, and is motel's alone.
func channelFailedRules(n *App) vmGroup {
	g := vmGroup{Name: "motel-channel-failed", Interval: clockText("1m"), Rules: []vmRule{}}
	if n == nil || n.cfg == nil {
		return g
	}
	// channel_failed's own reading of a failure: one definition of the
	// suspects, so that the two sources cannot drift apart.
	w := failWatch{kinds: map[string]string{}, channels: n.receivers}
	for name, ch := range n.cfg.channels {
		w.kinds[name] = channelKinds[ch.Kind]
	}
	type told struct{ channel, integration, suspects string }
	receivers := map[told][]string{}
	for name, chans := range n.receivers {
		var ints []string
		for _, ch := range chans {
			if k := w.kinds[ch]; k != "" && !slices.Contains(ints, k) {
				ints = append(ints, k)
			}
		}
		for _, in := range ints {
			sus := w.suspects(failedSample{integration: in, receiver: name})
			for _, op := range n.cfg.operator {
				if !slices.Contains(sus, op) {
					k := told{op, in, strings.Join(sus, ",")}
					receivers[k] = append(receivers[k], name)
				}
			}
		}
	}
	keys := make([]told, 0, len(receivers))
	for k := range receivers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.channel != b.channel {
			return a.channel < b.channel
		}
		if a.integration != b.integration {
			return a.integration < b.integration
		}
		return a.suspects < b.suspects
	})
	for _, k := range keys {
		recv := receivers[k]
		sort.Strings(recv)
		quoted := make([]string, len(recv))
		for i, r := range recv {
			quoted[i] = regexp.QuoteMeta(r)
		}
		g.Rules = append(g.Rules, vmRule{
			Alert: alertChannelFailed,
			Expr: fmt.Sprintf(`sum(increase(%s{integration=%q,receiver_name=~%q}[5m])) > 0`,
				failedMetric, k.integration, strings.Join(quoted, "|")),
			Labels: map[string]string{
				labelChannel: k.channel, "severity": SeverityWarning, "subject": "box",
				"integration": k.integration, "suspects": k.suspects,
			},
			Annotations: map[string]string{"summary": fmt.Sprintf(
				"Alertmanager could not deliver through %s; failing channel: %s (seen by the metrics store)",
				k.integration, strings.ReplaceAll(k.suspects, ",", " or "))},
		})
	}
	return g
}

// renderRules is the rule file for a config: the fixed groups, the
// ChannelFailed rules of its monitor block's channels, and the
// operator's threshold and disk_fill lines (operatorrules.go). With no
// notify (n nil: a metrics line alone, or no config) every group is empty:
// Alertmanager keeps the last config notify rendered when the channels
// are removed (its Stop releases only the engine), so a rule that fired
// then would still reach those channels,
// a deployer's among them. Empty groups, not none: vmalert refuses a
// file with no group (-dryRun: "No rules for validation").
func renderRules(n *App) []byte {
	fixed := fixedRules()
	if n == nil {
		fixed.Rules = []vmRule{}
	}
	f := vmRuleFile{Groups: []vmGroup{fixed, channelFailedRules(n), operatorRules(n)}}
	var b bytes.Buffer
	b.WriteString(rulesHeader)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		// Only strings, slices, maps and structs: there is nothing
		// yaml.v3 cannot encode here.
		panic(err)
	}
	_ = enc.Close()
	return b.Bytes()
}
