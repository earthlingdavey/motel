package wire

import "time"

// SeriesRanges are the windows GET /_motel/apps/<app>/series?range=
// takes, the first the default: a fixed set, so that a token cannot
// ask the store for any span it likes. Any other is refused (422), on
// a box with no store too. Experimental, as every route under /_motel/.
var SeriesRanges = []string{"1h", "24h", "7d", "14d"}

// Series is GET /_motel/apps/<app>/series's body: the app's numbers
// over one of SeriesRanges, as fixed panels of points at the window's
// fixed step (notify/DESIGN-metrics.md, "An app's series"). Nothing in
// it is the client's to choose but the window: no panel, query or
// step. Experimental: its fields may change until a release's notes
// say otherwise.
type Series struct {
	// Store is StoreOn (the store answered: Panels is what it holds of
	// the app over the window, maybe nothing), StoreOff (the config's
	// monitor block has no metrics line, or there is none: the box keeps
	// no numbers) or StoreUnanswered (the store did not answer within
	// the box's budget: Panels is empty, and says nothing). A client
	// reads a word it does not know as not knowing.
	Store  string `json:"store" text:"moteld"`
	Window string `json:"window" text:"moteld"`
	// StepSeconds is the window's step, the time between two points of
	// a panel; absent unless the store answered.
	StepSeconds int `json:"step_seconds,omitempty"`
	// End is the window's end, the time the store was asked (the
	// window is the span before it); absent unless the store answered.
	End *time.Time `json:"end,omitempty"`
	// Panels are in the order of the Panel names below, each present
	// only when the store holds a point of it in the window: the
	// request panels only for an app behind observe.
	Panels []Panel `json:"panels"`
}

// Panel is one number over the window.
type Panel struct {
	// Name is one of the Panel names below, and Unit its unit.
	Name string `json:"name" text:"moteld"`
	Unit string `json:"unit" text:"moteld"`
	// Points are [unix seconds, value] pairs, oldest first, each at a
	// multiple of the step. A step with no point is a gap: the box
	// sends no pair for it, never a zero.
	Points [][2]float64 `json:"points"`
}

// The panels, in their order in a body, and each one's unit. Each
// point is of the step that ends at its time:
//
//   - memory_bytes: the highest push of the app's working set in the
//     step, in bytes (a peak between two points is not lost);
//   - cpu_cores: the CPU the app's units used in the step, every unit
//     together, a second of the step: its average cores (a unit
//     stopped or started inside it counted for its part);
//   - requests_per_second: the responses observe counted, every class
//     and release, over the step, per second;
//   - 5xx_share: the 5xx share of the responses sent (a client gone
//     first, aborted, left out) in the step; no point for a step with
//     none sent;
//   - p95_ms: the 95th percentile of the app's own responses' time to
//     headers in the step, in milliseconds, interpolated in the
//     histogram's buckets; no point for a step with none.
//
// The request panels are absent for an app no observe handler names,
// whose requests the box does not count.
const (
	PanelMemory   = "memory_bytes"
	PanelCPU      = "cpu_cores"
	PanelRequests = "requests_per_second"
	Panel5xx      = "5xx_share"
	PanelP95      = "p95_ms"
)

// The panels' units.
const (
	UnitBytes     = "bytes"
	UnitCores     = "cores"
	UnitPerSecond = "per_second"
	UnitShare     = "share"
	UnitMs        = "ms"
)
