package notify

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"
)

// The timings the route tree carries (DESIGN-notify.md, "The route
// tree"), each measured on hotserve-1 (alertmanager_edge_tests.md):
const (
	// One-shots: every event its own group (group_by ['...']) and no
	// wait, so a second distinct one-shot is never held for a tick and
	// lost once its endsAt passes, as it was under a shared group.
	eventGroupWait      = "0s"
	eventGroupInterval  = "1m"
	eventRepeatInterval = "1h"
	// State alerts: 30 s to batch a burst, a recovery within a minute of
	// motel's resolve (50 s measured, against 290 s on the 5 m default),
	// a reminder every 4 h while something stays wrong, 24 h for disk.
	stateGroupWait      = "30s"
	stateGroupInterval  = "1m"
	stateRepeatInterval = "4h"
	diskRepeatInterval  = "24h"
	// The heartbeat: Alertmanager re-sends the firing Watchdog every
	// five minutes, which is Healthchecks' period. The repeat is under
	// the group interval on purpose: Alertmanager re-sends at a flush
	// only when the last send's log entry is older than repeat_interval,
	// and that entry is stamped after the send returns, so a repeat
	// equal to the interval is a few milliseconds short at the next
	// flush and the ping goes out every other one, every ten minutes.
	heartbeatInterval = "5m"
	heartbeatRepeat   = "4m"
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
	// Continue is set on a channel's leaf alone: its siblings, the
	// audience's other channels, are matched too.
	Continue bool `yaml:"continue,omitempty"`
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
	URLFile      string        `yaml:"url_file"`
	SendResolved bool          `yaml:"send_resolved"`
	HTTPConfig   *amHTTPConfig `yaml:"http_config,omitempty"`
}

// amHTTPConfig carries the fixed headers an ntfy channel's receiver
// sends: ntfy reads a message's priority from X-Priority.
type amHTTPConfig struct {
	Headers map[string]amHeader `yaml:"http_headers"`
}

type amHeader struct {
	Values []string `yaml:"values,flow"`
}

// ntfyPriority is the X-Priority an ntfy channel's message carries, by
// the receiver's severity: a critical is urgent (ntfy's 5: long
// vibration bursts and a pop-over), a warning high (4); info is ntfy's
// default, and so is a receiver of mixed severities. A header is fixed
// per receiver, so a state's recovery carries its alert's priority.
var ntfyPriority = map[string]string{SeverityCritical: "5", SeverityWarning: "4"}

