package wire

import "time"

// MetricsRanges are the windows GET /<app>?range= takes, the first the
// default: a fixed set, so that a token cannot ask the store for any
// span it likes. Any other is refused (422), on a box with no metrics
// too.
var MetricsRanges = []string{"1h", "24h"}

// Metrics is the status's `metrics` object: the metrics store's
// summary of the app over one of MetricsRanges (notify/DESIGN-metrics.md,
// "metrics in GET /<app>"). Every fraction is rounded (the memory
// values are bytes as pushed), so none marshals as the run of digits
// the response filter would read as a secret. A store that did not
// answer is the object with Error alone.
type Metrics struct {
	Window string `json:"window,omitempty"`
	// At is the newest point of the window's range, of memory or CPU,
	// not the time of a push; absent with no point.
	At     *time.Time     `json:"at,omitempty"`
	Memory *MemorySummary `json:"memory_bytes,omitempty"`
	// MemoryMax is the cap's last push within 75 s, whatever the
	// window; absent with none (no cap, the app stopped, or the pushes
	// failing).
	MemoryMax *float64        `json:"memory_max_bytes,omitempty"`
	CPU       *CPUSummary     `json:"cpu_cores,omitempty"`
	Requests  *RequestSummary `json:"requests,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// MemorySummary is the app's memory over the window, in bytes.
type MemorySummary struct {
	// Now is the last push's within 75 s, whatever the window; absent
	// with none (the app stopped, or the pushes failing).
	Now *float64 `json:"now,omitempty"`
	// Max is absent when the window holds no raw sample: the range's
	// first step can still carry one from just before it.
	Max *float64 `json:"max,omitempty"`
}

// CPUSummary is the app's CPU over the window, in cores.
type CPUSummary struct {
	// Avg5m is the mean of the hour's CPU points over the five minutes
	// up to now, whatever the window, each the unit that ran at its
	// minute (so it spans a deploy); absent with no push of a unit
	// within 75 s (the app stopped, or the pushes failing).
	Avg5m *float64 `json:"avg_5m,omitempty"`
	Avg   float64  `json:"avg"`
}

// RequestSummary is what observe counted over the window, every
// release together. Absent when the store holds no request series for
// the app (no observer on its route).
type RequestSummary struct {
	Total     uint64            `json:"total"`
	PerSecond float64           `json:"per_second"`
	ByClass   map[string]uint64 `json:"by_class,omitempty"`
	// Share5xx is of the responses sent: a client gone first (aborted)
	// is in the total, not here. Absent with none sent.
	Share5xx *float64 `json:"5xx_share,omitempty"`
	// P50 and P95 are the app's own responses' time to headers (cache
	// hits left out), interpolated in the histogram's buckets; absent
	// with none.
	P50 *float64 `json:"p50_ms,omitempty"`
	P95 *float64 `json:"p95_ms,omitempty"`
	// CacheHitShare is hits (stale ones too) of the responses a cache
	// spoke for; absent when none did.
	CacheHitShare *float64 `json:"cache_hit_share,omitempty"`
}
