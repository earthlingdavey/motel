package access

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestDecideTable writes Decide's table out cell by cell: every kind
// of principal, the zero one included, against every action, on the
// resource each action is taken on. A cell the test does not list
// fails it, so an action or a kind added to the table is added here.
func TestDecideTable(t *testing.T) {
	kinds := []struct {
		name string
		p    Principal
	}{
		{"none", Principal{}},
		{"deployer of blog", Principal{Kind: AppDeployer, App: "blog"}},
		{"box channel", Principal{Kind: BoxChannel}},
		{"viewer of blog", Principal{Kind: Person, Name: "bob", Views: &Views{Apps: []string{"blog"}}}},
		{"viewer of the box", Principal{Kind: Person, Name: "carol", Views: &Views{Box: true}}},
		{"person with no grant", Principal{Kind: Person, Name: "dave", Views: &Views{}}},
	}
	// The resource each action is taken on: the deployer's own app for
	// an app action, the box ("") for a box action.
	on := map[Action]string{
		AppStatus: "blog", AppDeployRecord: "blog", AppDeploy: "blog", AppRollback: "blog",
		BoxStatus: "", BoxPushResult: "", BoxPush: "", BoxWhoami: "", BoxApps: "", BoxBackups: "",
		AppAlerts: "blog", BoxAlerts: "", AppSeries: "blog",
	}
	want := map[string]map[Action]bool{
		"none": {
			AppStatus: false, AppDeployRecord: false, AppDeploy: false, AppRollback: false,
			BoxStatus: false, BoxPushResult: false, BoxPush: false, BoxWhoami: false, BoxApps: false, BoxBackups: false, AppAlerts: false, BoxAlerts: false, AppSeries: false,
		},
		"deployer of blog": {
			AppStatus: true, AppDeployRecord: true, AppDeploy: true, AppRollback: true,
			BoxStatus: false, BoxPushResult: false, BoxPush: false, BoxWhoami: false, BoxApps: false, BoxBackups: false, AppAlerts: false, BoxAlerts: false, AppSeries: false,
		},
		"box channel": {
			AppStatus: false, AppDeployRecord: false, AppDeploy: false, AppRollback: false,
			BoxStatus: true, BoxPushResult: true, BoxPush: true, BoxWhoami: false, BoxApps: false, BoxBackups: false, AppAlerts: false, BoxAlerts: false, AppSeries: false,
		},
		"viewer of blog": {
			AppStatus: true, AppDeployRecord: true, AppDeploy: false, AppRollback: false,
			BoxStatus: false, BoxPushResult: false, BoxPush: false, BoxWhoami: true, BoxApps: true, BoxBackups: false, AppAlerts: true, BoxAlerts: false, AppSeries: true,
		},
		"viewer of the box": {
			AppStatus: true, AppDeployRecord: true, AppDeploy: false, AppRollback: false,
			BoxStatus: true, BoxPushResult: false, BoxPush: false, BoxWhoami: true, BoxApps: true, BoxBackups: true, AppAlerts: true, BoxAlerts: true, AppSeries: true,
		},
		"person with no grant": {
			AppStatus: false, AppDeployRecord: false, AppDeploy: false, AppRollback: false,
			BoxStatus: false, BoxPushResult: false, BoxPush: false, BoxWhoami: true, BoxApps: true, BoxBackups: false, AppAlerts: false, BoxAlerts: false, AppSeries: false,
		},
	}
	for _, k := range kinds {
		for _, a := range Actions() {
			allowed, listed := want[k.name][a]
			if !listed {
				t.Errorf("%s, %s: no cell in the test", k.name, a)
				continue
			}
			if err := Decide(k.p, a, on[a]); (err == nil) != allowed {
				t.Errorf("Decide(%s, %s, %q) = %v, want allowed %v", k.name, a, on[a], err, allowed)
			}
		}
	}
	for name, cells := range want {
		for a := range cells {
			if !slices.Contains(Actions(), a) {
				t.Errorf("%s, %s: the test has a cell for an action the table does not know", name, a)
			}
		}
	}
}

