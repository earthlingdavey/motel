package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
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
	// Source is the detector: liveswap, backup, tls, disk, metrics,
	// rules (the operator's post_deploy), or external:<name> for
	// `moteld monitor send`.
	Source  string
	Summary string
	// Release and DeployedBy are labels on a one-shot (part of what
	// happened) and annotations on a state alert, whose identity is
	// what is wrong, never when or by whom (#418).
	Release, DeployedBy string
	// Safe are names the summary carries that the filter's heuristics
	// must leave as written, beside Release: a key's fingerprint, which
	// looks generated, and a person's name.
	Safe []string
	// Firing is a state alert's condition: true while it holds, false
	// when it has cleared.
	Firing bool
	// Hold is how long a state alert's condition must stay clear before
	// its recovery is posted; at least minHold.
	Hold time.Duration
	// Details are an external producer's key=value pairs (moteld monitor
	// send --detail), annotations detail_<key>, through the filter.
	Details map[string]string
	// Standing, on a state alert, is a detector's report of what it
	// told before and has not ended (liveswap's ReportStanding at Start,
	// and the producers' states kept in external.json): ignored for a key
	// the table holds; for one it does not —
	// a moteld restarted since, its table empty while Alertmanager still
	// holds the alert — taken as told: a firing one fires, a cleared one
	// is held as after a firing one was told, so its recovery comes
	// after the hold and never at resolve_timeout.
	Standing bool
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
	// bucketSize transitions per key at once, refilled one per
	// bucketRefill: a flapping key sends four, then one a quarter-hour
	// (up to about eight in its first hour, four an hour after).
	bucketSize   = 4
	bucketRefill = 15 * time.Minute
	// retryFor is how long a post is retried before it is given up,
	// as a journal line and a counter.
	retryFor = 5 * time.Minute
	// queueMax bounds what waits for Alertmanager.
	queueMax = 512
	// earlyMax bounds the people's announcements kept while no config
	// runs notify (engine.early).
	earlyMax = 64
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
	// lastSeen is when the detector last said it fires: for a source
	// that never says it has cleared (tlsStale), the end is its silence.
	lastSeen time.Time
}

// tlsStale is how long a cert_failed key may go unreported before it is
// taken as cleared. CertMagic reports cert_failed on every failed
// attempt for a name it manages, and its longest retry interval is six
// hours (certmagic's retryIntervals, repeated for up to 30 days), so a
// name still failing is reported at least every six hours. A name taken
// out of the config is never reported again, and never obtained, so no
// cert_obtained clears it: thirteen hours is two retries missed and an
// hour more.
const tlsStale = 13 * time.Hour

// pending is a post waiting for Alertmanager.
type pending struct {
	key     string
	alerts  []amAlert
	first   time.Time
	attempt int
	// journal says the post carries an event: a failure to deliver it
	// is a `monitor: delivery failed` line. Re-posts, the heartbeat and
	// the mute fail at debug.
	journal bool
	// end is the watchdog_* resolve the post carries (toldEnd): stamped
	// when Alertmanager takes it, forgotten if it is given up.
	end *toldEnd
}

