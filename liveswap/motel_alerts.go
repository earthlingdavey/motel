package liveswap

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// boxAlerts is what the box's Alertmanager holds, as GET /_motel/alerts
// sends it before the filter: every alert and every silence that has
// not ended, newest first (the active silences before the pending
// ones), each list cut to its cap. The source's own
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
	// The active ones first, which hold alerts now, so that a cut at the
	// cap drops silences that start later before any that hold.
	slices.SortStableFunc(out.Silences, func(x, y wire.Silence) int {
		if xa, ya := x.State == wire.SilenceActive, y.State == wire.SilenceActive; xa != ya {
			if xa {
				return -1
			}
			return 1
		}
		if c := y.StartsAt.Compare(x.StartsAt); c != 0 {
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

// boxAlertsBody is wire.BoxAlerts with each row as the bytes the filter
// left (TestBoxAlertsBodyIsTheWireType holds the two to one shape).
type boxAlertsBody struct {
	Monitor      string            `json:"monitor"`
	Alerts       []json.RawMessage `json:"alerts"`
	Silences     []json.RawMessage `json:"silences"`
	MoreAlerts   bool              `json:"more_alerts,omitempty"`
	MoreSilences bool              `json:"more_silences,omitempty"`
}

// alerts is GET /_motel/alerts, for a viewer of the box: every alert
// the box's Alertmanager holds, with the app and the instance each
// names and whether a silence or the deploy mute holds it, and every
// silence that has not ended, with its matchers, who made it and why.
// A silence's comment and maker are the operator's own words, and an
// instance names paths and the box's other sites: a viewer of the box's
// alone.
//
// The body is about several apps, so it is made of rows, as the apps
// list is (redact.go, rule 2): an alert whose app label names an app
// this config runs is read through that app's handle and filtered by
// its filter alone, the versions its status names safe, so a value of
// its env_file is masked as on GET /_motel/apps/<app>/alerts; every
// other alert, and every silence, passes a filter with no app's
// values, its shape and entropy layers, the app names, silence ids and
// release names it holds safe strings. No filter sees two apps' values,
// and the list adds only its punctuation and the box's own words
// (monitor, more_*) after every row's last pass.
func (h *Handler) alerts(w http.ResponseWriter, r *http.Request, p *Person) error {
	box, err := h.allowBox(p.Principal, access.BoxAlerts)
	if err != nil {
		// serveMotel asked Decide about the row's action already: the two
		// falling apart.
		return RespondJSON(w, http.StatusForbidden, ForbiddenBody(err), nil)
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
			// withheld, never filtered without its values.
			apps[a.App] = appHandle{}
			continue
		}
		apps[a.App] = app
		named = append(named, app.ma)
	}
	// An app's filter knows its values once a Start installed it, as
	// for GET /_motel/apps/<app>/alerts and the apps list.
	if late := h.awaitRead(r, p.Name, named...); late != nil {
		return h.installing(w, late)
	}
	filters := map[string]*Redactor{}
	for name, app := range apps {
		if app.ma == nil {
			filters[name] = &Redactor{withhold: "the box did not allow this app's alerts to be read"}
			continue
		}
		filters[name] = app.ma.redactorFor(app.ma.status())
	}
	body := boxAlertsBody{Monitor: held.Monitor, MoreAlerts: held.MoreAlerts, MoreSilences: held.MoreSilences,
		Alerts: make([]json.RawMessage, 0, len(held.Alerts)), Silences: make([]json.RawMessage, 0, len(held.Silences))}
	none := NewRedactor(nil, boxSafe(held))
	for _, a := range held.Alerts {
		f, own := filters[a.App]
		if !own {
			f = none
		}
		body.Alerts = append(body.Alerts, json.RawMessage(alertRow(a, f, own)))
	}
	for _, s := range held.Silences {
		body.Silences = append(body.Silences, json.RawMessage(silenceRow(s, none)))
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err // a row that is not JSON: redactJSON returns JSON whatever it is given
	}
	return respondFiltered(w, http.StatusOK, string(b))
}

// alertRow is one alert's row, as the bytes sent, through f, which is
// its app's filter when own. A row the filter withheld keeps the words
// of motel's and Alertmanager's that say what holds it, in its summary
// the filter's sentence, which passed that filter, and its app only
// when own: an app this config runs, the route's word, as an apps list
// row's name; another label's value passed no filter. Never the
// alert's name or instance, which a value may be part of.
//
// The row's times are Alertmanager's, never the app's bytes: they pass
// no filter and are put back after its last pass, so that an env_file
// value that is a piece of one (a date) cannot leave a time no client
// reads and with it the whole list unread. Every row then has its keys
// sorted, as a row the filter changed has.
func alertRow(a wire.BoxAlert, f *Redactor, own bool) string {
	raw, err := json.Marshal(a)
	var fields map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(raw, &fields)
	}
	if err != nil {
		fields = map[string]json.RawMessage{} // strings and times: nothing to fail
	}
	times := map[string]json.RawMessage{}
	for _, k := range []string{"starts_at", "silenced_until"} {
		if v, ok := fields[k]; ok {
			times[k] = v
			delete(fields, k)
		}
	}
	raw, _ = json.Marshal(fields) // raw values from a marshal: nothing to fail
	out := f.redactJSON(raw)
	why, ok := withheld(out, "name")
	if !ok {
		var row map[string]json.RawMessage
		if json.Unmarshal([]byte(out), &row) == nil {
			for k, v := range times {
				row[k] = v
			}
			if b, err := json.Marshal(row); err == nil {
				return string(b)
			}
		}
		why = "response withheld"
	}
	kept := wire.BoxAlert{Alert: wire.Alert{Summary: why, StartsAt: a.StartsAt,
		SilencedUntil: a.SilencedUntil, Inhibited: a.Inhibited}}
	if slices.Contains(alertStates, a.State) {
		kept.State = a.State
	}
	if own {
		kept.App = a.App
	}
	if slices.Contains(severities, a.Severity) {
		kept.Severity = a.Severity
	}
	b, _ := json.Marshal(kept) // strings and times: nothing to fail
	return string(b)
}

// silenceRow is one silence's row, as the bytes sent, through f. A row
// the filter withheld keeps its state and times, Alertmanager's words,
// and its id when it is one Alertmanager makes (a UUID), so that root
// can still expire it; its comment is the filter's sentence, and it
// loses its matchers and maker, which a value may be part of.
func silenceRow(s wire.Silence, f *Redactor) string {
	raw, err := json.Marshal(s)
	if err != nil {
		raw = []byte(`{}`) // strings and times: nothing to fail
	}
	out := f.redactJSON(raw)
	why, ok := withheld(out, "id")
	if !ok {
		return out
	}
	kept := wire.Silence{State: s.State, Matchers: []wire.Matcher{}, StartsAt: s.StartsAt, EndsAt: s.EndsAt, Comment: why}
	if silenceIDRe.MatchString(s.ID) {
		kept.ID = s.ID
	}
	b, _ := json.Marshal(kept) // strings and times: nothing to fail
	return string(b)
}

// silenceIDRe is a silence's id as Alertmanager makes one.
var silenceIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// withheld is the filter's sentence when out is the body a filter
// withheld in place of a row, which has key: true when the row has it
// not.
func withheld(out, key string) (string, bool) {
	var row map[string]json.RawMessage
	if json.Unmarshal([]byte(out), &row) == nil {
		if _, ok := row[key]; ok {
			return "", false
		}
	}
	var why string
	_ = json.Unmarshal(row["error"], &why)
	if why == "" {
		why = "response withheld"
	}
	return why, true
}

// severities are the words motel's alerts carry for severity, and
// alertStates Alertmanager's for an alert's state.
var (
	severities  = []string{"critical", "warning", "info"}
	alertStates = []string{"active", "suppressed", "unprocessed"}
)

// boxSafe are the names the rows that pass no app's filter hold, which
// the entropy layer would mask: the apps named (as the backups body's),
// each silence's id and the release names its matchers hold (a git sha
// is high-entropy by design). With no known value, a safe string
// exempts only the heuristics (redact.go, rule 1).
func boxSafe(b wire.BoxAlerts) []string {
	var safe []string
	for _, a := range b.Alerts {
		if appNameRe.MatchString(a.App) {
			safe = append(safe, a.App)
		}
	}
	for _, s := range b.Silences {
		if silenceIDRe.MatchString(s.ID) {
			safe = append(safe, s.ID)
		}
		for _, m := range s.Matchers {
			switch {
			case m.Name == "app" && appNameRe.MatchString(m.Value):
				safe = append(safe, m.Value)
			case m.Name == "release" && m.Op == wire.MatchEqual && validVersion(m.Value):
				safe = append(safe, m.Value)
			}
		}
	}
	return safe
}
