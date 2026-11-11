package liveswap

import (
	"context"
	"maps"
	"slices"
)

// What the metrics store (the monitor block's metrics line) reads of
// liveswap (notify/DESIGN-metrics.md): each app's running instance, for
// the per-app series it pushes, and a hook that adds the app's numbers
// to GET /<app>. liveswap knows nothing of the store; it reports through
// these, as it reports its lifecycle through Notifier.

// AppSample is one app as it runs now.
type AppSample struct {
	App string
	// Release is the current release, running or not (Running says
	// which): after a crash it names the release that crashed. "" when
	// the app has no current release.
	Release string
	// Running is whether the instance is alive, as status reports it.
	Running bool
	// Cgroup is the running unit's cgroup path under /sys/fs/cgroup, as
	// the manager reports it; "" when unknown. Nothing of an app's or a
	// request's reaches it.
	Cgroup string
	// MemoryMax is the unit's MemoryMax= as the manager applied it; 0
	// when the unit has none, or when it is not known to be applied.
	MemoryMax uint64
	// Requests is what observe counted, the current release's
	// first and the one before's after it; empty when no observer has
	// seen a response for the app.
	Requests []ReleaseRequests
}

// AppSamples is every app this config runs, sorted by name.
func (a *App) AppSamples() []AppSample {
	out := make([]AppSample, 0, len(a.managed))
	for _, name := range slices.Sorted(maps.Keys(a.managed)) {
		out = append(out, a.managed[name].sample())
	}
	return out
}

func (ma *managedApp) sample() AppSample {
	c := ma.snapshot()
	ma.mu.Lock()
	defer ma.mu.Unlock()
	s := AppSample{App: ma.name, Requests: ma.requests.samples()}
	if ma.current == nil {
		return s
	}
	s.Release = ma.current.version
	hs := ma.current.handle.state()
	s.Running = c.runner != nil && c.runner.Alive(ma.current.handle)
	s.Cgroup = hs.Cgroup
	s.MemoryMax = sampleMemoryMax(hs)
	return s
}

// sampleMemoryMax is the unit's MemoryMax= when it is known to be
// applied and the kernel enforces it: a value the kernel ignores is not
// a cap, as status says (capsForStatus).
func sampleMemoryMax(hs handleState) uint64 {
	if hs.CapsUnknown || slices.Contains(hs.CapsUnenforced, "memory_max") {
		return 0
	}
	return hs.Caps.memoryMax
}

// MetricsSource answers the `metrics` object of GET /<app>, over the
// window `?range=` named (one of MetricsRanges). It must answer within
// its own short budget whatever the store does, and is called with the
// request's context; what it returns is marshalled into the status and
// passes the app's response filter with the rest.
type MetricsSource interface {
	AppMetrics(ctx context.Context, app, window string) any
}

// MetricsRanges are the windows `?range=` takes, the first the default:
// a fixed set, so that a token cannot ask the store for any span it
// likes.
var MetricsRanges = []string{"1h", "24h"}

// SetMetricsSource installs the metrics store's source on this config.
// Called from the monitor app's Provision, after liveswap's own, so a
// config whose monitor block has no metrics line has no `metrics` in its
// status.
func (a *App) SetMetricsSource(s MetricsSource) { a.metricsSource = s }