// expired says an alert of the post had an endsAt ahead when it was
// queued (a one-shot, ChannelFailed, the heartbeat) and it has passed:
// Alertmanager would take it as resolved and tell no one. An end (a
// resolve, the mute's end) is queued with endsAt at its queuing, and is
// not given up for it. Passed is on the wall clock, Alertmanager's (the
// JSON's endsAt against its own clock): now carries a monotonic reading
// that a clock step does not move, and Round(0) drops it. endsAt and
// first come from one reading, so either clock orders them alike.
func (p *pending) expired(now time.Time) bool {
	now = now.Round(0)
	for _, a := range p.alerts {
		if a.EndsAt.After(p.first) && !now.Before(a.EndsAt) {
			return true
		}
	}
	return false
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

	states map[string]*stateAlert
	mutes  map[string]time.Time
	// ends are the watchdog_* resolves posted in the last endWindow, and
	// those a mute has stood over since (toldEnd).
	ends          map[string]*toldEnd
	buckets       map[string]*bucket
	lastHeartbeat time.Time
	// lastOccurrence is the last one-shot's occurred_at (occurrence).
	lastOccurrence time.Time
	// early are a person's key's announcements (source access) told
	// while no config runs notify: a start's first seconds, when the
	// http app can serve before notify's Start, and liveswap's record has
	// already counted the network as told. At most earlyMax, told when a
	// config next does (point).
	early []Event

	queue    []*pending
	wake     chan struct{}
	retryFor time.Duration
	backoff  func(attempt int) time.Duration

	running bool
	cancel  context.CancelFunc
	done    chan struct{}
	// live are the configs started and not stopped, oldest first: a
	// reload that fails after notify's Start stops the new config while
	// the old one serves on, and the engine goes back to it.
	live []liveConfig

	// The polled sources, and what the active config points them at.
	backup     backupPoller
	disk       diskPoller
	backupPath string
	diskPaths  []string
	lastDisk   time.Time
	// seen is what the pollers measured last, copied under mu for
	// `moteld monitor rules` (ruleslist.go).
	seen polled

	// channel_failed's memory of Alertmanager's counter, and the read.
	fail failWatch
	// untold is each failure no channel was told of, by its key: the
	// heartbeat's hold (channelfailed.go).
	untold  map[string]*untold
	metrics func(context.Context) ([]byte, error)

	failures *failCounter

	// externalPath is the file the producers' states are kept in for a
	// restart (external.go), set once it has been read, at the first
	// configure of a config with the block; externalSaved is what was
	// written to it last (nil: no file).
	externalPath  string
	externalSaved []byte
}

func newEngine() *engine {
	return &engine{
		now:      time.Now,
		logger:   zap.NewNop(),
		states:   map[string]*stateAlert{},
		mutes:    map[string]time.Time{},
		ends:     map[string]*toldEnd{},
		buckets:  map[string]*bucket{},
		untold:   map[string]*untold{},
		fail:     failWatch{started: time.Now()},
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
	return cleanText(e.known, s, safe...)
}

// cleanText is clean with the known values given: a silence's comment
// is masked with the known values of the config its route checked.
func cleanText(known []string, s string, safe ...string) string {
	s, _ = liveswap.NewRedactor(known, safe).Redact(s)
	return deploytrust.CutRunes(s, maxSummary)
}

// report takes one event from a detector.
func (e *engine) report(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reportLocked(ev)
}

// reportLocked is report, with mu held.
func (e *engine) reportLocked(ev Event) {
	if e.active == nil {
		if ev.Source == sourceAccess && len(e.early) < earlyMax {
			e.early = append(e.early, ev)
		}
		return
	}
	ev.Summary = e.clean(ev.Summary, append([]string{ev.Release}, ev.Safe...)...)
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
		e.enqueue(&pending{key: ev.key(), alerts: []amAlert{oneShotAlert(ev, e.occurrence(now))}, first: now, journal: true})
		return
	}
	if strings.HasPrefix(ev.Source, sourceExternal) {
		defer e.persistExternal()
	}
	key := ev.key()
	st := e.states[key]
	if ev.Standing {
		if st != nil {
			return // the table holds it: nothing was lost
		}
		if !ev.Firing {
			// Told before this process: the alert Alertmanager holds is
			// the posted one, kept alive through the hold and resolved
			// after it.
			st = &stateAlert{ev: ev, posted: true, startsAt: now, lastSeen: now, clearSince: now, hold: max(ev.Hold, minHold)}
			e.states[key] = st
			e.journal(ev, "held")
			e.reconcile(key, st, now)
			st.ev.Standing = false // told again once; what follows is this process's
			return
		}
	}
	if ev.Firing && st != nil && st.posted && st.ev.Severity != ev.Severity {
		// The same key at another severity (a disk from 80 to 90 %) is
		// another alert to Alertmanager, whose labels carry it: the old
		// one is ended in the post that fires the new.
		if e.bucketFor(key).take(now) {
			old := st.ev
			e.journal(ev, "firing", zap.String("was", old.Severity))
			e.enqueue(&pending{key: key, alerts: []amAlert{stateAlertFor(old, st.startsAt, now), stateAlertFor(ev, now, time.Time{})}, first: now, journal: true})
			st.ev, st.firing, st.startsAt, st.lastPost, st.clearSince, st.lastSeen = ev, true, now, now, time.Time{}, now
			return
		}
		ev.Severity = st.ev.Severity // held at the old one until the bucket allows
	}
	if ev.Firing {
		if st == nil {
			st = &stateAlert{}
			e.states[key] = st
		}
		st.ev, st.firing, st.clearSince, st.lastSeen = ev, true, time.Time{}, now
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
	// A standing report's own lines say so; the event kept for the
	// key's later lines (a held post, the resolve) is this process's.
	st.ev.Standing = false
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
	case !st.firing && now.Sub(st.clearSince) >= st.hold && !e.muted(st.ev):
		if !e.bucketFor(key).take(now) {
			if !st.heldLogged {
				e.journal(st.ev, "resolved", zap.String("held", "more than four transitions an hour for this key; posted when the bucket allows"))
				st.heldLogged = true
			}
			e.repost(key, st, now)
			return
		}
		e.journal(st.ev, "resolved")
		p := e.postState(key, st, now, now, true)
		delete(e.states, key)
		if muteTarget(st.ev) {
			p.end = &toldEnd{ev: st.ev, startsAt: st.startsAt, queued: 1}
			e.ends[key] = p.end
		}
	default:
		// Firing and told, or cleared and held (by its hold, or by the
		// mute): keep it alive.
		e.repost(key, st, now)
	}
}

