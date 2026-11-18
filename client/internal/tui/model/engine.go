package model

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
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

// The box's limit for a person (liveswap/access's limiter): 2 requests
// a second, with a burst of 60.
const (
	BoxRate  = 2.0
	BoxBurst = 60
)

// The cadence. The engine keeps well inside the box's limit: its own
// bucket refills at Rate and holds Burst, half the box's, so the first
// paint of 30 apps is quick and a person's other `motel` command, or
// the client's one retry of a 401, still has room.
const (
	Rate  = 1.5
	Burst = BoxBurst / 2
	// FocusEvery is the open app's refresh: one read per 5 s.
	FocusEvery = 5 * time.Second
	// BackgroundEvery is the shortest refresh of every other app; with
	// more apps than the rest of the budget reads in it, the interval
	// grows with their number (background).
	BackgroundEvery = 30 * time.Second
	AppsEvery       = 60 * time.Second
	BoxEvery        = 30 * time.Second
	// MeEvery is who am I's refresh while its screen is open: a slow one,
	// since a person's grants change by a push, and the open app's 5 s
	// share is not spent while no app is open.
	MeEvery = 60 * time.Second
	// MeAlways is whoami's refresh otherwise, so a grant removed in a
	// push is seen in the title bar; a 403 asks it sooner, at most once
	// in MeEvery.
	MeAlways = 5 * time.Minute
	// Idle pauses polling when no key was pressed for this long, so an
	// unattended TUI neither asks for touches nor spends the rate.
	Idle = 10 * time.Minute
	// StaleAfter missed reads make a value stale.
	StaleAfter = 3
	// NoRetryAfter is the wait for a 429 that names none: 30 s refills
	// the box's whole burst of 60 at 2 a second, so the next read
	// cannot be refused for a burst this session spent.
	NoRetryAfter = time.Duration(BoxBurst/BoxRate) * time.Second
	maxBackoff   = 60 * time.Second
	// ListAgainAfter is the least time between two reads of the list:
	// ctrl-r held on the apps screen asks for it at each key repeat, and
	// the statuses must still have their turn.
	ListAgainAfter = 5 * time.Second
	// RecordAgainAfter is the least time between two reads of a record:
	// enter and esc held, or p and n, open it at each key repeat, and a
	// held ctrl-r asks it at each one.
	RecordAgainAfter = 5 * time.Second
	// RetryEvery is the least time between two r: each may be a touch
	// of a hardware key, and a signature that fails fast must not be
	// asked for as fast as a key repeats.
	RetryEvery = 5 * time.Second
	// share is the part of what is left of the rate after the open app
	// and the list that the other apps are given: the rest is room for
	// a record the person opens and for whoami.
	share = 0.85
)

// Engine decides what to read next and when. It is the only caller of
// the Client: one read at a time, never faster than its bucket, a 429
// waited out for every read, a 401, an unsigned token or an unverified
// certificate stopping every read until the person asks again (r). It
// keeps no box value; the Session does. Its methods may be called from
// any goroutine.
type Engine struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	filled time.Time
	jobs   map[Job]*entry
	apps   []string
	focus  string
	// boxOpen is the box screen open: GET / is read only then; meOpen,
	// who am I's, whose whoami is then read every MeEvery.
	boxOpen, meOpen bool
	// meAnswered is when whoami last answered; me403 when a 403 last
	// asked it again.
	meAnswered, me403 time.Time
	// misses counts reads in a row that got no answer from the box at
	// all; retryAt is when the next is tried.
	misses     int
	retryAt    time.Time
	slowUntil  time.Time
	stopped    Stop
	touched    time.Time
	inflight   *Job
	taken      *entry // the entry the read in flight was taken from
	cancel     context.CancelFunc
	recovering bool
	// lastFocus is when the open app was last read: focus reads, the
	// ones opening an app causes among them, come out of its share.
	lastFocus time.Time
	// lastList is when the list was last read: not again within
	// ListAgainAfter, whatever asks.
	lastList  time.Time
	lastRetry time.Time
	ticked    time.Time
	// latest is the latest time a caller gave: one before it is the
	// wall clock set back (seen).
	latest time.Time
	wake   chan struct{}
}

