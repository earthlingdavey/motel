package liveswap

import (
	"net/http"
	"slices"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// motelPrefix is the first segment of every route that is not an app's
// (DESIGN-access.md, "Routes that are not an app"): `_` is outside the
// app grammar, so no app can be named by it. liveswap_webhook checks it
// before it takes the app from the path's last segment.
const motelPrefix = "/_motel"

// motelRoute is one row of the routes under /_motel/: an exact path,
// taken by a GET with no query, the box action Decide is asked about,
// and the function that answers. Every row is a person's; a deploy
// token a global source accepts meets Decide's 403 (one that only an
// app's own sources accept, or the box channel's, is the flat 401
// before it: under /_motel/ a deploy token is judged against the
// global sources alone).
type motelRoute struct {
	path   string
	action access.Action
	serve  func(h *Handler, w http.ResponseWriter, r *http.Request, p *Person) error
}

var motelRoutes = []motelRoute{
	{motelPrefix + "/whoami", access.BoxWhoami, (*Handler).whoami},
	{motelPrefix + "/apps", access.BoxApps, (*Handler).apps},
	{motelPrefix + "/backups", access.BoxBackups, (*Handler).backups},
	{motelPrefix + "/alerts", access.BoxAlerts, (*Handler).alerts},
	{motelPrefix + "/rules", access.BoxRules, (*Handler).rules},
}

// findMotelRoute is the request's row, or nil: any other method, path
// or query under /_motel/ is the 404.
func findMotelRoute(r *http.Request) *motelRoute {
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return nil
	}
	for i := range motelRoutes {
		if rt := &motelRoutes[i]; rt.path == r.URL.Path {
			return rt
		}
	}
	return nil
}

// serveMotel answers a request under /_motel/ whose token verified: a
// person's (p), or a global deploy source's (p nil). A path with no row
// is a 404 for every caller, once authenticated.
func (h *Handler) serveMotel(w http.ResponseWriter, r *http.Request, who access.Principal, p *Person) error {
	if rt, name := findMotelAppRoute(r); rt != nil {
		return h.serveMotelApp(w, r, who, p, rt, name)
	}
	rt := findMotelRoute(r)
	if rt == nil {
		return RespondJSON(w, http.StatusNotFound, wire.Error{Error: "not found"})
	}
	if err := decide(who, rt.action, ""); err != nil {
		return RespondJSON(w, http.StatusForbidden, ForbiddenBody(err))
	}
	if p == nil {
		// Decide allowed a row to a caller who is no person: the table
		// and the routes falling apart.
		return RespondJSON(w, http.StatusForbidden, wire.Error{Error: "forbidden"})
	}
	return rt.serve(h, w, r, p)
}

// whoami is GET /_motel/whoami: who the token's key belongs to, what
// they may view from here, and the box's own facts.
func (h *Handler) whoami(w http.ResponseWriter, _ *http.Request, p *Person) error {
	body := wire.WhoAmI{
		Name:           p.Name,
		Key:            p.Token.Fingerprint,
		Roles:          []wire.RoleHeld{},
		Box:            h.people.Identity(),
		Version:        caddy.CustomVersion,
		Protocol:       Protocol,
		TokenExpiresAt: p.Token.ExpiresAt,
	}
	if v := p.Principal.Views; v != nil {
		// The box's first, then each app by name; on one scope viewer
		// before logs. Each is a grant that applies from here, by role.
		if v.Viewer.Box {
			body.Roles = append(body.Roles, wire.RoleHeld{Role: string(access.Viewer), Scope: "box"})
		}
		if v.Logs.Box {
			body.Roles = append(body.Roles, wire.RoleHeld{Role: string(access.Logs), Scope: "box"})
		}
		apps := slices.Concat(v.Viewer.Apps, v.Logs.Apps)
		slices.Sort(apps)
		for _, app := range slices.Compact(apps) {
			if slices.Contains(v.Viewer.Apps, app) {
				body.Roles = append(body.Roles, wire.RoleHeld{Role: string(access.Viewer), Scope: "app:" + app})
			}
			if slices.Contains(v.Logs.Apps, app) {
				body.Roles = append(body.Roles, wire.RoleHeld{Role: string(access.Logs), Scope: "app:" + app})
			}
		}
	}
	return RespondJSON(w, http.StatusOK, body)
}

// apps is GET /_motel/apps: the apps the person may view, of those this
// config runs, each with its summary; every one for a viewer of the
// box. A name a grant holds that this config does not run is left out.
//
// Each row is read through an app handle, as GET /<app> is.
func (h *Handler) apps(w http.ResponseWriter, r *http.Request, p *Person) error {
	names := []string{}
	for name := range h.app.managed {
		if p.Principal.Views.Covers(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	// Each row is the app's status at this moment, so the list waits
	// for the apps it names as GET /<app> does (awaitRead), under one
	// bound: in the first moments of a start a row would say an app
	// that serves is idle and has no deploy.
	listed := make([]*managedApp, len(names))
	for i, name := range names {
		listed[i] = h.app.managed[name]
	}
	if late := h.awaitRead(r, p.Name, listed...); late != nil {
		return h.installing(w)
	}
	body := wire.Apps{Apps: make([]wire.AppEntry, 0, len(names))}
	for _, name := range names {
		body.Apps = append(body.Apps, h.appRow(p, name))
	}
	return RespondJSON(w, http.StatusOK, body)
}

// appRow is one app's entry in GET /_motel/apps. The summary is the one
// GET /<app> would give: Decide is asked about the same action, and the
// values come from the same state (summaryStatus reads of the disk only
// the newest record, the one the summary carries).
func (h *Handler) appRow(p *Person, name string) wire.AppEntry {
	app, err := h.allowApp(p.Principal, access.AppStatus, name)
	if err != nil {
		// Views covered the name and Decide refused its status: the table
		// and the list falling apart. The name is the list's, which says no
		// more than that.
		return wire.AppEntry{Name: name, Error: "forbidden"}
	}
	return wire.AppEntry{Name: name, Summary: summaryOf(app.ma.summaryStatus())}
}

// summaryOf is the part of s the list carries: taken from the status
// GET /<app> answers with, never computed apart, so the two cannot
// disagree.
func summaryOf(s statusSnapshot) *wire.AppSummary {
	sum := &wire.AppSummary{Phase: s.Phase, Running: s.Running, CurrentVersion: s.CurrentVersion}
	if s.Watchdog != nil {
		sum.Watchdog = s.Watchdog.State
	}
	if len(s.Deploys) > 0 {
		newest := s.Deploys[0]
		sum.NewestDeploy = &newest
	}
	if s.Deploying != nil {
		sum.Deploying = s.Deploying.Version
	}
	return sum
}
