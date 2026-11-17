package access

import (
	"math"
	"sync"
	"time"
)

// The rate per person: a token bucket per name, refilled at RatePerSecond
// up to RateBurst. A valid status read costs about a millisecond of the
// box's CPU, so a person at the bound costs a fraction of a core, and a
// client refreshing a screen every ten seconds uses a twentieth of it.
// Only a verified token reaches it, so it holds declared names alone:
// config-sized, never traffic-sized. It counts a person's accepted
// requests; the failure budgets (deploytrust's Limiter) count refusals
// by address, and the two never touch.
const (
	RatePerSecond = 2
	RateBurst     = 60
	// rateLogEvery is how often a throttled person's line is written.
	rateLogEvery = time.Minute
	// rateNames holds the table across loads that add and remove
	// names: past it, every full bucket (a name idle for the 30 s a
	// bucket takes to refill) is dropped, which forgets nothing. It is
	// not a hard bound: a bucket in use is never dropped, since that
	// would refill a throttled person. The table passes rateNames only
	// while more names than that have each asked in the last 30 s,
	// which takes loads replacing every one of MaxKeyLines people in
	// use, twice in that time.
	rateNames = 2 * MaxKeyLines
)

// Rate is the buckets of every person, by name, for the process: a
// reload must not refill a throttled person, so it is carried across
// loads (SharedRate), as the failure budgets are.
type Rate struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time // when tokens was last brought up to date
	logged time.Time // when the throttled line was last written
}

var sharedRate = NewRate(time.Now)

// SharedRate is the process's Rate, which both webhooks take people's
// requests from. Tests make their own with NewRate.
func SharedRate() *Rate { return sharedRate }

// NewRate is a Rate on the clock now.
func NewRate(now func() time.Time) *Rate {
	return &Rate{now: now, buckets: map[string]*bucket{}}
}

// Take spends one request of name's. When none is left, ok is false and
// wait is how long until one is; log is true at most once a minute for
// a name while it is throttled, for the journal's line.
func (r *Rate) Take(name string) (ok bool, wait time.Duration, log bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b := r.buckets[name]
	if b == nil {
		if len(r.buckets) >= rateNames {
			r.dropFullLocked(now)
		}
		b = &bucket{tokens: RateBurst, at: now}
		r.buckets[name] = b
	}
	b.refill(now)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0, false
	}
	wait = time.Duration(math.Ceil((1 - b.tokens) / RatePerSecond * float64(time.Second)))
	if now.Sub(b.logged) >= rateLogEvery {
		b.logged, log = now, true
	}
	return false, wait, log
}

func (b *bucket) refill(now time.Time) {
	if d := now.Sub(b.at); d > 0 {
		b.tokens = math.Min(RateBurst, b.tokens+d.Seconds()*RatePerSecond)
	}
	b.at = now
}

func (r *Rate) dropFullLocked(now time.Time) {
	for name, b := range r.buckets {
		if b.refill(now); b.tokens >= RateBurst {
			delete(r.buckets, name)
		}
	}
}
