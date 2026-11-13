package notify

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		"rule regress post_deploy {\n\t\twindow 20m\n\t\tmax_5xx_share 0.02\n\t}",
		"rule blog_regress post_deploy app blog {\n\t\tmin_requests 30\n\t\tmax_p95 800ms\n\t\tfailures 2\n\t}",
	))
	b, _ := json.Marshal(a.Metrics)
	got := strings.NewReplacer(`\u003e`, ">", `\u003c`, "<").Replace(string(b))
	want := `{"rules":[` +
		`{"name":"errors_high","kind":"threshold","metric":"5xx_share","op":">","value":0.05,"for":600000000000},` +
		`{"name":"shop_slow","kind":"threshold","app":"shop","severity":"critical","metric":"p95","op":">","value":1.5,"for":900000000000,"min_requests":50},` +
		`{"name":"blog_quiet","kind":"threshold","app":"blog","metric":"request_rate","op":"<","value":0.1,"for":3600000000000},` +
		`{"name":"disk_filling","kind":"disk_fill","within":604800000000000},` +
		`{"name":"regress","kind":"post_deploy","window":1200000000000,"max_5xx_share":0.02},` +
		`{"name":"blog_regress","kind":"post_deploy","app":"blog","min_requests":30,"max_p95":800000000,"failures":2}]}`
	if got != want {
		t.Fatalf("metrics =\n%s\nwant\n%s", got, want)
	}
	r := a.Metrics.Rules
	if r[4].minRequests() != 100 || r[4].failures() != 3 || r[4].window().String() != "20m0s" || r[0].minRequests() != 20 || r[0].severity() != SeverityWarning {
		t.Errorf("defaults: %d %d %s %d %s", r[4].minRequests(), r[4].failures(), r[4].window(), r[0].minRequests(), r[0].severity())
	}
}

func TestRuleRefusals(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"rule", "rule takes <name> threshold|disk_fill|post_deploy"},
		{"rule x", "rule takes <name> threshold|disk_fill|post_deploy"},
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
		{"rule post_deploy_regressed threshold memory_share > 0.5 for 10m", "is one of motel's own alerts"},
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
		{"rule x post_deploy", "post_deploy needs max_5xx_share, max_p95 or both"},
		{"rule x post_deploy {\n\t}", "post_deploy needs max_5xx_share, max_p95 or both"},
		{"rule x post_deploy {\n\t\twindow 30m\n\t}", "post_deploy needs max_5xx_share, max_p95 or both"},
		{"rule x post_deploy {\n\t\tmax_5xx_share 0.02\n\t\tquery up\n\t}", `unknown post_deploy parameter "query"`},
		{"rule x post_deploy {\n\t\tmax_5xx_share 0.02\n\t\tseverity critical\n\t}", `unknown post_deploy parameter "severity"`},
		{"rule x post_deploy {\n\t\tmax_5xx_share 0.02 0.03\n\t}", "max_5xx_share takes one value"},
		{"rule x post_deploy {\n\t\tmax_5xx_share 0.02\n\t\tmax_5xx_share 0.03\n\t}", "max_5xx_share is given more than once"},
		{"rule x post_deploy {\n\t\tmax_5xx_share 2\n\t}", "max_5xx_share is a share between 0 and 1"},
		{"rule x post_deploy {\n\t\tmax_5xx_share 0\n\t}", "max_5xx_share is a share between 0 and 1"},
		{"rule x post_deploy {\n\t\tmax_p95 10s\n\t}", "max_p95 is from 1ms to under 10s"},
		{"rule x post_deploy {\n\t\tmax_p95 0.5ms\n\t}", "max_p95 is from 1ms to under 10s"},
		{"rule x post_deploy {\n\t\tmax_p95 800\n\t}", "is not a duration in ms or s"},
		{"rule x post_deploy {\n\t\tmax_p95 800ms\n\t\twindow 90s\n\t}", "window is whole minutes from 1m to 24h"},
		{"rule x post_deploy {\n\t\tmax_p95 800ms\n\t\twindow 25h\n\t}", "window is whole minutes from 1m to 24h"},
		{"rule x post_deploy {\n\t\tmax_p95 800ms\n\t\tfailures 0\n\t}", `failures: "0" is not a whole number from 1`},
		{"rule x post_deploy {\n\t\tmax_p95 800ms\n\t\tfailures 61\n\t}", "failures is from 1 to 60"},
		{"rule x post_deploy {\n\t\tmax_p95 800ms\n\t\twindow 2m\n\t}", "3 failures in a row need a window of at least 3 minutes"},
		{"rule x post_deploy {\n\t\tmax_p95 800ms\n\t\tmin_requests 0\n\t}", `min_requests: "0" is not a whole number from 1`},
		{"rule x post_deploy app {\n\t\tmax_p95 800ms\n\t}", "app takes a value"},
		{"rule x post_deploy shop {\n\t\tmax_p95 800ms\n\t}", `unknown option "shop"; it takes app`},
		{"rule x post_deploy {\n\t\tmax_p95 800ms {\n\t\t\tx\n\t\t}\n\t}", "max_p95 takes no block"},
		{"rule x threshold 5xx_share > 0.05 for 10m\n\trule x disk_fill within 7d", "rule x is defined twice"},
		{"rule a post_deploy {\n\t\tmax_p95 800ms\n\t}\n\trule b post_deploy {\n\t\tmax_5xx_share 0.1\n\t}", "a second post_deploy for every app"},
		{"rule a post_deploy app shop {\n\t\tmax_p95 800ms\n\t}\n\trule b post_deploy app shop {\n\t\tmax_5xx_share 0.1\n\t}", "a second post_deploy for shop"},
	} {
		if _, err := parse(t, withRules(c.line)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want %q", c.line, err, c.want)
		}
	}
	// One for every app and one per app is no duplicate: the app's
	// replaces the other for it.
	mustParse(t, withRules("rule a post_deploy {\n\t\tmax_p95 800ms\n\t}", "rule b post_deploy app shop {\n\t\tmax_p95 1s\n\t}"))
}

