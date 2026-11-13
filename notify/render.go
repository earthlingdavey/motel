package notify

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"
)

// The timings the route tree carries (DESIGN-notify.md, "The route
// tree"), each measured on hotserve-1 (alertmanager_edge_tests.md),
// divided by clockDiv (clock.go):
var (
	// One-shots: every event its own group (group_by ['...']) and no
	// wait, so a second distinct one-shot is never held for a tick and
	// lost once its endsAt passes, as it was under a shared group.
	eventGroupWait      = clockText("0s")
	eventGroupInterval  = clockText("1m")
	eventRepeatInterval = clockText("1h")
	// State alerts: 30 s to batch a burst, a recovery within a minute of
	// motel's resolve (50 s measured, against 290 s on the 5 m default),
	// a reminder every 4 h while something stays wrong, 24 h for disk.
	stateGroupWait      = clockText("30s")
	stateGroupInterval  = clockText("1m")
	stateRepeatInterval = clockText("4h")
	diskRepeatInterval  = clockText("24h")
	// The heartbeat: Alertmanager re-sends the firing Watchdog every
	// five minutes, which is Healthchecks' period. The repeat is under
	// the group interval on purpose: Alertmanager re-sends at a flush
	// only when the last send's log entry is older than repeat_interval,
	// and that entry is stamped after the send returns, so a repeat
	// equal to the interval is a few milliseconds short at the next
	// flush and the ping goes out every other one, every ten minutes.
	heartbeatInterval = clockText("5m")
	heartbeatRepeat   = clockText("4m")
)

// Label values motel sets, which routing reads.
const (
	kindEvent = "event"
	kindState = "state"

	alertWatchdog      = "Watchdog"
	alertChannelFailed = "ChannelFailed"
	alertNotifyTest    = "NotifyTest"
	alertDeployMute    = "deploy_in_progress"
	alertDiskLow       = "disk_low"

	// labelChannel addresses one channel directly: ChannelFailed (to
	// each of the operator's other channels) and NotifyTest.
	labelChannel = "notify_channel"
)

// amConfig is the part of Alertmanager's configuration motel writes.
// Every field is fixed by motel or comes from a checked line of the
// monitor block; nothing the block holds is a template.
type amConfig struct {
	Global       amGlobal    `yaml:"global"`
	Route        amRoute     `yaml:"route"`
	InhibitRules []amInhibit `yaml:"inhibit_rules"`
	Receivers    []amRecv    `yaml:"receivers"`
}

type amGlobal struct {
	ResolveTimeout string `yaml:"resolve_timeout"`
}

type amRoute struct {
	Receiver       string    `yaml:"receiver,omitempty"`
	Matchers       []string  `yaml:"matchers,omitempty,flow"`
	GroupBy        []string  `yaml:"group_by,omitempty,flow"`
	GroupWait      string    `yaml:"group_wait,omitempty"`
	GroupInterval  string    `yaml:"group_interval,omitempty"`
	RepeatInterval string    `yaml:"repeat_interval,omitempty"`
	Routes         []amRoute `yaml:"routes,omitempty"`
}

type amInhibit struct {
	SourceMatchers []string `yaml:"source_matchers,flow"`
	TargetMatchers []string `yaml:"target_matchers,flow"`
	Equal          []string `yaml:"equal,flow"`
}

type amRecv struct {
	Name           string      `yaml:"name"`
	WebhookConfigs []amWebhook `yaml:"webhook_configs,omitempty"`
	SlackConfigs   []amSlack   `yaml:"slack_configs,omitempty"`
	DiscordConfigs []amDiscord `yaml:"discord_configs,omitempty"`
}

// The *_file fields keep every URL out of the YAML, out of
// Alertmanager's /api/v2/status echo and out of its failure log
// (measured: alertmanager_delivery_test.md). send_resolved is never
// omitted: its default differs by integration.
type amWebhook struct {
	URLFile      string `yaml:"url_file"`
	SendResolved bool   `yaml:"send_resolved"`
}

type amSlack struct {
	APIURLFile   string `yaml:"api_url_file"`
	SendResolved bool   `yaml:"send_resolved"`
	Title        string `yaml:"title"`
	Text         string `yaml:"text"`
}

