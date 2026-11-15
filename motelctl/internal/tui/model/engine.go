package model

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/smallhoursorg/motel/motelctl/internal/client"
)

// Kind is what a job reads.
type Kind int

// The kinds, one a command.
const (
	KWhoAmI Kind = iota
	KApps
	KStatus
	KDeploy
	KBox
)

// Job is one read.
type Job struct {
	Kind         Kind
	App, Version string
}

// The cadence. A box allows a person 2 requests a second with a burst
// of 60 (liveswap/access's limiter). The engine keeps well inside it:
// its own bucket refills at 1.5 a second and holds 30, so the first
// paint of 30 apps is quick and a person's other motelctl, or the one
// retry of a 401 the client makes, still has room.
const (
	Rate       = 1.5
	Burst      = 30
	FocusEvery = 5 * time.Second
	// BackgroundEvery is each other app's refresh, spread over the
	// cycle so they do not come due together.
	BackgroundEvery = 30 * time.Second
	AppsEvery       = 60 * time.Second
	BoxEvery        = 30 * time.Second
	// Idle pauses polling when no key was pressed for this long, so an
	// unattended TUI neither asks for touches nor spends the rate.
	Idle = 10 * time.Minute
	// StaleAfter missed refreshes make a value stale.
	StaleAfter = 3
	maxBackoff = 60 * time.Second
)

// Engine decides what to read next and when. It is the only caller of
// the Client: one read at a time, never faster than its bucket, a 429
// waited out for every read, a 401 or an unverified certificate
// stopping every read until the person asks again. It keeps no box
// value; the Session does. Its methods may be called from any
// goroutine.
type Engine struct {
	mu      sync.Mutex
	rate    float64
	tokens  float64
	filled  time.Time
	jobs    map[Job]*entry
	apps    []string
	focus   string
	boxOpen bool
	// misses counts reads in a row that got no answer from the box at
	// all; retryAt is when the next is tried.
	misses     int
	retryAt    time.Time
	slowUntil  time.Time
	stopped    Stop
	touched    time.Time
	inflight   *Job
	cancel     context.CancelFunc
	recovering bool
	wake       chan struct{}
}

type entry struct {
	due    time.Time
	slot   time.Time // a background status's place in the cycle
	misses int
	// held is a read not asked again until the person does (a 403, a
	// 404, a one-off record already read).
	held bool
	once bool
}

// Stop is why every read stopped, and zero while they run.
type Stop int

// The stops.
const (
	Running Stop = iota
	// SignedOut is a 401 that stood: the box did not accept the key.
	SignedOut
	// NotSigned is a token that could not be signed (a touch timed
	// out, the agent refused).
	NotSigned
	// BadCertificate is a certificate that did not verify: motelctl
	// never continues past one.
	BadCertificate
)

// NewEngine starts at now, with whoami due at once, and then the apps
// list.
func NewEngine(now time.Time) *Engine {
	e := &Engine{rate: Rate, tokens: Burst, filled: now, touched: now, jobs: map[Job]*entry{}, wake: make(chan struct{}, 1)}
	e.jobs[Job{Kind: KWhoAmI}] = &entry{due: now, once: true}
	e.jobs[Job{Kind: KApps}] = &entry{due: now}
	return e
}

// SetApps is the box's list: each app's status comes due now, and its
// later refreshes are spread over the cycle.
func (e *Engine) SetApps(names []string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	keep := map[Job]bool{}
	respread := len(names) != len(e.apps)
	e.apps = slices.Clone(names)
	for i, n := range names {
		j := Job{Kind: KStatus, App: n}
		keep[j] = true
		en, ok := e.jobs[j]
		if !ok {
			en = &entry{due: now}
			e.jobs[j] = en
		}
		// Each app has its own place in the cycle, so a pass is spread
		// over it rather than every app coming due at once.
		if !ok || respread {
			en.slot = now.Add(time.Duration(i) * e.cycle() / time.Duration(len(names)))
		}
	}
	for j := range e.jobs {
		if j.Kind == KStatus && !keep[j] && j.App != e.focus {
			delete(e.jobs, j)
		}
	}
	e.poke()
}

// Focus is the app whose screen is open, refreshed every FocusEvery;
// "" when none is.
func (e *Engine) Focus(app string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.focus = app
	if app != "" {
		j := Job{Kind: KStatus, App: app}
		if _, ok := e.jobs[j]; !ok {
			e.jobs[j] = &entry{due: now}
		}
	}
	e.poke()
}

// BoxOpen says whether the box screen is open: GET / is read only then.
func (e *Engine) BoxOpen(open bool, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.boxOpen = open
	j := Job{Kind: KBox}
	if open {
		if _, ok := e.jobs[j]; !ok {
			e.jobs[j] = &entry{due: now}
		}
	}
	e.poke()
}

// Once asks for a read now, once: a deploy's record, which does not
// change.
func (e *Engine) Once(j Job, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if en, ok := e.jobs[j]; ok && en.held {
		return
	}
	e.jobs[j] = &entry{due: now, once: true}
	e.poke()
}

