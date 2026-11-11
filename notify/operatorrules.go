package notify

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	"github.com/smallhoursorg/motel/liveswap/backupdecl"
)

// The operator's rules (DESIGN-metrics.md, "Operator rules"): `rule`
// lines of the monitor block, each one of motel's kinds with its
// parameters, checked here. No line holds an expression, a selector or
// a template: what a rule reads is motel's text, so a config author
// cannot pull any app's series, or the box's, into a notification.
const (
	ruleThreshold  = "threshold"
	ruleDiskFill   = "disk_fill"
	rulePostDeploy = "post_deploy"

	sourceRules = "rules"
	// operatorGroup is the vmalert group of the operator's threshold
	// and disk_fill rules; post_deploy is motel's to evaluate
	// (postdeploy.go).
	operatorGroup = "motel-operator"

	// requestWindow is what a request metric is computed over, and
	// what request_rate divides by.
	requestWindow = 5 * time.Minute
)

// Rule is one `rule` line. Durations are caddy.Duration (a string or
// nanoseconds in JSON); Value is in the metric's unit: a share from 0
// to 1, seconds for p50 and p95, requests a second, cores.
type Rule struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	App      string `json:"app,omitempty"`
	Severity string `json:"severity,omitempty"`

	// threshold
	Metric string         `json:"metric,omitempty"`
	Op     string         `json:"op,omitempty"`
	Value  float64        `json:"value,omitempty"`
	For    caddy.Duration `json:"for,omitempty"`
	// MinRequests is the sample floor: a threshold over a share or a
	// quantile (default 20), and post_deploy's (default 100).
	MinRequests int `json:"min_requests,omitempty"`

	// disk_fill
	Within caddy.Duration `json:"within,omitempty"`

	// post_deploy
	Window      caddy.Duration `json:"window,omitempty"`
	Max5xxShare *float64       `json:"max_5xx_share,omitempty"`
	MaxP95      caddy.Duration `json:"max_p95,omitempty"`
	Failures    int            `json:"failures,omitempty"`
}

// The units a threshold's value is written in.
const (
	unitShare   = "share"   // 0.05
	unitLatency = "latency" // 800ms, 1.5s
	unitRate    = "rate"    // 2.5, a second
	unitCores   = "cores"   // 0.5
)

// thresholdMetric is one metric a threshold may name: its unit, the
// largest value it takes, whether a sample floor applies, and its
// words in the summary.
type thresholdMetric struct {
	unit  string
	max   float64
	floor bool
	words string
	// humanize is the vmalert template function the current value is
	// shown through.
	humanize string
}

var thresholdMetrics = map[string]thresholdMetric{
	"5xx_share":       {unitShare, 1, true, "the 5xx share of its responses over 5 minutes", "humanizePercentage"},
	"p50":             {unitLatency, maxLatency.Seconds(), true, "its median time to headers over 5 minutes", "humanizeDuration"},
	"p95":             {unitLatency, maxLatency.Seconds(), true, "its p95 time to headers over 5 minutes", "humanizeDuration"},
	"request_rate":    {unitRate, 1e6, false, "its requests a second over 5 minutes", "humanize"},
	"cache_hit_share": {unitShare, 1, true, "the hit share of the responses its cache spoke for over 5 minutes", "humanizePercentage"},
	"memory_share":    {unitShare, 1, false, "its working set's share of its memory cap", "humanizePercentage"},
	"cpu_cores":       {unitCores, 1024, false, "the CPU it used over 5 minutes, in cores", "humanize"},
}

// thresholdMetricNames is the list a refusal names, in a fixed order.
var thresholdMetricNames = []string{"5xx_share", "p50", "p95", "request_rate", "cache_hit_share", "memory_share", "cpu_cores"}

