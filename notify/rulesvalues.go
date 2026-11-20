package notify

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The store rules' numbers, evaluated on the box (DESIGN-metrics.md,
// "How close each alert is"): each store rule's value query at the time
// vmalert's newest evaluation can have read (now less the listing's
// query offset), and the state and active-since queries once each at
// the store's present, at most once per rulesEvery for the whole
// process, whatever the number of people, routes and configs, within
// rulesBudget. `moteld monitor rules` and the people's reads all take
// their numbers from it. A query is never a person's: the box runs the
// listing's own, which rules.yml holds.
const (
	// rulesEvery is how long one evaluation, or one failure, is given to
	// every reader: a token, stolen or not, drives at most one
	// evaluation (a query per store rule's value, and two) per
	// rulesEvery.
	// vmalert evaluates once a minute: a number is never newer than it.
	rulesEvery = 30 * time.Second
	// rulesBudget bounds one evaluation, every query of it: a read
	// never waits longer on a store that does not answer.
	rulesBudget = 2 * time.Second
	// rulesAtOnce is how many of its queries run at once: the store
	// (capped at 96 MiB, one or two cores) also serves vmalert, the
	// pusher and GET /<app>'s metrics.
	rulesAtOnce = 4
)

// rulesQuerier is the store's instant query at a time (the zero time:
// the store's present), its answer's points that are no number left out
// (storeClient.instantAt).
type rulesQuerier interface {
	instantAt(ctx context.Context, q string, t time.Time) ([]series, error)
}

// valueAsk is one store rule's value query.
type valueAsk struct{ name, query string }

// valueAsks are the listing's store rules, each with its value query,
// in the listing's order.
func valueAsks(l RulesListing) []valueAsk {
	var asks []valueAsk
	for _, e := range l.Alerts {
		if e.ValueQuery != "" {
			asks = append(asks, valueAsk{e.Name, e.ValueQuery})
		}
	}
	return asks
}

// subjectKey is what a store rule judges one series of: an app, a
// filesystem, or (both empty) the one series of a value with no label.
// vmalert's ALERTS and ALERTS_FOR_STATE carry the same labels, with the
// alert's name.
type subjectKey struct{ name, app, path string }

func keyOf(name string, labels map[string]string) subjectKey {
	return subjectKey{name, labels["app"], labels["path"]}
}

// evaluate is one evaluation of the store rules asks names, against
// store, with q's timing and state queries: each rule's readings, one
// per subject, the value rounded, the state vmalert last wrote and
// since when. Two rules with one value query (an operator's
// memory_share and app_memory_high) are one query. The first failure is
// the whole evaluation's: a set read in part would say "inactive" of a
// rule whose state was not read.
func evaluate(ctx context.Context, store rulesQuerier, q *StoreQueries, asks []valueAsk, now time.Time) (map[string][]Reading, error) {
	at := now.Add(-time.Duration(q.QueryOffsetSeconds) * time.Second).UTC().Truncate(time.Second)
	answers := map[string]*[]series{}
	var states, since []series
	type job struct {
		dst  *[]series
		q    string
		when time.Time
	}
	jobs := []job{{&states, q.StateQuery, time.Time{}}, {&since, q.ActiveSinceQuery, time.Time{}}}
	for _, a := range asks {
		if answers[a.query] == nil {
			answers[a.query] = new([]series)
			jobs = append(jobs, job{answers[a.query], a.query, at})
		}
	}
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, rulesAtOnce)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			*j.dst, errs[i] = store.instantAt(ctx, j.q, j.when)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	// Firing over pending: both can be in the lookback as one turns into
	// the other.
	state := map[subjectKey]string{}
	for _, s := range states {
		k := keyOf(s.labels["alertname"], s.labels)
		switch st := s.labels["alertstate"]; {
		case st == wire.RuleFiring:
			state[k] = st
		case st == wire.RulePending && state[k] == "":
			state[k] = st
		}
	}
	active := map[subjectKey]time.Time{}
	for _, s := range since {
		if v, ok := instantValue(s); ok && v > 0 {
			active[keyOf(s.labels["alertname"], s.labels)] = time.Unix(int64(v), 0).UTC()
		}
	}
	out := make(map[string][]Reading, len(asks))
	for _, a := range asks {
		seen := map[subjectKey]bool{}
		var rs []Reading
		for _, s := range *answers[a.query] {
			v, ok := lastValue(s)
			if !ok {
				continue
			}
			k := keyOf(a.name, s.labels)
			if seen[k] {
				continue // a value with labels the key does not tell apart: its first
			}
			seen[k] = true
			v = roundSignificant(v)
			rs = append(rs, storeReading(k, &v, at, state, active))
		}
		// vmalert holds a subject pending or firing whose value the store
		// gave none of at the time read (its last evaluation still in the
		// lookback): its state stands, with no number.
		for k := range state {
			if k.name == a.name && !seen[k] {
				rs = append(rs, storeReading(k, nil, at, state, active))
			}
		}
		slices.SortFunc(rs, func(x, y Reading) int { return cmp.Or(cmp.Compare(x.App, y.App), cmp.Compare(x.Instance, y.Instance)) })
		out[a.name] = rs
	}
	return out, nil
}

func storeReading(k subjectKey, v *float64, at time.Time, state map[subjectKey]string, active map[subjectKey]time.Time) Reading {
	r := Reading{App: k.app, Instance: k.path, Value: v, MeasuredAt: at, State: wire.RuleInactive}
	if st := state[k]; st != "" {
		r.State = st
		if t, ok := active[k]; ok {
			r.ActiveSince = &t
		}
	}
	return r
}

