package notify

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// viewCase is one history GET /<app> reads over the store, a push every
// 30 s: the push series (pushes, each its own series), the app's series
// and what the view's queries must read of them at eval.
type viewCase struct {
	app    string
	pushes []string
	series map[string]string // series selector -> values
	eval   string
	span   time.Duration
	want   map[string][]string // query (classes, p95, cache) -> exp_samples
}

// The view's request queries over the store's own MetricsQL, at the
// hour and the day: each case reads what the window holds, and the
// store's plain increase_pure and increase each misread some, the cases
// that made the view take the operator rules' guard.
//
//   - burst: a new release's first push carries 400 responses and
//     nothing follows, the pushes running for hours: 400 (increase
//     drops the first push: 0).
//   - gapped: 10 responses a push throughout, a 20-minute gap in the
//     pushes over the window's start: increase's 990, the first push
//     after the gap dropped (increase_pure: 4990, every count since
//     motel began).
//   - old: the metrics line first loaded on a box whose observer had
//     counted 7200, then 10 a push: 600, at the day 12000 (increase_pure:
//     7800, 19200).
//   - fresh: on a store ten minutes old, 22 responses, 21 cache misses
//     and a hit (e2e metrics 3a): 22, at the day too (increase: 0).
//   - twopush: fresh's with a second push series that began after the
//     responses: the push's first sample is the earliest.
//   - nopush: no push in the store at all: increase, which drops the
//     burst, and no error.
func TestMetricsViewCountsOverTheStore(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	req := func(app string) string {
		return `motel_app_requests_total{app="` + app + `",release="v1",class="2xx",job="motel"}`
	}
	ok2xx := func(v float64) []string { return []string{fmt.Sprintf(`{labels: '{class="2xx"}', value: %g}`, v)} }
	hist := func(app, values string, counts [3]int) map[string]string {
		m := map[string]string{}
		for i, le := range []string{"0.1", "1", "+Inf"} {
			m[`motel_app_response_seconds_bucket{app="`+app+`",release="v1",le="`+le+`",job="motel"}`] = fmt.Sprintf(values, counts[i])
		}
		m[`motel_app_response_seconds_count{app="`+app+`",release="v1",job="motel"}`] = fmt.Sprintf(values, counts[2])
		return m
	}
	fresh := func(app string, span time.Duration, eval string, pushes ...string) viewCase {
		c := viewCase{app: app, pushes: pushes, eval: eval, span: span, series: hist(app, "_x620 %dx2980", [3]int{20, 22, 22}),
			want: map[string][]string{
				"classes": ok2xx(22),
				"p95":     {`{labels: '{}', value: 0.505}`},
				"cache":   {`{labels: '{result="miss"}', value: 21}`, `{labels: '{result="hit"}', value: 1}`},
			}}
		c.series[req(app)] = "_x620 22x2980"
		c.series[`motel_app_cache_responses_total{app="`+app+`",release="v1",result="miss",job="motel"}`] = "_x620 21x2980"
		c.series[`motel_app_cache_responses_total{app="`+app+`",release="v1",result="hit",job="motel"}`] = "_x620 1x2980"
		return c
	}
	cases := []viewCase{
		{app: "burst", pushes: []string{"1x600"}, series: map[string]string{req("burst"): "_x400 400x200"},
			eval: "210m", span: time.Hour, want: map[string][]string{"classes": ok2xx(400)}},
		{app: "gapped", pushes: []string{"1x360 _x40 1x200"}, series: map[string]string{req("gapped"): "0+10x360 _x40 4000+10x200"},
			eval: "250m", span: time.Hour, want: map[string][]string{"classes": ok2xx(990)}},
		{app: "old", pushes: []string{"_x600 1x200"}, series: map[string]string{req("old"): "_x600 7200+10x200"},
			eval: "330m", span: time.Hour, want: map[string][]string{"classes": ok2xx(600)}},
		{app: "oldday", pushes: []string{"_x600 1x3000"}, series: map[string]string{req("oldday"): "_x600 7200+10x3000"},
			eval: "900m", span: 24 * time.Hour, want: map[string][]string{"classes": ok2xx(12000)}},
		fresh("fresh", time.Hour, "320m", "_x600 1x200"),
		fresh("freshday", 24*time.Hour, "900m", "_x600 1x3000"),
		fresh("twopush", time.Hour, "320m", "_x600 1x200", "_x630 1x170"),
		{app: "nopush", series: map[string]string{req("nopush"): "_x20 400x200"},
			eval: "60m", span: time.Hour, want: map[string][]string{"classes": ok2xx(0)}},
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, ok := viewUnittest(t, tool, dir, cases, requestQueries); !ok {
		t.Fatalf("vmalert-tool unittest:\n%s", out)
	}
	// Each alone misreads the cases above it names.
	plain := func(fn string) func(string, time.Duration) viewRequests {
		return func(sel string, span time.Duration) viewRequests {
			w := "[" + promDuration(span) + "]"
			b := fn + "(motel_app_response_seconds_bucket{" + sel + "}" + w + ")"
			return viewRequests{
				classes: "sum by (class) (" + fn + "(motel_app_requests_total{" + sel + "}" + w + "))",
				p95:     "histogram_quantile(0.95, sum by (le) (" + b + "))",
				cache:   "sum by (result) (" + fn + "(motel_app_cache_responses_total{" + sel + "}" + w + "))",
			}
		}
	}
	for fn, misread := range map[string][]string{
		"increase_pure": {"gapped", "old", "oldday"},
		"increase":      {"burst", "fresh", "freshday", "twopush"},
	} {
		for _, app := range misread {
			var one []viewCase
			for _, c := range cases {
				if c.app == app {
					one = append(one, c)
				}
			}
			if out, ok := viewUnittest(t, tool, dir, one, plain(fn)); ok {
				t.Errorf("plain %s reads %s as the guard does; the case proves nothing\n%s", fn, app, out)
			}
		}
	}
}

// viewUnittest runs cases through vmalert-tool, the queries built by
// qs, and says whether every one read as wanted.
func viewUnittest(t *testing.T, tool, dir string, cases []viewCase, qs func(string, time.Duration) viewRequests) (string, bool) {
	t.Helper()
	var b strings.Builder
	b.WriteString("rule_files: [" + RulesName + "]\nevaluation_interval: 30s\ntests:\n")
	for _, c := range cases {
		b.WriteString("  - interval: 30s\n    input_series:\n")
		for i, p := range c.pushes {
			sel := `process_start_time_seconds{job="motel"}`
			if i > 0 {
				sel = fmt.Sprintf(`process_start_time_seconds{job="motel",instance="%d"}`, i)
			}
			fmt.Fprintf(&b, "      - series: '%s'\n        values: '%s'\n", sel, p)
		}
		for s, v := range c.series {
			fmt.Fprintf(&b, "      - series: '%s'\n        values: '%s'\n", s, v)
		}
		b.WriteString("    metricsql_expr_test:\n")
		q := qs(`app="`+c.app+`"`, c.span)
		for _, name := range []string{"classes", "p95", "cache"} {
			want, ok := c.want[name]
			if !ok {
				continue
			}
			// The quantile to a millisecond: the store's interpolation
			// is a float's.
			expr := map[string]string{"classes": q.classes, "p95": "round(" + q.p95 + ", 0.001)", "cache": q.cache}[name]
			fmt.Fprintf(&b, "      - expr: '%s'\n        eval_time: %s\n        exp_samples:\n", strings.ReplaceAll(expr, "'", "''"), c.eval)
			for _, s := range want {
				fmt.Fprintf(&b, "          - %s\n", s)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "test.yml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(tool, "unittest", "-files", "test.yml")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err == nil && strings.Contains(string(out), "SUCCESS")
}
