package liveswap

import (
	"context"
	"net/http"
	"slices"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// RulesSource is the rules the box alerts on, for the people's reads
// of them (GET /_motel/rules, GET /_motel/apps/<app>/rules). notify,
// which liveswap cannot import (it imports liveswap), installs it on a
// config whose monitor block runs notify (SetRulesSource), as it
// installs AlertsSource. A source gives the box's whole listing: which
// of it a read may show (the rules that apply to one app, with that
// app's numbers; every rule, for a viewer of the box) is the route's to
// choose, through the handle Decide's answer made, never the source's.
type RulesSource interface {
	// Rules is the config's listing as `moteld monitor rules --json`
	// gives it: every alert motel can raise, each check with the number
	// its detector last read, each store rule with its numbers from the
	// process's one evaluation (Store.Values says whether the store
	// answered it). An error is a listing the box could not make: the
	// journal's to say, never a person's. It is called with the
	// request's context; the caller only reads what it returns.
	Rules(ctx context.Context) (wire.RulesListing, error)
}

// SetRulesSource installs notify's source on this config. Called from
// the monitor app's Provision, after liveswap's own, so a config whose
// monitor block runs no notify, or that has none, answers
// "store":"off" with no rule: nothing is raised there.
func (a *App) SetRulesSource(s RulesSource) { a.rulesSource = s }

// noRulesListing is what a person reads when the box could not make its
// listing: fixed words, the cause in the journal.
const noRulesListing = "the box could not list its rules: moteld's journal says why"

// peopleRules is the listing as a person reads it: the store's word in
// place of how to reach it, and every alert without its queries. The
// store's socket and the MetricsQL are the box's alone: a laptop cannot
// run them, and they name the store's internals. A guard keeps its op,
// its value and its meaning.
func peopleRules(l wire.RulesListing) wire.Rules {
	out := wire.Rules{Store: wire.StoreOff, RulesFile: l.RulesFile, Alerts: make([]wire.AlertListing, 0, len(l.Alerts))}
	if l.Store != nil {
		// Any word but on is the store unanswered: a reading the box did
		// not say it has is not had.
		out.Store = wire.StoreUnanswered
		if l.Store.Values == wire.StoreOn {
			out.Store = wire.StoreOn
		}
	}
	for _, a := range l.Alerts {
		a.ValueQuery = ""
		if a.Guards != nil {
			g := make([]wire.GuardListing, len(a.Guards))
			for i, x := range a.Guards {
				x.Query, x.Match = "", ""
				g[i] = x
			}
			a.Guards = g
		}
		out.Alerts = append(out.Alerts, a)
	}
	return out
}

// listRules is src's listing as a person reads it, or false when the
// box could not make it, which the journal says.
func listRules(ctx context.Context, src RulesSource, logger *zap.Logger) (wire.Rules, bool) {
	if src == nil {
		return wire.Rules{Store: wire.StoreOff, Alerts: []wire.AlertListing{}}, true
	}
	l, err := src.Rules(ctx)
	if err != nil {
		logger.Warn("a person's read of the rules was answered 500: the box could not list them", zap.Error(err))
		return wire.Rules{}, false
	}
	return peopleRules(l), true
}

// boxRules is every rule, for a viewer of the box.
func (b boxHandle) boxRules(ctx context.Context, logger *zap.Logger) (wire.Rules, bool) {
	return listRules(ctx, b.rules, logger)
}

// rules is the rules that apply to the handle's app, and of their
// numbers that app's alone: each alert whose subject is an app, every
// app or this one (a rule pinned to another app, or to apps this is not
// among, is left out), with the
// readings of this app. Not a box check (a disk, the backups, a
// certificate, the store's own), and not another app's number. The
// rules file's word is kept: a state is vmalert's, and when the file it
// reads is not this config's, the app's viewer is told so too.
func (a appHandle) rules(ctx context.Context, src RulesSource, logger *zap.Logger) (wire.Rules, bool) {
	all, ok := listRules(ctx, src, logger)
	if !ok {
		return all, false
	}
	out := wire.Rules{Store: all.Store, RulesFile: all.RulesFile, Alerts: []wire.AlertListing{}}
	for _, al := range all.Alerts {
		if al.Subject != "app" || al.App != "" && al.App != a.ma.name || len(al.Apps) > 0 && !slices.Contains(al.Apps, a.ma.name) {
			continue
		}
		if len(al.Apps) > 0 {
			// Pinned to this app among others: said as this app's, and the
			// others not named.
			al.App, al.Apps = a.ma.name, nil
		}
		var mine []wire.Reading
		for _, r := range al.Readings {
			if r.App == a.ma.name {
				mine = append(mine, r)
			}
		}
		al.Readings = mine
		out.Alerts = append(out.Alerts, al)
	}
	return out, true
}

// rulesSafe are the names a rules body holds that the entropy layer
// could mask: each alert's name (motel's, or a rule line's, held to its
// grammar), and the apps named (as the backups body's).
func rulesSafe(b wire.Rules) []string {
	var safe []string
	for _, al := range b.Alerts {
		safe = append(safe, al.Name)
		for _, s := range append([]string{al.App}, al.Apps...) {
			if appNameRe.MatchString(s) {
				safe = append(safe, s)
			}
		}
		for _, r := range al.Readings {
			for _, s := range []string{r.App, r.Instance} {
				if appNameRe.MatchString(s) {
					safe = append(safe, s)
				}
			}
		}
	}
	return safe
}

// respondRules writes a rules body. It holds no byte of an app's: the
// alert names and thresholds are the Caddyfile's (in the box's
// repository, not a secret by policy), the words motel's, the numbers
// the store's and the detectors', the times the box's. So it passes a
// filter that knows no app's values, as the backups body does: its
// shape and entropy layers run over it, and no env_file value shaped
// like a date, a number or a word of the body can make a body no client
// reads.
func respondRules(w http.ResponseWriter, body wire.Rules, ok bool) error {
	if !ok {
		return RespondJSON(w, http.StatusInternalServerError, wire.Error{Error: noRulesListing}, NewRedactor(nil, nil))
	}
	return RespondJSON(w, http.StatusOK, body, NewRedactor(nil, rulesSafe(body)))
}

// rules is GET /_motel/rules, for a viewer of the box: every alert motel
// can raise here, what each fires on, and how close each is: a check's
// last reading, a store rule's value for each app or filesystem it
// judges and whether vmalert holds it pending or firing, and since
// when. The disks' paths and every app's numbers are in it: a viewer of
// the box's alone.
func (h *Handler) rules(w http.ResponseWriter, r *http.Request, p *Person) error {
	box, err := h.allowBox(p.Principal, access.BoxRules)
	if err != nil {
		// serveMotel asked Decide about the row's action already: the two
		// falling apart.
		return RespondJSON(w, http.StatusForbidden, ForbiddenBody(err), nil)
	}
	body, ok := box.boxRules(r.Context(), h.logger)
	return respondRules(w, body, ok)
}

// appRules is GET /_motel/apps/<app>/rules: the rules that apply to the
// handle's app, with its numbers alone.
func (h *Handler) appRules(w http.ResponseWriter, r *http.Request, app appHandle) error {
	body, ok := app.rules(r.Context(), h.app.rulesSource, h.logger)
	return respondRules(w, body, ok)
}
