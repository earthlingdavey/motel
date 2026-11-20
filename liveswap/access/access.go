// Package access decides who may take which action on the two
// webhooks: liveswap_webhook's app routes and its routes under
// /_motel/, and box_webhook's `/`. Each handler finds the request's
// row in its route table, authenticates where it always has, and asks
// Decide whether the principal the token stands for may take the row's
// action. Anything the table below does not list is refused.
//
// The principals are a deploy source of one app, the box channel's
// source, and a person whose token the box verified. A token a global
// source verified names no app, and is answered 404 before the route
// table on an app's path. What a deploy source may do is what it could
// do before Decide existed, so on an app's path and on the box's own
// Decide refuses nothing that its authentication accepted; under
// /_motel/ it refuses a global source's token the people's routes
// (liveswap/DESIGN-liveswap.md, "Who may take which action"). A person may read: an app's status, deploys, alerts and rules where
// a grant covers the app, the box's status, its backups', every
// alert and silence and every rule with a grant on the box, and who they are and which apps they may view
// whatever their grants (DESIGN-access.md).
package access

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Action is what a route does, named for the table and the journal.
// An app action is taken on the app the path names; a box action on
// the box.
type Action string

// The actions of the two webhooks, one per route that does something.
const (
	AppStatus       Action = "app.status"        // GET /<app>
	AppDeployRecord Action = "app.deploy_record" // GET /<app>?deploy=<v>
	AppDeploy       Action = "app.deploy"        // POST /<app>, a gzip or a JSON body
	AppRollback     Action = "app.rollback"      // POST /<app>?rollback=<v>
	BoxStatus       Action = "box.status"        // GET /
	BoxPushResult   Action = "box.push_result"   // GET /?result=<id>
	BoxPush         Action = "box.push"          // POST /
	BoxWhoami       Action = "box.whoami"        // GET /_motel/whoami
	BoxApps         Action = "box.apps"          // GET /_motel/apps
	BoxBackups      Action = "box.backups"       // GET /_motel/backups
	AppAlerts       Action = "app.alerts"        // GET /_motel/apps/<app>/alerts
	BoxAlerts       Action = "box.alerts"        // GET /_motel/alerts
	AppRules        Action = "app.rules"         // GET /_motel/apps/<app>/rules
	BoxRules        Action = "box.rules"         // GET /_motel/rules
)

// MotelActions are the actions liveswap_webhook serves under /_motel/,
// beside the apps' routes: box actions, and app actions on the app the
// path names after /_motel/apps/. box_webhook serves the other box
// actions.
var MotelActions = []Action{BoxWhoami, BoxApps, BoxBackups, AppAlerts, BoxAlerts, AppRules, BoxRules}

// Kind is which sort of principal a verified token stands for.
type Kind int

// The principal kinds. The zero Kind is none and is refused everything.
const (
	// AppDeployer is a deploy source of the app Principal.App: a token
	// one of that app's deploy_trust sources accepted.
	AppDeployer Kind = iota + 1
	// BoxChannel is a source the box block's deploy_trust accepted:
	// it may present pushes.
	BoxChannel
	// Person is a person whose token the box verified (CheckToken):
	// Principal.Name, who may view Principal.Views.
	Person
)

// Principal is who a verified token is, as far as Decide needs to know.
type Principal struct {
	Kind Kind
	// App is the app an AppDeployer deploys: the name in the path its
	// token was verified for. Empty for every other kind.
	App string
	// Name is a Person's name, and Views what their grants let them view
	// from where the request came from (Registry.Views). Empty and nil
	// for every other kind.
	Name  string
	Views *Views
}

// cell is one allowed pair of the table.
type cell struct {
	kind   Kind
	action Action
}

// allowed is every (kind, action) that is allowed. A pair not listed
// is refused: adding a principal or an action is adding rows here.
var allowed = []cell{
	{AppDeployer, AppStatus},
	{AppDeployer, AppDeployRecord},
	{AppDeployer, AppDeploy},
	{AppDeployer, AppRollback},
	{BoxChannel, BoxStatus},
	{BoxChannel, BoxPushResult},
	{BoxChannel, BoxPush},
	// A person's cells are the most a person may take; the grants that
	// apply from the request's address narrow them (decidePerson).
	{Person, AppStatus},
	{Person, AppDeployRecord},
	{Person, BoxStatus},
	{Person, BoxWhoami},
	{Person, BoxApps},
	{Person, BoxBackups},
	{Person, AppAlerts},
	{Person, BoxAlerts},
	{Person, AppRules},
	{Person, BoxRules},
}