// muteTarget says the deploy mute's inhibit rule (render.go) takes ev:
// a watchdog_* alert of an app.
func muteTarget(ev Event) bool { return ev.App != "" && strings.HasPrefix(ev.Name, "watchdog_") }

// muted says the app's deploy mute stands over ev. Alertmanager mutes a
// resolve under it as it does a firing alert, then drops it as sent, so
// the recovery is held until the mute has ended (measured on 0.28.1,
// DESIGN-notify.md "Behaviour"). With mu held.
func (e *engine) muted(ev Event) bool {
	_, ok := e.mutes[ev.App]
	return ok && muteTarget(ev)
}

// toldEnd is a watchdog_* resolve the engine posted. Alertmanager judges
// the mute when the alert's group flushes, up to a group_interval after
// it takes the post, so a deploy that starts in that time swallows it
// too: the mute marks it, and its end posts it again. A resolve that was
// sent before is not sent twice (Alertmanager's notification log:
// measured). One whose every post the queue gave up on is forgotten, as
// a failed delivery: Alertmanager never had it.
type toldEnd struct {
	ev       Event
	startsAt time.Time
	at       time.Time // Alertmanager last took a post of it; zero: none yet
	queued   int       // its posts waiting in the queue (the resolve, a mute's end's)
	muted    bool      // a mute of its app began while a post waited or within endWindow of at
}

// endPosted settles a post of t that Alertmanager took (took) or that
// the queue gave up on. With mu held.
func (e *engine) endPosted(key string, t *toldEnd, took bool) {
	t.queued--
	if took {
		t.at = e.now()
	}
	if t.queued == 0 && t.at.IsZero() && e.ends[key] == t {
		delete(e.ends, key)
	}
}

// endWindow is how long a resolve Alertmanager took may yet meet a mute:
// twice the state routes' group_interval (stateGroupInterval, 1 m).
const endWindow = 2 * time.Minute

func (e *engine) repost(key string, st *stateAlert, now time.Time) {
	if now.Sub(st.lastPost) >= repostEvery {
		e.postState(key, st, now, time.Time{}, false)
	}
}

func (e *engine) postState(key string, st *stateAlert, now, endsAt time.Time, journal bool) *pending {
	st.lastPost = now
	p := &pending{key: key, alerts: []amAlert{stateAlertFor(st.ev, st.startsAt, endsAt)}, first: now, journal: journal}
	e.enqueue(p)
	return p
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
		for _, t := range e.ends {
			if t.ev.App == app && (t.queued > 0 || !t.at.IsZero() && now.Sub(t.at) < endWindow) {
				t.muted = true
			}
		}
		return
	}
	if _, ok := e.mutes[app]; !ok {
		return
	}
	e.endMute(app, now)
}

