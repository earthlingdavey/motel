package liveswap

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// motelAppsPrefix begins every app-scoped route under /_motel/:
// /_motel/apps/<app>/<read>, a read about one app for a person.
const motelAppsPrefix = motelPrefix + "/apps/"

// motelAppRoute is one row of the app-scoped routes under /_motel/
// (DESIGN-access.md, "Routes that are not an app"): a GET of
// /_motel/apps/<app>/<read>, the app action Decide is asked about on
// that app, and the function that answers, which is given the app's
// handle from allowApp and never its name.
//
// No row takes a query yet, so any query is the 422 that says so and
// echoes nothing. A row that comes to take one declares its keys and
// the closed set or the grammar of each beside its read, and the
// query is parsed strictly against them: an unknown key is still the
// 422, naming the keys the row takes.
type motelAppRoute struct {
	read   string
	action access.Action
	serve  func(h *Handler, w http.ResponseWriter, r *http.Request, app appHandle) error
}

var motelAppRoutes = []motelAppRoute{
	{"alerts", access.AppAlerts, (*Handler).appAlerts},
}

// findMotelAppRoute is the request's app-scoped row and the app its
// path names, or nil. The path is taken as sent, not cleaned, and must
// be exactly /_motel/apps/<app>/<read>: the app segment is held to the
// app grammar before anything else is looked at, and anything else
// (another segment, a trailing slash, an empty or dotted one) is no
// row, the 404 of every path under /_motel/ that has none. The query
// is the row's to judge (serveMotelApp), after Decide.
func findMotelAppRoute(r *http.Request) (*motelAppRoute, string) {
	if r.Method != http.MethodGet {
		return nil, ""
	}
	rest, ok := strings.CutPrefix(r.URL.Path, motelAppsPrefix)
	if !ok {
		return nil, ""
	}
	name, read, ok := strings.Cut(rest, "/")
	if !ok || !appNameRe.MatchString(name) {
		return nil, ""
	}
	for i := range motelAppRoutes {
		if rt := &motelAppRoutes[i]; rt.read == read {
			return rt, name
		}
	}
	return nil, ""
}

// serveMotelApp answers an app-scoped row under /_motel/ for a caller
// whose token verified: a person (p), or a global deploy source (p
// nil), which no row allows. Decide comes before the app is looked
// for, as on GET /<app>: a person whose grants do not cover the name
// is told the same 403 whether it is an app here or not, and only a
// viewer of the box, who may list the apps, is told a name is none.
func (h *Handler) serveMotelApp(w http.ResponseWriter, r *http.Request, who access.Principal, p *Person, rt *motelAppRoute, name string) error {
	app, err := h.allowApp(who, rt.action, name)
	switch {
	case errors.Is(err, errUnknownApp):
		return RespondJSON(w, http.StatusNotFound, wire.Error{Error: fmt.Sprintf("unknown app %q", name)}, nil)
	case err != nil:
		return RespondJSON(w, http.StatusForbidden, ForbiddenBody(err), nil)
	}
	if p == nil {
		// Decide allowed a row to a caller who is no person: the table
		// and the routes falling apart.
		return RespondJSON(w, http.StatusForbidden, wire.Error{Error: "forbidden"}, nil)
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		// The route's own words, naming the keys it takes (none), and
		// nothing of the request's or the app's (rule 6).
		return RespondJSON(w, http.StatusUnprocessableEntity, wire.Error{
			Error: "GET /_motel/apps/{app}/" + rt.read + " takes no query"}, NewRedactor(nil, nil))
	}
	// After Decide: what the read is about is the app a Start
	// installed, whose filter knows the app's values.
	if late := h.awaitRead(r, p.Name, app.ma); late != nil {
		return h.installing(w, late)
	}
	return rt.serve(h, w, r, app)
}

