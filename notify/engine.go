package notify

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

// Event is one thing a detector reports (DESIGN-notify.md, "Events").
type Event struct {
	// Name is the alertname: watchdog_throttled, deploy_failed, …
	Name string
	// Kind is kindState (fires, then resolves once) or kindEvent (a
	// one-shot).
	Kind     string
	Severity string
	// App is the app the event is about; empty for the box's own. Only
	// an app event carries the app label, which is what routes it to
	// the app's channels: a box event about an app (a stale backup)
	// names the app in Instance instead.
	App      string
	Instance string
	// Source is the detector: liveswap, backup, tls, disk, or
	// external:<name> for `motel notify send`.
	Source  string
	Summary string
	// Release and DeployedBy are labels on a one-shot (part of what
	// happened) and annotations on a state alert, whose identity is
	// what is wrong, never when or by whom (#418).
	Release, DeployedBy string
	// Firing is a state alert's condition: true while it holds, false
	// when it has cleared.
	Firing bool
	// Hold is how long a state alert's condition must stay clear before
	// its recovery is posted; at least minHold.
	Hold time.Duration
	// Details are an external producer's key=value pairs (motel notify
	// send --detail), annotations detail_<key>, through the filter.
	Details map[string]string
}

func (ev Event) subject() string {
	if ev.App != "" {
		return "app:" + ev.App
	}
	return "box"
}

// key is #418's dedupe key: <source>/<subject>/<name>[/<instance>].
func (ev Event) key() string {
	k := ev.Source + "/" + ev.subject() + "/" + ev.Name
	if ev.Instance != "" {
		k += "/" + ev.Instance
	}
	return k
}

// The engine's timings (DESIGN-notify.md, "Behaviour"):
const (
	// repostEvery keeps a firing state alert alive in Alertmanager,
	// whose resolve_timeout (5 m) would otherwise resolve it.
	repostEvery = time.Minute
	// oneShotLife is a one-shot's endsAt: long enough that it cannot
	// expire inside any group_interval tick (alertmanager_edge_tests.md).
	oneShotLife = 5 * time.Minute
	// minHold is the least a recovery waits: #418's watchdog rule,
	// max(30s, watchdog_grace), with 30 s for every other state.
	minHold = 30 * time.Second
	// bucketSize transitions per key, refilled one per bucketRefill:
	// four messages an hour from a flapping key, at most.
	bucketSize   = 4
	bucketRefill = 15 * time.Minute
	// retryFor is how long a post is retried before it is given up,
	// as a journal line and a counter.
	retryFor = 5 * time.Minute
	// queueMax bounds what waits for Alertmanager.
	queueMax = 512
	// maxSummary is the summary's length, in runes.
	maxSummary = 200
	// heartbeatEvery and heartbeatLife are the Watchdog's: posted every
	// minute, alive four, so one missed post is no gap.
	heartbeatEvery = time.Minute
	heartbeatLife  = 4 * time.Minute
)

// poster is Alertmanager's alert API.
type poster interface {
	postAlerts(ctx context.Context, alerts []amAlert) error
}

