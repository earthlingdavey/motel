package liveswap

import (
	"encoding/json"
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
// token meets Decide's 403.
type motelRoute struct {
	path   string
	action access.Action
	serve  func(h *Handler, w http.ResponseWriter, r *http.Request, p *Person) error
}

var motelRoutes = []motelRoute{
	{motelPrefix + "/whoami", access.BoxWhoami, (*Handler).whoami},
	{motelPrefix + "/apps", access.BoxApps, (*Handler).apps},
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
	rt := findMotelRoute(r)
	if rt == nil {
		return RespondJSON(w, http.StatusNotFound, wire.Error{Error: "not found"}, nil)
	}
	if err := decide(who, rt.action, ""); err != nil {
		return RespondJSON(w, http.StatusForbidden, ForbiddenBody(err), nil)
	}
	if p == nil {
		// Decide allowed a row to a caller who is no person: the table
		// and the routes falling apart.
		return RespondJSON(w, http.StatusForbidden, wire.Error{Error: "forbidden"}, nil)
	}
	return rt.serve(h, w, r, p)
}

// whoami is GET /_motel/whoami: who the token's key belongs to, what
// they may view from here, and the box's own facts. Every value in it
// is a word the box checked against its grammar when it read the
// lines, so the filter's entropy layer is told they are safe.
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
	safe := []string{body.Name, body.Key, body.Box, body.Version}
	if v := p.Principal.Views; v != nil {
		if v.Box {
			body.Roles = append(body.Roles, wire.RoleHeld{Role: string(access.Viewer), Scope: "box"})
		}
		for _, app := range v.Apps {
			body.Roles = append(body.Roles, wire.RoleHeld{Role: string(access.Viewer), Scope: "app:" + app})
			safe = append(safe, app, "app:"+app)
		}
	}
	return RespondJSON(w, http.StatusOK, body, NewRedactor(nil, safe))
}

// apps is GET /_motel/apps: the apps the person may view, of those this
// config runs, each with its summary; every one for a viewer of the
// box. A name a grant holds that this config does not run is left out.
//
// Each row is a body of its own to the filter (redact.go, rule 2): it
// is read through an app handle, as GET /<app> is, and filtered by that
// app's filter alone, so no filter ever sees two apps' values and no
// row can carry another app's. After every row's last pass the list
// adds only the key around them: the rows are carried as raw JSON, so
// the encoder writes the text each filter left, as withField does.
func (h *Handler) apps(w http.ResponseWriter, _ *http.Request, p *Person) error {
	names := []string{}
	for name := range h.app.managed {
		if p.Principal.Views.Covers(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	body := struct {
		Apps []json.RawMessage `json:"apps"`
	}{make([]json.RawMessage, 0, len(names))}
	for _, name := range names {
		body.Apps = append(body.Apps, json.RawMessage(h.appRow(p, name)))
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err // a row that is not JSON: redactJSON returns JSON whatever it is given
	}
	return respondFiltered(w, http.StatusOK, string(b))
}

// appRow is one app's entry in GET /_motel/apps, as the bytes sent. The
// summary is the one GET /<app> would give: Decide is asked about the
// same action, and the values come from the same status, through the
// same filter.
func (h *Handler) appRow(p *Person, name string) string {
	app, err := h.allowApp(p.Principal, access.AppStatus, name)
	if err != nil {
		// Views covered the name and Decide refused its status: the table
		// and the list falling apart. The name is the list's, which says no
		// more than that.
		return nameOnlyRow(name, "forbidden")
	}
	ma := app.ma
	s := ma.status()
	raw, err := json.Marshal(wire.AppEntry{Name: name, Summary: summaryOf(s)})
	if err != nil {
		// A record's raw times that are not JSON, which GET /<app> could
		// not send either.
		return nameOnlyRow(name, "the app's status could not be written")
	}
	row := ma.redactorFor(s).redactJSON(raw)
	var got struct {
		Name    string          `json:"name"`
		Error   string          `json:"error"`
		Summary json.RawMessage `json:"summary"`
	}
	if json.Unmarshal([]byte(row), &got) == nil && got.Name == name && len(got.Summary) > 0 && string(got.Summary) != "null" {
		return row
	}
	// The filter withheld the row: what it sent is its sentence, past the
	// app's last pass. The name goes back beside it, as the list names it
	// for any app the person may view.
	if got.Error == "" {
		got.Error = "response withheld"
	}
	return nameOnlyRow(name, got.Error)
}

// nameOnlyRow is an entry with no summary, and why.
func nameOnlyRow(name, why string) string {
	b, _ := json.Marshal(wire.AppEntry{Name: name, Error: why}) // two strings: nothing to fail
	return string(b)
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
	return sum
}
