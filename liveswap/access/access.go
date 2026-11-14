// Package access decides who may take which action on the two
// webhooks: liveswap_webhook's app routes and box_webhook's `/`. Each
// handler finds the request's row in its route table, authenticates
// where it always has, and asks Decide whether the principal the token
// stands for may take the row's action. Anything the table below does
// not list is refused.
//
// The principals are the ones a deploy token can be today: a deploy
// source of one app, a global source, and the box channel's source.
// What each may do is what it could do before Decide existed, so
// Decide refuses nothing that authentication accepted
// (liveswap/DESIGN-liveswap.md, "Who may take which action").
package access

import (
	"fmt"
	"slices"
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
)

// Kind is which sort of principal a verified token stands for.
type Kind int

// The principal kinds. The zero Kind is none and is refused everything.
const (
	// AppDeployer is a deploy source of the app Principal.App: a token
	// one of that app's deploy_trust sources accepted.
	AppDeployer Kind = iota + 1
	// GlobalDeployer is a global liveswap source, which verifies the
	// token for a name that is no app. It is a deployer of no app here
	// and holds no action: the handler answers it 404.
	GlobalDeployer
	// BoxChannel is a source the box block's deploy_trust accepted:
	// it may present pushes.
	BoxChannel
)

// Principal is who a verified token is, as far as Decide needs to know.
type Principal struct {
	Kind Kind
	// App is the app an AppDeployer deploys: the name in the path its
	// token was verified for. Empty for every other kind.
	App string
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
}

// Decide says whether p may take action a on resource: the app's name
// for an app action, "" for a box action. nil is allowed; anything
// else is refused, an action or a kind the table does not know
// included. An AppDeployer is allowed on its own app only. The error
// says why, for a test; no principal of today's meets one on a route.
func Decide(p Principal, a Action, resource string) error {
	if !slices.Contains(allowed, cell{p.Kind, a}) {
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