// endMute ends app's mute and posts right behind its end what the mute
// held or may have swallowed, before a flush of the group could tell
// the alert it ends: each resolve it stood over (toldEnd), unless its
// key has fired since, and each recovery it held. With mu held.
func (e *engine) endMute(app string, now time.Time) {
	delete(e.mutes, app)
	e.enqueue(&pending{key: "mute/" + app, alerts: []amAlert{muteAlert(app, now, now)}, first: now})
	for key, t := range e.ends {
		switch {
		case t.ev.App != app || !t.muted:
		case e.states[key] != nil && e.states[key].posted:
			// Fired since, and told: that alert is the key's now. One the
			// flapping guard holds unposted is not, and posts after this.
			delete(e.ends, key)
		default:
			e.enqueue(&pending{key: key, alerts: []amAlert{stateAlertFor(t.ev, t.startsAt, now)}, first: now, end: t})
			t.queued++
			t.muted = false
		}
	}
	for key, st := range e.states {
		if st.ev.App == app && !st.firing && muteTarget(st.ev) {
			e.reconcile(key, st, now)
		}
	}
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
	// Held while a failure no channel was told of stands: the check off
	// the box is what reports it.
	if now.Sub(e.lastHeartbeat) >= heartbeatEvery && !e.heartbeatHeld() {
		e.lastHeartbeat = now
		e.enqueue(&pending{key: "heartbeat", alerts: []amAlert{{
			Labels:      map[string]string{"alertname": alertWatchdog, "severity": "none"},
			Annotations: map[string]string{"summary": "moteld is running and can reach Alertmanager"},
			StartsAt:    now, EndsAt: now.Add(heartbeatLife),
		}}, first: now})
	}
	for key, st := range e.states {
		if st.ev.Source == "tls" && st.firing && now.Sub(st.lastSeen) >= tlsStale {
			e.logger.Info("monitor: cert_failed not reported again; taken as cleared",
				zap.String("key", key), zap.Duration("unreported_for", now.Sub(st.lastSeen)))
			st.firing, st.clearSince, st.hold = false, now, minHold
		}
		e.reconcile(key, st, now)
	}
	e.persistExternal() // a recovery's hold ended
	for key, t := range e.ends {
		if t.queued == 0 && !t.muted && now.Sub(t.at) >= endWindow {
			delete(e.ends, key)
		}
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

// labelOccurredAt is a one-shot's time, its startsAt to the nanosecond
// (occurredAtLayout): it makes each occurrence its own alert to
// Alertmanager, which would take a second one-shot with the same
// labels for the first and send nothing (DESIGN-notify.md, "Events").
// Set once, when the alert is built, so a retried post is the same
// alert. No route, silence or inhibition matches on it.
const (
	labelOccurredAt  = "occurred_at"
	occurredAtLayout = "2006-01-02T15:04:05.000000000Z07:00"
)

// occurrence is a one-shot's time: now, on the wall clock, or a
// nanosecond after the last one-shot's when now is not after it and
// less than a second before it (two in one clock reading, a small step
// back), so that no two occurrences in a process share one. Called
// with mu held.
func (e *engine) occurrence(now time.Time) time.Time {
	at := now.Round(0).UTC()
	if !at.After(e.lastOccurrence) && e.lastOccurrence.Sub(at) < time.Second {
		at = e.lastOccurrence.Add(time.Nanosecond)
	}
	e.lastOccurrence = at
	return at
}

// oneShotAlert is a one-shot's alert at its occurrence's time.
func oneShotAlert(ev Event, at time.Time) amAlert {
	l := labelsFor(ev)
	if ev.Release != "" {
		l["release"] = ev.Release
	}
	if ev.DeployedBy != "" {
		l["deployed_by"] = ev.DeployedBy
	}
	l[labelOccurredAt] = at.Format(occurredAtLayout)
	return amAlert{Labels: l, Annotations: annotations(ev), StartsAt: at, EndsAt: at.Add(oneShotLife)}
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

// journal is the `monitor: event` line, written before the post: with
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
	if ev.Standing {
		// Told before this process started; reported again.
		fields = append(fields, zap.Bool("standing", true))
	}
	e.logger.Info("monitor: event", append(fields, extra...)...)
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
	if p.end != nil {
		e.endPosted(p.key, p.end, false)
	}
	log := e.logger.Debug
	if p.journal {
		log = e.logger.Error
	}
	log("monitor: delivery failed", zap.String("key", p.key), zap.Int("attempts", p.attempt), zap.Error(err))
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
	if p.expired(e.now()) {
		e.queue = e.queue[1:]
		e.gaveUp(p, errors.New("its endsAt passed before Alertmanager took it, which would take it as resolved and tell no one"))
		e.mu.Unlock()
		return true, 0
	}
	post := e.post
	e.mu.Unlock()

	err := post.postAlerts(ctx, p.alerts)

	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == 0 || e.queue[0] != p {
		// Dropped while posting (a full queue's oldest, given up): a
		// resolve Alertmanager took all the same may yet meet a mute.
		if err == nil && p.end != nil && e.active != nil {
			p.end.at = e.now()
			if e.ends[p.key] == nil {
				e.ends[p.key] = p.end
			}
		}
		return true, 0
	}
	p.attempt++
	var refused *amStatusError
	switch {
	case err == nil:
		e.queue = e.queue[1:]
		if p.end != nil {
			e.endPosted(p.key, p.end, true)
		}
		return true, 0
	case errors.As(err, &refused) && refused.code < 500, e.now().Sub(p.first) >= e.retryFor:
		// Refused (4xx) is final; retried for long enough is too. A 5xx
		// is Alertmanager's own trouble, retried like a refused connect.
		e.queue = e.queue[1:]
		e.gaveUp(p, err)
		return true, 0
	case ctx.Err() != nil:
		return false, 0
	default:
		e.logger.Debug("monitor: Alertmanager unreachable, retrying", zap.String("key", p.key), zap.Int("attempt", p.attempt), zap.Error(err))
		return false, e.backoff(p.attempt)
	}
}

// poll asks the polled sources, outside the lock: the disk every
// minute, the backup record every five.
func (e *engine) poll(ctx context.Context) {
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
		ctx, cancel := context.WithTimeout(ctx, amRequestTimeout)
		e.scrapeFailed(ctx, read)
		cancel()
	}
	evs := e.backup.poll(now, path)
	if diskDue {
		evs = append(evs, e.disk.poll(paths)...)
	}
	e.mu.Lock()
	// A config that serves may have been settled while the sources were
	// read (a reload's old Stop): what was read of a source it dropped is
	// not listed, and its events are not told, since they would fire
	// again what prune has just ended, with no poll of this config to
	// clear it (backup_record off reads nothing).
	if path == e.backupPath {
		e.seen.backups, e.seen.backupsAt = e.backup.read, e.backup.readAt
	}
	if diskDue {
		for i := range e.disk.read {
			if e.disk.read[i].at.IsZero() {
				e.disk.read[i].at = now
			}
		}
		e.seen.disks = slices.DeleteFunc(slices.Clone(e.disk.read), func(r diskReading) bool { return !slices.Contains(e.diskPaths, r.path) })
	}
	// Told before the lock is let go, so that no settle comes between the
	// filter and the reports. A poll carries a few events: one per
	// backup key and per filesystem.
	for _, ev := range slices.DeleteFunc(evs, e.unread) {
		e.reportLocked(ev)
	}
	e.mu.Unlock()
}

// run is the engine's goroutine: the queue, and a tick every
// tickEvery.
func (e *engine) run(ctx context.Context, tickEvery time.Duration) {
	defer close(e.done)
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	e.tick()
	e.poll(ctx)
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
			e.poll(ctx)
		case <-e.wake:
		case <-retry:
			retry = nil
		}
	}
}