// lastValue is an instant answer's one value, when it is a number:
// negative too (a predicted free space below zero).
func lastValue(s series) (float64, bool) {
	if len(s.points) == 0 {
		return 0, false
	}
	return s.points[len(s.points)-1].v, true
}

// roundSignificant keeps v to six significant digits: an unrounded share
// marshals as a run of twenty digits or more (0.00005740305648246404),
// which a response filter's entropy layer reads as a generated secret
// (metricsview.go, roundTo); six are more than a screen shows.
func roundSignificant(v float64) float64 {
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 6, 64), 64)
	if err != nil {
		return v
	}
	return r
}

// rulesEntry is one evaluation, for every reader until it is
// rulesEvery old; done closes when it is in.
type rulesEntry struct {
	key  string
	at   time.Time
	got  map[string][]Reading
	ok   bool
	done chan struct{}
}

// rulesView is the process's one evaluation at a time and its answer.
type rulesView struct {
	now func() time.Time

	mu sync.Mutex
	// held is each set of queries' newest evaluation, by rulesKey: two
	// configs whose rules differ (a reload's old and new, for a moment)
	// each keep theirs, rather than each read evaluating again. An entry
	// rulesEvery old is dropped at the next get.
	held map[string]*rulesEntry
}

// theRuleValues is the process's: the admin route and every config's
// source read through it, so the people and root share one evaluation.
var theRuleValues = &rulesView{now: time.Now}

// get answers from the entry under rulesEvery old that evaluated the
// same queries, or waits for the evaluation in flight, or makes it. A
// reader that gives up first is told the store did not answer; the
// evaluation it waited for still fills the entry for the next.
func (v *rulesView) get(ctx context.Context, logger *zap.Logger, store rulesQuerier, q *StoreQueries, asks []valueAsk) (map[string][]Reading, bool) {
	if logger == nil {
		logger = zap.NewNop()
	}
	key := rulesKey(q, asks)
	v.mu.Lock()
	now := v.now()
	stale := func(e *rulesEntry) bool { return now.Sub(e.at) >= rulesEvery || now.Before(e.at) }
	if v.held == nil {
		v.held = map[string]*rulesEntry{}
	}
	for k, old := range v.held {
		if stale(old) {
			delete(v.held, k)
		}
	}
	e := v.held[key]
	if e == nil {
		e = &rulesEntry{key: key, at: now, done: make(chan struct{})}
		v.held[key] = e
		v.mu.Unlock()
		// The evaluation is the box's, not this reader's: one who goes
		// away does not end it for the next. The entry is closed however
		// it ends, a panic too.
		defer close(e.done)
		ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rulesBudget)
		got, err := evaluate(ectx, store, q, asks, now)
		cancel()
		if err != nil {
			// A reader is told only that the store did not answer: the
			// cause is the operator's, at most one line per rulesEvery.
			logger.Warn("monitor metrics: the rules' numbers were not read: the metrics store did not answer their queries; moteld monitor rules and the people's reads of the rules say the store did not answer", zap.Error(err))
		}
		e.got, e.ok = got, err == nil
		return e.got, e.ok
	}
	v.mu.Unlock()
	select {
	case <-e.done:
		return e.got, e.ok
	case <-ctx.Done():
		return nil, false
	}
}

// rulesKey is what one evaluation asks: the store, the timing and every
// query, so a config whose rules differ never reads another's numbers.
func rulesKey(q *StoreQueries, asks []valueAsk) string {
	parts := []string{q.Socket, strconv.Itoa(q.QueryOffsetSeconds), q.StateQuery, q.ActiveSinceQuery}
	for _, a := range asks {
		parts = append(parts, a.name, a.query)
	}
	return strings.Join(parts, "\x00")
}

// attachValues gives each store rule of l its readings from got, and
// says in l.Store.Values whether the store answered (ok).
func attachValues(l *RulesListing, got map[string][]Reading, ok bool) {
	if l.Store == nil {
		return
	}
	l.Store.Values = wire.StoreUnanswered
	if !ok {
		return
	}
	l.Store.Values = wire.StoreOn
	for i := range l.Alerts {
		if l.Alerts[i].ValueQuery != "" {
			l.Alerts[i].Readings = got[l.Alerts[i].Name]
		}
	}
}

// withValues is attachValues with the process's evaluation over store.
// A nil store (no metrics line) adds nothing.
func withValues(ctx context.Context, l *RulesListing, store rulesQuerier, logger *zap.Logger) {
	if l.Store == nil {
		return
	}
	if store == nil {
		attachValues(l, nil, false)
		return
	}
	got, ok := theRuleValues.get(ctx, logger, store, l.Store, valueAsks(*l))
	attachValues(l, got, ok)
}

// rulesSource is liveswap's RulesSource for one config: its listing,
// with what its detectors last read and the store's numbers.
type rulesSource struct{ a *App }

func (s rulesSource) Rules(ctx context.Context) (RulesListing, error) {
	return listRulesNow(ctx, s.a)
}

// listRulesNow is the listing of the config a as the admin route and
// the people's reads give it: the detectors' last numbers, the pusher's
// and the watchdogs', and the store's from the shared evaluation.
func listRulesNow(ctx context.Context, a *App) (RulesListing, error) {
	e := theEngine
	e.mu.Lock()
	seen := e.seen
	e.mu.Unlock()
	l, err := listRules(a, seen, thePusher.read(a.Metrics), watchdogBudgets(a))
	if err != nil {
		return RulesListing{}, err
	}
	withValues(ctx, &l, storeOf(a.Metrics), a.logger)
	return l, nil
}

// storeOf is the store a config's rules are evaluated against: nil with
// no metrics line. A var, so that a test stands a store in.
var storeOf = func(m *MetricsStore) rulesQuerier {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client
}
