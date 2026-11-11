package notify

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withRules is fullBlock with a metrics line and the rule lines given.
func withRules(lines ...string) string {
	return strings.TrimSuffix(fullBlock, "}") + "\tmetrics\n\t" + strings.Join(lines, "\n\t") + "\n}"
}

// rulesApp is withRules, resolved and rendered as Provision does.
func rulesApp(t *testing.T, lines ...string) *App {
	t.Helper()
	a := mustParse(t, withRules(lines...))
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = r
	_, _, a.receivers = render(r, testStateDir)
	return a
}

func TestRuleLines(t *testing.T) {
	a := mustParse(t, withRules(
		"rule errors_high threshold 5xx_share > 0.05 for 10m",
		"rule shop_slow threshold p95 > 1.5s for 15m app shop min_requests 50 severity critical",
		"rule blog_quiet threshold request_rate < 0.1 for 1h app blog",
		"rule disk_filling disk_fill within 7d",
	))
	b, _ := json.Marshal(a.Metrics)
	got := strings.NewReplacer(`\u003e`, ">", `\u003c`, "<").Replace(string(b))
	want := `{"rules":[` +
		`{"name":"errors_high","kind":"threshold","metric":"5xx_share","op":">","value":0.05,"for":600000000000},` +
		`{"name":"shop_slow","kind":"threshold","app":"shop","severity":"critical","metric":"p95","op":">","value":1.5,"for":900000000000,"min_requests":50},` +
		`{"name":"blog_quiet","kind":"threshold","app":"blog","metric":"request_rate","op":"<","value":0.1,"for":3600000000000},` +
		`{"name":"disk_filling","kind":"disk_fill","within":604800000000000}]}`
	if got != want {
		t.Fatalf("metrics =\n%s\nwant\n%s", got, want)
	}
	r := a.Metrics.Rules
	if r[0].minRequests() != 20 || r[0].severity() != SeverityWarning {
		t.Errorf("defaults: %d %s", r[0].minRequests(), r[0].severity())
	}
}

