package liveswap

import (
	"cmp"
	"context"
	"net/http"
	"slices"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// boxAlerts is what the box's Alertmanager holds, as GET /_motel/alerts
// sends it before the filter: every alert and every silence that has
// not ended, the alerts newest first, the silences that hold now
// (active) newest first and then those to come (pending) soonest
// first, each list cut to its cap. The source's own
// answer is the box's whole one; the route only orders and cuts it.
func (b boxHandle) boxAlerts(ctx context.Context) wire.BoxAlerts {
	out := wire.BoxAlerts{Monitor: wire.MonitorOff, Alerts: []wire.BoxAlert{}, Silences: []wire.Silence{}}
	if b.alerts == nil {
		return out
	}
	held, ok := b.alerts.Alerts(ctx)
	if !ok {
		out.Monitor = wire.MonitorUnanswered
		return out
	}
	out.Monitor = wire.MonitorOn
	for _, a := range held.Alerts {
		out.Alerts = append(out.Alerts, wire.BoxAlert{Alert: a.Alert, App: a.App, Instance: a.Instance})
	}
	for _, s := range held.Silences {
		if s.State == wire.SilenceActive || s.State == wire.SilencePending {
			out.Silences = append(out.Silences, s)
		}
	}
	slices.SortStableFunc(out.Alerts, func(x, y wire.BoxAlert) int {
		if c := y.StartsAt.Compare(x.StartsAt); c != 0 {
			return c
		}
		return cmp.Or(cmp.Compare(x.Name, y.Name), cmp.Compare(x.App, y.App), cmp.Compare(x.Instance, y.Instance), cmp.Compare(x.Summary, y.Summary))
	})
	// The active ones first, which hold alerts now, newest first; then
	// the pending ones, soonest first: a cut at the cap drops the
	// silences that start last.
	slices.SortStableFunc(out.Silences, func(x, y wire.Silence) int {
		if xa, ya := x.State == wire.SilenceActive, y.State == wire.SilenceActive; xa != ya {
			if xa {
				return -1
			}
			return 1
		}
		c := y.StartsAt.Compare(x.StartsAt)
		if x.State != wire.SilenceActive {
			c = -c
		}
		if c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})
	if len(out.Alerts) > wire.BoxAlertsMax {
		out.Alerts, out.MoreAlerts = out.Alerts[:wire.BoxAlertsMax], true
	}
	if len(out.Silences) > wire.SilencesMax {
		out.Silences, out.MoreSilences = out.Silences[:wire.SilencesMax], true
	}
	return out
}

// alerts is GET /_motel/alerts, for a viewer of the box: every alert
// the box's Alertmanager holds, with the app and the instance each
// names and whether a silence or the deploy mute holds it, and every
// silence that has not ended, with its matchers, who made it and why.
// A silence's comment and maker are the operator's own words, and an
// instance names paths and the box's other sites: a viewer of the box's
// alone.
//
// No filter runs over it: an alert motel raised passed its app's filter
// when it was told (lifecycle.go), and the rest is the operator's and
// Alertmanager's text, sent as Alertmanager holds it. An alert whose app
// label names an app this config runs is read through that app's
// handle, so Decide is asked about each app it names.
func (h *Handler) alerts(w http.ResponseWriter, r *http.Request, p *Person) error {
	box, err := h.allowBox(p.Principal, access.BoxAlerts)
	if err != nil {
		// serveMotel asked Decide about the row's action already: the two
		// falling apart.
		return RespondJSON(w, http.StatusForbidden, ForbiddenBody(err))
	}
	held := box.boxAlerts(r.Context())
	// The apps the rows name that this config runs, each read through
	// its handle once: Decide allowed the box, which covers every app.
	apps := map[string]appHandle{}
	var named []*managedApp
	for _, a := range held.Alerts {
		if _, seen := apps[a.App]; seen || a.App == "" || h.app.managed[a.App] == nil {
			continue
		}
		app, err := h.allowApp(p.Principal, access.AppAlerts, a.App)
		if err != nil {
			// Decide allowed the box, which covers every app, and refused
			// one: the table and the grants falling apart. Its rows are
			// motel's and Alertmanager's words alone.
			apps[a.App] = appHandle{}
			continue
		}
		apps[a.App] = app
		named = append(named, app.ma)
	}
	// Each app it names is read once a Start installed it, as for GET
	// /_motel/apps/<app>/alerts and the apps list.
	if late := h.awaitRead(r, p.Name, named...); late != nil {
		return h.installing(w)
	}
	body := wire.BoxAlerts{Monitor: held.Monitor, MoreAlerts: held.MoreAlerts, MoreSilences: held.MoreSilences,
		Alerts: make([]wire.BoxAlert, 0, len(held.Alerts)), Silences: held.Silences}
	if body.Silences == nil {
		body.Silences = []wire.Silence{}
	}
	for _, a := range held.Alerts {
		if app, named := apps[a.App]; named && app.ma == nil {
			// The row of an app the box did not allow: the words of
			// motel's and Alertmanager's that say what holds it, and its
			// times.
			kept := wire.BoxAlert{Alert: wire.Alert{Summary: "the box did not allow this app's alerts to be read", StartsAt: a.StartsAt,
				SilencedUntil: a.SilencedUntil, Inhibited: a.Inhibited}, App: a.App}
			if slices.Contains(alertStates, a.State) {
				kept.State = a.State
			}
			if slices.Contains(severities, a.Severity) {
				kept.Severity = a.Severity
			}
			a = kept
		}
		body.Alerts = append(body.Alerts, a)
	}
	return RespondJSON(w, http.StatusOK, body)
}

// severities are the words motel's alerts carry for severity, and
// alertStates Alertmanager's for an alert's state.
var (
	severities  = []string{"critical", "warning", "info"}
	alertStates = []string{"active", "suppressed", "unprocessed"}
)
