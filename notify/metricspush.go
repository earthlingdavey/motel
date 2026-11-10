package notify

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"go.uber.org/zap"
)

// The push and the watch of the chain (DESIGN-metrics.md, "What motel
// pushes", "The chain"): one per process, as notify's engine is, and
// pointed by each config's Start at its own registry and store.
const (
	pushEvery = 30 * time.Second
	// storeDownAfter is how long every push must have failed before
	// metrics_store_down fires.
	storeDownAfter = 5 * time.Minute
	// rulesDownReads is how many reads in a row, a minute apart, must
	// find vmalert not up before metrics_rules_down fires: a fresh
	// store has not scraped it yet.
	rulesDownReads = 3
	// rulesQuery is the store's view of vmalert.
	rulesQuery = `max(max_over_time(up{job="vmalert"}[3m]))`

	eventStoreDown = "metrics_store_down"
	eventRulesDown = "metrics_rules_down"
	sourceMetrics  = "metrics"
)

// storeAPI is the store, as the pusher uses it.
type storeAPI interface {
	importText(ctx context.Context, text []byte) error
	query(ctx context.Context, q string) ([]series, error)
}

type liveStore struct {
	m      *MetricsStore
	store  storeAPI
	gather prometheus.Gatherer
	logger *zap.Logger
}

// pusher is the process's push loop and chain watch.
type pusher struct {
	mu     sync.Mutex
	now    func() time.Time
	report func(Event)
	logger *zap.Logger
	active *MetricsStore
	store  storeAPI
	gather prometheus.Gatherer
	live   []liveStore

	failingSince time.Time
	storeDown    bool
	rulesBad     int
	rulesDown    bool

	failed prometheus.Counter

	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func newPusher(report func(Event)) *pusher {
	return &pusher{
		now: time.Now, report: report, logger: zap.NewNop(),
		failed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "motel_metrics_push_failed_total",
			Help: "Pushes of motel's metrics to the metrics store that failed.",
		}),
	}
}

// thePusher reports through notify's engine, which drops what it is
// told while no config runs a notify block.
var thePusher = newPusher(theEngine.report)

// registerFailed puts the push's failure counter on a config's
// registry, beside notify's own.
func (p *pusher) registerFailed(reg *prometheus.Registry) {
	if reg == nil {
		return
	}
	if err := reg.Register(p.failed); err != nil {
		var dup prometheus.AlreadyRegisteredError
		_ = errors.As(err, &dup)
	}
}

func storeEvent(name string, firing bool) Event {
	ev := Event{Name: name, Kind: kindState, Severity: SeverityWarning, Source: sourceMetrics, Firing: firing}
	switch name {
	case eventStoreDown:
		ev.Summary = "motel could not push its metrics to the metrics store for five minutes"
	default:
		ev.Summary = "the metrics store has not seen vmalert up for three minutes: the store's rules are not evaluated"
	}
	return ev
}