// configure points the engine at a config and starts its goroutine the
// first time. Called from App.Start.
type liveConfig struct {
	a      *App
	post   poster
	known  []string
	logger *zap.Logger
}

func (e *engine) configure(a *App, post poster, known []string, logger *zap.Logger) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.live = slices.DeleteFunc(e.live, func(l liveConfig) bool { return l.a == a })
	e.live = append(e.live, liveConfig{a, post, known, logger})
	e.point(a, post, known, logger)
	// A reload's candidate (an older config is live) may yet be stopped
	// by another app's failed Start while the older one serves on: what
	// it would end waits until it serves, the older one's Stop (release).
	if len(e.live) == 1 {
		e.settle(a)
	}
	if !e.running && a.cfg != nil {
		// The engine starts with no mutes: at a process's start a deploy
		// the last process ran ended with it, and a reload that took the
		// block out forgot them with the table (release). Alertmanager
		// keeps each to resolve_timeout, where it would swallow a
		// watchdog_* recovery this engine posts (muted): the mute of every
		// app the config runs is ended, in one post before any other. The
		// end of a mute that is not there reaches no one.
		now := e.now()
		ends := make([]amAlert, 0, len(a.cfg.apps))
		for _, app := range a.cfg.apps {
			ends = append(ends, muteAlert(app, now, now))
		}
		if len(ends) > 0 {
			e.enqueue(&pending{key: "mute/", alerts: ends, first: now})
		}
	}
	if e.externalPath == "" && a.cfg != nil {
		e.restoreExternal(a.cfg.apps)
	}
	if !e.running {
		ctx, cancel := context.WithCancel(context.Background())
		e.running, e.cancel, e.done = true, cancel, make(chan struct{})
		go e.run(ctx, 5*time.Second)
	}
}