// title_link and fallback are never omitted: Alertmanager's defaults
// link to its own web UI at its external URL, http://<hostname>:9093,
// where nothing listens (the socket is its only listener, and Debian's
// package ships no UI). An empty title_link is no link.
type amSlack struct {
	APIURLFile   string `yaml:"api_url_file"`
	SendResolved bool   `yaml:"send_resolved"`
	Title        string `yaml:"title"`
	TitleLink    string `yaml:"title_link"`
	Text         string `yaml:"text"`
	Fallback     string `yaml:"fallback"`
	Color        string `yaml:"color"`
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
	// Slack's attachment colour by severity, where Alertmanager's default
	// is danger for anything firing: red for a critical, Slack's yellow
	// for a warning, blue for info (and for a group of mixed severities,
	// which only the fallback receiver can hold), green once resolved.
	// Discord's colour is fixed by Alertmanager (0.28 has no field for
	// it): dark red for anything firing.
	slackColor = `{{ if eq .Status "resolved" }}good{{ else if eq .CommonLabels.severity "critical" }}danger{{ else if eq .CommonLabels.severity "warning" }}warning{{ else }}#439FE0{{ end }}`
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
//
// Every receiver holds one channel (DESIGN-notify.md, "The route
// tree"): an audience is a route whose children are one leaf per
// channel, each under continue, so that Alertmanager's
// alertmanager_notifications_failed_total{receiver_name=…} names the
// one channel that failed, never a working one of the same integration
// beside it.
func render(r *resolved, stateDir string) ([]byte, map[string]string, map[string][]string) {
	files := make(map[string]string, len(r.urls))
	for key, v := range r.urls {
		files[urlFileName(key)] = v
	}
	file := func(key string) string { return filepath.Join(stateDir, urlFileName(key)) }

	cfg := amConfig{Global: amGlobal{ResolveTimeout: "5m"}}
	receivers := map[string][]string{}
	add := func(rc amRecv) string {
		cfg.Receivers = append(cfg.Receivers, rc)
		return rc.Name
	}
	// recv is channel n's receiver for one use ("ch", "fallback",
	// "event-<severity>", "state-<severity>"), made at its first use
	// and shared by every route that sends that use to n. sev is the
	// severity its alerts carry, "" for the fallback, whose may be any.
	// A channel's own receiver (ch-) carries ChannelFailed's warning,
	// and a test at it too, so that a test proves the priority the
	// failure would arrive at.
	recv := func(use, n string, resolved bool, sev string) string {
		name := use + "-" + n
		if _, ok := receivers[name]; ok {
			return name
		}
		receivers[name] = []string{n}
		rc := amRecv{Name: name}
		ch := r.channels[n]
		switch channelKinds[ch.Kind] {
		case "slack":
			rc.SlackConfigs = []amSlack{{APIURLFile: file(ch.Secret), SendResolved: resolved,
				Title: msgTitle, Text: msgText, Fallback: msgTitle, Color: slackColor}}
		case "discord":
			rc.DiscordConfigs = []amDiscord{{WebhookURLFile: file(ch.Secret), SendResolved: resolved, Title: msgTitle, Message: msgText}}
		default:
			w := amWebhook{URLFile: file(ch.Secret), SendResolved: resolved}
			if p := ntfyPriority[sev]; ch.Kind == "ntfy" && p != "" {
				w.HTTPConfig = &amHTTPConfig{Headers: map[string]amHeader{"X-Priority": {Values: []string{p}}}}
			}
			rc.WebhookConfigs = []amWebhook{w}
		}
		return add(rc)
	}
	// fanout is one leaf per channel, each matching everything its
	// parent does and going on to the next.
	fanout := func(use string, channels []string, resolved bool, sev string) []amRoute {
		var out []amRoute
		for _, n := range channels {
			out = append(out, amRoute{Receiver: recv(use, n, resolved, sev), Continue: true})
		}
		return out
	}

	// The root's receiver is the blackhole, which every route without
	// a receiver of its own inherits: a severity no channel takes, for
	// one. Every alert motel posts matches a route below, the last of
	// which is the fallback, the operator's.
	blackhole := add(amRecv{Name: "blackhole"})
	cfg.Route = amRoute{
		Receiver:       blackhole,
		GroupBy:        []string{"alertname", "app", "subject"},
		GroupWait:      stateGroupWait,
		GroupInterval:  stateGroupInterval,
		RepeatInterval: stateRepeatInterval,
	}

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
			Receiver: recv("ch", n, false, SeverityWarning),
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
	// app that has channels of its own, each fanned out to the
	// operator's channels and the app's, by severity. A box event (no
	// app label) stays on the kind's own audience, the operator's.
	audience := func(kind string, extra []string) amRoute {
		chans := func(sev string) []string {
			var out []string
			for _, n := range append(slices.Clone(r.operator), extra...) {
				if r.channels[n].receives(sev) && !slices.Contains(out, n) {
					out = append(out, n)
				}
			}
			return out
		}
		resolved := kind == kindState
		// A severity route stays when no channel takes it, its leaves
		// none: the alert ends at the blackhole there, and does not fall
		// through to the next.
		var rt amRoute
		for _, sev := range []string{SeverityCritical, SeverityWarning} {
			rt.Routes = append(rt.Routes, amRoute{
				Matchers: []string{fmt.Sprintf("severity=%q", sev)},
				Routes:   fanout(kind+"-"+sev, chans(sev), resolved, sev),
			})
		}
		// Info, and any other severity.
		rt.Routes = append(rt.Routes, amRoute{Routes: fanout(kind+"-"+SeverityInfo, chans(SeverityInfo), resolved, SeverityInfo)})
		return rt
	}
	kindRoute := func(kind string, matchers []string, groupBy []string, wait, interval, repeat string, perApp bool) amRoute {
		rt := audience(kind, nil)
		rt.Matchers = matchers
		rt.GroupBy, rt.GroupWait, rt.GroupInterval, rt.RepeatInterval = groupBy, wait, interval, repeat
		var apps []amRoute
		for _, app := range r.apps {
			extra := r.appChannels[app]
			if !perApp || len(extra) == 0 {
				continue
			}
			ar := audience(kind, extra)
			ar.Matchers = []string{fmt.Sprintf("app=%q", app)}
			apps = append(apps, ar)
		}
		rt.Routes = append(apps, rt.Routes...)
		return rt
	}
	cfg.Route.Routes = append(cfg.Route.Routes,
		// disk_low is the box's alone.
		kindRoute(kindState, []string{fmt.Sprintf("kind=%q", kindState), fmt.Sprintf("alertname=%q", alertDiskLow)},
			nil, stateGroupWait, stateGroupInterval, diskRepeatInterval, false),
		kindRoute(kindEvent, []string{fmt.Sprintf("kind=%q", kindEvent)},
			[]string{"..."}, eventGroupWait, eventGroupInterval, eventRepeatInterval, true),
		kindRoute(kindState, []string{fmt.Sprintf("kind=%q", kindState)},
			nil, stateGroupWait, stateGroupInterval, stateRepeatInterval, true),
		// 3. Anything else (nothing motel posts): the operator's.
		amRoute{Routes: fanout("fallback", r.operator, true, "")},
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