type entry struct {
	due time.Time
	// phase is a background app's place in its cycle, so a pass is
	// spread over it rather than every app coming due at once.
	phase  time.Duration
	misses int
	// held is a read not asked again until the person asks (a 403, a
	// 404, a record read, whatever its read ended in); failed says its
	// last read failed, so asking again (ctrl-r, opening it again)
	// reads it.
	held, failed bool
	once         bool
	// slowed is the read a 429 answered: the first after the wait.
	slowed bool
	// woke is a row not re-read since the machine slept: stale.
	woke bool
	// every is the interval the read after the last answer was due by,
	// against which the row's age is judged; answered is when that
	// answer came.
	every    time.Duration
	answered time.Time
	// asked is when a record's read was last taken.
	asked time.Time
}

// Stop is why every read stopped, and zero while they run.
type Stop int

// The stops.
const (
	Running Stop = iota
	SignedOut
	NotSigned
	BadCertificate
	// NotThisKey: whoami answered about another key or box.
	NotThisKey
)

// NewEngine starts at now, with whoami due at once, and then the apps
// list.
func NewEngine(now time.Time) *Engine {
	e := &Engine{rate: Rate, tokens: Burst, filled: now, touched: now, latest: now, jobs: map[Job]*entry{}, wake: make(chan struct{}, 1)}
	e.askMe(now)
	e.jobs[Job{Kind: KApps}] = &entry{due: now}
	return e
}

// background is each other app's interval: BackgroundEvery, or longer
// when the rate left over after the open app and the list cannot read
// them all in it.
func (e *Engine) background() time.Duration {
	left := e.rate - 1/AppsEvery.Seconds()
	if e.focus != "" {
		left -= 1 / FocusEvery.Seconds()
	}
	n := len(e.apps)
	if e.focus != "" && slices.Contains(e.apps, e.focus) {
		n--
	}
	if n <= 0 || left <= 0 {
		return BackgroundEvery
	}
	need := time.Duration(float64(n) / (left * share) * float64(time.Second))
	return max(BackgroundEvery, need)
}

// Interval is how often app is read now: the open app every FocusEvery,
// every other one as background says. A row is stale only against its
// own interval.
func (e *Engine) Interval(app string) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if app == e.focus {
		return FocusEvery
	}
	return e.background()
}

// SetApps is the box's list: a new app's status comes due now, and each
// is given its place in the cycle.
func (e *Engine) SetApps(names []string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	e.apps = slices.Clone(names)
	keep := map[Job]bool{}
	every := e.background()
	for i, n := range names {
		j := Job{Kind: KStatus, App: n}
		keep[j] = true
		en, ok := e.jobs[j]
		if !ok {
			en = &entry{due: now}
			e.jobs[j] = en
		}
		en.phase = time.Duration(i) * every / time.Duration(len(names))
	}
	for j := range e.jobs {
		if j.Kind == KStatus && !keep[j] && j.App != e.focus {
			delete(e.jobs, j)
			e.dropLocked(j)
		}
	}
	e.poke()
}

// Focus is the app whose screen is open, read every FocusEvery; ""
// when none is. The app left goes back to its place in the cycle.
func (e *Engine) Focus(app string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	if app == e.focus {
		return
	}
	old := e.focus
	e.focus = app
	if en, ok := e.jobs[Job{Kind: KStatus, App: old}]; ok && old != "" {
		// Closed: back to its place in the cycle, and judged by the
		// background interval from now, not the open app's.
		en.due, en.every = e.nextSlot(en, now), e.background()
	}
	if app != "" {
		// Read at once only when its last answer is older than the open
		// app's interval; and never sooner than the open app's share
		// allows (next).
		j := Job{Kind: KStatus, App: app}
		if en, ok := e.jobs[j]; !ok {
			e.jobs[j] = &entry{due: now}
		} else if en.answered.IsZero() || now.Sub(en.answered) >= FocusEvery {
			en.due = now
		} else {
			en.due = en.answered.Add(FocusEvery)
		}
	}
	e.poke()
}

