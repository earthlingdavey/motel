package client

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestNoWaitHandsBackTheWait: the TUI's client hands every 429 back at
// once, with no wait of its own and no second request, whatever the
// wait it asks (one the commands would wait out, one they would not):
// the wait the box asked, at least a second, and at most an hour, the
// client's cap, however large the number or far the date.
func TestNoWaitHandsBackTheWait(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		after string
		want  time.Duration
	}{
		{"0", time.Second},
		{"12", 12 * time.Second},
		{"60", time.Minute},
		{"61", 61 * time.Second},
		{"3600", time.Hour},
		{"3601", time.Hour},
		{"7200", time.Hour},
		{"18446744083", time.Hour},
		{"9223372036854775807", time.Hour},
		{now.Add(2 * time.Hour).Format(http.TimeFormat), time.Hour},
	} {
		b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", c.after)
			motelJSON(w, http.StatusTooManyRequests, `{"error":"slow down"}`)
		})
		b.client.NoWait = true
		b.client.Now = func() time.Time { return now }
		_, err := b.client.Get(context.Background(), "/blog", nil)
		f := failure(t, err)
		if f.Status != http.StatusTooManyRequests || f.RetryAfter != c.want {
			t.Errorf("Retry-After %q: status %d, wait %s; want %s", c.after, f.Status, f.RetryAfter, c.want)
		}
		if len(b.slept) != 0 || b.hits.Load() != 1 {
			t.Errorf("Retry-After %q: slept %v over %d requests, want none over one", c.after, b.slept, b.hits.Load())
		}
	}
}