func TestRuleRefusals(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"rule Errors threshold 5xx_share > 0.05 for 10m", "a rule's name is lowercase letters"},
		{"rule 9lives threshold 5xx_share > 0.05 for 10m", "a rule's name is lowercase letters"},
		{"rule a-b threshold 5xx_share > 0.05 for 10m", "a rule's name is lowercase letters"},
		{"rule " + strings.Repeat("a", 49) + " threshold 5xx_share > 0.05 for 10m", "at most 48"},
		{"rule app_memory_high threshold memory_share > 0.5 for 10m", "is one of motel's own alerts"},
		{"rule metrics_store_full threshold memory_share > 0.5 for 10m", "is one of motel's own alerts"},
		{"rule metrics_store_down threshold memory_share > 0.5 for 10m", "is one of motel's own alerts"},
		{"rule channelfailed threshold memory_share > 0.5 for 10m", "is one of motel's own alerts"},
		{"rule watchdog threshold memory_share > 0.5 for 10m", "is one of motel's own alerts"},
		{"rule watchdog_x threshold memory_share > 0.5 for 10m", "is one of motel's own alerts"},
		{"rule deploy_failed threshold memory_share > 0.5 for 10m", "is one of motel's own alerts"},
		{"rule disk_low disk_fill within 7d", "is one of motel's own alerts"},
		{"rule x sum 5xx_share > 0.05 for 10m", `unknown kind "sum"`},
		{"rule x threshold 5xx_share > 0.05", "threshold takes <metric> > | < <value> for <duration>"},
		{"rule x threshold 5xx_share > 0.05 during 10m", "threshold takes <metric>"},
		{"rule x threshold errors > 0.05 for 10m", `unknown metric "errors"; a threshold reads one of 5xx_share, p50, p95`},
		{`rule x threshold motel_app_up{app="shop"} > 0 for 10m`, "unknown metric"},
		{"rule x threshold 5xx_share >= 0.05 for 10m", `the comparison is > or <, not ">="`},
		{"rule x threshold 5xx_share = 0.05 for 10m", "the comparison is > or <"},
		{"rule x threshold 5xx_share > 5% for 10m", `"5%" is not a plain number`},
		{"rule x threshold 5xx_share > -0.1 for 10m", "is not a plain number"},
		{"rule x threshold 5xx_share > 1e-2 for 10m", "is not a plain number"},
		{"rule x threshold 5xx_share > 1.5 for 10m", "5xx_share is from 0 to 1, not 1.5"},
		{"rule x threshold 5xx_share > 1 for 10m", "5xx_share > 1 can never hold"},
		{"rule x threshold cache_hit_share < 0 for 10m", "cache_hit_share < 0 can never hold"},
		{"rule x threshold p95 > 800 for 10m", "is not a duration in ms or s"},
		{"rule x threshold p95 > 1m for 10m", "is not a duration in ms or s"},
		{"rule x threshold p95 > 0.5ms for 10m", "p95 is from 1ms to 10s"},
		{"rule x threshold p50 > 11s for 10m", "p50 is from 1ms to 10s"},
		{"rule x threshold p50 > 10s for 10m", "can never hold"},
		{"rule x threshold request_rate > 0.5s for 10m", "is not a plain number"},
		{"rule x threshold cpu_cores > 2000 for 10m", "cpu_cores is from 0 to 1024"},
		{"rule x threshold memory_share > 0.9 for 30s", "for is whole seconds from 1m to 24h"},
		{"rule x threshold memory_share > 0.9 for 25h", "for is whole seconds from 1m to 24h"},
		{"rule x threshold memory_share > 0.9 for 90.5s", "for is whole seconds"},
		{"rule x threshold memory_share > 0.9 for soon", `"soon" is not a duration`},
		{"rule x threshold memory_share > 0.9 for 10m min_requests 20", "min_requests is the sample floor of a share or a quantile"},
		{"rule x threshold cpu_cores > 0.9 for 10m min_requests 20", "not of cpu_cores"},
		{"rule x threshold request_rate > 0.9 for 10m min_requests 20", "not of request_rate"},
		{"rule x threshold 5xx_share > 0.05 for 10m min_requests 0", `min_requests: "0" is not a whole number from 1`},
		{"rule x threshold 5xx_share > 0.05 for 10m min_requests many", `"many" is not a whole number from 1`},
		{"rule x threshold 5xx_share > 0.05 for 10m min_requests -1", "is not a whole number"},
		{"rule x threshold 5xx_share > 0.05 for 10m severity info", `severity is warning or critical, not "info"`},
		{"rule x threshold 5xx_share > 0.05 for 10m severity", "severity takes a value"},
		{"rule x threshold 5xx_share > 0.05 for 10m app", "app takes a value"},
		{"rule x threshold 5xx_share > 0.05 for 10m app Shop", `app "Shop" is not an app name`},
		{"rule x threshold 5xx_share > 0.05 for 10m app shop app blog", "app is given more than once"},
		{"rule x threshold 5xx_share > 0.05 for 10m expr up", `unknown option "expr"; it takes app, min_requests, severity`},
		{"rule x threshold 5xx_share > 0.05 for 10m {\n\t\tapp shop\n\t}", "a threshold rule takes no block"},
		{"rule x disk_fill 7d", "disk_fill takes within <duration>"},
		{"rule x disk_fill within 30m", "within is whole seconds from 1h to 30d"},
		{"rule x disk_fill within 31d", "within is whole seconds from 1h to 30d"},
		{"rule x disk_fill within 7d app shop", `unknown option "app"; it takes severity`},
		{"rule x disk_fill within 7d severity page", "severity is warning or critical"},
		{"rule x threshold 5xx_share > 0.05 for 10m\n\trule x disk_fill within 7d", "rule x is defined twice"},
	} {
		if _, err := parse(t, withRules(c.line)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want %q", c.line, err, c.want)
		}
	}
}

// A rule line reads the store's numbers: without the metrics line it is
// refused, naming the line, wherever the metrics line would be.
func TestRuleNeedsTheMetricsLine(t *testing.T) {
	src := strings.TrimSuffix(fullBlock, "}") + "\trule errors_high threshold 5xx_share > 0.05 for 10m\n}"
	_, err := parse(t, src)
	if err == nil || !strings.Contains(err.Error(), errRuleNeedsMetrics.Error()) || !strings.Contains(err.Error(), ":11") {
		t.Fatalf("err = %v, want the refusal at the rule's line", err)
	}
	// After the rule, the metrics line counts.
	mustParse(t, strings.TrimSuffix(fullBlock, "}")+"\trule errors_high threshold 5xx_share > 0.05 for 10m\n\tmetrics\n}")
}