// nextSlot is a background app's next read after now: its phase, plus
// whole intervals from the start of time, so every app keeps its place.
func (e *Engine) nextSlot(en *entry, now time.Time) time.Time {
	every := e.background()
	base := now.Truncate(every).Add(en.phase)
	for !base.After(now) {
		base = base.Add(every)
	}
	return base
}

// BoxOpen says whether the box screen is open: GET / is read only then.
func (e *Engine) BoxOpen(open bool, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	e.boxOpen = open
	if _, ok := e.jobs[Job{Kind: KBox}]; open && !ok {
		e.jobs[Job{Kind: KBox}] = &entry{due: now}
	}
	e.poke()
}

// MeOpen says whether who am I's screen is open. whoami is always
// read, every MeAlways; while who am I is open, every MeEvery, and at
// once on opening it if its answer is older than that.
func (e *Engine) MeOpen(open bool, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	if open == e.meOpen {
		return
	}
	e.meOpen = open
	en := e.me()
	switch {
	case open && (e.meAnswered.IsZero() || now.Sub(e.meAnswered) >= MeEvery):
		en.due = now
	case !e.meAnswered.IsZero():
		en.due = e.meAnswered.Add(e.meEvery())
	}
	e.poke()
}

// me is whoami's entry, which is always there.
func (e *Engine) me() *entry {
	j := Job{Kind: KWhoAmI}
	en, ok := e.jobs[j]
	if !ok {
		en = &entry{}
		e.jobs[j] = en
	}
	return en
}

// askMe makes whoami due now.
func (e *Engine) askMe(now time.Time) {
	en := e.me()
	en.due, en.held, en.failed = now, false, false
}

func (e *Engine) meEvery() time.Duration {
	if e.meOpen {
		return MeEvery
	}
	return MeAlways
}

// Once is a deploy's record opened: read now, and read again each time
// it is opened, since a rollback or a redeploy rewrites a version's
// record on the box. Not while a read of it is due or in flight (one
// left and cancelled is not), and not again within RecordAgainAfter of
// the last asked.
func (e *Engine) Once(j Job, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	en, ok := e.jobs[j]
	switch {
	case !ok:
		e.jobs[j] = &entry{due: now, once: true}
	case !en.held || now.Sub(en.asked) < RecordAgainAfter:
		return // due, or in flight (the same entry, which a read in flight is for), or asked just now
	default:
		en.held, en.due = false, now
	}
	e.poke()
}

// Drop is a record left: a read of it not yet answered is not wanted,
// and is cancelled when it is in flight. A record never read is
// forgotten; one read before is held as its last read left it.
func (e *Engine) Drop(j Job) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if en, ok := e.jobs[j]; ok && en.once && !en.held {
		if en.answered.IsZero() && !en.failed {
			delete(e.jobs, j)
		} else {
			en.held = true
		}
	}
	e.dropLocked(j)
}

func (e *Engine) dropLocked(j Job) {
	if e.inflight != nil && *e.inflight == j && e.cancel != nil {
		e.cancel()
	}
}

// Again is ctrl-r: the reads of what the screen shows, and nothing
// else, come due now, a held one too (a 403 is asked again only so),
// and a record. A read already due keeps its time, so a held ctrl-r (a
// press at each key repeat) never puts a read that waits behind one
// just asked; the list is held to ListAgainAfter, a record to
// RecordAgainAfter, the open app to FocusEvery (next). A record's read
// in flight is the one asked for.
func (e *Engine) Again(now time.Time, shown ...Job) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	for _, j := range shown {
		en, ok := e.jobs[j]
		if !ok {
			en = &entry{once: j.Kind == KDeploy, due: now}
			e.jobs[j] = en
		}
		en.held, en.failed = false, false
		if en.due.After(now) {
			en.due = now
		}
	}
	e.poke()
}

// Retry is r after a stop: one whoami, as a new start, then the reads
// as they were. It is the only way out of a stop, so a refused key is
// asked again once a press, and no more than once in RetryEvery: each
// may sign, and a signature may be a touch. It says whether it asks.
func (e *Engine) Retry(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	if e.stopped == Running || e.inflight != nil || !e.lastRetry.IsZero() && now.Sub(e.lastRetry) < RetryEvery {
		return false
	}
	e.lastRetry = now
	e.stopped = Running
	e.misses = 0
	e.askMe(now)
	e.poke()
	return true
}