// Drop forgets a one-off read the person left before it came, and
// cancels it when it is in flight: its answer is not wanted.
func (e *Engine) Drop(j Job) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if en, ok := e.jobs[j]; ok && en.once && !en.held {
		delete(e.jobs, j)
	}
	if e.inflight != nil && *e.inflight == j && e.cancel != nil {
		e.cancel()
	}
}

// Again is ctrl-r: every read the person can see comes due now, the
// held ones too (a 403 is asked again only so).
func (e *Engine) Again(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for j, en := range e.jobs {
		if en.once && en.held {
			continue
		}
		en.held = false
		if j.Kind != KStatus || j.App == e.focus || e.focus == "" {
			en.due = now
		}
	}
	e.poke()
}

// Retry is r after a stop: one whoami, as a new start. It is the only
// way out of a stop, so a refused key is asked again once a press.
func (e *Engine) Retry(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped == Running {
		return
	}
	e.stopped = Running
	e.misses = 0
	e.jobs[Job{Kind: KWhoAmI}] = &entry{due: now, once: true}
	e.poke()
}

// Touch is a key pressed: polling resumes when it had paused.
func (e *Engine) Touch(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.touched = now
	e.poke()
}

// Take returns the read to make now, or false and how long to wait
// before asking again (zero: until woken).
func (e *Engine) Take(now time.Time) (Job, bool, time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inflight != nil || e.stopped != Running || now.Sub(e.touched) >= Idle {
		return Job{}, false, 0
	}
	e.refill(now)
	gate := now
	if e.slowUntil.After(gate) {
		gate = e.slowUntil
	}
	if e.misses > 0 && e.retryAt.After(gate) {
		gate = e.retryAt
	}
	j, due, ok := e.next(gate)
	if !ok {
		return Job{}, false, 0
	}
	if due.After(gate) {
		gate = due
	}
	if e.tokens < 1 {
		if t := now.Add(time.Duration((1 - e.tokens) / e.rate * float64(time.Second))); t.After(gate) {
			gate = t
		}
	}
	if gate.After(now) {
		return Job{}, false, gate.Sub(now)
	}
	e.tokens--
	e.inflight = &j
	return j, true, 0
}

// next is the job to run first of those due by at: a one-off the
// person asked for, then whoami, the focused app, the list, the box,
// and the most overdue of the rest. With none due, it is the one due
// soonest.
func (e *Engine) next(at time.Time) (Job, time.Time, bool) {
	var best Job
	var bestDue time.Time
	bestRank, found := 99, false
	for j, en := range e.jobs {
		if en.held || (j.Kind == KBox && !e.boxOpen) {
			continue
		}
		r := e.rank(j, en)
		if en.due.After(at) {
			r += 10 // not yet due: after every job that is
		}
		if !found || r < bestRank || r == bestRank && en.due.Before(bestDue) || r == bestRank && en.due.Equal(bestDue) && j.App < best.App {
			best, bestDue, bestRank, found = j, en.due, r, true
		}
	}
	return best, bestDue, found
}

func (e *Engine) rank(j Job, en *entry) int {
	switch {
	case en.once:
		return 0
	case j.Kind == KWhoAmI:
		return 1
	case j.Kind == KStatus && j.App == e.focus:
		return 2
	case j.Kind == KApps:
		return 3
	case j.Kind == KBox:
		return 4
	}
	return 5
}

func (e *Engine) refill(now time.Time) {
	if now.After(e.filled) {
		e.tokens = min(Burst, e.tokens+now.Sub(e.filled).Seconds()*e.rate)
		e.filled = now
	}
}

// cycle is how long one pass over the background apps takes: their
// interval, or longer when there are more apps than the rate covers.
func (e *Engine) cycle() time.Duration {
	c := BackgroundEvery
	if n := len(e.apps); n > 0 {
		need := time.Duration(float64(n) / (e.rate * 0.8) * float64(time.Second))
		c = max(c, need)
	}
	return c
}

// Cycle is the background cycle now: a row older than StaleAfter of
// them is stale even with no read failed.
func (e *Engine) Cycle() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cycle()
}

// Started gives the in-flight read the cancel function of its context,
// so Drop can end it.
func (e *Engine) Started(cancel context.CancelFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancel = cancel
}

// Outcome is what a read's error means to the engine.
type Outcome int

// The outcomes.
const (
	Answered Outcome = iota
	// Cancelled: the person left; nothing is counted.
	Cancelled
	// NoAnswer: the box did not answer at all, or something before it
	// answered for it (a gateway's 5xx): every read backs off.
	NoAnswer
	// Refused: this read alone failed (a 403, a 404, a body not
	// motel's); the rest carry on.
	Refused
	// Slowed: a 429.
	Slowed
	// Stopped: a 401 that stood, a token not signed, a certificate.
	Stopped
)