// The bounds of each parameter.
const (
	// maxLatency is the top of the response histogram's buckets
	// (liveswap.ResponseBuckets): a quantile above it reads as it.
	minLatency           = time.Millisecond
	maxLatency           = 10 * time.Second
	minFor               = time.Minute
	maxFor               = 24 * time.Hour
	minWithin            = time.Hour
	maxWithin            = 30 * 24 * time.Hour
	minWindow            = time.Minute
	maxWindow            = 24 * time.Hour
	maxMinRequests       = 1_000_000
	maxFailures          = 60
	defaultMinRequests   = 20
	defaultPostDeployMin = 100
	defaultWindow        = 30 * time.Minute
	defaultFailures      = 3
	diskFillFor          = 30 * time.Minute
)

var (
	ruleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	// A plain decimal: no sign, no exponent, no unit.
	decimalRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	latencyRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(ms|s)$`)
)

// errRuleNeedsMetrics is the refusal of a rule line in a block with no
// metrics line.
var errRuleNeedsMetrics = errors.New("a rule line needs the monitor block's metrics line: rules are evaluated over the metrics store's numbers")

// parseRule reads one `rule` line, the dispenser on its name:
//
//	rule <name> threshold <metric> >|< <value> for <dur> [app <name>] [min_requests N] [severity warning|critical]
//	rule <name> disk_fill within <dur> [severity warning|critical]
//	rule <name> post_deploy [app <name>] {
//	    window <dur>  min_requests N  max_5xx_share <share>  max_p95 <dur>  failures N
//	}
func parseRule(d *caddyfile.Dispenser) (Rule, error) {
	args := d.RemainingArgs()
	if len(args) < 2 {
		return Rule{}, d.Err("rule takes <name> threshold|disk_fill|post_deploy and its parameters")
	}
	r := Rule{Name: args[0], Kind: args[1]}
	if err := checkRuleName(r.Name); err != nil {
		return Rule{}, d.Err(err.Error())
	}
	errf := func(format string, a ...any) error {
		return d.Errf("rule %s: "+format, append([]any{r.Name}, a...)...)
	}
	rest := args[2:]
	switch r.Kind {
	case ruleThreshold:
		if len(rest) < 5 || rest[3] != "for" {
			return Rule{}, errf("threshold takes <metric> > | < <value> for <duration>, then [app <name>] [min_requests N] [severity warning|critical]")
		}
		r.Metric, r.Op = rest[0], rest[1]
		m, ok := thresholdMetrics[r.Metric]
		if !ok {
			return Rule{}, errf("unknown metric %q; a threshold reads one of %s", r.Metric, strings.Join(thresholdMetricNames, ", "))
		}
		v, err := parseValue(m.unit, rest[2])
		if err != nil {
			return Rule{}, errf("%s: %v", r.Metric, err)
		}
		r.Value = v
		if r.For, err = parseRuleDuration(rest[4]); err != nil {
			return Rule{}, errf("for: %v", err)
		}
		if err := parseOptions(rest[5:], map[string]func(string) error{
			"app": func(v string) error { r.App = v; return nil },
			"min_requests": func(v string) error {
				n, err := parseCount(v)
				r.MinRequests = n
				return err
			},
			"severity": func(v string) error { r.Severity = v; return nil },
		}); err != nil {
			return Rule{}, errf("%v", err)
		}
	case ruleDiskFill:
		if len(rest) < 2 || rest[0] != "within" {
			return Rule{}, errf("disk_fill takes within <duration>, then [severity warning|critical]")
		}
		var err error
		if r.Within, err = parseRuleDuration(rest[1]); err != nil {
			return Rule{}, errf("within: %v", err)
		}
		if err := parseOptions(rest[2:], map[string]func(string) error{
			"severity": func(v string) error { r.Severity = v; return nil },
		}); err != nil {
			return Rule{}, errf("%v", err)
		}
	case rulePostDeploy:
		if err := parseOptions(rest, map[string]func(string) error{
			"app": func(v string) error { r.App = v; return nil },
		}); err != nil {
			return Rule{}, errf("%v", err)
		}
		seen := map[string]bool{}
		for nesting := d.Nesting(); d.NextBlock(nesting); {
			key := d.Val()
			vals := d.RemainingArgs()
			if len(vals) != 1 {
				return Rule{}, errf("%s takes one value", key)
			}
			if seen[key] {
				return Rule{}, errf("%s is given more than once", key)
			}
			seen[key] = true
			v := vals[0]
			var err error
			switch key {
			case "window":
				r.Window, err = parseRuleDuration(v)
			case "min_requests":
				r.MinRequests, err = parseCount(v)
			case "max_5xx_share":
				var s float64
				if s, err = parseValue(unitShare, v); err == nil {
					r.Max5xxShare = &s
				}
			case "max_p95":
				var s float64
				if s, err = parseValue(unitLatency, v); err == nil {
					r.MaxP95 = caddy.Duration(time.Duration(math.Round(s * float64(time.Second))))
				}
			case "failures":
				r.Failures, err = parseCount(v)
			default:
				return Rule{}, errf("unknown post_deploy parameter %q; it takes window, min_requests, max_5xx_share, max_p95 and failures", key)
			}
			if err != nil {
				return Rule{}, errf("%s: %v", key, err)
			}
			if d.NextBlock(d.Nesting()) {
				return Rule{}, errf("%s takes no block", key)
			}
		}
	default:
		return Rule{}, errf("unknown kind %q; a rule is threshold, disk_fill or post_deploy", r.Kind)
	}
	if r.Kind != rulePostDeploy && d.NextBlock(d.Nesting()) {
		return Rule{}, errf("a %s rule takes no block", r.Kind)
	}
	if err := r.check(); err != nil {
		return Rule{}, d.Err(err.Error())
	}
	return r, nil
}

// parseOptions reads `<key> <value>` pairs, each key at most once.
func parseOptions(args []string, set map[string]func(string) error) error {
	seen := map[string]bool{}
	for len(args) > 0 {
		key := args[0]
		f, ok := set[key]
		if !ok {
			keys := make([]string, 0, len(set))
			for k := range set {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			return fmt.Errorf("unknown option %q; it takes %s", key, strings.Join(keys, ", "))
		}
		if len(args) < 2 {
			return fmt.Errorf("%s takes a value", key)
		}
		if seen[key] {
			return fmt.Errorf("%s is given more than once", key)
		}
		seen[key] = true
		if err := f(args[1]); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		args = args[2:]
	}
	return nil
}

// parseValue reads a value in a unit's own form: a plain decimal, or a
// duration in ms or s for a latency (in seconds).
func parseValue(unit, s string) (float64, error) {
	if unit == unitLatency {
		if !latencyRe.MatchString(s) {
			return 0, fmt.Errorf("%q is not a duration in ms or s, like 800ms or 1.5s", s)
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%q: %w", s, err)
		}
		return d.Seconds(), nil
	}
	if !decimalRe.MatchString(s) {
		return 0, fmt.Errorf("%q is not a plain number, like 0.05 or 2", s)
	}
	return strconv.ParseFloat(s, 64)
}

// parseCount reads a count, at least 1: a JSON config's 0 is the
// default, so a line never writes it.
func parseCount(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || !decimalRe.MatchString(s) || n < 1 {
		return 0, fmt.Errorf("%q is not a whole number from 1", s)
	}
	return n, nil
}

func parseRuleDuration(s string) (caddy.Duration, error) {
	d, err := caddy.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration, like 10m or 2h", s)
	}
	return caddy.Duration(d), nil
}

// checkRuleName: the name is the alert's name and part of its key, so
// it is of one grammar and never one of motel's own.
func checkRuleName(name string) error {
	if !ruleNameRe.MatchString(name) {
		return fmt.Errorf("rule %q: a rule's name is lowercase letters, digits and underscores, starting with a letter, at most 48", name)
	}
	if slices.ContainsFunc(reservedNames, func(n string) bool { return strings.EqualFold(n, name) }) ||
		strings.HasPrefix(name, "watchdog_") || strings.HasPrefix(name, "metrics_store_") {
		return fmt.Errorf("rule %s: %s is one of motel's own alerts; name the rule something else", name, name)
	}
	return nil
}

// check holds one rule to its kind's grammar: the Caddyfile's lines,
// and a JSON config's, which never went through parseRule.
func (r Rule) check() error {
	if err := checkRuleName(r.Name); err != nil {
		return err
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("rule %s: "+format, append([]any{r.Name}, a...)...)
	}
	if r.App != "" && !backupdecl.ValidAppName(r.App) {
		return bad("app %q is not an app name", r.App)
	}
	switch r.Severity {
	case "", SeverityWarning, SeverityCritical:
	default:
		return bad("severity is warning or critical, not %q", r.Severity)
	}
	if r.Kind != ruleThreshold && (r.Metric != "" || r.Op != "" || r.Value != 0 || r.For != 0) {
		return bad("metric, op, value and for are a threshold's")
	}
	if r.Kind != ruleDiskFill && r.Within != 0 {
		return bad("within is disk_fill's")
	}
	if r.Kind != rulePostDeploy && (r.Window != 0 || r.Max5xxShare != nil || r.MaxP95 != 0 || r.Failures != 0) {
		return bad("window, max_5xx_share, max_p95 and failures are post_deploy's")
	}
	if r.MinRequests != 0 && (r.MinRequests < 1 || r.MinRequests > maxMinRequests) {
		return bad("min_requests is from 1 to %d", maxMinRequests)
	}
	switch r.Kind {
	case ruleThreshold:
		m, ok := thresholdMetrics[r.Metric]
		if !ok {
			return bad("unknown metric %q; a threshold reads one of %s", r.Metric, strings.Join(thresholdMetricNames, ", "))
		}
		if r.Op != ">" && r.Op != "<" {
			return bad("the comparison is > or <, not %q", r.Op)
		}
		if err := checkValue(r.Metric, m, r.Op, r.Value); err != nil {
			return bad("%v", err)
		}
		if d := time.Duration(r.For); d < minFor || d > maxFor || d%time.Second != 0 {
			return bad("for is whole seconds from 1m to 24h, not %s", d)
		}
		if r.MinRequests != 0 && !m.floor {
			return bad("min_requests is the sample floor of a share or a quantile of the responses (5xx_share, p50, p95, cache_hit_share), not of %s", r.Metric)
		}
	case ruleDiskFill:
		if r.App != "" {
			return bad("disk_fill is the box's: it takes no app")
		}
		if r.MinRequests != 0 {
			return bad("min_requests is a request rule's")
		}
		if d := time.Duration(r.Within); d < minWithin || d > maxWithin || d%time.Second != 0 {
			return bad("within is whole seconds from 1h to 30d, not %s", d)
		}
	case rulePostDeploy:
		if r.Severity != "" {
			return bad("post_deploy's event is a warning: it takes no severity")
		}
		if d := time.Duration(r.Window); d != 0 && (d < minWindow || d > maxWindow || d%time.Minute != 0) {
			return bad("window is whole minutes from 1m to 24h, not %s", d)
		}
		if r.Failures != 0 && (r.Failures < 1 || r.Failures > maxFailures) {
			return bad("failures is from 1 to %d", maxFailures)
		}
		if r.Max5xxShare == nil && r.MaxP95 == 0 {
			return bad("post_deploy needs max_5xx_share, max_p95 or both: what a regression is")
		}
		if s := r.Max5xxShare; s != nil && (*s <= 0 || *s >= 1) {
			return bad("max_5xx_share is a share between 0 and 1, not %v", *s)
		}
		if d := time.Duration(r.MaxP95); d != 0 && (d < minLatency || d >= maxLatency) {
			return bad("max_p95 is from 1ms to under 10s (the top of the latency buckets), not %s", d)
		}
		if checks := int(r.window() / time.Minute); r.failures() > checks {
			return bad("%d failures in a row need a window of at least %d minutes: a check is made each minute", r.failures(), r.failures())
		}
	default:
		return bad("unknown kind %q; a rule is threshold, disk_fill or post_deploy", r.Kind)
	}
	return nil
}

// checkValue holds a threshold's value to its metric's range, and
// refuses one that can never hold.
func checkValue(metric string, m thresholdMetric, op string, v float64) error {
	lo := 0.0
	if m.unit == unitLatency {
		lo = minLatency.Seconds()
	}
	if v < lo || v > m.max {
		if m.unit == unitLatency {
			return fmt.Errorf("%s is from 1ms to 10s (the top of the latency buckets), not %s", metric, secondsText(v))
		}
		return fmt.Errorf("%s is from %s to %s, not %s", metric, numberText(lo), numberText(m.max), numberText(v))
	}
	// A share is at most 1, and a quantile at most the top bucket.
	capped := m.unit == unitShare || m.unit == unitLatency
	if op == "<" && v == 0 || op == ">" && capped && v >= m.max {
		return fmt.Errorf("%s %s %s can never hold", metric, op, valueText(m.unit, v))
	}
	return nil
}

// Defaults.
func (r Rule) severity() string {
	if r.Severity == "" {
		return SeverityWarning
	}
	return r.Severity
}

func (r Rule) minRequests() int {
	switch {
	case r.MinRequests != 0:
		return r.MinRequests
	case r.Kind == rulePostDeploy:
		return defaultPostDeployMin
	}
	return defaultMinRequests
}

func (r Rule) window() time.Duration {
	if r.Window == 0 {
		return defaultWindow
	}
	return time.Duration(r.Window)
}

func (r Rule) failures() int {
	if r.Failures == 0 {
		return defaultFailures
	}
	return r.Failures
}

// checkRules holds the lines together: names unique, one post_deploy
// for every app and one per app; and, given this config's apps (nil
// for no liveswap block), each app a rule names configured.
func checkRules(rules []Rule, apps map[string]bool) error {
	names := map[string]bool{}
	postDeploy := map[string]bool{}
	for _, r := range rules {
		if err := r.check(); err != nil {
			return err
		}
		if names[r.Name] {
			return fmt.Errorf("rule %s is defined twice", r.Name)
		}
		names[r.Name] = true
		if r.Kind == rulePostDeploy {
			if postDeploy[r.App] {
				scope := "every app"
				if r.App != "" {
					scope = r.App
				}
				return fmt.Errorf("rule %s: a second post_deploy for %s; there is one for every app, and one per app that replaces it", r.Name, scope)
			}
			postDeploy[r.App] = true
		}
	}
	if apps == nil {
		return nil
	}
	for _, r := range rules {
		if r.App != "" && !apps[r.App] {
			return fmt.Errorf("rule %s names app %s, which the liveswap block does not configure", r.Name, r.App)
		}
	}
	return nil
}

// operatorRules is the motel-operator group: the threshold and
// disk_fill lines, rendered as motel's own expressions. Empty with no
// notify (n nil), as the fixed groups are.
func operatorRules(n *App) vmGroup {
	g := vmGroup{Name: operatorGroup, Interval: "1m", Rules: []vmRule{}}
	if n == nil || n.Metrics == nil {
		return g
	}
	for _, r := range n.Metrics.Rules {
		switch r.Kind {
		case ruleThreshold:
			g.Rules = append(g.Rules, thresholdRule(r))
		case ruleDiskFill:
			g.Rules = append(g.Rules, diskFillRule(r))
		}
	}
	return g
}

// ruleLabels are stateLabels for an operator's rule: source rules, and
// #418's reserved key, rules/<subject>/<name>.
func ruleLabels(name, severity, subject string) map[string]string {
	return map[string]string{
		"kind": kindState, "severity": severity, "subject": subject,
		"source": sourceRules, "key": sourceRules + "/" + subject + "/" + name,
	}
}

func thresholdRule(r Rule) vmRule {
	m := thresholdMetrics[r.Metric]
	subject, who := "app:{{ $labels.app }}", "{{ $labels.app }}"
	if r.App != "" {
		subject, who = "app:"+r.App, r.App
	}
	above := "above"
	if r.Op == "<" {
		above = "below"
	}
	return vmRule{
		Alert:  r.Name,
		Expr:   thresholdExpr(r),
		For:    promDuration(time.Duration(r.For)),
		Labels: ruleLabels(r.Name, r.severity(), subject),
		Annotations: map[string]string{"summary": fmt.Sprintf("%s: %s has been %s %s for %s (rule %s); now {{ $value | %s }}",
			who, m.words, above, valueText(m.unit, r.Value), promDuration(time.Duration(r.For)), r.Name, m.humanize)},
	}
}

// thresholdExpr is a threshold's expression: the metric's, per app (by
// (app), which carries the label notify's route tree sends to the
// app's channels by), over the request window, with the sample floor
// for a share or a quantile. The request counters are read through
// increase_pure, never increase: an app's series appears with its first
// push already at that push's count, and the store's increase drops it
// (DESIGN-metrics.md, "metrics in GET /<app>").
func thresholdExpr(r Rule) string {
	app := ""
	if r.App != "" {
		app = "app=" + strconv.Quote(r.App)
	}
	sel := func(extra ...string) string {
		var parts []string
		if app != "" {
			parts = append(parts, app)
		}
		if parts = append(parts, extra...); len(parts) == 0 {
			return ""
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	win := "[" + promDuration(requestWindow) + "]"
	sum := func(metric string, extra ...string) string {
		return "sum by (app) (increase_pure(" + metric + sel(extra...) + win + "))"
	}
	cmp := " " + r.Op + " " + numberText(r.Value)
	floor := " >= " + strconv.Itoa(r.minRequests())
	switch r.Metric {
	case "5xx_share":
		// Of the responses sent: a client gone first is in the total,
		// never in the share (GET /<app>'s 5xx_share).
		sent := sum("motel_app_requests_total", `class!="aborted"`)
		return "(" + sum("motel_app_requests_total", `class="5xx"`) + " / " + sent + ")" + cmp + " and " + sent + floor
	case "p50", "p95":
		q := "0.5"
		if r.Metric == "p95" {
			q = "0.95"
		}
		return "histogram_quantile(" + q + ", sum by (app, le) (increase_pure(motel_app_response_seconds_bucket" + sel() + win + ")))" +
			cmp + " and " + sum("motel_app_response_seconds_count") + floor
	case "request_rate":
		return sum("motel_app_requests_total") + " / " + strconv.Itoa(int(requestWindow.Seconds())) + cmp
	case "cache_hit_share":
		// A hit result's series exists once there is one: an app whose
		// cache spoke and never hit has a share of 0, not none.
		spoke := sum("motel_app_cache_responses_total")
		return "(" + sum("motel_app_cache_responses_total", `result=~"hit|stale"`) + " or " + spoke + " * 0) / " + spoke + cmp + " and " + spoke + floor
	case "memory_share":
		return "max by (app) (motel_app_memory_bytes" + sel() + " / motel_app_memory_max_bytes" + sel() + ")" + cmp
	case "cpu_cores":
		// rate, not increase_pure: each unit's counter starts at zero
		// when the unit does, and its first value is the CPU it used
		// before the window.
		return "sum by (app) (rate(motel_app_cpu_seconds_total" + sel() + win + "))" + cmp
	}
	panic("thresholdExpr: unchecked metric " + r.Metric)
}

// diskFillRule is the store's disk rule's shape over motel's own disk
// series, one alert per filesystem, with the horizon from within.
func diskFillRule(r Rule) vmRule {
	h := strconv.Itoa(int(time.Duration(r.Within).Seconds()))
	return vmRule{
		Alert: r.Name,
		Expr: "min by (path) (predict_linear(" + diskAvailMetric + "[6h], " + h + ")) < 0" +
			" and min by (path) (predict_linear(" + diskAvailMetric + "[1h], " + h + ")) < 0",
		For:    promDuration(diskFillFor),
		Labels: ruleLabels(r.Name, r.severity(), "box"),
		Annotations: map[string]string{"summary": fmt.Sprintf("the filesystem holding {{ $labels.path }} is predicted to fill within %s (rule %s)",
			promDuration(time.Duration(r.Within)), r.Name)},
	}
}

// promDuration writes d as the store reads a duration: its largest
// whole unit (d, h, m, s).
func promDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/time.Second)) + "s"
}

func numberText(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// secondsText writes seconds as a duration: 800ms, 1.5s.
func secondsText(v float64) string {
	return time.Duration(v * float64(time.Second)).Round(time.Microsecond).String()
}

func valueText(unit string, v float64) string {
	if unit == unitLatency {
		return secondsText(v)
	}
	return numberText(v)
}