// JSON never goes through parseRule: Provision holds it to the same
// grammar, and checks every app a rule names against liveswap's.
func TestRuleRefusalsAtProvision(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "deploy.pub")
	writePubKey(t, key)
	block := func(rules string) string {
		return "{\n\tadmin off\n\tliveswap {\n\t\troot " + filepath.Join(dir, "liveswap") + "\n\t\tartifact_allowlist example.org\n\t\tapp blog {\n\t\t\tcommand ./server\n" +
			"\t\t\tdeploy_trust local {\n\t\t\t\tpublic_key " + key + "\n\t\t\t\taudience blog\n\t\t\t}\n\t\t}\n\t}\n" +
			"\tmonitor {\n\t\tmetrics /run/x/vm.sock\n\t\t" + rules + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	}
	if _, err := provision(t, block("rule blog_errors threshold 5xx_share > 0.05 for 10m app blog")); err != nil {
		t.Fatalf("a rule on a configured app: %v", err)
	}
	if _, err := provision(t, block("rule shop_errors threshold 5xx_share > 0.05 for 10m app shop")); err == nil ||
		!strings.Contains(err.Error(), "rule shop_errors names app shop, which the liveswap block does not configure") {
		t.Errorf("a rule on an unknown app: %v", err)
	}
	noLiveswap := "{\n\tadmin off\n\tmonitor {\n\t\tmetrics /run/x/vm.sock\n\t\trule e threshold 5xx_share > 0.05 for 10m app blog\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	if _, err := provision(t, noLiveswap); err == nil || !strings.Contains(err.Error(), "no liveswap block") {
		t.Errorf("a rule naming an app with no liveswap block: %v", err)
	}
	for _, c := range []struct {
		rule Rule
		want string
	}{
		{Rule{Name: "x", Kind: ruleThreshold, Metric: "5xx_share", Op: ">", Value: 0.05}, "for is whole seconds from 1m to 24h"},
		{Rule{Name: "x", Kind: ruleThreshold, Metric: "up", Op: ">", Value: 0.05, For: 600e9}, `unknown metric "up"`},
		{Rule{Name: "x", Kind: ruleDiskFill, Within: 3600e9, Metric: "p95"}, "metric, op, value and for are a threshold's"},
		{Rule{Name: "x", Kind: ruleThreshold, Metric: "5xx_share", Op: ">", Value: 0.05, For: 600e9, Within: 3600e9}, "within is disk_fill's"},
		{Rule{Name: "x", Kind: ruleDiskFill, Within: 3600e9, App: "blog"}, "disk_fill is the box's"},
		{Rule{Name: "x", Kind: ruleDiskFill, Within: 3600e9, MinRequests: 5}, "min_requests is a request rule's"},
		{Rule{Name: "x", Kind: "expr"}, `unknown kind "expr"`},
		{Rule{Name: "X", Kind: ruleDiskFill, Within: 3600e9}, "a rule's name is lowercase"},
	} {
		if err := checkRules([]Rule{c.rule}, map[string]bool{"blog": true}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.rule, err, c.want)
		}
	}
}