type amDiscord struct {
	WebhookURLFile string `yaml:"webhook_url_file"`
	SendResolved   bool   `yaml:"send_resolved"`
	Title          string `yaml:"title"`
	Message        string `yaml:"message"`
}

// The message text for the integrations that are not handed the
// payload whole: motel's own one-line summaries, which carry no secret
// and no log tail. Fixed here, never from the block.
const (
	msgTitle = `[{{ .Status | toUpper }}] {{ .CommonLabels.alertname }}{{ if .CommonLabels.app }} {{ .CommonLabels.app }}{{ end }}`
	// Each alert's own status, where it differs from the group's: a
	// grouped notification names which of its alerts has resolved.
	msgText = `{{ range .Alerts }}{{ if eq .Status "resolved" }}[RESOLVED] {{ end }}{{ .Annotations.summary }}{{ "\n" }}{{ end }}`
)

const configHeader = "# Rendered by motel from the Caddyfile's monitor block (notify/DESIGN-notify.md).\n" +
	"# Do not edit: motel rewrites this file on every config load and reloads\n" +
	"# Alertmanager. Every URL is a url_file beside it, never in this file.\n"

// ConfigName is the rendered file's name in the state directory;
// Alertmanager's drop-in points --config.file at it.
const ConfigName = "alertmanager.yml"

// urlFileName is the file holding one secret's value, beside the config.
func urlFileName(key string) string { return "url-" + key }

