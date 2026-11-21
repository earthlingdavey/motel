package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	// readOnlyStale is how many asks in a row the store must leave
	// unanswered before its last "read-only" stops holding the other two
	// events back.
	readOnlyStale = 3
	// rulesQuery is the store's view of vmalert.
	rulesQuery = `max(max_over_time(up{job="vmalert"}[3m]))`

	eventStoreDown     = "metrics_store_down"
	eventRulesDown     = "metrics_rules_down"
	eventStoreReadOnly = "metrics_store_read_only"
	eventStoreKilled   = "metrics_store_killed"
	sourceMetrics      = "metrics"

	// storeKilledName is the state directory's file the store's
	// ExecStopPost= adds a line to at each kill at its MemoryMax=
	// (units/metrics/10-motel.conf): the kill's time and the limit, in
	// seconds and bytes.
	storeKilledName = "store-killed"
	// killHold is how long after one metrics_store_killed the next
	// waits: a store killed again and again by the same reads is told
	// once an hour, with how many kills, not at each.
	killHold = time.Hour
)

// finalPushTimeout bounds the import of the push when the pusher stops
// (release): a store that does not answer must not hold moteld's stop,
// which the unit gives 5 s in all (packaging/moteld.service,
// TimeoutStopSec) before its SIGKILL. A push over the socket takes
// milliseconds. The gather before it has its own bound, diskStatWait,
// and is outside this one, so a hung disk does not cost the import its
// time. That bound is spent once at most: a statfs that hangs makes the
// loop's last gather wait it out (release waits for the loop), and the
// push's gather then finds that statfs still out and does not wait
// again; or the loop was idle, and the push's gather waits. So a hung
// disk and a store that does not answer, two faults, hold the stop
// about 4 s. A var so that a test need not wait it out.
var finalPushTimeout = 2 * time.Second

// storeAPI is the store, as the pusher uses it.
type storeAPI interface {
	importText(ctx context.Context, text []byte) error
	query(ctx context.Context, q string) ([]series, error)
	readOnly(ctx context.Context) (bool, error)
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
	// tellKill is report for metrics_store_killed: false when nothing
	// took it (no config runs notify), and the kills are kept.
	tellKill func(Event) bool
	logger   *zap.Logger
	active   *MetricsStore
	store    storeAPI
	gather   prometheus.Gatherer
	live     []liveStore

	failingSince time.Time
	storeDown    bool
	rulesBad     int
	rulesDown    bool
	// pushedAt and rulesReadAt are when the last push ended and the
	// last read of vmalert's up was answered: `moteld monitor rules`
	// lists failingSince and rulesBad as of them.
	pushedAt    time.Time
	rulesReadAt time.Time
	// readOnly is the last answer the store gave, and what was told;
	// readOnlyMisses counts the asks it has not answered since.
	readOnly       bool
	readOnlyMisses int

	failed prometheus.Counter

	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func newPusher(report func(Event)) *pusher {
	p := &pusher{
		now: time.Now, report: report, logger: zap.NewNop(),
		failed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "motel_metrics_push_failed_total",
			Help: "Pushes of motel's metrics to the metrics store that failed.",
		}),
	}
	p.tellKill = func(ev Event) bool { p.report(ev); return true }
	return p
}

// thePusher reports through notify's engine, which drops what it is
// told while no config runs notify (a monitor block with channels).
var thePusher = func() *pusher {
	p := newPusher(theEngine.report)
	p.tellKill = theEngine.reportIfTelling
	return p
}()

// registerFailed puts the push's failure counter on a config's
// registry, beside notify's own.
// The counter registered already (the same config's registry) is no
// error.
func (p *pusher) registerFailed(reg *prometheus.Registry) error {
	if reg == nil {
		return nil
	}
	err := reg.Register(p.failed)
	var dup prometheus.AlreadyRegisteredError
	if errors.As(err, &dup) {
		return nil
	}
	return err
}