func TestOperatorRulesRender(t *testing.T) {
	a := rulesApp(t,
		"rule errors_high threshold 5xx_share > 0.05 for 10m",
		"rule shop_slow threshold p95 > 1.5s for 15m app shop min_requests 50 severity critical",
		"rule blog_quiet threshold request_rate < 0.1 for 1h app blog",
		"rule hits_low threshold cache_hit_share < 0.5 for 30m",
		"rule mem threshold memory_share > 0.8 for 5m",
		"rule busy threshold cpu_cores > 1.5 for 10m app shop",
		"rule median threshold p50 > 200ms for 10m",
		"rule disk_filling disk_fill within 7d severity critical",
	)
	f := parseRules(t, renderRules(a))
	if len(f.Groups) != 3 || f.Groups[2].Name != operatorGroup || f.Groups[2].Interval != "1m" {
		t.Fatalf("groups = %+v", f.Groups)
	}
	rules := map[string]vmRule{}
	for _, r := range f.Groups[2].Rules {
		rules[r.Alert] = r
	}
	if len(rules) != 8 {
		t.Fatalf("operator rules = %+v; want every line", f.Groups[2].Rules)
	}
	every := func(r string) string { return `sum by (app) (increase_pure(motel_app_requests_total{` + r + `}[5m]))` }
	shop := func(metric, extra string) string {
		return `sum by (app) (increase_pure(` + metric + `{app="shop"` + extra + `}[5m]))`
	}
	for name, want := range map[string]string{
		"errors_high": "(" + every(`class="5xx"`) + " / " + every(`class!="aborted"`) + ") > 0.05 and " + every(`class!="aborted"`) + " >= 20",
		"shop_slow": `histogram_quantile(0.95, sum by (app, le) (increase_pure(motel_app_response_seconds_bucket{app="shop"}[5m]))) > 1.5 and ` +
			shop("motel_app_response_seconds_count", "") + " >= 50",
		"blog_quiet": `sum by (app) (increase_pure(motel_app_requests_total{app="blog"}[5m])) / 300 < 0.1`,
		"hits_low": `(sum by (app) (increase_pure(motel_app_cache_responses_total{result=~"hit|stale"}[5m])) or sum by (app) (increase_pure(motel_app_cache_responses_total[5m])) * 0)` +
			` / sum by (app) (increase_pure(motel_app_cache_responses_total[5m])) < 0.5 and sum by (app) (increase_pure(motel_app_cache_responses_total[5m])) >= 20`,
		"mem":          `max by (app) (motel_app_memory_bytes / motel_app_memory_max_bytes) > 0.8`,
		"busy":         `sum by (app) (rate(motel_app_cpu_seconds_total{app="shop"}[5m])) > 1.5`,
		"median":       `histogram_quantile(0.5, sum by (app, le) (increase_pure(motel_app_response_seconds_bucket[5m]))) > 0.2 and sum by (app) (increase_pure(motel_app_response_seconds_count[5m])) >= 20`,
		"disk_filling": `min by (path) (predict_linear(motel_disk_avail_bytes[6h], 604800)) < 0 and min by (path) (predict_linear(motel_disk_avail_bytes[1h], 604800)) < 0`,
	} {
		r := rules[name]
		if r.Expr != want {
			t.Errorf("%s: expr =\n%s\nwant\n%s", name, r.Expr, want)
		}
		// The request counters through increase_pure: the store's
		// increase drops a new series' first value.
		if strings.Contains(r.Expr, "increase(") {
			t.Errorf("%s reads a counter through increase: %s", name, r.Expr)
		}
	}
	for name, want := range map[string]map[string]string{
		"errors_high":  {"kind": "state", "severity": "warning", "subject": "app:{{ $labels.app }}", "source": "rules", "key": "rules/app:{{ $labels.app }}/errors_high"},
		"shop_slow":    {"kind": "state", "severity": "critical", "subject": "app:shop", "source": "rules", "key": "rules/app:shop/shop_slow"},
		"disk_filling": {"kind": "state", "severity": "critical", "subject": "box", "source": "rules", "key": "rules/box/disk_filling"},
	} {
		got := rules[name].Labels
		if len(got) != len(want) {
			t.Errorf("%s: labels %v, want %v", name, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: label %s = %q, want %q", name, k, got[k], v)
			}
		}
	}
	for name, want := range map[string][2]string{
		"errors_high":  {"10m", "{{ $labels.app }}: the 5xx share of its responses over 5 minutes has been above 0.05 for 10m (rule errors_high); now {{ $value | humanizePercentage }}"},
		"shop_slow":    {"15m", "shop: its p95 time to headers over 5 minutes has been above 1.5s for 15m (rule shop_slow); now {{ $value | humanizeDuration }}"},
		"blog_quiet":   {"1h", "blog: its requests a second over 5 minutes has been below 0.1 for 1h (rule blog_quiet); now {{ $value | humanize }}"},
		"disk_filling": {"30m", "the filesystem holding {{ $labels.path }} is predicted to fill within 7d (rule disk_filling)"},
	} {
		if r := rules[name]; r.For != want[0] || r.Annotations["summary"] != want[1] {
			t.Errorf("%s: for %q, summary %q; want %q, %q", name, r.For, r.Annotations["summary"], want[0], want[1])
		}
	}
	// A store-alone block: the group is there, empty, as the fixed ones.
	if g := parseRules(t, renderRules(nil)).Groups[2]; g.Name != operatorGroup || len(g.Rules) != 0 {
		t.Errorf("no notify: %+v", g)
	}
}