// render is the Alertmanager configuration for a resolved block, the
// URL files it names (file name → value), and each receiver's channels,
// which channel_failed reads a failing receiver against. stateDir is
// where the files will be written, which the url_file paths name.
func render(r *resolved, stateDir string) ([]byte, map[string]string, map[string][]string) {
	files := make(map[string]string, len(r.urls))
	for key, v := range r.urls {
		files[urlFileName(key)] = v
	}
	file := func(key string) string { return filepath.Join(stateDir, urlFileName(key)) }

	cfg := amConfig{Global: amGlobal{ResolveTimeout: clockText("5m")}}
	receivers := map[string][]string{}
	recv := func(name string, channels []string, resolved bool) amRecv {
		rc := amRecv{Name: name}
		receivers[name] = slices.Clone(channels)
		for _, n := range channels {
			ch := r.channels[n]
			switch channelKinds[ch.Kind] {
			case "slack":
				rc.SlackConfigs = append(rc.SlackConfigs, amSlack{APIURLFile: file(ch.Secret), SendResolved: resolved, Title: msgTitle, Text: msgText})
			case "discord":
				rc.DiscordConfigs = append(rc.DiscordConfigs, amDiscord{WebhookURLFile: file(ch.Secret), SendResolved: resolved, Title: msgTitle, Message: msgText})
			default:
				rc.WebhookConfigs = append(rc.WebhookConfigs, amWebhook{URLFile: file(ch.Secret), SendResolved: resolved})
			}
		}
		return rc
	}
	add := func(rc amRecv) string {
		cfg.Receivers = append(cfg.Receivers, rc)
		return rc.Name
	}

	// The root's receiver is the operator's, and no alert should reach
	// it: every alert motel posts matches a route below.
	cfg.Route = amRoute{
		Receiver:       add(recv("fallback", r.operator, true)),
		GroupBy:        []string{"alertname", "app", "subject"},
		GroupWait:      stateGroupWait,
		GroupInterval:  stateGroupInterval,
		RepeatInterval: stateRepeatInterval,
	}
	blackhole := add(amRecv{Name: "blackhole"})

	// 1. Plumbing first, so nothing below can swallow it.
	heartbeat := blackhole
	if r.heartbeat != "" {
		heartbeat = add(amRecv{Name: "heartbeat", WebhookConfigs: []amWebhook{{URLFile: file(r.heartbeat), SendResolved: false}}})
		receivers[heartbeat] = []string{} // no channel: its failure is no channel's, and every operator channel hears it
	}
	direct := amRoute{
		Receiver:       blackhole,
		Matchers:       []string{labelChannel + `=~".+"`},
		GroupBy:        []string{"..."},
		GroupWait:      eventGroupWait,
		GroupInterval:  eventGroupInterval,
		RepeatInterval: stateRepeatInterval,
	}
	for _, n := range r.order {
		direct.Routes = append(direct.Routes, amRoute{
			Matchers: []string{fmt.Sprintf("%s=%q", labelChannel, n)},
			Receiver: add(recv("ch-"+n, []string{n}, false)),
		})
	}
	cfg.Route.Routes = append(cfg.Route.Routes,
		amRoute{
			Receiver:       heartbeat,
			Matchers:       []string{fmt.Sprintf("alertname=%q", alertWatchdog)},
			GroupBy:        []string{"alertname"},
			GroupWait:      eventGroupWait,
			GroupInterval:  heartbeatInterval,
			RepeatInterval: heartbeatRepeat,
		},
		direct,
		// It exists to inhibit, and reaches no one.
		amRoute{Receiver: blackhole, Matchers: []string{fmt.Sprintf("alertname=%q", alertDeployMute)}},
	)

	// 2. One route per kind, carrying the timing; under it a route per
	// app that has channels of its own, each with a receiver composed
	// of the operator's channels and the app's, by severity. A box
	// event (no app label) stays on the kind's own receivers, which
	// are the operator's.
	audience := func(kind, who string, extra []string) amRoute {
		chans := func(sev string) []string {
			var out []string
			for _, n := range append(slices.Clone(r.operator), extra...) {
				if r.channels[n].receives(sev) && !slices.Contains(out, n) {
					out = append(out, n)
				}
			}
			return out
		}
		name := func(sev string) string { return kind + "-" + who + "-" + sev }
		resolved := kind == kindState
		rt := amRoute{Receiver: add(recv(name(SeverityInfo), chans(SeverityInfo), resolved))}
		for _, sev := range []string{SeverityCritical, SeverityWarning} {
			rt.Routes = append(rt.Routes, amRoute{
				Matchers: []string{fmt.Sprintf("severity=%q", sev)},
				Receiver: add(recv(name(sev), chans(sev), resolved)),
			})
		}
		return rt
	}
	kindRoute := func(kind string, matchers []string, groupBy []string, wait, interval, repeat, tag string) amRoute {
		rt := audience(kind, tag+"box", nil)
		rt.Matchers = matchers
		rt.GroupBy, rt.GroupWait, rt.GroupInterval, rt.RepeatInterval = groupBy, wait, interval, repeat
		var apps []amRoute
		for _, app := range r.apps {
			if tag == "disk-" {
				break // disk_low is the box's alone
			}
			extra := r.appChannels[app]
			if len(extra) == 0 {
				continue
			}
			ar := audience(kind, tag+"app-"+app, extra)
			ar.Matchers = []string{fmt.Sprintf("app=%q", app)}
			apps = append(apps, ar)
		}
		rt.Routes = append(apps, rt.Routes...)
		return rt
	}
	cfg.Route.Routes = append(cfg.Route.Routes,
		kindRoute(kindState, []string{fmt.Sprintf("kind=%q", kindState), fmt.Sprintf("alertname=%q", alertDiskLow)},
			nil, stateGroupWait, stateGroupInterval, diskRepeatInterval, "disk-"),
		kindRoute(kindEvent, []string{fmt.Sprintf("kind=%q", kindEvent)},
			[]string{"..."}, eventGroupWait, eventGroupInterval, eventRepeatInterval, ""),
		kindRoute(kindState, []string{fmt.Sprintf("kind=%q", kindState)},
			nil, stateGroupWait, stateGroupInterval, stateRepeatInterval, ""),
	)

	// The deploy mute: an app's deploy in progress silences its own
	// watchdog alerts, whatever their severity (#418's are critical).
	cfg.InhibitRules = []amInhibit{{
		SourceMatchers: []string{fmt.Sprintf("alertname=%q", alertDeployMute)},
		TargetMatchers: []string{`alertname=~"watchdog_.*"`},
		Equal:          []string{"app"},
	}}

	var b bytes.Buffer
	b.WriteString(configHeader)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		// Only strings, slices and structs: there is nothing yaml.v3
		// cannot encode here.
		panic(err)
	}
	_ = enc.Close()
	return b.Bytes(), files, receivers
}

func (ch Channel) floor() string {
	if ch.Severity == "" {
		return SeverityInfo
	}
	return ch.Severity
}

// receives reports whether the channel takes an alert of severity s.
func (ch Channel) receives(s string) bool { return severityRank(s) >= severityRank(ch.floor()) }
