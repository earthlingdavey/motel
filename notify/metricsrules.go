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

	ruleStoreReadOnly = "metrics_store_read_only"
	ruleStoreDiskFill = "metrics_store_disk_fill"
	ruleAppMemoryHigh = "app_memory_high"
)

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
func fixedRules() vmGroup {
	app := stateLabels(ruleAppMemoryHigh, SeverityWarning, "app:{{ $labels.app }}")
	return vmGroup{Name: "motel-store", Interval: "1m", Rules: []vmRule{
		{
			Alert: ruleStoreReadOnly, Expr: "max(vm_storage_is_read_only) > 0", For: "2m",
			Labels:      stateLabels(ruleStoreReadOnly, SeverityCritical, "box"),
			Annotations: map[string]string{"summary": "the metrics store is read-only: its disk is below its free-space floor, and nothing new is kept"},
		},
		{
			Alert: ruleStoreDiskFill, Expr: "min(predict_linear(vm_free_disk_space_bytes[6h], 86400)) < 0", For: "30m",
			Labels:      stateLabels(ruleStoreDiskFill, SeverityWarning, "box"),
			Annotations: map[string]string{"summary": "the metrics store's disk is predicted to fill within a day"},
		},
		{
			Alert: ruleAppMemoryHigh, Expr: "max by (app) (motel_app_memory_bytes / motel_app_memory_max_bytes) > 0.9", For: "10m",
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
	g := vmGroup{Name: "motel-channel-failed", Interval: "1m", Rules: []vmRule{}}
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

// renderRules is the rule file for a config: the fixed groups, and the
// ChannelFailed rules of its notify block. With no notify block (n nil)
// both groups are empty: Alertmanager keeps the last config notify
// rendered when the block is removed (its Stop releases only the
// engine), so a rule that fired then would still reach those channels,
// a deployer's among them. Empty groups, not none: vmalert refuses a
// file with no group (-dryRun: "No rules for validation").
func renderRules(n *App) []byte {
	fixed := fixedRules()
	if n == nil {
		fixed.Rules = []vmRule{}
	}
	f := vmRuleFile{Groups: []vmGroup{fixed, channelFailedRules(n)}}
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