// A name an app or a release could hold is quoted in, never spliced.
func TestThresholdQuotesTheApp(t *testing.T) {
	r := Rule{Name: "x", Kind: ruleThreshold, App: "a-b", Metric: "memory_share", Op: ">", Value: 0.5, For: 600e9}
	if got := thresholdExpr(r); got != `max by (app) (motel_app_memory_bytes{app="a-b"} / motel_app_memory_max_bytes{app="a-b"}) > 0.5` {
		t.Fatal(got)
	}
}

func TestOperatorRulesPassVMAlertDryRun(t *testing.T) {
	tool := vmTool(t, "vmalert")
	path := filepath.Join(t.TempDir(), RulesName)
	a := rulesApp(t,
		"rule errors_high threshold 5xx_share > 0.05 for 10m",
		"rule shop_slow threshold p95 > 1.5s for 15m app shop",
		"rule hits_low threshold cache_hit_share < 0.5 for 30m",
		"rule mem threshold memory_share > 0.8 for 5m",
		"rule busy threshold cpu_cores > 1.5 for 10m app shop",
		"rule rate threshold request_rate > 100 for 2m",
		"rule disk_filling disk_fill within 7d",
	)
	if err := os.WriteFile(path, renderRules(a), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(tool, "-dryRun", "-rule="+path).CombinedOutput(); err != nil {
		t.Fatalf("vmalert -dryRun: %v\n%s", err, out)
	}
}

// vmalert-tool runs the operator's rules over input series: a 5xx
// share over its bound fires per app, with the app's labels, once the
// floor is met; one under the floor does not; a filling disk fires for
// its path.
func TestOperatorRulesFire(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	a := rulesApp(t,
		"rule errors_high threshold 5xx_share > 0.5 for 2m",
		"rule shop_hits threshold cache_hit_share < 0.5 for 2m app shop min_requests 5",
		"rule disk_filling disk_fill within 1d",
	)
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(a), 0o600); err != nil {
		t.Fatal(err)
	}
	// shop answers 5xx from its first push on, its series appearing at
	// 30 already (increase_pure counts them); blog has two 5xx of three,
	// under the floor of 20. shop's cache spoke and never hit. A disk
	// loses a GB a minute.
	test := `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'motel_app_requests_total{app="shop",release="",class="5xx",job="motel"}'
        values: '30+30x20'
      - series: 'motel_app_requests_total{app="shop",release="",class="2xx",job="motel"}'
        values: '0x20'
      - series: 'motel_app_requests_total{app="shop",release="",class="aborted",job="motel"}'
        values: '0+5x20'
      - series: 'motel_app_requests_total{app="blog",release="v1",class="5xx",job="motel"}'
        values: '2x20'
      - series: 'motel_app_requests_total{app="blog",release="v1",class="2xx",job="motel"}'
        values: '1x20'
      - series: 'motel_app_cache_responses_total{app="shop",release="",result="miss",job="motel"}'
        values: '10+10x20'
      - series: 'motel_disk_avail_bytes{path="/var/lib/liveswap",job="motel"}'
        values: '100000000000-1000000000x480'
    alert_rule_test:
      - eval_time: 4m
        groupname: motel-operator
        alertname: errors_high
        exp_alerts:
          - exp_labels: {app: shop, kind: state, severity: warning, subject: "app:shop", source: rules, key: "rules/app:shop/errors_high"}
            exp_annotations: {summary: "shop: the 5xx share of its responses over 5 minutes has been above 0.5 for 2m (rule errors_high); now 100%"}
      - eval_time: 4m
        groupname: motel-operator
        alertname: shop_hits
        exp_alerts:
          - exp_labels: {app: shop, kind: state, severity: warning, subject: "app:shop", source: rules, key: "rules/app:shop/shop_hits"}
            exp_annotations: {summary: "shop: the hit share of the responses its cache spoke for over 5 minutes has been below 0.5 for 2m (rule shop_hits); now 0%"}
      - eval_time: 7h
        groupname: motel-operator
        alertname: disk_filling
        exp_alerts:
          - exp_labels: {path: /var/lib/liveswap, kind: state, severity: warning, subject: box, source: rules, key: "rules/box/disk_filling"}
            exp_annotations: {summary: "the filesystem holding /var/lib/liveswap is predicted to fill within 1d (rule disk_filling)"}
`
	if err := os.WriteFile(filepath.Join(dir, "test.yml"), []byte(test), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(tool, "unittest", "-files", "test.yml")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SUCCESS") {
		t.Fatalf("vmalert-tool unittest: %v\n%s", err, out)
	}
}
