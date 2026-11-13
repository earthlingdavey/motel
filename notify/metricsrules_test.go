package notify

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.yaml.in/yaml/v3"
)

// notifyApp is the full block, resolved and rendered as Provision does.
func notifyApp(t *testing.T) *App {
	t.Helper()
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = r
	_, _, a.receivers = render(r, testStateDir)
	return a
}

func parseRules(t *testing.T, b []byte) vmRuleFile {
	t.Helper()
	var f vmRuleFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatalf("the rendered rules are not YAML: %v\n%s", err, b)
	}
	return f
}

// Without notify (a metrics line alone) no rule alerts: Alertmanager would still hold
// the last config notify rendered, and send the store's alerts to its
// channels. The groups stay, empty, which vmalert accepts (-dryRun
// refuses a file with no group at all).
func TestRenderRulesWithoutNotify(t *testing.T) {
	f := parseRules(t, renderRules(nil))
	if len(f.Groups) != 3 || f.Groups[0].Name != "motel-store" || f.Groups[1].Name != "motel-channel-failed" || f.Groups[2].Name != operatorGroup {
		t.Fatalf("groups = %+v", f.Groups)
	}
	for _, g := range f.Groups {
		if len(g.Rules) != 0 {
			t.Errorf("no notify, yet group %s has rules: %+v", g.Name, g.Rules)
		}
	}
	if !strings.HasPrefix(string(renderRules(nil)), rulesHeader) {
		t.Error("no header")
	}
}

// A reload that drops the channels and keeps the metrics line: the
// rules file the new config writes holds no alerting rule.
func TestRulesAfterNotifyIsRemoved(t *testing.T) {
	dir := t.TempDir()
	both := &MetricsStore{stateDirOverride: dir, rules: renderRules(notifyApp(t)), logger: zap.NewNop()}
	both.applyRules()
	alone := &MetricsStore{stateDirOverride: dir, rules: renderRules(nil), logger: zap.NewNop()}
	alone.applyRules()
	b, err := os.ReadFile(filepath.Join(dir, RulesName))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range parseRules(t, b).Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				t.Errorf("after the channels went, vmalert still has %s", r.Alert)
			}
		}
	}
}

func TestFixedRulesAreMotelsStateAlerts(t *testing.T) {
	f := parseRules(t, renderRules(notifyApp(t)))
	if len(f.Groups[0].Rules) != 2 {
		t.Fatalf("fixed rules = %+v", f.Groups[0].Rules)
	}
	for _, r := range f.Groups[0].Rules {
		if r.Labels["kind"] != kindState || r.Labels["source"] != sourceVMAlert || r.For == "" || r.Labels["severity"] == "" ||
			r.Labels["key"] != "vmalert/"+r.Labels["subject"]+"/"+r.Alert {
			t.Errorf("rule %s: labels %v, for %q: not routed as one of motel's state alerts", r.Alert, r.Labels, r.For)
		}
		// One large write tilts the six-hour line alone; the hour's must agree.
		if r.Alert == ruleStoreDiskFill {
			for _, part := range []string{"[6h]", " and ", "[1h]"} {
				if !strings.Contains(r.Expr, part) {
					t.Errorf("%s reads one window (no %q): %s", r.Alert, part, r.Expr)
				}
			}
		}
	}
}

// The second source deduplicates only if, for every failure motel's
// channel_failed can see (a receiver of the render and an integration
// among its channels), the rules that fire carry exactly the label sets
// motel's own posts carry: the same alerts, to the same channels.
func TestChannelFailedRulesCarryMotelsLabels(t *testing.T) {
	a := notifyApp(t)
	rules := channelFailedRules(a).Rules
	if len(rules) == 0 {
		t.Fatal("no ChannelFailed rules")
	}
	recvRe := regexp.MustCompile(`receiver_name=~"([^"]*)"`)
	key := func(l map[string]string) string {
		ks := slices.Sorted(maps.Keys(l))
		var b strings.Builder
		for _, k := range ks {
			fmt.Fprintf(&b, "%s=%s,", k, l[k])
		}
		return b.String()
	}
	checked := 0
	for recv, chans := range a.receivers {
		ints := map[string]bool{}
		for _, ch := range chans {
			ints[channelKinds[a.cfg.channels[ch].Kind]] = true
		}
		for in := range ints {
			te := newTestEngine(t)
			te.setConfig(a, te.p, te.known, te.logger)
			body := metricsHead + failedLine(in, recv, "clientError", 0)
			read := func(context.Context) ([]byte, error) { return []byte(body), nil }
			te.scrapeFailed(context.Background(), read)
			body = metricsHead + failedLine(in, recv, "clientError", 1)
			te.scrapeFailed(context.Background(), read)
			var want []string
			for _, al := range te.drain() {
				want = append(want, key(al.Labels))
			}
			var got []string
			for _, r := range rules {
				m := recvRe.FindStringSubmatch(r.Expr)
				if m == nil {
					t.Fatalf("rule without a receiver matcher: %s", r.Expr)
				}
				if r.Labels["integration"] != in || !regexp.MustCompile("^(?:"+m[1]+")$").MatchString(recv) {
					continue
				}
				l := maps.Clone(r.Labels)
				l["alertname"] = r.Alert
				got = append(got, key(l))
			}
			sort.Strings(want)
			sort.Strings(got)
			if !slices.Equal(got, want) {
				t.Errorf("%s failing on %s:\n rules %v\n motel %v", in, recv, got, want)
			}
			checked++
		}
	}
	if checked < 5 {
		t.Fatalf("only %d receiver/integration pairs checked", checked)
	}
}

