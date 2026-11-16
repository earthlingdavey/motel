package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// The token line: expired at and before now, a duration only while it
// is within an hour, never one a far date would overflow, the time
// itself always; and no line when the answer held no time.
func TestWhoAmIExpiry(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		has     bool
		expires time.Time
		want    string // "" for no token line
	}{
		{"in a second", true, now.Add(time.Second), "token    expires 2026-11-15 10:00:01 UTC (in 1s)\n"},
		{"in half a second, which rounds to now", true, now.Add(400 * time.Millisecond), "token    expired 2026-11-15 10:00:00 UTC\n"},
		{"now", true, now, "token    expired 2026-11-15 10:00:00 UTC\n"},
		{"a second ago", true, now.Add(-time.Second), "token    expired 2026-11-15 09:59:59 UTC\n"},
		{"in an hour", true, now.Add(time.Hour), "token    expires 2026-11-15 11:00:00 UTC (in 1h0m0s)\n"},
		{"far off", true, time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), "token    expires 9999-12-31 23:59:59 UTC (more than an hour from now, longer than a token lives)\n"},
		{"year 1", true, time.Time{}, "token    expired 0001-01-01 00:00:00 UTC\n"},
		{"no time in the answer", false, now.Add(time.Minute), ""},
	} {
		var b bytes.Buffer
		if err := WriteWhoAmI(&b, WhoAmI{Name: termtext.From("dave"), HasExpiry: c.has, Expires: c.expires, Now: now}); err != nil {
			t.Fatal(err)
		}
		_, line, found := strings.Cut(b.String(), "token")
		if got := map[bool]string{true: "token" + line, false: ""}[found]; got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