// TestAllowedCells pins the table itself, both ways: every allowed
// (kind, action) pair is written out here, and the package's table
// holds exactly these. TestDecideTable asks each action on its own
// resource, where the scope check refuses an app kind on a box action
// (and the reverse) before the table is read; this sees such a cell.
func TestAllowedCells(t *testing.T) {
	want := []cell{
		{AppDeployer, AppStatus},
		{AppDeployer, AppDeployRecord},
		{AppDeployer, AppDeploy},
		{AppDeployer, AppRollback},
		{BoxChannel, BoxStatus},
		{BoxChannel, BoxPushResult},
		{BoxChannel, BoxPush},
		{Person, AppStatus},
		{Person, AppDeployRecord},
		{Person, BoxStatus},
		{Person, BoxWhoami},
		{Person, BoxApps},
		{Person, BoxBackups},
		{Person, AppAlerts},
		{Person, BoxAlerts},
		{Person, AppSeries},
	}
	for _, c := range allowed {
		if !slices.Contains(want, c) {
			t.Errorf("the table allows kind %d to take %s; the test does not", c.kind, c.action)
		}
	}
	for _, c := range want {
		if !slices.Contains(allowed, c) {
			t.Errorf("the table does not allow kind %d to take %s; the test does", c.kind, c.action)
		}
	}
	if len(allowed) != len(want) {
		t.Errorf("the table has %d cells, want %d: one is listed twice", len(allowed), len(want))
	}
}

// TestDecideScope: a deployer is allowed on its own app only, and the
// box channel on the box only, whatever the action; an action the
// table does not know is refused to every kind.
func TestDecideScope(t *testing.T) {
	blog := Principal{Kind: AppDeployer, App: "blog"}
	for _, c := range []struct {
		name     string
		p        Principal
		a        Action
		resource string
	}{
		{"a deployer on another app", blog, AppDeploy, "shop"},
		{"a deployer on the box", blog, AppStatus, ""},
		{"a deployer of no app on the box", Principal{Kind: AppDeployer}, AppStatus, ""},
		{"the box channel on an app", Principal{Kind: BoxChannel}, BoxStatus, "blog"},
		{"an unknown action", blog, "app.logs", "blog"},
		{"no action", Principal{Kind: BoxChannel}, "", ""},
		{"an unknown kind", Principal{Kind: BoxChannel + 1}, BoxStatus, ""},
	} {
		if err := Decide(c.p, c.a, c.resource); err == nil {
			t.Errorf("%s: allowed, want refused", c.name)
		}
	}
}

// TestActionsAreAppOrBox: every action is an app's or the box's, so
// the two webhooks' route tables between them can hold every action
// (liveswap's TestEveryActionHasARoute holds app.*, box's box.*).
func TestActionsAreAppOrBox(t *testing.T) {
	if len(Actions()) == 0 {
		t.Fatal("no actions")
	}
	for _, a := range Actions() {
		if !strings.HasPrefix(string(a), "app.") && !strings.HasPrefix(string(a), "box.") {
			t.Errorf("%s is neither an app action nor a box action", a)
		}
	}
}