func TestChannelFailedRulesShareReceivers(t *testing.T) {
	rules := channelFailedRules(notifyApp(t)).Rules
	seen := map[string]bool{}
	for _, r := range rules {
		k := r.Labels[labelChannel] + "/" + r.Labels["integration"] + "/" + r.Labels["suspects"]
		if seen[k] {
			t.Errorf("two rules for %s: receivers sharing all three are one", k)
		}
		seen[k] = true
		if r.Labels[labelChannel] == "pager" {
			// pager takes criticals alone, and ChannelFailed is a warning.
			t.Errorf("a rule tells pager, below its floor: %+v", r.Labels)
		}
		if strings.Contains(r.Expr, `"heartbeat"`) || strings.Contains(r.Expr, "|heartbeat") || strings.Contains(r.Expr, "heartbeat|") {
			t.Errorf("the heartbeat's receiver is motel's alone: %s", r.Expr)
		}
	}
}

// vmalert is Debian's victoria-metrics'; the tests that need it skip
// without it. `make test` runs in golang:trixie, which lacks it; the
// e2e image has it, and so does a dev container after
// `apt-get install victoria-metrics`.
func vmTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed (Debian: victoria-metrics)", name)
	}
	return p
}

// The e2e metrics box runs, by name, every test of notify's that needs
// vmalert, vmalert-tool or the store's binary, and every one that needs
// amtool (`make test` skips them): a test added here and not to
// e2e/metrics/run.sh's lists would run nowhere.
func TestE2EMetricsListsEveryVMAlertTest(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	need := map[string][]string{} // the helper a test calls → the tests
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && (id.Name == "vmTool" || id.Name == "amtool") && !slices.Contains(need[id.Name], fn.Name.Name) {
						need[id.Name] = append(need[id.Name], fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	run, err := os.ReadFile(filepath.Join("..", "e2e", "metrics", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for helper, list := range map[string]string{"vmTool": "vmtests", "amtool": "amtests"} {
		m := regexp.MustCompile(`(?m)^` + list + `='\^\(([^)]*)\)\$'$`).FindSubmatch(run)
		if m == nil {
			t.Fatalf("e2e/metrics/run.sh has no %s='^(...)$' line", list)
		}
		listed := strings.Split(string(m[1]), "|")
		slices.Sort(need[helper])
		slices.Sort(listed)
		if len(need[helper]) == 0 || !slices.Equal(need[helper], listed) {
			t.Errorf("e2e/metrics/run.sh's %s = %v; the tests that call %s are %v", list, listed, helper, need[helper])
		}
	}
}

func TestRenderedRulesPassVMAlertDryRun(t *testing.T) {
	tool := vmTool(t, "vmalert")
	for name, n := range map[string]*App{"without notify": nil, "with the full block": notifyApp(t)} {
		dir := t.TempDir()
		path := filepath.Join(dir, RulesName)
		if err := os.WriteFile(path, renderRules(n), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(tool, "-dryRun", "-rule="+path).CombinedOutput()
		if err != nil {
			t.Errorf("%s: vmalert -dryRun: %v\n%s", name, err, out)
		}
	}
}

// vmalert-tool runs the rendered rules over input series: the store's
// ChannelFailed fires with motel's labels, and app_memory_high carries
// the app's subject and key, which route it to the app's channels.
func TestRenderedRulesFireAsMotelsAlerts(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(notifyApp(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	test := `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'alertmanager_notifications_failed_total{integration="discord",receiver_name="event-warning-alice",reason="clientError",job="alertmanager"}'
        values: '0 0 1 1 1'
      # bob (json) fails, pager (ntfy) beside it delivers: one integration,
      # webhook, and a receiver each, so the rule names bob alone.
      - series: 'alertmanager_notifications_failed_total{integration="webhook",receiver_name="event-critical-bob",reason="clientError",job="alertmanager"}'
        values: '0 0 1 1 1'
      - series: 'alertmanager_notifications_failed_total{integration="webhook",receiver_name="event-critical-pager",reason="clientError",job="alertmanager"}'
        values: '0 0 0 0 0'
      - series: 'motel_app_memory_bytes{app="blog",job="motel"}'
        values: '95x20'
      - series: 'motel_app_memory_max_bytes{app="blog",job="motel"}'
        values: '100x20'
    alert_rule_test:
      - eval_time: 4m
        groupname: motel-channel-failed
        alertname: ChannelFailed
        exp_alerts:
          - exp_labels: {notify_channel: ops, severity: warning, subject: box, integration: discord, suspects: alice}
            exp_annotations: {summary: "Alertmanager could not deliver through discord; failing channel: alice (seen by the metrics store)"}
          - exp_labels: {notify_channel: ops, severity: warning, subject: box, integration: webhook, suspects: bob}
            exp_annotations: {summary: "Alertmanager could not deliver through webhook; failing channel: bob (seen by the metrics store)"}
      - eval_time: 15m
        groupname: motel-store
        alertname: app_memory_high
        exp_alerts:
          - exp_labels: {app: blog, kind: state, severity: warning, subject: "app:blog", source: vmalert, key: "vmalert/app:blog/app_memory_high"}
            exp_annotations: {summary: "blog has used over 90 % of its memory cap for ten minutes"}
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