// setConfig is configure's state for a config that serves, with mu
// held: point, then settle.
func (e *engine) setConfig(a *App, post poster, known []string, logger *zap.Logger) {
	e.point(a, post, known, logger)
	e.settle(a)
}

// point sends the engine's posts, and reads its failures, through a's
// Alertmanager, which loaded a's render at its Start. With mu held.
func (e *engine) point(a *App, post poster, known []string, logger *zap.Logger) {
	e.active, e.post, e.known, e.logger = a, post, known, logger
	e.deploySucceeded = a.deploySucceeded()
	if c, ok := post.(*amClient); ok {
		e.metrics = c.metrics
	}
	e.fail.operator, e.fail.channels = nil, a.receivers
	e.fail.kinds = map[string]string{}
	if a.cfg != nil {
		e.fail.operator = failureAudience(a.cfg)
		for name, ch := range a.cfg.channels {
			e.fail.kinds[name] = channelKinds[ch.Kind]
		}
	}
	early := e.early
	e.early = nil
	for _, ev := range early {
		e.reportLocked(ev)
	}
}

// settle points the pollers at a's sources and ends what a no longer
// has (prune, keepUntold), once a serves: what it ends is told, and is
// not undone if a reload falls back. With mu held.
func (e *engine) settle(a *App) {
	if a.backupRecord() != e.backupPath {
		e.seen.backups = nil // another record's ages are not this one's
	}
	e.backupPath, e.diskPaths = a.backupRecord(), a.diskPaths
	// A filesystem the new config does not measure is not listed (its
	// alert is pruned below); the rest keep their reading until the
	// next poll's.
	e.seen.disks = slices.DeleteFunc(slices.Clone(e.seen.disks), func(r diskReading) bool { return !slices.Contains(a.diskPaths, r.path) })
	if a.cfg != nil {
		e.prune(a.cfg.apps)
		e.keepUntold(a.cfg.channels)
	}
}

// prune ends what no detector of the new config can clear: the keys of
// an app it no longer runs, the backup keys once the record is off, a
// filesystem's once its path is not measured; and an app's mute. Each
// fired key is resolved, as a journal line, so its channels hear the
// end of it. Called with mu held.
func (e *engine) prune(apps []string) {
	now := e.now()
	// The mute first, so that a resolve of its app's watchdog_* follows
	// its end rather than going under it (muted).
	for app := range e.mutes {
		if !slices.Contains(apps, app) {
			e.endMute(app, now)
		}
	}
	for key, st := range e.states {
		ev := st.ev
		gone := ev.App != "" && !slices.Contains(apps, ev.App) || e.unread(ev)
		if !gone {
			continue
		}
		if st.posted {
			e.journal(ev, "resolved", zap.String("why", "the config no longer has its source"))
			e.postState(key, st, now, now, true)
		}
		delete(e.states, key)
	}
	e.persistExternal()
	// The pollers' own memory is their goroutine's (poll runs outside
	// mu): a clear they report later for a key pruned here finds no
	// state, and is nothing.
}