// pushOnce gathers the active config's registry and imports it.
func (p *pusher) pushOnce(ctx context.Context) {
	p.mu.Lock()
	a, st, g, log := p.active, p.store, p.gather, p.logger
	p.mu.Unlock()
	if a == nil {
		return
	}
	mfs, gerr := g.Gather()
	if gerr != nil {
		// A pedantic registry still returns what it could gather.
		log.Debug("metrics_store: gathering the registry", zap.Error(gerr))
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	var err error
	for _, mf := range mfs {
		if err = enc.Encode(mf); err != nil {
			break
		}
	}
	if err == nil {
		err = st.importText(ctx, buf.Bytes())
	}
	now := p.now()
	var evs []Event
	p.mu.Lock()
	if p.active != a {
		p.mu.Unlock()
		return // another config took the pusher while this one pushed
	}
	if err != nil {
		p.failed.Inc()
		if p.failingSince.IsZero() {
			p.failingSince = now
			log.Warn("metrics_store: push failed", zap.String("store", a.store()), zap.Error(err))
		} else {
			log.Debug("metrics_store: push failed", zap.String("store", a.store()), zap.Error(err))
		}
		if now.Sub(p.failingSince) >= storeDownAfter {
			p.storeDown = true
			evs = append(evs, storeEvent(eventStoreDown, true))
		}
	} else {
		if !p.failingSince.IsZero() {
			log.Info("metrics_store: push succeeded again", zap.Duration("failing_for", now.Sub(p.failingSince)))
		}
		p.failingSince = time.Time{}
		if p.storeDown {
			p.storeDown = false
			evs = append(evs, storeEvent(eventStoreDown, false))
		}
	}
	p.mu.Unlock()
	for _, ev := range evs {
		p.report(ev)
	}
}

// checkRules asks the store whether it has seen vmalert up. A query the
// store does not answer is no reading: metrics_store_down says that.
func (p *pusher) checkRules(ctx context.Context) {
	p.mu.Lock()
	a, st := p.active, p.store
	p.mu.Unlock()
	if a == nil {
		return
	}
	res, err := st.query(ctx, rulesQuery)
	if err != nil {
		return
	}
	up := false
	for _, s := range res {
		if len(s.points) > 0 && s.points[len(s.points)-1].v >= 1 {
			up = true
		}
	}
	var evs []Event
	p.mu.Lock()
	if p.active != a {
		p.mu.Unlock()
		return
	}
	if up {
		p.rulesBad = 0
		if p.rulesDown {
			p.rulesDown = false
			evs = append(evs, storeEvent(eventRulesDown, false))
		}
	} else {
		p.rulesBad++
		if p.rulesBad >= rulesDownReads {
			p.rulesDown = true
			evs = append(evs, storeEvent(eventRulesDown, true))
		}
	}
	p.mu.Unlock()
	for _, ev := range evs {
		p.report(ev)
	}
}

func (p *pusher) run(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(pushEvery)
	defer ticker.Stop()
	odd := false
	for {
		pctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
		p.pushOnce(pctx)
		if odd = !odd; odd {
			p.checkRules(pctx)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// configure points the pusher at a config and starts its goroutine
// the first time. Called from MetricsStore.Start.
func (p *pusher) configure(l liveStore) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = slices.DeleteFunc(p.live, func(x liveStore) bool { return x.m == l.m })
	p.live = append(p.live, l)
	p.setLive(l)
	if !p.running {
		ctx, cancel := context.WithCancel(context.Background())
		p.running, p.cancel, p.done = true, cancel, make(chan struct{})
		go p.run(ctx)
	}
}

func (p *pusher) setLive(l liveStore) {
	p.active, p.store, p.gather, p.logger = l.m, l.store, l.gather, l.logger
}

// release is MetricsStore.Stop: as notify's engine, it goes back to an
// older config still live (a reload that failed after Start), and with
// none it stops, reporting both chain keys cleared, since nothing can
// clear them any more.
func (p *pusher) release(m *MetricsStore) {
	p.mu.Lock()
	p.live = slices.DeleteFunc(p.live, func(x liveStore) bool { return x.m == m })
	if p.active != m {
		p.mu.Unlock()
		return
	}
	if n := len(p.live); n > 0 {
		p.setLive(p.live[n-1])
		p.mu.Unlock()
		return
	}
	p.active = nil
	var evs []Event
	if p.storeDown {
		evs = append(evs, storeEvent(eventStoreDown, false))
	}
	if p.rulesDown {
		evs = append(evs, storeEvent(eventRulesDown, false))
	}
	p.failingSince, p.storeDown, p.rulesBad, p.rulesDown = time.Time{}, false, 0, false
	cancel, done, running := p.cancel, p.done, p.running
	p.running = false
	p.mu.Unlock()
	if running {
		cancel()
		<-done
	}
	for _, ev := range evs {
		p.report(ev)
	}
}
