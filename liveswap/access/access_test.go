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
		{"global deployer", Principal{Kind: GlobalDeployer}},
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
		"global deployer": {
			AppStatus: false, AppDeployRecord: false, AppDeploy: false, AppRollback: false,
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
