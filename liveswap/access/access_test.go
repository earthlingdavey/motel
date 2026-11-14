package access

import (
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
	}
	// The resource each action is taken on: the deployer's own app for
	// an app action, the box ("") for a box action.
	on := map[Action]string{
		AppStatus: "blog", AppDeployRecord: "blog", AppDeploy: "blog", AppRollback: "blog",
		BoxStatus: "", BoxPushResult: "", BoxPush: "",
	}
	want := map[string]map[Action]bool{
		"none": {
			AppStatus: false, AppDeployRecord: false, AppDeploy: false, AppRollback: false,
			BoxStatus: false, BoxPushResult: false, BoxPush: false,
		},
		"deployer of blog": {
			AppStatus: true, AppDeployRecord: true, AppDeploy: true, AppRollback: true,
			BoxStatus: false, BoxPushResult: false, BoxPush: false,
		},
		"box channel": {
			AppStatus: false, AppDeployRecord: false, AppDeploy: false, AppRollback: false,
			BoxStatus: true, BoxPushResult: true, BoxPush: true,
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