// Forbidden is Decide's refusal of a principal the box verified, for
// an action it may name to them: the lowest principal that may take it,
// and on what kind of scope. It names no app and nothing of the
// request, so one answer serves an app that exists and one that does
// not. Its words are neutral: a declared key, not a kind of signer.
type Forbidden struct {
	// Needs is "viewer" (a grant), "deployer" (a deploy token of the
	// app, or the box's) or "key" (a token a declared key signed).
	Needs string
	// Scope is "app" or "box".
	Scope string
}

func (f *Forbidden) Error() string {
	switch f.Needs + " " + f.Scope {
	case "viewer app":
		return "this needs the viewer role on this app"
	case "viewer box":
		return "this needs the viewer role on the box"
	case "deployer app":
		return "this needs a deploy token of this app"
	case "deployer box":
		return "this needs a deploy token of the box"
	}
	return "this needs a token signed by a declared key"
}

// Decide says whether p may take action a on resource: the app's name
// for an app action, "" for a box action. nil is allowed; anything
// else is refused, an action or a kind the table does not know
// included. An AppDeployer is allowed on its own app only. A person
// refused, or a deploy token on a person's route, gets a *Forbidden,
// which the handlers answer as a 403 naming what is needed; any other
// error says why for a test, and no principal of today's meets one on
// a route.
func Decide(p Principal, a Action, resource string) error {
	if p.Kind == Person {
		return decidePerson(p, a, resource)
	}
	if !slices.Contains(allowed, cell{p.Kind, a}) {
		if slices.Contains(MotelActions, a) && (p.Kind == AppDeployer || p.Kind == BoxChannel) {
			// A deploy token is told what it would take: no deploy
			// source may ask who anyone is, or read a person's reads of
			// an app.
			scope := "box"
			if strings.HasPrefix(string(a), "app.") {
				scope = "app"
			}
			return &Forbidden{Needs: "key", Scope: scope}
		}
		return fmt.Errorf("principal kind %d may not take %s", p.Kind, a)
	}
	switch p.Kind {
	case AppDeployer:
		if resource == "" || resource != p.App {
			return fmt.Errorf("a deployer of %q may not take %s on %q", p.App, a, resource)
		}
	case BoxChannel:
		if resource != "" {
			return fmt.Errorf("%s is taken on the box, not on %q", a, resource)
		}
	}
	return nil
}

// decidePerson is Decide for a person: the table's cells, narrowed by
// the views. An app action on an app no view covers is the 403 naming
// viewer on an app, the same whether the app exists or not; an act, or
// a box read only the box's source takes, names the deployer.
func decidePerson(p Principal, a Action, resource string) error {
	app := strings.HasPrefix(string(a), "app.")
	switch {
	case p.Name == "":
		return errors.New("a person with no name")
	case app && resource == "":
		return fmt.Errorf("%s is taken on an app", a)
	case !app && resource != "":
		return fmt.Errorf("%s is taken on the box, not on %q", a, resource)
	case !slices.Contains(allowed, cell{Person, a}):
		if !slices.Contains(Actions(), a) {
			return fmt.Errorf("no principal may take %s", a)
		}
		scope := "box"
		if app {
			scope = "app"
		}
		return &Forbidden{Needs: "deployer", Scope: scope}
	}
	switch a {
	case AppStatus, AppDeployRecord, AppAlerts, AppRules:
		if !p.Views.Covers(resource) {
			return &Forbidden{Needs: "viewer", Scope: "app"}
		}
	case BoxStatus, BoxBackups, BoxAlerts, BoxRules:
		if p.Views == nil || !p.Views.Box {
			return &Forbidden{Needs: "viewer", Scope: "box"}
		}
	}
	return nil
}

// Actions is every action Decide's table allows to some principal, in
// the table's order: the set each webhook's route table is held to.
func Actions() []Action {
	var out []Action
	for _, c := range allowed {
		if !slices.Contains(out, c.action) {
			out = append(out, c.action)
		}
	}
	return out
}
