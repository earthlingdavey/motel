package model_test

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// ownBucket fails when calls ever go past the TUI's own promise,
// written here as its numbers: 1.5 a second with a burst of 30, the
// bucket full at the start.
func ownBucket(t *testing.T, calls []call) {
	t.Helper()
	tokens, last := 30.0, t0
	for _, c := range calls {
		tokens = min(30, tokens+c.at.Sub(last).Seconds()*1.5)
		last = c.at
		tokens--
		if tokens < -1e-3 { // a token's rounding, not a read
			t.Fatalf("read %v at +%s goes past 1.5 a second with a burst of 30", c.j, c.at.Sub(t0))
		}
	}
}

// TestBurstAfterAQuietSpell: however long the reads were few, the
// bucket holds 30 at most. Three apps are read for ten minutes, far
// below the rate; then the box lists 200, every new one due at once,
// and the reads made then still keep to 1.5 a second with a burst of
// 30.
func TestBurstAfterAQuietSpell(t *testing.T) {
	e := model.NewEngine(t0)
	calls := sim(e, names(3), t0, 10*time.Minute, 10*time.Millisecond, ok)
	more := t0.Add(10 * time.Minute)
	e.SetApps(names(200), more)
	calls = append(calls, sim(e, names(200), more, time.Minute, time.Millisecond, ok)...)
	ownBucket(t, calls)
	first := 0
	for _, c := range calls {
		if !c.at.Before(more) && c.at.Before(more.Add(time.Second)) {
			first++
		}
	}
	if first < 25 {
		t.Fatalf("%d reads in the second the list grew: the test reads no burst", first)
	}
}