// RetryReady says whether r would ask now: stopped, and RetryEvery
// since the last. The screen forgets the token first, then retries, so
// no read can go out with the refused token between the two.
func (e *Engine) RetryReady(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	return e.stopped != Running && e.inflight == nil && (e.lastRetry.IsZero() || now.Sub(e.lastRetry) >= RetryEvery)
}

// Touch is a key pressed: polling resumes when it had paused.
func (e *Engine) Touch(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	e.touched = now
	e.poke()
}

// Take returns the read to make now, or false and how long to wait
// before asking again (zero: until woken).
func (e *Engine) Take(now time.Time) (Job, bool, time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
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
	e.inflight, e.taken = &j, e.jobs[j]
	if e.taken.once {
		e.taken.asked = now
	}
	if j.Kind == KStatus && j.App == e.focus {
		e.lastFocus = now
	}
	if j.Kind == KApps {
		e.lastList = now
	}
	return j, true, 0
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// next is the job to run first of those due by at: a one-off the
// person asked for, then whoami, the open app, the list, and the most
// overdue of the rest. With none due, it is the one due soonest.
func (e *Engine) next(at time.Time) (Job, time.Time, bool) {
	var best Job
	var bestDue time.Time
	bestRank, found := 99, false
	for j, en := range e.jobs {
		if en.held || j.Kind == KBox && !e.boxOpen {
			continue
		}
		r := e.rank(j, en)
		due := en.due
		if j.Kind == KStatus && j.App == e.focus && !e.lastFocus.IsZero() {
			due = maxTime(due, e.lastFocus.Add(FocusEvery))
		}
		if j.Kind == KApps && !e.lastList.IsZero() {
			due = maxTime(due, e.lastList.Add(ListAgainAfter))
		}
		if en.once && !en.asked.IsZero() {
			due = maxTime(due, en.asked.Add(RecordAgainAfter))
		}
		if due.After(at) {
			// Not yet due: after every job that is, and among the rest the
			// soonest first, whatever its kind, so the wait it gives is
			// the wait for the next read of any kind.
			r = 100
		}
		if !found || r < bestRank || r == bestRank && due.Before(bestDue) || r == bestRank && due.Equal(bestDue) && j.App < best.App {
			best, bestDue, bestRank, found = j, due, r, true
		}
	}
	return best, bestDue, found
}

func (e *Engine) rank(j Job, en *entry) int {
	switch {
	case en.slowed:
		return -3
	case j.Kind == KWhoAmI:
		// Before any one-off: after a stop it is r's new start, which no
		// record the person had open may go ahead of.
		return -2
	case en.once:
		return 0
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
	// NoAnswer: the box did not answer at all: every read backs off.
	NoAnswer
	// Refused: this read alone failed; the rest carry on.
	Refused
	// Slowed: a 429.
	Slowed
	// Stopped: every read stops until r.
	Stopped
)

// Classify says what err means.
func Classify(err error) (Outcome, Stop) {
	var f *Failure
	switch {
	case err == nil:
		return Answered, Running
	case errors.Is(err, context.Canceled):
		return Cancelled, Running
	case !errors.As(err, &f):
		return NoAnswer, Running
	}
	switch f.Kind {
	case FailSignedOut:
		return Stopped, SignedOut
	case FailNotSigned:
		return Stopped, NotSigned
	case FailCertificate:
		return Stopped, BadCertificate
	case FailAboutAnother:
		return Stopped, NotThisKey
	case FailSlowed:
		return Slowed, Running
	case FailNoAnswer:
		return NoAnswer, Running
	}
	return Refused, Running
}

// Done records how the read taken last ended, at now.
func (e *Engine) Done(j Job, now time.Time, err error) Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	taken := e.taken
	e.inflight, e.taken, e.cancel = nil, nil, nil
	if taken != nil {
		taken.slowed = false
	}
	out, stop := Classify(err)
	en := e.jobs[j]
	if en == nil || en != taken {
		// Dropped while in flight, and perhaps asked for again since:
		// what is in the table now is not what this read was for.
		e.poke()
		return out
	}
	if fromBox := out == Answered || out == Slowed || out == Refused && !notSent(err); fromBox && e.misses > 0 {
		// Back after no answer, whatever the box said: whoami again,
		// since the box may have been upgraded while it did not answer.
		// A failure the client made without sending anything says
		// nothing of the box, and a run of no answer goes on.
		e.misses, e.recovering = 0, true
		if j.Kind != KWhoAmI {
			e.askMe(now)
		}
	}
	switch out {
	case Answered:
		en.misses, en.failed, en.answered, en.woke = 0, false, now, false
		if j.Kind == KWhoAmI {
			e.meAnswered = now
		}
	case Cancelled:
		// A record read before stays as Drop left it, or as opening it
		// again since asks.
		if en.answered.IsZero() && !en.failed {
			delete(e.jobs, j)
		}
		e.poke()
		return out
	case NoAnswer:
		en.misses++
		e.misses++
		e.retryAt = now.Add(min(maxBackoff, time.Second<<min(e.misses, 6)))
		// A record is held as a refused one is: a 5xx is no answer too,
		// and a record asked again after each backoff, ahead of every
		// other read, would be the only read made while its screen is
		// open. Opening it again or ctrl-r asks it again.
		if en.once {
			en.failed = true
		}
	case Refused:
		en.misses++
		en.failed = true
		en.held = en.once || Forbidden(err) || NotFound(err)
		// A 403 may be a grant removed in a push: whoami again, so the
		// title bar says what this key now holds; at most once in
		// MeEvery.
		if Forbidden(err) && j.Kind != KWhoAmI && (e.meAnswered.IsZero() || now.Sub(e.meAnswered) >= MeEvery) && now.Sub(e.me403) >= MeEvery {
			e.me403 = now
			e.askMe(now)
		}
	case Slowed:
		var f *Failure
		_ = errors.As(err, &f)
		wait := f.RetryAfter
		if wait <= 0 {
			wait = NoRetryAfter
		}
		// The wait stands in for the backoff of a run of no answer, which
		// the 429, an answer, ended.
		e.slowUntil = now.Add(wait)
		// The bucket is spent and does not fill during the wait, so the
		// reads resume at the rate, not in a burst.
		e.tokens, e.filled = 0, e.slowUntil
		// The rest of the session runs slower, so it is not asked again.
		e.rate = max(0.5, e.rate*0.75)
		// First of all once the wait ends, whatever else came due in it.
		en.due, en.slowed = time.Time{}, true
		e.poke()
		return out
	case Stopped:
		e.stopped = stop
		if stop == NotThisKey {
			e.forgetBox(now)
		}
		return out
	}
	switch {
	case en.once:
		// Answered, refused or not answered: not asked again until the
		// person asks.
		en.held = true

	case j.Kind == KStatus && j.App == e.focus:
		en.due, en.every = now.Add(FocusEvery), FocusEvery
	case j.Kind == KStatus:
		en.due, en.every = e.nextSlot(en, now), e.background()
	case j.Kind == KApps:
		en.due = now.Add(AppsEvery)
	case j.Kind == KBox:
		en.due = now.Add(BoxEvery)
	case j.Kind == KWhoAmI:
		en.due = now.Add(e.meEvery())
	}
	e.poke()
	return out
}

// forgetBox drops every read but whoami's after an answer about another
// key or box: the apps it listed and what was due for them. After r,
// whoami first, then the list, as at the start.
func (e *Engine) forgetBox(now time.Time) {
	e.apps, e.lastFocus, e.lastList = nil, time.Time{}, time.Time{}
	for j := range e.jobs {
		if j.Kind != KWhoAmI {
			delete(e.jobs, j)
		}
	}
	e.jobs[Job{Kind: KApps}] = &entry{due: now}
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
	e.seen(now)
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

// Row is what the apps table needs of an app's reads: misses in a
// row, whether it is held (not asked until ctrl-r), and the interval
// its age is judged by: the one its next read was due by when its last
// answer came, so an app just opened is not judged by the new one.
func (e *Engine) Row(app string) (misses int, held bool, every time.Duration, woke bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.jobs[Job{Kind: KStatus, App: app}]
	switch {
	case !ok:
		return 0, false, e.background(), false
	case en.every == 0:
		every = e.background()
		if app == e.focus {
			every = FocusEvery
		}
		return en.misses, en.held, every, en.woke
	}
	return en.misses, en.held, en.every, en.woke
}

// WakeAfter is a gap between two ticks of the clock that only a
// machine asleep makes: the TUI's clock ticks every second.
const WakeAfter = 30 * time.Second

// Tick is the clock's tick, every second while the screen is up. A gap
// past WakeAfter is a laptop that slept: every row is stale until read
// again, the reads are spread over the cycle again rather than all due
// at once, whoami is asked again (the box may have changed meanwhile),
// and the bucket starts empty. now is wall-clock time: a monotonic
// reading stops while the machine sleeps. A step of the clock back is
// seen at whatever call gives the first time after it (seen); the tick
// only makes sure one comes within a second.
func (e *Engine) Tick(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen(now) {
		e.ticked = now
		return true
	}
	last := e.ticked
	e.ticked = now
	if last.IsZero() || now.Sub(last) <= WakeAfter {
		return false
	}
	e.wakeLocked(now)
	return true
}

// seen is the first thing each call given a time does with it. A time
// before the latest given is the wall clock set back: every time the
// engine keeps is moved back by the step then, before anything is
// recorded at the new time, so none is moved twice; without it no read
// would be due, and the bucket would not fill, for the length of the
// step. Run is woken, since the wait it sleeps on was given before the
// step. A step back past WakeAfter is a wake too (it says whether it
// was one): the clock may have been set back while the machine slept,
// and how old each row is is then not known.
func (e *Engine) seen(now time.Time) bool {
	if !now.Before(e.latest) {
		e.latest = now
		return false
	}
	d := now.Sub(e.latest)
	e.shift(d)
	e.latest = now
	e.poke()
	if -d > WakeAfter {
		e.wakeLocked(now)
		return true
	}
	return false
}

// wakeLocked is a wake from sleep at now (Tick).
func (e *Engine) wakeLocked(now time.Time) {
	every := e.background()
	for j, en := range e.jobs {
		if j.Kind != KStatus {
			continue
		}
		en.woke = true
		if j.App == e.focus {
			en.due = now
		} else {
			en.due = now.Add(en.phase % every)
		}
	}
	if en, ok := e.jobs[Job{Kind: KApps}]; ok {
		en.due = now
	}
	e.askMe(now)
	// Empty, and filling from now, or from the end of a 429's wait that
	// still runs, as the 429 left it.
	e.tokens, e.filled = 0, maxTime(now, e.slowUntil)
	e.poke()
}

// shift moves every time the engine keeps by d: due times, answers,
// waits, the bucket's fill time and the last tick.
func (e *Engine) shift(d time.Duration) {
	move := func(ts ...*time.Time) {
		for _, t := range ts {
			if !t.IsZero() {
				*t = t.Add(d)
			}
		}
	}
	move(&e.filled, &e.meAnswered, &e.me403, &e.retryAt, &e.slowUntil, &e.touched, &e.lastFocus, &e.lastList, &e.lastRetry, &e.ticked)
	for _, en := range e.jobs {
		move(&en.due, &en.answered, &en.asked)
	}
}

// CanAskAgain says whether ctrl-r would ask now: not while every read
// is stopped, nor while a 429's wait runs.
func (e *Engine) CanAskAgain(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	return e.stopped == Running && !e.slowUntil.After(now)
}

// SlowUntil is the end of a 429's wait, zero when none runs.
func (e *Engine) SlowUntil(now time.Time) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen(now)
	if e.slowUntil.After(now) {
		return e.slowUntil
	}
	return time.Time{}
}

// Stopped is why every read stopped, Running while they run.
func (e *Engine) Stopped() Stop {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopped
}

// RateNow is the engine's rate now: Rate, less after a 429.
func (e *Engine) RateNow() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rate
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
// record how it ended, then call after (when not nil), so that what
// after does (apply the answer, on the UI's goroutine) sees the engine
// as the read left it: a stop it caused is already a stop.
func (e *Engine) Run(ctx context.Context, clock Clock, do func(ctx context.Context, j Job) error, after func()) {
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
		if after != nil {
			after()
		}
	}
}