// A refusal of the lines together names the second rule's line; a
// latency is kept to the nanosecond as written.
func TestRuleLinesTogether(t *testing.T) {
	_, err := parse(t, withRules("rule x threshold 5xx_share > 0.05 for 10m", "rule y disk_fill within 7d", "rule x disk_fill within 1d"))
	if err == nil || !strings.Contains(err.Error(), "rule x is defined twice, at Testfile:14") {
		t.Errorf("a duplicate: %v", err)
	}
	_, err = parse(t, withRules("rule a post_deploy {\n\t\tmax_p95 1s\n\t}", "rule b post_deploy {\n\t\tmax_p95 2s\n\t}"))
	if err == nil || !strings.Contains(err.Error(), "rule b: a second post_deploy for every app") || !strings.Contains(err.Error(), "at Testfile:15") {
		t.Errorf("a second post_deploy: %v", err)
	}
	a := mustParse(t, withRules("rule a post_deploy {\n\t\tmax_p95 1001ms\n\t}", "rule b threshold p95 > 1.001s for 10m"))
	if got := time.Duration(a.Metrics.Rules[0].MaxP95); got != 1001*time.Millisecond {
		t.Errorf("max_p95 1001ms = %s", got)
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
	secrets := filepath.Join(dir, "_box.env")
	if err := os.WriteFile(secrets, []byte("NOTIFY_OPS_URL=https://ntfy.sh/ops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notify := "\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tsecrets_file " + secrets + "\n"
	block := func(rules string) string {
		return "{\n\tadmin off\n\tliveswap {\n\t\troot " + filepath.Join(dir, "liveswap") + "\n\t\tartifact_allowlist example.org\n\t\tapp blog {\n\t\t\tcommand ./server\n" +
			"\t\t\tdeploy_trust local {\n\t\t\t\tpublic_key " + key + "\n\t\t\t\taudience blog\n\t\t\t}\n\t\t}\n\t}\n" +
			"\tmonitor {\n" + notify + "\t\tmetrics /run/x/vm.sock\n\t\t" + rules + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	}
	if _, err := provision(t, block("rule blog_errors threshold 5xx_share > 0.05 for 10m app blog")); err != nil {
		t.Fatalf("a rule on a configured app: %v", err)
	}
	if _, err := provision(t, block("rule shop_errors threshold 5xx_share > 0.05 for 10m app shop")); err == nil ||
		!strings.Contains(err.Error(), "rule shop_errors names app shop, which the liveswap block does not configure") {
		t.Errorf("a rule on an unknown app: %v", err)
	}
	if _, err := provision(t, block("rule shop_regress post_deploy app shop {\n\t\t\tmax_p95 1s\n\t\t}")); err == nil ||
		!strings.Contains(err.Error(), "names app shop") {
		t.Errorf("a post_deploy on an unknown app: %v", err)
	}
	// memory_share of an app with no memory_max: nothing to divide by.
	if _, err := provision(t, block("rule blog_mem threshold memory_share > 0.8 for 5m app blog")); err == nil ||
		!strings.Contains(err.Error(), "rule blog_mem: app blog has no memory_max, so its memory_share has nothing to divide by") {
		t.Errorf("memory_share of an uncapped app: %v", err)
	}
	capped := strings.Replace(block("rule blog_mem threshold memory_share > 0.8 for 5m app blog"), "command ./server\n", "command ./server\n\t\t\tmemory_max 512MB\n", 1)
	if _, err := provision(t, capped); err != nil {
		t.Errorf("memory_share of a capped app: %v", err)
	}
	if _, err := provision(t, block("rule mem threshold memory_share > 0.8 for 5m")); err != nil {
		t.Errorf("an every-app memory_share: %v", err)
	}
	// A block with no channel: the rules would reach no one.
	alone := "{\n\tadmin off\n\tmonitor {\n\t\tmetrics /run/x/vm.sock\n\t\trule e threshold 5xx_share > 0.05 for 10m\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	if _, err := provision(t, alone); err == nil || !strings.Contains(err.Error(), "rule e reaches no one: the monitor block has no channel") {
		t.Errorf("rules in a block with no channel: %v", err)
	}
	noLiveswap := "{\n\tadmin off\n\tmonitor {\n" + notify + "\t\tmetrics /run/x/vm.sock\n\t\trule e threshold 5xx_share > 0.05 for 10m app blog\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
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
		{Rule{Name: "x", Kind: ruleThreshold, Metric: "5xx_share", Op: ">", Value: 0.05, For: 600e9, Failures: 2}, "are post_deploy's"},
		{Rule{Name: "x", Kind: ruleDiskFill, Within: 3600e9, App: "blog"}, "disk_fill is the box's"},
		{Rule{Name: "x", Kind: ruleDiskFill, Within: 3600e9, MinRequests: 5}, "min_requests is a request rule's"},
		{Rule{Name: "x", Kind: rulePostDeploy, MaxP95: 1e9, Severity: SeverityCritical}, "it takes no severity"},
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
		"rule regress post_deploy {\n\t\tmax_5xx_share 0.02\n\t}",
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
		t.Fatalf("operator rules = %+v; want the threshold and disk_fill lines, no post_deploy", f.Groups[2].Rules)
	}
	inc := func(sel string) string { return counterIncrease(sel, 5*time.Minute, 0) }
	hist := func(m string) string { return histogramIncrease("motel_app_response_seconds", m, 5*time.Minute, 0) }
	every := func(r string) string { return `sum by (app) (` + inc(`motel_app_requests_total{`+r+`}`) + `)` }
	shop := func(metric, extra string) string {
		return `sum by (app) (` + inc(metric+`{app="shop"`+extra+`}`) + `)`
	}
	// The request thresholds hold for the apps in motel's last push.
	pushed := func(sel string) string {
		return ` and on (app) (max by (app) (tlast_over_time(motel_app_observed` + sel + `[75s]))` +
			` > scalar(max(tlast_over_time(process_start_time_seconds{job="motel"}[75s]))) - 15)`
	}
	for name, want := range map[string]string{
		"errors_high": "(" + every(`class="5xx"`) + " / " + every(`class!="aborted"`) + ") > 0.05 and " + every(`class!="aborted"`) + " >= 20" + pushed(""),
		"shop_slow": `histogram_quantile(0.95, sum by (app, le) (` + hist(`{app="shop"}`) + `)) > 1.5 and ` +
			shop("motel_app_response_seconds_count", "") + " >= 50" + pushed(`{app="shop"}`),
		"blog_quiet": `sum by (app) (` + inc(`motel_app_requests_total{app="blog"}`) + `) / 300 < 0.1` + pushed(`{app="blog"}`),
		"hits_low": `(sum by (app) (` + inc(`motel_app_cache_responses_total{result=~"hit|stale"}`) + `) or sum by (app) (` + inc(`motel_app_cache_responses_total`) + `) * 0)` +
			` / sum by (app) (` + inc(`motel_app_cache_responses_total`) + `) < 0.5 and sum by (app) (` + inc(`motel_app_cache_responses_total`) + `) >= 20` + pushed(""),
		"mem":    `max by (app) (motel_app_memory_bytes / motel_app_memory_max_bytes) > 0.8`,
		"busy":   `sum by (app) (rate(motel_app_cpu_seconds_total{app="shop"}[5m])) > 1.5`,
		"median": `histogram_quantile(0.5, sum by (app, le) (` + hist(``) + `)) > 0.2 and sum by (app) (` + inc(`motel_app_response_seconds_count`) + `) >= 20` + pushed(""),
		"disk_filling": `min by (path) (predict_linear(motel_disk_avail_bytes[42h], 604800)) < 0 and min by (path) (predict_linear(motel_disk_avail_bytes[7h], 604800)) < 0` +
			` and max by (path) (present_over_time(motel_disk_avail_bytes[42h] offset 35h))`,
	} {
		r := rules[name]
		if r.Expr != want {
			t.Errorf("%s: expr =\n%s\nwant\n%s", name, r.Expr, want)
		}
	}
	// The guard itself, whole: increase_pure for a series new to the
	// store, or back below its last value, first seen over half a push
	// after the push's first sample in the window and the four pushes
	// before it; increase otherwise. A histogram's buckets are told by
	// its count.
	if got, want := counterIncrease(`m{app="a"}`, 150*time.Second, 90*time.Second),
		`((increase_pure(m{app="a"}[150s] offset 90s) unless (present_over_time(m{app="a"}[1d] offset 4m)`+
			` unless (last_over_time(m{app="a"}[1d] offset 4m) > first_over_time(m{app="a"}[150s] offset 90s))))`+
			` and (tfirst_over_time(m{app="a"}[150s] offset 90s) > scalar(min(tfirst_over_time(process_start_time_seconds{job="motel"}[270s] offset 90s))) + 15))`+
			` or increase(m{app="a"}[150s] offset 90s)`; got != want {
		t.Errorf("counterIncrease =\n%s\nwant\n%s", got, want)
	}
	if got, want := histogramIncrease("h", `{app="a"}`, 5*time.Minute, 0),
		`((increase_pure(h_bucket{app="a"}[5m]) unless ignoring(le) (present_over_time(h_count{app="a"}[1d] offset 5m)`+
			` unless (last_over_time(h_count{app="a"}[1d] offset 5m) > first_over_time(h_count{app="a"}[5m]))))`+
			` and ignoring(le) (tfirst_over_time(h_count{app="a"}[5m]) > scalar(min(tfirst_over_time(process_start_time_seconds{job="motel"}[7m]))) + 15))`+
			` or increase(h_bucket{app="a"}[5m])`; got != want {
		t.Errorf("histogramIncrease =\n%s\nwant\n%s", got, want)
	}
	for name, want := range map[string]map[string]string{
		"errors_high":  {"kind": "state", "severity": "warning", "subject": "app:{{ $labels.app }}", "source": "rules", "key": "rules/app:{{ $labels.app }}/errors_high"},
		"shop_slow":    {"kind": "state", "severity": "critical", "subject": "app:shop", "source": "rules", "key": "rules/app:shop/shop_slow"},
		"disk_filling": {"kind": "state", "severity": "critical", "subject": "box", "source": "rules", "key": "rules/box/disk_filling/{{ $labels.path }}"},
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
		"disk_filling": {"210m", "the filesystem holding {{ $labels.path }} is predicted to fill within 7d (rule disk_filling)"},
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
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x480'
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
      - series: 'motel_app_observed{app="shop",job="motel"}'
        values: '1x20'
      - series: 'motel_app_observed{app="blog",job="motel"}'
        values: '1x20'
      - series: 'motel_app_cache_responses_total{app="shop",release="",result="miss",job="motel"}'
        values: '10+10x20'
      - series: 'motel_disk_avail_bytes{path="/var/lib/liveswap",job="motel"}'
        values: '100000000000-1000000000x480'
      - series: 'motel_disk_avail_bytes{path="/var/lib/caddy",job="motel"}'
        values: '50000000000-1000000000x480'
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
          - exp_labels: {path: /var/lib/caddy, kind: state, severity: warning, subject: box, source: rules, key: "rules/box/disk_filling//var/lib/caddy"}
            exp_annotations: {summary: "the filesystem holding /var/lib/caddy is predicted to fill within 1d (rule disk_filling)"}
          - exp_labels: {path: /var/lib/liveswap, kind: state, severity: warning, subject: box, source: rules, key: "rules/box/disk_filling//var/lib/liveswap"}
            exp_annotations: {summary: "the filesystem holding /var/lib/liveswap is predicted to fill within 1d (rule disk_filling)"}
`
	runUnittest(t, tool, dir, test)
}

// runUnittest runs vmalert-tool's unittest over test, in dir beside
// its rules.yml, and fails the test unless every case read as wanted.
func runUnittest(t *testing.T, tool, dir, test string) {
	t.Helper()
	if out, ok := vmUnittest(t, tool, dir, test); !ok {
		t.Fatalf("vmalert-tool unittest:\n%s", out)
	}
}

// vmUnittest runs vmalert-tool's unittest over test, in dir beside its
// rules.yml, and says whether every case read as wanted, with the
// tool's output.
func vmUnittest(t *testing.T, tool, dir, test string) (string, bool) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "test.yml"), []byte(test), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(tool, "unittest", "-files", "test.yml")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%v\n%s", err, out), false
	}
	return string(out), strings.Contains(string(out), "SUCCESS")
}

// A request counter over the store's own MetricsQL: a series new to the
// store once the push is older than the window counts from zero (fresh
// answers a burst of 400 and nothing after, which increase would drop),
// and so does one back below its last value (reused, a release name
// rolled back to with a fresh counter); one back after a 20-minute gap
// in the pushes is not read as its lifetime (gapped counts 10 a minute
// throughout, 500 by its return); nor is one that came with the push
// itself (the metrics line first loaded on a box whose observer had
// counted 5000).
func TestOperatorRulesCountAfterAGap(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	a := rulesApp(t, "rule busy threshold request_rate > 1 for 2m")
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(a), 0o600); err != nil {
		t.Fatal(err)
	}
	test := `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x90'
      - series: 'motel_app_requests_total{app="fresh",release="v2",class="2xx",job="motel"}'
        values: '_x30 400x60'
      - series: 'motel_app_requests_total{app="gapped",release="v1",class="2xx",job="motel"}'
        values: '0+10x30 _x20 500+10x40'
      - series: 'motel_app_requests_total{app="reused",release="v1",class="2xx",job="motel"}'
        values: '0+100x10 _x30 400x50'
      - series: 'motel_app_observed{app="fresh",job="motel"}'
        values: '_x30 1x60'
      - series: 'motel_app_observed{app="gapped",job="motel"}'
        values: '1x30 _x20 1x40'
      - series: 'motel_app_observed{app="reused",job="motel"}'
        values: '1x10 _x30 1x50'
    alert_rule_test:
      - eval_time: 33m
        groupname: motel-operator
        alertname: busy
        exp_alerts:
          - exp_labels: {app: fresh, kind: state, severity: warning, subject: "app:fresh", source: rules, key: "rules/app:fresh/busy"}
            exp_annotations: {summary: "fresh: its requests a second over 5 minutes has been above 1 for 2m (rule busy); now 1.333"}
      - eval_time: 44m
        groupname: motel-operator
        alertname: busy
        exp_alerts:
          - exp_labels: {app: reused, kind: state, severity: warning, subject: "app:reused", source: rules, key: "rules/app:reused/busy"}
            exp_annotations: {summary: "reused: its requests a second over 5 minutes has been above 1 for 2m (rule busy); now 1.333"}
      - eval_time: 54m
        groupname: motel-operator
        alertname: busy
        exp_alerts: []
  - interval: 1m
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '_x30 1x60'
      - series: 'motel_app_requests_total{app="old",release="v1",class="2xx",job="motel"}'
        values: '_x30 5000+10x60'
      - series: 'motel_app_observed{app="old",job="motel"}'
        values: '_x30 1x60'
    alert_rule_test:
      - eval_time: 34m
        groupname: motel-operator
        alertname: busy
        exp_alerts: []
  - interval: 1m
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x59 _x20 1x40'
      - series: 'motel_app_requests_total{app="born",release="v2",class="2xx",job="motel"}'
        values: '_x80 900+50x40'
      - series: 'motel_app_requests_total{app="restarted",release="v1",class="2xx",job="motel"}'
        values: '0+50x59 _x20 750+50x40'
      - series: 'motel_app_requests_total{app="after",release="v3",class="2xx",job="motel"}'
        values: '_x90 400x30'
      - series: 'motel_app_observed{app="born",job="motel"}'
        values: '_x80 1x40'
      - series: 'motel_app_observed{app="restarted",job="motel"}'
        values: '1x59 _x20 1x40'
      - series: 'motel_app_observed{app="after",job="motel"}'
        values: '_x90 1x30'
    alert_rule_test:
      - eval_time: 84m
        groupname: motel-operator
        alertname: busy
        exp_alerts: []
      - eval_time: 93m
        groupname: motel-operator
        alertname: busy
        exp_alerts:
          - exp_labels: {app: after, kind: state, severity: warning, subject: "app:after", source: rules, key: "rules/app:after/busy"}
            exp_annotations: {summary: "after: its requests a second over 5 minutes has been above 1 for 2m (rule busy); now 1.333"}
`
	runUnittest(t, tool, dir, test)
}

// On a store younger than the window (here a minute old), a series
// that comes after the push's first sample is new since and counts from
// zero: fresh's 22 responses (e2e metrics 3a's), which increase would
// drop, fire a rate of 22 over 5 minutes; with no push in the store at
// all the guard is increase, which drops them, and nothing fires or
// fails.
func TestOperatorRulesCountOnAFreshStore(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	a := rulesApp(t, "rule busy threshold request_rate > 0.05 for 2m")
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(a), 0o600); err != nil {
		t.Fatal(err)
	}
	test := `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '_x30 1x30'
      - series: 'motel_app_requests_total{app="fresh",release="v1",class="2xx",job="motel"}'
        values: '_x31 22x29'
      - series: 'motel_app_observed{app="fresh",job="motel"}'
        values: '_x31 1x29'
    alert_rule_test:
      - eval_time: 34m
        groupname: motel-operator
        alertname: busy
        exp_alerts:
          - exp_labels: {app: fresh, kind: state, severity: warning, subject: "app:fresh", source: rules, key: "rules/app:fresh/busy"}
            exp_annotations: {summary: "fresh: its requests a second over 5 minutes has been above 0.05 for 2m (rule busy); now 73.33m"}
  - interval: 1m
    input_series:
      - series: 'motel_app_requests_total{app="nopush",release="v1",class="2xx",job="motel"}'
        values: '_x31 22x29'
      - series: 'motel_app_observed{app="nopush",job="motel"}'
        values: '_x31 1x29'
    alert_rule_test:
      - eval_time: 34m
        groupname: motel-operator
        alertname: busy
        exp_alerts: []
`
	runUnittest(t, tool, dir, test)
}

// A request threshold reads the apps motel's last push says are
// observed, at pushEvery's 30 s. gone's observe line is taken out at
// 12m30s while the pushes go on: its 2 a second falls a push's worth at
// each push after, and it is out from the first, so at 13m10s a
// `request_rate < 1.9` reads quiet and reattached alone (gone's 1.8
// was the bound's) and `request_rate < 1.5` never fires for it. quiet,
// observed and never reached, fires and stays firing at 6m, a minute
// after the last push before two lost ones (the most one lost push
// leaves at an evaluation before the next). reattached's push at 9m
// carries it as observed but not its counts, as a restart's first
// push before liveswap has reattached its release: it stays firing.
// With every push stopped at 12m, all's fall fires nothing either. A
// restart at 10m15s: no push at 10m30s, the new process's from 11m,
// its counts from 12m once liveswap has reattached the release, from
// zero; idle, observed throughout, stays firing (0.1 a second read
// as 0.08 and 0.06 meanwhile: the reattach row).
func TestOperatorRulesReadThePushedApps(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	a := rulesApp(t, "rule low threshold request_rate < 1.5 for 1m")
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(a), 0o600); err != nil {
		t.Fatal(err)
	}
	near := thresholdExpr(Rule{Name: "near", Kind: ruleThreshold, Metric: "request_rate", Op: "<", Value: 1.9, For: 60e9})
	firing := ""
	for _, app := range []string{"quiet", "reattached"} {
		firing += `
          - exp_labels: {app: ` + app + `, kind: state, severity: warning, subject: "app:` + app + `", source: rules, key: "rules/app:` + app + `/low"}
            exp_annotations: {summary: "` + app + `: its requests a second over 5 minutes has been below 1.5 for 1m (rule low); now 0"}`
	}
	test := `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 30s
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x10 _ _ 1x40'
      - series: 'motel_app_observed{app="live",job="motel"}'
        values: '1x10 _ _ 1x40'
      - series: 'motel_app_requests_total{app="live",release="v1",class="2xx",job="motel"}'
        values: '0+60x10 _ _ 780+60x40'
      - series: 'motel_app_observed{app="quiet",job="motel"}'
        values: '1x10 _ _ 1x40'
      - series: 'motel_app_requests_total{app="quiet",release="v1",class="2xx",job="motel"}'
        values: '0x10 _ _ 0x40'
      - series: 'motel_app_observed{app="reattached",job="motel"}'
        values: '1x10 _ _ 1x40'
      - series: 'motel_app_requests_total{app="reattached",release="v1",class="2xx",job="motel"}'
        values: '0x10 _ _ 0x4 _ 0x34'
      - series: 'motel_app_observed{app="gone",job="motel"}'
        values: '1x25'
      - series: 'motel_app_requests_total{app="gone",release="v1",class="2xx",job="motel"}'
        values: '0+60x25'
    metricsql_expr_test:
      - expr: '` + near + `'
        eval_time: 13m10s
        exp_samples:
          - labels: '{app="quiet"}'
            value: 0
          - labels: '{app="reattached"}'
            value: 0
    alert_rule_test:
      - eval_time: 6m
        groupname: motel-operator
        alertname: low
        exp_alerts:` + firing + `
      - eval_time: 9m
        groupname: motel-operator
        alertname: low
        exp_alerts:` + firing + `
      - eval_time: 15m
        groupname: motel-operator
        alertname: low
        exp_alerts:` + firing + `
      - eval_time: 17m
        groupname: motel-operator
        alertname: low
        exp_alerts:` + firing + `
  - interval: 30s
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x24'
      - series: 'motel_app_observed{app="all",job="motel"}'
        values: '1x24'
      - series: 'motel_app_requests_total{app="all",release="v1",class="2xx",job="motel"}'
        values: '0+60x24'
    alert_rule_test:
      - eval_time: 15m
        groupname: motel-operator
        alertname: low
        exp_alerts: []
      - eval_time: 17m
        groupname: motel-operator
        alertname: low
        exp_alerts: []
  - interval: 30s
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x20 _ 650x20'
      - series: 'motel_app_observed{app="idle",job="motel"}'
        values: '1x20 _ 1x20'
      - series: 'motel_app_requests_total{app="idle",release="v1",class="4xx",job="motel"}'
        values: '0+3x20 _ _ _ 0+3x18'
    alert_rule_test:
      - eval_time: 10m
        groupname: motel-operator
        alertname: low
        exp_alerts:
          - exp_labels: {app: idle, kind: state, severity: warning, subject: "app:idle", source: rules, key: "rules/app:idle/low"}
            exp_annotations: {summary: "idle: its requests a second over 5 minutes has been below 1.5 for 1m (rule low); now 100m"}
      - eval_time: 11m
        groupname: motel-operator
        alertname: low
        exp_alerts:
          - exp_labels: {app: idle, kind: state, severity: warning, subject: "app:idle", source: rules, key: "rules/app:idle/low"}
            exp_annotations: {summary: "idle: its requests a second over 5 minutes has been below 1.5 for 1m (rule low); now 80m"}
      - eval_time: 12m
        groupname: motel-operator
        alertname: low
        exp_alerts:
          - exp_labels: {app: idle, kind: state, severity: warning, subject: "app:idle", source: rules, key: "rules/app:idle/low"}
            exp_annotations: {summary: "idle: its requests a second over 5 minutes has been below 1.5 for 1m (rule low); now 60m"}
      - eval_time: 13m
        groupname: motel-operator
        alertname: low
        exp_alerts:
          - exp_labels: {app: idle, kind: state, severity: warning, subject: "app:idle", source: rules, key: "rules/app:idle/low"}
            exp_annotations: {summary: "idle: its requests a second over 5 minutes has been below 1.5 for 1m (rule low); now 60m"}
`
	runUnittest(t, tool, dir, test)
}

// A histogram is read whole, by its count: under a release name rolled
// back to with a fresh counter, reused's buckets that came back below
// their last values and the one above it are all counted from zero (380
// of 400 under 0.1 s), not some through increase_pure and one through
// increase (0.1 s read as 0, a p95 of 0.97 s). slow is new to the store
// (all in increase_pure); kept came back after a gap at its last values,
// its history slow and its new answers fast, all through increase.
// Each app is motel_app_observed while its histogram is pushed, as in
// motel's push, for the rule reads the apps the push says are observed.
func TestOperatorRulesHistogramIsReadWhole(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	a := rulesApp(t, "rule slow threshold p95 > 500ms for 2m min_requests 5")
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(a), 0o600); err != nil {
		t.Fatal(err)
	}
	test := `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x90'
      - series: 'motel_app_response_seconds_bucket{app="reused",release="v1",le="0.1",job="motel"}'
        values: '0+1x10 _x30 380x50'
      - series: 'motel_app_response_seconds_bucket{app="reused",release="v1",le="1",job="motel"}'
        values: '0+50x10 _x30 395x50'
      - series: 'motel_app_response_seconds_bucket{app="reused",release="v1",le="+Inf",job="motel"}'
        values: '0+100x10 _x30 400x50'
      - series: 'motel_app_response_seconds_count{app="reused",release="v1",job="motel"}'
        values: '0+100x10 _x30 400x50'
      - series: 'motel_app_observed{app="reused",job="motel"}'
        values: '1x10 _x30 1x50'
      - series: 'motel_app_response_seconds_bucket{app="slow",release="v1",le="0.1",job="motel"}'
        values: '_x40 0x50'
      - series: 'motel_app_response_seconds_bucket{app="slow",release="v1",le="1",job="motel"}'
        values: '_x40 50x50'
      - series: 'motel_app_response_seconds_bucket{app="slow",release="v1",le="+Inf",job="motel"}'
        values: '_x40 50x50'
      - series: 'motel_app_response_seconds_count{app="slow",release="v1",job="motel"}'
        values: '_x40 50x50'
      - series: 'motel_app_observed{app="slow",job="motel"}'
        values: '_x40 1x50'
      - series: 'motel_app_response_seconds_bucket{app="kept",release="v1",le="0.1",job="motel"}'
        values: '0x10 _x30 0+10x50'
      - series: 'motel_app_response_seconds_bucket{app="kept",release="v1",le="1",job="motel"}'
        values: '0x10 _x30 0+10x50'
      - series: 'motel_app_response_seconds_bucket{app="kept",release="v1",le="+Inf",job="motel"}'
        values: '0+100x10 _x30 1000+10x50'
      - series: 'motel_app_response_seconds_count{app="kept",release="v1",job="motel"}'
        values: '0+100x10 _x30 1000+10x50'
      - series: 'motel_app_observed{app="kept",job="motel"}'
        values: '1x10 _x30 1x50'
    alert_rule_test:
      - eval_time: 44m
        groupname: motel-operator
        alertname: slow
        exp_alerts:
          - exp_labels: {app: slow, kind: state, severity: warning, subject: "app:slow", source: rules, key: "rules/app:slow/slow"}
            exp_annotations: {summary: "slow: its p95 time to headers over 5 minutes has been above 500ms for 2m (rule slow); now 955ms"}
`
	runUnittest(t, tool, dir, test)
}

// disk_fill's shape is the store's own rule's up to a day, and scaled
// to the horizon beyond it.
func TestDiskFillShape(t *testing.T) {
	for _, c := range []struct {
		within, long, short, hold time.Duration
	}{
		{time.Hour, 6 * time.Hour, time.Hour, 30 * time.Minute},
		{24 * time.Hour, 6 * time.Hour, time.Hour, 30 * time.Minute},
		{36 * time.Hour, 9 * time.Hour, 90 * time.Minute, 45 * time.Minute},
		{7 * 24 * time.Hour, 42 * time.Hour, 7 * time.Hour, 210 * time.Minute},
		{30 * 24 * time.Hour, 180 * time.Hour, 30 * time.Hour, 15 * time.Hour},
		{25*time.Hour + 7*time.Second, 6*time.Hour + 15*time.Minute, 62 * time.Minute, 31 * time.Minute},
	} {
		if l, s, h := diskFillShape(c.within); l != c.long || s != c.short || h != c.hold {
			t.Errorf("within %s: %s, %s, %s; want %s, %s, %s", c.within, l, s, h, c.long, c.short, c.hold)
		}
	}
}

// disk_fill's history is any sample older than its long window's last
// five sixths: a two-hour gap in the pushes (the store down) leaves no
// hole when it slides into the long window's first sixth, five hours
// on, and a rule still waits five hours from the series' first sample.
func TestDiskFillHistoryOverAGap(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	a := rulesApp(t, "rule disk_day disk_fill within 1d")
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(a), 0o600); err != nil {
		t.Fatal(err)
	}
	test := `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'motel_disk_avail_bytes{path="/var/lib/liveswap",job="motel"}'
        values: '1440000000000-1000000000x599 _x120 720000000000-1000000000x480'
    alert_rule_test:
      - eval_time: 5h20m
        groupname: motel-operator
        alertname: disk_day
        exp_alerts: []
      - eval_time: 16h30m
        groupname: motel-operator
        alertname: disk_day
        exp_alerts:
          - exp_labels: {path: /var/lib/liveswap, kind: state, severity: warning, subject: box, source: rules, key: "rules/box/disk_day//var/lib/liveswap"}
            exp_annotations: {summary: "the filesystem holding /var/lib/liveswap is predicted to fill within 1d (rule disk_day)"}
`
	runUnittest(t, tool, dir, test)
}

// A one-off step of a fifth of the free space (a deploy's extraction, a
// staged backup) is no trend at 7d, whose 6 h and 1 h windows fired on
// it within the hour; a disk that fills in three and a half days does
// fire.
func TestDiskFillStepIsNoTrend(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	a := rulesApp(t, "rule disk_week disk_fill within 7d")
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(a), 0o600); err != nil {
		t.Fatal(err)
	}
	test := `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'motel_disk_avail_bytes{path="/var/lib/liveswap",job="motel"}'
        values: '20000000000x3000 16000000000x600'
      - series: 'motel_disk_avail_bytes{path="/var/lib/caddy",job="motel"}'
        values: '100000000000-20000000x3600'
    alert_rule_test:
      - eval_time: 50h55m
        groupname: motel-operator
        alertname: disk_week
        exp_alerts:
          - exp_labels: {path: /var/lib/caddy, kind: state, severity: warning, subject: box, source: rules, key: "rules/box/disk_week//var/lib/caddy"}
            exp_annotations: {summary: "the filesystem holding /var/lib/caddy is predicted to fill within 7d (rule disk_week)"}
      - eval_time: 53h
        groupname: motel-operator
        alertname: disk_week
        exp_alerts:
          - exp_labels: {path: /var/lib/caddy, kind: state, severity: warning, subject: box, source: rules, key: "rules/box/disk_week//var/lib/caddy"}
            exp_annotations: {summary: "the filesystem holding /var/lib/caddy is predicted to fill within 7d (rule disk_week)"}
`
	runUnittest(t, tool, dir, test)
}