// Classify says what err means.
func Classify(err error) (Outcome, Stop) {
	var f *client.Failure
	switch {
	case err == nil:
		return Answered, Running
	case errors.Is(err, context.Canceled):
		return Cancelled, Running
	case !errors.As(err, &f):
		return NoAnswer, Running
	case f.Exit == client.ExitRefused:
		return Stopped, SignedOut
	case f.Exit == client.ExitCertificate:
		return Stopped, BadCertificate
	case f.Exit == client.ExitUsage:
		return Stopped, NotSigned
	case f.Status == 429: // Too Many Requests
		return Slowed, Running
	case f.Exit == client.ExitUnknown && (f.Status == 0 || f.Status >= 500):
		return NoAnswer, Running
	}
	return Refused, Running
}

// Done records how the read taken last ended, at now.
func (e *Engine) Done(j Job, now time.Time, err error) Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inflight, e.cancel = nil, nil
	out, stop := Classify(err)
	en := e.jobs[j]
	if en == nil {
		en = &entry{}
	}
	switch out {
	case Answered:
		en.misses = 0
		if e.misses > 0 {
			// Back after no answer: whoami again, since the box may have
			// been upgraded while it did not answer.
			e.misses, e.recovering = 0, true
			if j.Kind != KWhoAmI {
				e.jobs[Job{Kind: KWhoAmI}] = &entry{due: now, once: true}
			}
		}
	case Cancelled:
		delete(e.jobs, j)
		e.poke()
		return out
	case NoAnswer:
		en.misses++
		e.misses++
		e.retryAt = now.Add(min(maxBackoff, time.Second<<min(e.misses, 6)))
	case Refused:
		// A 403 or a 404 is not asked again until the person does
		// (ctrl-r): a grant removed, an app gone. A body that is not
		// motel's is asked again in its turn.
		var f *client.Failure
		en.misses++
		en.held = en.once || errors.As(err, &f) && (f.Exit == client.ExitForbidden || f.Exit == client.ExitNotFound)
	case Slowed:
		var f *client.Failure
		_ = errors.As(err, &f)
		e.slowUntil = now.Add(max(f.RetryAfter, time.Second))
		// The rest of the session runs slower, so it is not asked again.
		e.rate = max(0.5, e.rate*0.75)
		en.due = e.slowUntil
		e.poke()
		return out
	case Stopped:
		e.stopped = stop
		if j.Kind == KWhoAmI {
			delete(e.jobs, j)
		}
		return out
	}
	switch {
	case en.once:
		if out == Answered || en.held {
			en.held = true
		}
		if out == Answered && j.Kind == KWhoAmI {
			delete(e.jobs, j)
		}
	case j.Kind == KStatus && j.App == e.focus:
		en.due = now.Add(FocusEvery)
	case j.Kind == KStatus:
		c := e.cycle()
		next := en.slot
		if next.IsZero() {
			next = now
		}
		for !next.After(now) {
			next = next.Add(c)
		}
		en.slot, en.due = next, next
	case j.Kind == KApps:
		en.due = now.Add(AppsEvery)
	case j.Kind == KBox:
		en.due = now.Add(BoxEvery)
	}
	e.poke()
	return out
}

// Health is what the title bar says of the reads.
type Health struct {
	Stop    Stop
	Paused  bool
	Misses  int
	RetryIn time.Duration
	SlowIn  time.Duration
	// Back is set once, on the first answer after a run of none.
	Back bool
}

// Health is the engine's state at now.
func (e *Engine) Health(now time.Time) Health {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := Health{Stop: e.stopped, Paused: now.Sub(e.touched) >= Idle, Misses: e.misses, Back: e.recovering}
	e.recovering = false
	if e.misses > 0 && e.retryAt.After(now) {
		h.RetryIn = e.retryAt.Sub(now)
	}
	if e.slowUntil.After(now) {
		h.SlowIn = e.slowUntil.Sub(now)
	}
	return h
}

// Misses is how many reads of j in a row failed.
func (e *Engine) Misses(j Job) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if en, ok := e.jobs[j]; ok {
		return en.misses
	}
	return 0
}

// Woken is signalled when something changed what Take would say.
func (e *Engine) Woken() <-chan struct{} { return e.wake }

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Clock is time for the driver: the real one, or a test's.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Run drives the engine until ctx ends: take a read, make it with do,
// record how it ended. do returns the read's error; it applies the
// answer itself (on the UI's goroutine).
func (e *Engine) Run(ctx context.Context, clock Clock, do func(ctx context.Context, j Job) error) {
	for ctx.Err() == nil {
		j, ok, wait := e.Take(clock.Now())
		if !ok {
			var timer <-chan time.Time
			if wait > 0 {
				timer = clock.After(wait)
			}
			select {
			case <-ctx.Done():
			case <-e.wake:
			case <-timer:
			}
			continue
		}
		jctx, cancel := context.WithCancel(ctx)
		e.Started(cancel)
		err := do(jctx, j)
		cancel()
		e.Done(j, clock.Now(), err)
	}
}
