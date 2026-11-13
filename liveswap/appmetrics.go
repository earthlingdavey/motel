package liveswap

import (
	"context"
	"maps"
	"slices"

	"github.com/smallhoursorg/motel/liveswap/wire"
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
	// Observed is whether an observe handler of this config names the
	// app: its current release's counts are then its numbers even
	// before its first response, where an app with none has no numbers.
	// This config's, not every loaded one's (managedApp.observers): a
	// reload that takes the line out still has the old config's handler
	// until the old config's requests in flight end and its Cleanup
	// runs, and the counts its pushes read meanwhile would grow by
	// those alone, read as an app no request reaches.
	Observed bool
}

// AppSamples is every app this config runs, sorted by name.
func (a *App) AppSamples() []AppSample {
	out := make([]AppSample, 0, len(a.managed))
	for _, name := range slices.Sorted(maps.Keys(a.managed)) {
		_, observed := a.observed.Load(name)
		out = append(out, a.managed[name].sample(observed))
	}
	return out
}

func (ma *managedApp) sample(observed bool) AppSample {
	c := ma.snapshot()
	ma.mu.Lock()
	defer ma.mu.Unlock()
	s := AppSample{App: ma.name, Requests: ma.requests.samples(), Observed: observed}
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
// window `?range=` named (one of wire.MetricsRanges). It must answer
// within its own short budget whatever the store does, and is called
// with the request's context; what it returns is marshalled into the
// status and passes the app's response filter with the rest. nil
// leaves the object out.
type MetricsSource interface {
	AppMetrics(ctx context.Context, app, window string) *wire.Metrics
}

// SetMetricsSource installs the metrics store's source on this config.
// Called from the monitor app's Provision, after liveswap's own, so a
// config whose monitor block has no metrics line has no `metrics` in its
// status.
func (a *App) SetMetricsSource(s MetricsSource) { a.metricsSource = s }