// appAlerts is GET /_motel/apps/<app>/alerts: the alerts Alertmanager
// holds whose app label is the handle's app, each with whether a
// silence or the deploy mute holds it. Chosen here, after the fetch, by
// the label alone: no matcher is built or sent. Nothing of the box's
// own alerts is in it, not even one that names the app in its
// instance (backup_stale/<app>), and no silence. The body is about one
// app, so it passes that app's filter, as GET /<app> does, with the
// versions its status names as safe strings: a summary names the
// release a deploy failed at.
func (h *Handler) appAlerts(w http.ResponseWriter, r *http.Request, app appHandle) error {
	body := app.alerts(r.Context(), h.app.alertsSource)
	f := app.ma.redactorFor(app.ma.status())
	if out, ok := filterAlerts(body, f); ok {
		return respondFiltered(w, http.StatusOK, out)
	}
	// The filter withheld the body, or changed its structure, or the
	// bytes with the times put back hold a value of the app's: the body
	// whole through the filter, as it always was, times and all.
	return RespondJSON(w, http.StatusOK, body, f)
}

// filterAlerts is the alerts body through the app's filter, its times
// passing none: they are taken out of each alert and put back after the
// filter's last pass, so that an env_file value that is a piece of one
// (a date) cannot leave a time no client reads, and with it the answer
// unread. It reports false, for the caller to send the body whole
// through the filter as before, when the filter withheld the body (its
// env_file unreadable) or changed its structure (a value spanning two
// alerts). The filter runs over the words without the times between
// them, so a value made of JSON's own punctuation and a time's last
// characters can stand in the bytes sent (an accepted row in
// DESIGN-access.md).
func filterAlerts(body wire.Alerts, f *Redactor) (string, bool) {
	// Each alert's words in the wire type's order, its times left out.
	type words struct {
		Name      string `json:"name"`
		Severity  string `json:"severity"`
		Summary   string `json:"summary"`
		State     string `json:"state"`
		Inhibited bool   `json:"inhibited,omitempty"`
	}
	rows := make([]words, len(body.Alerts))
	for i, a := range body.Alerts {
		rows[i] = words{a.Name, a.Severity, a.Summary, a.State, a.Inhibited}
	}
	raw, _ := json.Marshal(struct {
		Monitor string  `json:"monitor"`
		Alerts  []words `json:"alerts"`
	}{body.Monitor, rows}) // strings and bools: nothing to fail
	var got map[string]json.RawMessage
	var alerts []wire.Alert
	if json.Unmarshal([]byte(f.redactJSON(raw)), &got) != nil || json.Unmarshal(got["alerts"], &alerts) != nil || len(alerts) != len(body.Alerts) {
		return "", false
	}
	// Each alert as its wire type writes it, its times put back: what the
	// filter left of its words, Alertmanager's times.
	for i := range alerts {
		alerts[i].StartsAt, alerts[i].SilencedUntil = body.Alerts[i].StartsAt, body.Alerts[i].SilencedUntil
	}
	var b []byte
	var err error
	if len(got) == 2 {
		// The filter changed nothing at the top: the monitor first, as the
		// wire type writes it.
		b, err = json.Marshal(struct {
			Monitor json.RawMessage `json:"monitor"`
			Alerts  []wire.Alert    `json:"alerts"`
		}{got["monitor"], alerts})
	} else {
		// A changed body's keys sorted, its redacted_env with them, as
		// redactJSON writes one.
		got["alerts"], err = json.Marshal(alerts)
		if err == nil {
			b, err = json.Marshal(got)
		}
	}
	if err != nil {
		return "", false
	}
	return string(b), true
}

// alerts is what src holds of the handle's app, newest first.
func (a appHandle) alerts(ctx context.Context, src AlertsSource) wire.Alerts {
	out := wire.Alerts{Monitor: wire.MonitorOff, Alerts: []wire.Alert{}}
	if src == nil {
		return out
	}
	held, ok := src.Alerts(ctx)
	if !ok {
		out.Monitor = wire.MonitorUnanswered
		return out
	}
	out.Monitor = wire.MonitorOn
	for _, al := range held.Alerts {
		if al.App == a.ma.name {
			out.Alerts = append(out.Alerts, al.Alert)
		}
	}
	slices.SortStableFunc(out.Alerts, func(x, y wire.Alert) int {
		if c := y.StartsAt.Compare(x.StartsAt); c != 0 {
			return c
		}
		if c := cmp.Compare(x.Name, y.Name); c != 0 {
			return c
		}
		return cmp.Compare(x.Summary, y.Summary)
	})
	return out
}