// TestDecidePersonRefusals: what a person is told when Decide refuses,
// cell by cell: the role and scope that would take it, the same for
// every app a person's grants do not cover; a deploy source on a route
// of /_motel/ is told it needs a declared key; and nothing a person
// sends is taken on the wrong kind of resource.
func TestDecidePersonRefusals(t *testing.T) {
	bob := Principal{Kind: Person, Name: "bob", Views: &Views{Apps: []string{"blog"}}}
	for _, c := range []struct {
		name     string
		p        Principal
		a        Action
		resource string
		want     *Forbidden // nil: refused with no body to name
	}{
		{"another app", bob, AppStatus, "shop", &Forbidden{"viewer", "app"}},
		{"an app that does not exist", bob, AppDeployRecord, "ghost", &Forbidden{"viewer", "app"}},
		{"the box's status", bob, BoxStatus, "", &Forbidden{"viewer", "box"}},
		{"a push's result", bob, BoxPushResult, "", &Forbidden{"deployer", "box"}},
		{"a push", bob, BoxPush, "", &Forbidden{"deployer", "box"}},
		{"a deploy of a viewed app", bob, AppDeploy, "blog", &Forbidden{"deployer", "app"}},
		{"a rollback of a viewed app", bob, AppRollback, "blog", &Forbidden{"deployer", "app"}},
		{"no views", Principal{Kind: Person, Name: "dave"}, AppStatus, "blog", &Forbidden{"viewer", "app"}},
		{"a deployer asks who it is", Principal{Kind: AppDeployer}, BoxWhoami, "", &Forbidden{"key", "box"}},
		{"the box channel lists apps", Principal{Kind: BoxChannel}, BoxApps, "", &Forbidden{"key", "box"}},
		{"another app's alerts", bob, AppAlerts, "shop", &Forbidden{"viewer", "app"}},
		{"alerts of an app that does not exist", bob, AppAlerts, "ghost", &Forbidden{"viewer", "app"}},
		{"alerts with no views", Principal{Kind: Person, Name: "dave"}, AppAlerts, "blog", &Forbidden{"viewer", "app"}},
		{"a global deployer reads an app's alerts", Principal{Kind: AppDeployer}, AppAlerts, "blog", &Forbidden{"key", "app"}},
		{"a deployer reads its own app's alerts", Principal{Kind: AppDeployer, App: "blog"}, AppAlerts, "blog", &Forbidden{"key", "app"}},
		{"the box channel reads an app's alerts", Principal{Kind: BoxChannel}, AppAlerts, "blog", &Forbidden{"key", "app"}},
		{"an app's alerts on the box", bob, AppAlerts, "", nil},
		{"another app's series", bob, AppSeries, "shop", &Forbidden{"viewer", "app"}},
		{"the series of an app that does not exist", bob, AppSeries, "ghost", &Forbidden{"viewer", "app"}},
		{"series with no views", Principal{Kind: Person, Name: "dave"}, AppSeries, "blog", &Forbidden{"viewer", "app"}},
		{"a global deployer reads an app's series", Principal{Kind: AppDeployer}, AppSeries, "blog", &Forbidden{"key", "app"}},
		{"a deployer reads its own app's series", Principal{Kind: AppDeployer, App: "blog"}, AppSeries, "blog", &Forbidden{"key", "app"}},
		{"the box channel reads an app's series", Principal{Kind: BoxChannel}, AppSeries, "blog", &Forbidden{"key", "app"}},
		{"an app's series on the box", bob, AppSeries, "", nil},
		{"the box's alerts", bob, BoxAlerts, "", &Forbidden{"viewer", "box"}},
		{"the box's alerts with no views", Principal{Kind: Person, Name: "dave"}, BoxAlerts, "", &Forbidden{"viewer", "box"}},
		{"a deployer reads the box's alerts", Principal{Kind: AppDeployer}, BoxAlerts, "", &Forbidden{"key", "box"}},
		{"the box channel reads the box's alerts", Principal{Kind: BoxChannel}, BoxAlerts, "", &Forbidden{"key", "box"}},
		{"an app action on the box", bob, AppStatus, "", nil},
		{"a box action on an app", bob, BoxWhoami, "blog", nil},
		{"no name", Principal{Kind: Person, Views: &Views{Box: true}}, BoxStatus, "", nil},
		{"an unknown action", bob, "app.logs", "blog", nil},
		{"a kind past the table", Principal{Kind: Person + 1}, BoxWhoami, "", nil},
	} {
		err := Decide(c.p, c.a, c.resource)
		var f *Forbidden
		switch {
		case err == nil:
			t.Errorf("%s: allowed", c.name)
		case c.want == nil && errors.As(err, &f):
			t.Errorf("%s: %v names what it needs", c.name, err)
		case c.want != nil && (!errors.As(err, &f) || *f != *c.want):
			t.Errorf("%s: %v, want %+v", c.name, err, *c.want)
		}
	}
	for _, f := range []Forbidden{{"viewer", "app"}, {"viewer", "box"}, {"deployer", "app"}, {"deployer", "box"}, {"key", "box"}, {"key", "app"}} {
		if msg := f.Error(); strings.Contains(msg, "person") || strings.Contains(msg, "motel") {
			t.Errorf("%+v says %q", f, msg)
		}
	}
}
