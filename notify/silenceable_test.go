package notify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
)

// stringConsts are the package-level string constants of the parsed
// files, by name.
func stringConsts(t *testing.T, files []*ast.File) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, sp := range g.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							out[n.Name], _ = strconv.Unquote(lit.Value)
						}
					}
				}
			}
		}
	}
	return out
}

func parseDir(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// emittedAlertNames are the alert names this package's emitters spell
// out, read from its source: an Event literal's Name, the first argument
// of the helpers that make an event or a rule's labels (ev, storeEvent,
// state, stateLabels, ruleLabels), a vmRule's Alert, an "alertname"
// label, and the liveswap events lifecycleSink posts as one-shots under
// their own names. A name held in a variable (an operator rule's) is not
// read here; TestSilenceableAlertsFollowTheConfig has those.
func emittedAlertNames(t *testing.T) []string {
	fset := token.NewFileSet()
	own := parseDir(t, fset, ".")
	consts := stringConsts(t, own)
	ls := stringConsts(t, parseDir(t, fset, filepath.Join("..", "liveswap")))
	resolve := func(e ast.Expr) (string, bool) {
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				v, err := strconv.Unquote(x.Value)
				return v, err == nil
			}
		case *ast.Ident:
			v, ok := consts[x.Name]
			return v, ok
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "liveswap" {
				v, ok := ls[x.Sel.Name]
				return v, ok
			}
		}
		return "", false
	}
	typeName := func(e ast.Expr) string {
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		return ""
	}
	var names []string
	add := func(e ast.Expr) {
		if v, ok := resolve(e); ok {
			names = append(names, v)
		}
	}
	for _, f := range own {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				for _, el := range x.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					switch k := kv.Key.(type) {
					case *ast.Ident:
						if (k.Name == "Name" && typeName(x.Type) == "Event") || (k.Name == "Alert" && typeName(x.Type) == "vmRule") {
							add(kv.Value)
						}
					case *ast.BasicLit:
						if k.Value == `"alertname"` {
							add(kv.Value)
						}
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && len(x.Args) > 0 &&
					slices.Contains([]string{"ev", "storeEvent", "state", "stateLabels", "ruleLabels"}, id.Name) {
					add(x.Args[0])
				}
			case *ast.CaseClause:
				// lifecycleSink's one-shots: a case whose body calls oneShot.
				calls := false
				for _, st := range x.Body {
					if es, ok := st.(*ast.ExprStmt); ok {
						if c, ok := es.X.(*ast.CallExpr); ok && typeName(c.Fun) == "oneShot" {
							calls = true
						}
					}
				}
				if calls {
					for _, e := range x.List {
						add(e)
					}
				}
			}
			return true
		})
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// TestSilenceableAlertsAreWhatMotelRaises: every alert name the
// package's emitters spell out is silenceable on a box with the metrics
// line, or one of the names no silence takes; and every silenceable
// name is one an emitter spells out. A new detector's name that is not
// added to raisedAlerts or storeAlerts fails here.
func TestSilenceableAlertsAreWhatMotelRaises(t *testing.T) {
	emitted := emittedAlertNames(t)
	a := &App{Metrics: &MetricsStore{Rules: []Rule{{Name: "x", Kind: rulePostDeploy}}}}
	set := silenceableAlerts(a)
	for _, n := range emitted {
		if !slices.Contains(set, n) && !slices.Contains(notSilenceable, n) {
			t.Errorf("motel raises %s, which no silence can take: add it to raisedAlerts or storeAlerts, or to notSilenceable with the reason", n)
		}
	}
	for _, n := range set {
		if !slices.Contains(emitted, n) {
			t.Errorf("%s is silenceable but no emitter in the package raises it", n)
		}
	}
	for _, n := range notSilenceable {
		if !slices.Contains(emitted, n) {
			t.Errorf("%s is refused but no emitter raises it", n)
		}
		if slices.Contains(set, n) {
			t.Errorf("%s is silenceable", n)
		}
	}
	if len(emitted) < 20 {
		t.Fatalf("the source scan found only %v: the emitters' shape changed", emitted)
	}
}

// TestSilenceableAlertsFollowTheConfig: without the metrics line, the
// store's names are not taken; an operator rule's name is; Watchdog
// never is.
func TestSilenceableAlertsFollowTheConfig(t *testing.T) {
	plain := silenceableAlerts(&App{})
	if slices.Contains(plain, eventStoreDown) || slices.Contains(plain, ruleAppMemoryHigh) || !slices.Contains(plain, alertDiskLow) {
		t.Fatalf("without metrics: %v", plain)
	}
	a := &App{Metrics: &MetricsStore{Rules: []Rule{
		{Name: "shop_slow", Kind: ruleThreshold, Metric: "p95", Op: ">", Value: 500, For: caddy.Duration(5 * 60e9)},
		{Name: "disk_soon", Kind: ruleDiskFill, Within: caddy.Duration(3600e9)},
	}}}
	with := silenceableAlerts(a)
	for _, n := range []string{"shop_slow", "disk_soon", eventStoreDown, ruleAppMemoryHigh} {
		if !slices.Contains(with, n) {
			t.Errorf("%s is not silenceable: %v", n, with)
		}
	}
	if slices.Contains(with, eventPostDeployRegressed) {
		t.Errorf("post_deploy_regressed with no post_deploy rule: %v", with)
	}
	for _, set := range [][]string{plain, with} {
		if slices.Contains(set, alertWatchdog) {
			t.Fatalf("Watchdog is silenceable: %v", set)
		}
	}
}

// TestAlertScope: what --app and --release can narrow, from the labels
// motel's emitters and rendered rules carry.
func TestAlertScope(t *testing.T) {
	a := &App{Metrics: &MetricsStore{Rules: []Rule{
		{Name: "slow", Kind: ruleThreshold, Metric: "p95", Op: ">", Value: 500, For: caddy.Duration(5 * 60e9)},
		{Name: "shop_slow", Kind: ruleThreshold, App: "shop", Metric: "p95", Op: ">", Value: 500, For: caddy.Duration(5 * 60e9)},
		{Name: "disk_soon", Kind: ruleDiskFill, Within: caddy.Duration(3600e9)},
	}}}
	type scope struct {
		app     bool
		pinned  string
		release bool
	}
	for name, want := range map[string]scope{
		"rollback": {true, "", true}, "deploy_succeeded": {true, "", true}, eventPostDeployRegressed: {true, "", true},
		"deploy_failed": {true, "", false}, "deploy_refused": {true, "", false},
		"watchdog_throttled": {true, "", false}, ruleAppMemoryHigh: {true, "", false}, "slow": {true, "", false},
		"shop_slow": {true, "shop", false},
		"disk_low":  {}, "disk_soon": {}, ruleStoreDiskFill: {},
		alertChannelFailed: {}, "cert_failed": {}, eventStoreDown: {},
	} {
		app, pinned, rel := alertScope(a, name)
		if got := (scope{app, pinned, rel}); got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	all := silenceableAlerts(&App{Metrics: &MetricsStore{Rules: []Rule{{Name: "x", Kind: rulePostDeploy}}}})
	for _, n := range onDiskReleases {
		if !slices.Contains(all, n) {
			t.Errorf("%s carries a release but is not silenceable", n)
		}
	}
}