// bucket is one key's token bucket.
type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) take(now time.Time) bool {
	if b.at.IsZero() {
		b.tokens, b.at = bucketSize, now
	}
	b.tokens += float64(now.Sub(b.at)) / float64(bucketRefill)
	if b.tokens > bucketSize {
		b.tokens = bucketSize
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// stateAlert is one state key: what the detector says (firing) and
// what Alertmanager was told (posted).
type stateAlert struct {
	ev         Event
	firing     bool
	posted     bool
	startsAt   time.Time
	lastPost   time.Time
	clearSince time.Time
	hold       time.Duration
	heldLogged bool
}

// pending is a post waiting for Alertmanager.
type pending struct {
	key     string
	alerts  []amAlert
	first   time.Time
	attempt int
	// journal says the post carries an event: a failure to deliver it
	// is a `notify: delivery failed` line. Re-posts, the heartbeat and
	// the mute fail at debug.
	journal bool
}

// engine is notify's state, which outlives a config load: the firing
// table, the buckets, the posts waiting on Alertmanager. One per
// process (theEngine); each config's App points it at its own
// Alertmanager and settings at Start.
type engine struct {
	mu     sync.Mutex
	now    func() time.Time
	logger *zap.Logger
	post   poster
	// known is every channel URL, as KEY=value: the filter's known
	// values for every summary.
	known []string

	deploySucceeded bool
	active          *App // the config whose Start configured it; nil when none is running

	states        map[string]*stateAlert
	mutes         map[string]time.Time
	buckets       map[string]*bucket
	lastHeartbeat time.Time

	queue    []*pending
	wake     chan struct{}
	retryFor time.Duration
	backoff  func(attempt int) time.Duration

	running bool
	cancel  context.CancelFunc
	done    chan struct{}

	// The polled sources, and what the active config points them at.
	backup     backupPoller
	disk       diskPoller
	backupPath string
	diskPaths  []string
	lastDisk   time.Time

	// channel_failed's memory of Alertmanager's counter, and the read.
	fail    failWatch
	metrics func(context.Context) ([]byte, error)

	failures *failCounter
}

func newEngine() *engine {
	return &engine{
		now:      time.Now,
		logger:   zap.NewNop(),
		states:   map[string]*stateAlert{},
		mutes:    map[string]time.Time{},
		buckets:  map[string]*bucket{},
		wake:     make(chan struct{}, 1),
		retryFor: retryFor,
		backoff: func(attempt int) time.Duration {
			d := time.Second << min(attempt, 5)
			return min(d, 30*time.Second)
		},
		failures: newFailCounter(),
	}
}

// theEngine is the process's.
var theEngine = newEngine()

func (e *engine) bucketFor(key string) *bucket {
	b := e.buckets[key]
	if b == nil {
		b = &bucket{}
		e.buckets[key] = b
	}
	return b
}

// clean is s through liveswap's filter, its known values the channel
// URLs and safe the names the text carries (a release, which the
// entropy layer would mask when it is a git sha), cut to maxSummary.
// One filter per event: events are rare, and a filter holds its safe
// names.
func (e *engine) clean(s string, safe ...string) string {
	s, _ = liveswap.NewRedactor(e.known, safe).Redact(s)
	return deploytrust.CutRunes(s, maxSummary)
}

// report takes one event from a detector.
func (e *engine) report(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil {
		return
	}
	ev.Summary = e.clean(ev.Summary, ev.Release)
	if len(ev.Details) > 0 {
		d := make(map[string]string, len(ev.Details))
		for k, v := range ev.Details {
			d[k] = e.clean(v, ev.Release)
		}
		ev.Details = d
	}
	if ev.Severity == "" {
		ev.Severity = SeverityWarning
	}
	now := e.now()
	if ev.Kind == kindEvent {
		if ev.Name == "deploy_succeeded" && !e.deploySucceeded {
			return // off by default (#418's table); `deploy_succeeded on` sends it
		}
		if !e.bucketFor(ev.key()).take(now) {
			e.journal(ev, "oneshot", zap.String("suppressed", "more than four an hour for this key"))
			return
		}
		e.journal(ev, "oneshot")
		e.enqueue(&pending{key: ev.key(), alerts: []amAlert{oneShotAlert(ev, now)}, first: now, journal: true})
		return
	}
	key := ev.key()
	st := e.states[key]
	if ev.Firing && st != nil && st.posted && st.ev.Severity != ev.Severity {
		// The same key at another severity (a disk from 80 to 90 %) is
		// another alert to Alertmanager, whose labels carry it: the old
		// one is ended in the post that fires the new.
		if e.bucketFor(key).take(now) {
			old := st.ev
			e.journal(ev, "firing", zap.String("was", old.Severity))
			e.enqueue(&pending{key: key, alerts: []amAlert{stateAlertFor(old, st.startsAt, now), stateAlertFor(ev, now, time.Time{})}, first: now, journal: true})
			st.ev, st.firing, st.startsAt, st.lastPost, st.clearSince = ev, true, now, now, time.Time{}
			return
		}
		ev.Severity = st.ev.Severity // held at the old one until the bucket allows
	}
	if ev.Firing {
		if st == nil {
			st = &stateAlert{}
			e.states[key] = st
		}
		st.ev, st.firing, st.clearSince = ev, true, time.Time{}
	} else {
		if st == nil {
			return // nothing was told
		}
		st.firing = false
		if st.clearSince.IsZero() {
			st.clearSince, st.hold = now, max(ev.Hold, minHold)
		}
	}
	e.reconcile(key, st, now)
}

// reconcile moves one state key towards what its detector says, as far
// as its bucket and its hold allow. Called with mu held.
func (e *engine) reconcile(key string, st *stateAlert, now time.Time) {
	switch {
	case st.firing && !st.posted:
		if !e.bucketFor(key).take(now) {
			if !st.heldLogged {
				e.journal(st.ev, "firing", zap.String("held", "more than four transitions an hour for this key; posted when the bucket allows"))
				st.heldLogged = true
			}
			return
		}
		st.posted, st.startsAt, st.heldLogged = true, now, false
		e.journal(st.ev, "firing")
		e.postState(key, st, now, time.Time{}, true)
	case !st.firing && !st.posted:
		delete(e.states, key)
	case !st.firing && now.Sub(st.clearSince) >= st.hold:
		if !e.bucketFor(key).take(now) {
			if !st.heldLogged {
				e.journal(st.ev, "resolved", zap.String("held", "more than four transitions an hour for this key; posted when the bucket allows"))
				st.heldLogged = true
			}
			e.repost(key, st, now)
			return
		}
		e.journal(st.ev, "resolved")
		e.postState(key, st, now, now, true)
		delete(e.states, key)
	default:
		// Firing and told, or cleared and held: keep it alive.
		e.repost(key, st, now)
	}
}

func (e *engine) repost(key string, st *stateAlert, now time.Time) {
	if now.Sub(st.lastPost) >= repostEvery {
		e.postState(key, st, now, time.Time{}, false)
	}
}

func (e *engine) postState(key string, st *stateAlert, now, endsAt time.Time, journal bool) {
	st.lastPost = now
	e.enqueue(&pending{key: key, alerts: []amAlert{stateAlertFor(st.ev, st.startsAt, endsAt)}, first: now, journal: journal})
}

// mute posts deploy_in_progress for an app while its deploy runs; the
// inhibit rule silences the app's watchdog alerts under it.
func (e *engine) mute(app string, on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil || app == "" {
		return
	}
	now := e.now()
	if on {
		e.mutes[app] = now
		e.enqueue(&pending{key: "mute/" + app, alerts: []amAlert{muteAlert(app, now, time.Time{})}, first: now})
		return
	}
	if _, ok := e.mutes[app]; !ok {
		return
	}
	delete(e.mutes, app)
	e.enqueue(&pending{key: "mute/" + app, alerts: []amAlert{muteAlert(app, now, now)}, first: now})
}

// tick is the engine's minute: the heartbeat, re-posts, held
// recoveries.
func (e *engine) tick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil {
		return
	}
	now := e.now()
	if now.Sub(e.lastHeartbeat) >= heartbeatEvery {
		e.lastHeartbeat = now
		e.enqueue(&pending{key: "heartbeat", alerts: []amAlert{{
			Labels:      map[string]string{"alertname": alertWatchdog, "severity": "none"},
			Annotations: map[string]string{"summary": "motel is running and can reach Alertmanager"},
			StartsAt:    now, EndsAt: now.Add(heartbeatLife),
		}}, first: now})
	}
	for key, st := range e.states {
		e.reconcile(key, st, now)
	}
	for app, at := range e.mutes {
		if now.Sub(at) >= repostEvery {
			e.mutes[app] = now
			e.enqueue(&pending{key: "mute/" + app, alerts: []amAlert{muteAlert(app, now, time.Time{})}, first: now})
		}
	}
}

func labelsFor(ev Event) map[string]string {
	l := map[string]string{
		"alertname": ev.Name,
		"kind":      ev.Kind,
		"severity":  ev.Severity,
		"subject":   ev.subject(),
		"key":       ev.key(),
		"source":    ev.Source,
	}
	if ev.App != "" {
		l["app"] = ev.App
	}
	if ev.Instance != "" {
		l["instance"] = ev.Instance
	}
	return l
}

func oneShotAlert(ev Event, now time.Time) amAlert {
	l := labelsFor(ev)
	if ev.Release != "" {
		l["release"] = ev.Release
	}
	if ev.DeployedBy != "" {
		l["deployed_by"] = ev.DeployedBy
	}
	return amAlert{Labels: l, Annotations: annotations(ev), StartsAt: now, EndsAt: now.Add(oneShotLife)}
}

func annotations(ev Event) map[string]string {
	ann := map[string]string{"summary": ev.Summary}
	for k, v := range ev.Details {
		ann["detail_"+k] = v
	}
	return ann
}

// stateAlertFor is the alert for a state key; a zero endsAt leaves
// Alertmanager's resolve_timeout to end it if motel stops posting, and
// endsAt now resolves it at once (measured: the younger resolved alert
// wins the merge).
func stateAlertFor(ev Event, startsAt, endsAt time.Time) amAlert {
	ann := annotations(ev)
	if ev.Release != "" {
		ann["release"] = ev.Release
	}
	if ev.DeployedBy != "" {
		ann["deployed_by"] = ev.DeployedBy
	}
	return amAlert{Labels: labelsFor(ev), Annotations: ann, StartsAt: startsAt, EndsAt: endsAt}
}

func muteAlert(app string, now, endsAt time.Time) amAlert {
	return amAlert{
		Labels:      map[string]string{"alertname": alertDeployMute, "kind": "mute", "app": app, "severity": "none"},
		Annotations: map[string]string{"summary": "a deploy of " + app + " is in progress"},
		StartsAt:    now, EndsAt: endsAt,
	}
}

// journal is the `notify: event` line, written before the post: with
// Alertmanager down, the event is still on record.
func (e *engine) journal(ev Event, state string, extra ...zap.Field) {
	fields := []zap.Field{
		zap.String("key", ev.key()), zap.String("state", state),
		zap.String("severity", ev.Severity), zap.String("summary", ev.Summary),
		zap.String("source", ev.Source),
	}
	if ev.Release != "" {
		fields = append(fields, zap.String("release", ev.Release))
	}
	if ev.DeployedBy != "" {
		fields = append(fields, zap.String("deployed_by", ev.DeployedBy))
	}
	e.logger.Info("notify: event", append(fields, extra...)...)
}

// enqueue adds a post. Called with mu held. A full queue drops its
// oldest, as a failed delivery.
func (e *engine) enqueue(p *pending) {
	if len(e.queue) >= queueMax {
		old := e.queue[0]
		e.queue = e.queue[1:]
		e.gaveUp(old, errors.New("the queue is full"))
	}
	e.queue = append(e.queue, p)
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *engine) gaveUp(p *pending, err error) {
	e.failures.inc()
	log := e.logger.Debug
	if p.journal {
		log = e.logger.Error
	}
	log("notify: delivery failed", zap.String("key", p.key), zap.Int("attempts", p.attempt), zap.Error(err))
}

// attempt tries the queue's head once. It reports whether to try again
// at once (true) or after wait.
func (e *engine) attempt(ctx context.Context) (again bool, wait time.Duration) {
	e.mu.Lock()
	if len(e.queue) == 0 {
		e.mu.Unlock()
		return false, 0
	}
	p := e.queue[0]
	post := e.post
	e.mu.Unlock()

	err := post.postAlerts(ctx, p.alerts)

	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == 0 || e.queue[0] != p {
		return true, 0 // dropped while posting
	}
	p.attempt++
	var refused *amStatusError
	switch {
	case err == nil:
		e.queue = e.queue[1:]
		return true, 0
	case errors.As(err, &refused), e.now().Sub(p.first) >= e.retryFor:
		// Refused is final; retried for long enough is too.
		e.queue = e.queue[1:]
		e.gaveUp(p, err)
		return true, 0
	case ctx.Err() != nil:
		return false, 0
	default:
		e.logger.Debug("notify: Alertmanager unreachable, retrying", zap.String("key", p.key), zap.Int("attempt", p.attempt), zap.Error(err))
		return false, e.backoff(p.attempt)
	}
}

// poll asks the polled sources, outside the lock: the disk every
// minute, the backup record every five.
func (e *engine) poll() {
	e.mu.Lock()
	if e.active == nil {
		e.mu.Unlock()
		return
	}
	now := e.now()
	path, paths := e.backupPath, slices.Clone(e.diskPaths)
	diskDue := now.Sub(e.lastDisk) >= time.Minute
	if diskDue {
		e.lastDisk = now
	}
	read := e.metrics
	e.mu.Unlock()
	if diskDue && read != nil {
		ctx, cancel := context.WithTimeout(context.Background(), amRequestTimeout)
		e.scrapeFailed(ctx, read)
		cancel()
	}
	evs := e.backup.poll(now, path)
	if diskDue {
		evs = append(evs, e.disk.poll(paths)...)
	}
	for _, ev := range evs {
		e.report(ev)
	}
}

// run is the engine's goroutine: the queue, and a tick every
// tickEvery.
func (e *engine) run(ctx context.Context, tickEvery time.Duration) {
	defer close(e.done)
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	e.tick()
	e.poll()
	var retry <-chan time.Time
	for {
		if retry == nil {
			for {
				again, wait := e.attempt(ctx)
				if again {
					continue
				}
				if wait > 0 {
					retry = time.After(wait)
				}
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.tick()
			e.poll()
		case <-e.wake:
		case <-retry:
			retry = nil
		}
	}
}

// configure points the engine at a config and starts its goroutine the
// first time. Called from App.Start.
func (e *engine) configure(a *App, post poster, known []string, logger *zap.Logger) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setConfig(a, post, known, logger)
	if !e.running {
		ctx, cancel := context.WithCancel(context.Background())
		e.running, e.cancel, e.done = true, cancel, make(chan struct{})
		go e.run(ctx, 5*time.Second)
	}
}

// setConfig is configure's state, with mu held.
func (e *engine) setConfig(a *App, post poster, known []string, logger *zap.Logger) {
	e.active, e.post, e.known, e.logger = a, post, known, logger
	e.deploySucceeded = a.DeploySucceeded
	e.backupPath, e.diskPaths = a.backupRecord(), a.diskPaths
	if c, ok := post.(*amClient); ok {
		e.metrics = c.metrics
	}
	e.fail.teacher, e.fail.channels = nil, a.receivers
	e.fail.kinds = map[string]string{}
	if a.cfg != nil {
		e.fail.teacher = slices.Clone(a.cfg.teacher)
		for name, ch := range a.cfg.channels {
			e.fail.kinds[name] = channelKinds[ch.Kind]
		}
		e.prune(a.cfg.apps)
	}
}

// prune ends what no detector of the new config can clear: the keys of
// an app it no longer runs, the backup keys once the record is off, a
// filesystem's once its path is not measured; and an app's mute. Each
// fired key is resolved, as a journal line, so its channels hear the
// end of it. Called with mu held.
func (e *engine) prune(apps []string) {
	now := e.now()
	for key, st := range e.states {
		ev := st.ev
		gone := ev.App != "" && !slices.Contains(apps, ev.App) ||
			ev.Source == "backup" && e.backupPath == "" ||
			ev.Source == "disk" && !slices.Contains(e.diskPaths, ev.Instance)
		if !gone {
			continue
		}
		if st.posted {
			e.journal(ev, "resolved", zap.String("why", "the config no longer has its source"))
			e.postState(key, st, now, now, true)
		}
		delete(e.states, key)
	}
	if e.backupPath == "" {
		e.backup.set.prev = nil
	}
	for k := range e.disk.set.prev {
		if !slices.Contains(e.diskPaths, e.disk.set.prev[k].Instance) {
			delete(e.disk.set.prev, k)
		}
	}
	for app := range e.mutes {
		if !slices.Contains(apps, app) {
			delete(e.mutes, app)
			e.enqueue(&pending{key: "mute/" + app, alerts: []amAlert{muteAlert(app, now, now)}, first: now})
		}
	}
}

// release is App.Stop: when the stopping config is the one the engine
// serves, the engine stops taking events and stops its goroutine. A
// reload's new config has started already (Caddy starts the new before
// it stops the old), so then this config is not the active one and the
// engine runs on.
func (e *engine) release(a *App) {
	e.mu.Lock()
	if e.active != a {
		e.mu.Unlock()
		return
	}
	e.active = nil
	cancel, done, running := e.cancel, e.done, e.running
	e.running = false
	e.mu.Unlock()
	if running {
		cancel()
		<-done
	}
}

// failCounter is motel_notify_delivery_failed_total, registered on
// each config's metrics registry (the admin API's /metrics).
type failCounter struct{ c prometheus.Counter }

func newFailCounter() *failCounter {
	return &failCounter{c: prometheus.NewCounter(prometheus.CounterOpts{
		Name: "motel_notify_delivery_failed_total",
		Help: "Posts to Alertmanager that motel gave up on: refused, or unreachable for five minutes.",
	})}
}

func (f *failCounter) inc() { f.c.Inc() }

func (f *failCounter) register(reg *prometheus.Registry) {
	if reg == nil {
		return
	}
	if err := reg.Register(f.c); err != nil {
		var dup prometheus.AlreadyRegisteredError
		if !errors.As(err, &dup) {
			return
		}
	}
}
