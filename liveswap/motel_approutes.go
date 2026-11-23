package liveswap

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
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
// that app, the query keys it takes, and the function that answers,
// which is given the app's handle from allowApp and never its name,
// and the query as parsed against the keys.
//
// A row declares each key it takes and the closed set of its values,
// the first the default, or the grammar its value must match; the
// query is parsed strictly against them (parseQuery), after Decide. A
// query that does not parse, a key the row does not take, a key given
// twice and a value outside its set or its grammar are each the 422
// that names what the row takes, in the route's own words, and echoes
// nothing of the request's; a row with no key takes no query at all,
// an empty one among them.
type motelAppRoute struct {
	read   string
	action access.Action
	keys   []queryKey
	serve  func(h *Handler, w http.ResponseWriter, r *http.Request, app appHandle, q appQuery) error
}

// queryKey is one query key a row takes: the closed set of its values,
// the first the value when the query does not name the key; or, for a
// key no set can hold (a cursor), the grammar its value must match
// whole, and then no default: the key is in the query only when named.
// A key has a set or a grammar, never both (TestAppRouteKeys).
type queryKey struct {
	name    string
	values  []string
	grammar *keyGrammar
}

// keyGrammar is what a key with no closed set holds its value to: an
// anchored pattern the whole value must match, and the words the 422
// names it by, which quote nothing a request sent.
type keyGrammar struct {
	re   *regexp.Regexp
	says string
}

// appQuery is the value of each key a row takes, as the query named it
// or its default: only those keys, and only values of their sets or
// matching their grammars. A key with a grammar is in it only when the
// query named it (no grammar takes an empty value, so q[name] == "" is
// the key not named).
type appQuery map[string]string

var motelAppRoutes = []motelAppRoute{
	{"alerts", access.AppAlerts, nil, (*Handler).appAlerts},
	{"rules", access.AppRules, nil, (*Handler).appRules},
	{"series", access.AppSeries, []queryKey{{name: "range", values: wire.SeriesRanges}}, (*Handler).appSeries},
	{"logs", access.AppLogs, []queryKey{{name: "lines", values: wire.LogsLines}, {name: "after", grammar: journalCursor}}, (*Handler).appLogs},
}

// parseQuery is the request's query against the keys rt takes, or
// false, for the 422: as url.ParseQuery reads it (an empty query, and
// empty pieces between the &s, name nothing), each key one rt takes,
// named once, with a value of its set or one its grammar takes whole.
// A row with no key takes no query, so any query, an empty one too, is
// refused.
func (rt *motelAppRoute) parseQuery(r *http.Request) (appQuery, bool) {
	out := appQuery{}
	for _, k := range rt.keys {
		if k.grammar == nil {
			out[k.name] = k.values[0]
		}
	}
	if r.URL.RawQuery == "" && !r.URL.ForceQuery {
		return out, true
	}
	if len(rt.keys) == 0 {
		return nil, false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, false
	}
	for name, vs := range q {
		i := slices.IndexFunc(rt.keys, func(k queryKey) bool { return k.name == name })
		if i < 0 || len(vs) != 1 {
			return nil, false
		}
		if g := rt.keys[i].grammar; g != nil {
			// No word of the row's to pass on: the value as
			// url.ParseQuery decoded it, held whole to the grammar.
			if !g.re.MatchString(vs[0]) {
				return nil, false
			}
			out[name] = vs[0]
			continue
		}
		j := slices.Index(rt.keys[i].values, vs[0])
		if j < 0 {
			return nil, false
		}
		// The row's own value, not the request's bytes: what goes on
		// to the read and its body is the row's word.
		out[rt.keys[i].name] = rt.keys[i].values[j]
	}
	return out, true
}

// takes is the 422's sentence: the route by its pattern, and the keys
// it takes with their values or their grammars' words, the row's own
// words alone.
func (rt *motelAppRoute) takes() string {
	route := "GET /_motel/apps/{app}/" + rt.read
	if len(rt.keys) == 0 {
		return route + " takes no query"
	}
	parts := make([]string, len(rt.keys))
	for i, k := range rt.keys {
		if k.grammar != nil {
			parts[i] = k.name + " (" + k.grammar.says + ")"
			continue
		}
		parts[i] = k.name + " (one of " + strings.Join(k.values, ", ") + ")"
	}
	return route + " takes only " + strings.Join(parts, " and ") + ", each once"
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
		return RespondJSON(w, http.StatusNotFound, wire.Error{Error: fmt.Sprintf("unknown app %q", name)})
	case err != nil:
		if rt.action == access.AppLogs && p != nil {
			h.logsRefused(r, p.Name, name, err)
		}
		return RespondJSON(w, http.StatusForbidden, ForbiddenBody(err))
	}
	if p == nil {
		// Decide allowed a row to a caller who is no person: the table
		// and the routes falling apart.
		return RespondJSON(w, http.StatusForbidden, wire.Error{Error: "forbidden"})
	}
	q, ok := rt.parseQuery(r)
	if !ok {
		// The route's own words, naming the keys it takes, and nothing
		// of the request's or the app's.
		return RespondJSON(w, http.StatusUnprocessableEntity, wire.Error{Error: rt.takes()})
	}
	// After Decide: what the read is about is the app a Start
	// installed.
	if late := h.awaitRead(r, p.Name, app.ma); late != nil {
		return h.installing(w)
	}
	return rt.serve(h, w, r, app, q)
}

// appAlerts is GET /_motel/apps/<app>/alerts: the alerts Alertmanager
// holds whose app label is the handle's app, each with whether a
// silence or the deploy mute holds it. Chosen here, after the fetch, by
// the label alone: no matcher is built or sent. Nothing of the box's
// own alerts is in it, not even one that names the app in its
// instance (backup_stale/<app>), and no silence. Nothing of it is the
// app's text: an alert motel raised is its own words (a lifecycle
// event's summary passed the app's filter when it was told, lifecycle.go),
// and a store rule's is the operator's annotation.
func (h *Handler) appAlerts(w http.ResponseWriter, r *http.Request, app appHandle, _ appQuery) error {
	return RespondJSON(w, http.StatusOK, app.alerts(r.Context(), h.app.alertsSource))
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