func storeEvent(name string, firing bool) Event {
	ev := Event{Name: name, Kind: kindState, Severity: SeverityWarning, Source: sourceMetrics, Firing: firing}
	switch name {
	case eventStoreReadOnly:
		ev.Severity = SeverityCritical
		ev.Summary = "the metrics store is read-only: its disk is below its free-space floor, and nothing new is kept"
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
	text, err := gatherText(g, log)
	if err == nil {
		err = st.importText(ctx, text)
	}
	now := p.now()
	var evs []Event
	p.mu.Lock()
	if p.active != a {
		p.mu.Unlock()
		return // another config took the pusher while this one pushed
	}
	p.pushedAt = now
	if err != nil {
		p.failed.Inc()
		if p.failingSince.IsZero() {
			p.failingSince = now
			log.Warn("monitor metrics: push failed", zap.String("store", a.store()), zap.Error(err))
		} else {
			log.Debug("monitor metrics: push failed", zap.String("store", a.store()), zap.Error(err))
		}
		// A read-only store refuses every import: metrics_store_read_only
		// names that, and a second alert for its consequence would not.
		if now.Sub(p.failingSince) >= storeDownAfter && !p.heldReadOnly() {
			p.storeDown = true
			evs = append(evs, storeEvent(eventStoreDown, true))
		}
	} else {
		if !p.failingSince.IsZero() {
			log.Info("monitor metrics: push succeeded again", zap.Duration("failing_for", now.Sub(p.failingSince)))
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

// gatherText is g gathered, in the text format the store imports: a
// tick's push (pushOnce) and the stop's (finalPush).
func gatherText(g prometheus.Gatherer, log *zap.Logger) ([]byte, error) {
	mfs, gerr := g.Gather()
	if gerr != nil {
		// A pedantic registry still returns what it could gather.
		log.Debug("monitor metrics: gathering the registry", zap.Error(gerr))
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// checkReadOnly asks the store itself whether it is read-only, and
// says whether it is. motel asks, not a rule: a read-only store refuses
// its own scrape with every other write, so the flag never reaches a
// series a rule could read (measured on Debian's 1.112.0). It is told
// at every read that finds it set, as the other two are: the engine
// drops what it is told while no config runs notify, and one told only
// when it changed would then never be told. An ask the store does not
// answer is no reading: what was told stands, and for readOnlyStale
// asks so does the hold on the other two events (heldReadOnly).
// It returns whether that hold stands.
func (p *pusher) checkReadOnly(ctx context.Context) bool {
	p.mu.Lock()
	a, st, log := p.active, p.store, p.logger
	p.mu.Unlock()
	if a == nil {
		return false
	}
	ro, err := st.readOnly(ctx)
	var evs []Event
	p.mu.Lock()
	if p.active != a {
		p.mu.Unlock()
		return false
	}
	if err != nil {
		if p.readOnlyMisses == 0 {
			log.Warn("monitor metrics: could not read whether the store is read-only", zap.String("store", a.store()), zap.Error(err))
		}
		p.readOnlyMisses++
		held := p.heldReadOnly()
		p.mu.Unlock()
		return held
	}
	p.readOnlyMisses = 0
	switch {
	case ro:
		// What the store kept of vmalert is as old as the floor: the
		// count of reads without it starts again when it writes.
		p.rulesBad, p.rulesReadAt = 0, p.now()
		p.readOnly = true
		evs = append(evs, storeEvent(eventStoreReadOnly, true))
	case p.readOnly:
		p.readOnly = false
		evs = append(evs, storeEvent(eventStoreReadOnly, false))
	}
	p.mu.Unlock()
	for _, ev := range evs {
		p.report(ev)
	}
	return ro
}

// heldReadOnly is whether the store's last answer was read-only and
// still stands: what follows from a read-only store is then not told
// as something else. One ask the store does not answer leaves it
// standing (a store at its floor is slow); readOnlyStale in a row do
// not, since the store may by then be down. Called with mu held.
func (p *pusher) heldReadOnly() bool {
	return p.readOnly && p.readOnlyMisses < readOnlyStale
}

// checkChain is the minute's watch: whether the store is read-only,
// and unless that holds, whether it has seen vmalert up. A read-only store
// keeps no scrape of vmalert, so its answer to the second is no
// reading. Each ask has a budget of its own: a slow /metrics must not
// leave the query none.
func (p *pusher) checkChain(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	ro := p.checkReadOnly(rctx)
	cancel()
	if ro {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	defer cancel()
	p.checkRules(cctx)
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
	p.rulesReadAt = p.now()
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
		p.checkKilled()
		pctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
		p.pushOnce(pctx)
		cancel()
		if odd = !odd; odd {
			// Budgets of its own: a slow push must not leave the check
			// none, which it would take for no reading.
			p.checkChain(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// killSettle is how long a taken store-killed waits before it is read:
// a kill's line written through a descriptor opened just before the
// rename lands in it within milliseconds, and a read before then would
// lose it with the file.
const killSettle = 5 * time.Second

// checkKilled tells of the store's kills that its ExecStopPost= left in
// the state directory (storeKilledName): one one-shot for all the lines
// there, at most one a killHold (the time of the last kept beside them,
// storeKilledName+".told", so that a moteld restart keeps it), and the
// lines taken, so that each kill is told once. The file waits while
// nothing can tell it (moteld down, no config running notify: tellKill
// says so, and nothing is taken) and through the hold, so what the next
// one-shot says is every kill since the last. Taken by a rename to a
// name of its own, so a kill that adds its line later starts a new
// file, and read a tick later (killSettle), so one that opened it just
// before is in it: a file taken this tick waits for the next tell, and
// those taken before are told now, so a store killed at every start is
// still told. A taken file a read failed on is read with the next.
func (p *pusher) checkKilled() {
	p.mu.Lock()
	a, log, now, tell := p.active, p.logger, p.now(), p.tellKill
	p.mu.Unlock()
	if a == nil {
		return
	}
	path := filepath.Join(a.stateDir(), storeKilledName)
	if b, err := os.ReadFile(path + ".told"); err == nil { //nolint:gosec // the state directory's own file
		if t, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			// A time ahead of now (the clock stepped back) holds nothing.
			if d := now.Sub(time.Unix(t, 0)); d >= 0 && d < killHold {
				return
			}
		}
	}
	if err := os.Rename(path, path+".taken."+strconv.FormatInt(now.UnixNano(), 10)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("monitor metrics: could not read the store's kills", zap.String("file", path), zap.Error(err))
	}
	taken, _ := filepath.Glob(path + ".taken.*")
	var lines []byte
	var read []string
	for _, f := range taken {
		at, err := strconv.ParseInt(strings.TrimPrefix(f, path+".taken."), 10, 64)
		if d := now.Sub(time.Unix(0, at)); err == nil && d >= 0 && d < killSettle {
			continue // taken this tick: read with the next tell
		}
		b, err := os.ReadFile(f) //nolint:gosec // the state directory's own file
		if err != nil {
			log.Warn("monitor metrics: could not read the store's kills", zap.String("file", f), zap.Error(err))
			continue
		}
		lines, read = append(lines, b...), append(read, f)
	}
	ev, n := killedEvent(lines)
	if n > 0 {
		if !tell(ev) {
			return // nothing took it: the files wait
		}
		log.Warn("monitor metrics: the metrics store was killed by the kernel; systemd restarted it", zap.String("summary", ev.Summary),
			zap.Int("kills", n), zap.String("box_kills", ev.Details["box_kills"]), zap.String("limit", ev.Details["limit"]), zap.String("since", ev.Details["first"]))
		if err := os.WriteFile(path+".told", []byte(strconv.FormatInt(now.Unix(), 10)+"\n"), 0o600); err != nil {
			log.Warn("monitor metrics: could not keep when the store's kills were told", zap.Error(err))
		}
	}
	for _, f := range read {
		_ = os.Remove(f)
	}
}

// killedEvent is metrics_store_killed for the store-killed lines b, and
// how many kills they hold: each line is the kill's Unix time and the
// limit in bytes (systemd's word when there is none), or "box" for a
// kill of the box's own out-of-memory killer under the limit. A line
// that does not read so is still a kill.
func killedEvent(b []byte) (Event, int) {
	var first time.Time
	limit, n, box, changed := "", 0, 0, false
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		n++
		if t, err := strconv.ParseInt(f[0], 10, 64); err == nil && (first.IsZero() || time.Unix(t, 0).Before(first)) {
			first = time.Unix(t, 0)
		}
		switch {
		case len(f) < 2:
		case f[1] == "box":
			box++
		default:
			l := f[1]
			if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				l = strconv.FormatUint(v>>20, 10) + " MiB"
			}
			if limit != "" && l != limit {
				changed = true
			}
			limit = l
		}
	}
	times := "once"
	if n > 1 {
		times = strconv.Itoa(n) + " times"
	}
	at := ""
	if !first.IsZero() {
		at = first.UTC().Format(" since 2006-01-02 15:04 UTC")
		if n == 1 {
			at = first.UTC().Format(" at 2006-01-02 15:04 UTC")
		}
	}
	how := "at its memory limit"
	switch {
	case changed:
		how += " (the last " + limit + ")"
	case limit != "":
		how += " (" + limit + ")"
	}
	remedy := "systemctl edit victoria-metrics, MemoryMax= under [Service], then systemctl restart victoria-metrics; what fills it is how many apps and windows are read at once"
	switch {
	case box == n:
		how, remedy = "when the box ran out of memory", "the box as a whole is short of memory: a higher limit for the store would not help"
	case box > 0:
		how += ", " + strconv.Itoa(box) + " of them when the box ran out of memory"
	}
	summary := fmt.Sprintf("the metrics store was killed %s%s, %s; it restarted by itself, missing pushes, rule checks and reads meanwhile", times, at, how)
	if len([]rune(summary)) > maxSummary {
		// The details carry the time and the box's share whole.
		summary = fmt.Sprintf("the metrics store was killed %s, %s; it restarted by itself", times, how)
	}
	ev := Event{
		Name: eventStoreKilled, Kind: kindEvent, Severity: SeverityWarning, Source: sourceMetrics,
		Summary: summary,
		Details: map[string]string{"kills": strconv.Itoa(n), "remedy": remedy},
	}
	if limit != "" {
		ev.Details["limit"] = limit
	}
	if box > 0 {
		ev.Details["box_kills"] = strconv.Itoa(box)
	}
	if !first.IsZero() {
		ev.Details["first"] = first.UTC().Format(time.RFC3339)
	}
	return ev, n
}

// finalPush is release's push of what was counted since the last tick.
// Without it a moteld restart lost up to a push of every app's counts,
// and the store's request_rate read up to a tenth low for the five
// minutes the restart sat in its window: a `request_rate <` rule fired
// and recovered on the hotserve-1 deploy of ecb829a. Its import has its
// own budget, finalPushTimeout, started after the gather, since the
// loop's context is cancelled; a failure is a journal line, as a
// tick's, and is not counted: the counter goes with the process.
// failing is whether the pushes were failing already, which the first
// tick's warning said; socket is the store's, for the line.
func finalPush(socket string, st storeAPI, g prometheus.Gatherer, log *zap.Logger, failing bool) {
	text, err := gatherText(g, log)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), finalPushTimeout)
		err = st.importText(ctx, text)
		cancel()
	}
	switch {
	case err == nil:
	case failing:
		log.Debug("monitor metrics: the push at stop failed", zap.String("store", socket), zap.Error(err))
	default:
		log.Warn("monitor metrics: the push at stop failed: the counts since the last push are not in the store", zap.String("store", socket), zap.Error(err))
	}
}

// configure points the pusher at a config and starts its goroutine
// the first time. Called from MetricsStore.start.
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
	if p.active != nil && l.m != nil && p.active.store() != l.m.store() {
		// Another store: the one before's outage is not its history, and
		// must not count toward its alerts. What is firing stays told,
		// until a read of this store clears it or bears it out.
		p.failingSince, p.rulesBad, p.readOnlyMisses = time.Time{}, 0, 0
		p.pushedAt, p.rulesReadAt = time.Time{}, time.Time{}
	}
	p.active, p.store, p.gather, p.logger = l.m, l.store, l.gather, l.logger
}

// release is the monitor app's Stop: as notify's engine, it goes back to an
// older config still live (a reload that failed after Start), and with
// none it stops, pushes once more (finalPush) and reports the chain
// keys cleared, since nothing can clear them any more.
func (p *pusher) release(m *MetricsStore) {
	p.mu.Lock()
	p.live = slices.DeleteFunc(p.live, func(x liveStore) bool { return x.m == m })
	if p.active != m {
		p.mu.Unlock()
		return
	}
	if n := len(p.live); n > 0 {
		// The config that started last was stopped while an older one
		// serves on: a reload that failed after Start. Its Start had
		// written its rules; the serving one's go back.
		older := p.live[n-1]
		p.setLive(older)
		p.mu.Unlock()
		older.m.applyRules()
		return
	}
	// What the last push is made with, once the loop has stopped.
	socket, st, g, log, failing := m.store(), p.store, p.gather, p.logger, !p.failingSince.IsZero()
	// Nothing of the stopped config is held: its registry holds its
	// apps, its store client a transport.
	p.setLive(liveStore{logger: p.logger})
	var evs []Event
	if p.storeDown {
		evs = append(evs, storeEvent(eventStoreDown, false))
	}
	if p.rulesDown {
		evs = append(evs, storeEvent(eventRulesDown, false))
	}
	if p.readOnly {
		evs = append(evs, storeEvent(eventStoreReadOnly, false))
	}
	p.failingSince, p.storeDown, p.rulesBad, p.rulesDown = time.Time{}, false, 0, false
	p.pushedAt, p.rulesReadAt = time.Time{}, time.Time{}
	p.readOnly, p.readOnlyMisses = false, 0
	cancel, done, running := p.cancel, p.done, p.running
	p.running = false
	p.mu.Unlock()
	if running {
		cancel()
		<-done
		finalPush(socket, st, g, log, failing)
	}
	for _, ev := range evs {
		p.report(ev)
	}
	// A config serving on with no metrics line took the line away:
	// vmalert and the store still run (setup's units), and the
	// stopped config's rules would go on reaching the channels. motel's
	// own shutdown leaves them, so the store's alerts reach the operator
	// while moteld is down: Caddy empties its running context before it
	// stops the last config's apps (caddy.Stop), so configRunning
	// (load.go) is a reload's alone here.
	if configRunning() {
		m.retireRules()
	}
}