// unread says the settled config does not read ev's source: the backup
// record off, or a filesystem it does not measure. With mu held.
func (e *engine) unread(ev Event) bool {
	return ev.Source == "backup" && e.backupPath == "" ||
		ev.Source == "disk" && !slices.Contains(e.diskPaths, ev.Instance)
}

// release is App.Stop: when the stopping config is the one the engine
// serves, and no other started config is live, the engine stops taking
// events and stops its goroutine. A reload's new config has started
// already (Caddy starts the new before it stops the old), so then this
// config is not the active one and the engine runs on; a reload that
// failed stops the new one, and the engine goes back to the old.
func (e *engine) release(a *App) {
	e.mu.Lock()
	e.live = slices.DeleteFunc(e.live, func(l liveConfig) bool { return l.a == a })
	if e.active != a {
		// A reload that succeeded stops the config before it: the one the
		// engine serves is now the one Caddy serves, and what its Start
		// left until then is done.
		if e.active != nil {
			e.settle(e.active)
		}
		e.mu.Unlock()
		return
	}
	if n := len(e.live); n > 0 {
		// The config that started last was stopped while an older one
		// serves on: a reload that failed after notify's Start. Its
		// Start had written its render; the serving one's goes back.
		l := e.live[n-1]
		e.setConfig(l.a, l.post, l.known, l.logger)
		e.mu.Unlock()
		l.logger.Info("monitor: the config serving before a failed reload has the engine again")
		if am, ok := l.post.(*amClient); ok && l.a.rendered != nil {
			l.a.applyRender(am)
		}
		return
	}
	e.active = nil
	// No config runs notify any more: what the table holds can no longer
	// be cleared by anything, and must not fire again if a later config
	// brings the block back. Alertmanager ends what it was told at
	// resolve_timeout.
	e.states, e.mutes, e.buckets = map[string]*stateAlert{}, map[string]time.Time{}, map[string]*bucket{}
	e.ends = map[string]*toldEnd{}
	e.untold = map[string]*untold{}
	// The producers' states are kept on disk at a shutdown, for the next
	// process; a reload's are forgotten below with the render.
	external, logger := e.externalPath, e.logger
	e.externalSaved = nil
	cancel, done, running := e.cancel, e.done, e.running
	e.running = false
	e.mu.Unlock()
	if running {
		cancel()
		<-done
	}
	// What waits for Alertmanager must not be posted either, whatever its
	// age, by a later config that brings the block back. With the
	// goroutine stopped, no post still queued is known to be delivered
	// (the cancel may cut one whose body Alertmanager had taken): each is
	// given up as a full queue's oldest is, at a reload and at moteld's
	// shutdown alike.
	e.mu.Lock()
	for _, p := range e.queue {
		e.gaveUp(p, errors.New("no config runs notify any more"))
	}
	e.queue = nil
	e.mu.Unlock()
	// A config serves on without notify: a reload took the block (or its
	// channels) away. Alertmanager would end what it holds at
	// resolve_timeout through the last render's receivers, a recovery
	// that did not happen on every channel, so it is given the config
	// that sends nothing, and the URL files go. moteld's own shutdown
	// leaves the render, so that what fired is kept alive across a
	// restart: Caddy empties its running context before it stops the
	// last config's apps (caddy.Stop), so configRunning (load.go) is a
	// reload's alone here, as for the store's rules (pusher.release).
	if configRunning() {
		if external != "" {
			if err := removeExternal(external); err != nil {
				logger.Warn("monitor: the producers' states kept for a restart could not be removed: a later start with the block would tell them again", zap.String("file", external), zap.Error(err))
			}
		}
		a.applyNothing()
	}
}

// failCounter is motel_notify_delivery_failed_total, registered on
// each config's metrics registry (the admin API's /metrics).
type failCounter struct{ c prometheus.Counter }

func newFailCounter() *failCounter {
	return &failCounter{c: prometheus.NewCounter(prometheus.CounterOpts{
		Name: "motel_notify_delivery_failed_total",
		Help: "Posts to Alertmanager that motel gave up on: refused, unreachable for five minutes, past their endsAt, or queued when notify stopped.",
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
