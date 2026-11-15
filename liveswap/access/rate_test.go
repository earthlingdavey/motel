package access

import (
	"fmt"
	"testing"
	"time"
)

// TestRateBounds: a burst of RateBurst is taken at once and the next is
// refused, with the wait until one is back; RatePerSecond come back each
// second, and no more than the burst however long a name is idle; each
// name has its own bucket; the throttled line is said once a minute.
func TestRateBounds(t *testing.T) {
	now := time.Unix(1794657600, 0)
	r := NewRate(func() time.Time { return now })
	for i := range RateBurst {
		if ok, _, _ := r.Take("alice"); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, wait, log := r.Take("alice")
	if ok || wait != time.Second/RatePerSecond || !log {
		t.Fatalf("past the burst: ok %v, wait %v, log %v", ok, wait, log)
	}
	if ok, _, log := r.Take("alice"); ok || log {
		t.Errorf("again at once: ok %v, log %v; want refused, no second line", ok, log)
	}
	if ok, _, _ := r.Take("bob"); !ok {
		t.Error("bob is throttled for alice's requests")
	}
	now = now.Add(time.Second)
	for i := range RatePerSecond {
		if ok, _, _ := r.Take("alice"); !ok {
			t.Fatalf("a second on, request %d refused", i+1)
		}
	}
	if ok, _, _ := r.Take("alice"); ok {
		t.Errorf("a second on, request %d taken", RatePerSecond+1)
	}
	now = now.Add(time.Hour)
	for i := range RateBurst {
		if ok, _, _ := r.Take("alice"); !ok {
			t.Fatalf("an hour on, request %d refused", i+1)
		}
	}
	if ok, _, log := r.Take("alice"); ok || !log {
		t.Errorf("an hour on, past the burst: ok %v, log %v", ok, log)
	}
}

// TestRateTableBounded: names come only from verified tokens, but loads
// add and remove them; past the bound, the idle (full) buckets are
// dropped and a throttled one is kept.
func TestRateTableBounded(t *testing.T) {
	now := time.Unix(1794657600, 0)
	r := NewRate(func() time.Time { return now })
	for range RateBurst + 1 {
		r.Take("busy")
	}
	for i := range rateNames {
		r.Take(fmt.Sprintf("n%d", i))
	}
	// A second on, every name that took one request is full again, and
	// busy, which spent its burst, is not.
	now = now.Add(time.Second)
	r.Take("new")
	if _, kept := r.buckets["busy"]; !kept || len(r.buckets) != 2 {
		t.Errorf("%d buckets, busy kept %v; want busy and new", len(r.buckets), kept)
	}
	if a, b := SharedRate(), SharedRate(); a != b || a == nil {
		t.Error("the shared rate is not one for the process")
	}
}
