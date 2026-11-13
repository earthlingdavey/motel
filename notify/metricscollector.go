package notify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/smallhoursorg/motel/liveswap"
)

// The per-app series (DESIGN-metrics.md, "What motel pushes"), read at
// each gather from what liveswap reports and from the running unit's
// cgroup. Registered on the config's registry, so the admin /metrics
// serves them too.
var (
	appUpDesc = prometheus.NewDesc("motel_app_up",
		"1 while an instance of the app runs, else 0.", []string{"app"}, nil)
	appReleaseDesc = prometheus.NewDesc("motel_app_release_info",
		"The app's current release, running or not (motel_app_up says which), as a label; always 1.", []string{"app", "release"}, nil)
	appMemoryDesc = prometheus.NewDesc("motel_app_memory_bytes",
		"The running unit's working set: its cgroup's memory.current less memory.stat's inactive_file.", []string{"app"}, nil)
	appMemoryMaxDesc = prometheus.NewDesc("motel_app_memory_max_bytes",
		"The running unit's MemoryMax=, when it has one.", []string{"app"}, nil)
	appCPUDesc = prometheus.NewDesc("motel_app_cpu_seconds_total",
		"CPU the running unit has used, from its cgroup's cpu.stat: one series per unit, since each new unit counts from zero.", []string{"app", "unit"}, nil)
	// 1 for an app the running config observes, whatever its release:
	// the request thresholds read the apps in the last push alone
	// (operatorrules.go, pushedNow).
	appObservedDesc = prometheus.NewDesc("motel_app_observed",
		"1 for an app a route of the running config observes; absent otherwise.", []string{"app"}, nil)
	// observe's counts, per release: the current one and the
	// one before.
	appRequestsDesc = prometheus.NewDesc("motel_app_requests_total",
		"Responses observe saw, by status class (aborted: the client went before the headers).", []string{"app", "release", "class"}, nil)
	appResponseDesc = prometheus.NewDesc("motel_app_response_seconds",
		"The app's own responses' time to headers, as observe timed them (cache hits and upgrades left out).", []string{"app", "release"}, nil)
	appCacheDesc = prometheus.NewDesc("motel_app_cache_responses_total",
		"Responses a cache in front of the app spoke for, by its Cache-Status verdict.", []string{"app", "release", "result"}, nil)
)

// appSampler is liveswap's side: *liveswap.App.
type appSampler interface {
	AppSamples() []liveswap.AppSample
}

// appCollector is a prometheus.Collector over one config's apps.
type appCollector struct {
	apps appSampler
	// root is where the cgroup tree is mounted: /sys/fs/cgroup, or a
	// test's directory.
	root string
}

func (c appCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{appUpDesc, appReleaseDesc, appMemoryDesc, appMemoryMaxDesc, appCPUDesc,
		appObservedDesc, appRequestsDesc, appResponseDesc, appCacheDesc} {
		ch <- d
	}
}

func (c appCollector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range c.apps.AppSamples() {
		up := 0.0
		if s.Running {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(appUpDesc, prometheus.GaugeValue, up, s.App)
		if s.Release != "" {
			ch <- prometheus.MustNewConstMetric(appReleaseDesc, prometheus.GaugeValue, 1, s.App, s.Release)
		}
		// Whether or not an instance runs: a release that crashed owns
		// the 502s and 503s that follow.
		collectRequests(ch, s)
		if !s.Running {
			continue
		}
		if s.MemoryMax > 0 {
			ch <- prometheus.MustNewConstMetric(appMemoryMaxDesc, prometheus.GaugeValue, float64(s.MemoryMax), s.App)
		}
		dir, ok := c.cgroupDir(s.Cgroup)
		if !ok {
			continue
		}
		if v, ok := readWorkingSet(dir); ok {
			ch <- prometheus.MustNewConstMetric(appMemoryDesc, prometheus.GaugeValue, float64(v), s.App)
		}
		if usec, ok := readCPUUsage(filepath.Join(dir, "cpu.stat")); ok {
			ch <- prometheus.MustNewConstMetric(appCPUDesc, prometheus.CounterValue, float64(usec)/1e6, s.App, path.Base(s.Cgroup))
		}
	}
}

// collectRequests is one app's request series. A release with no
// response yet has none, but the current release of an app an observer
// counts for: its zeros are the app's numbers, so that a request_rate
// rule's `<` holds for an app that has had no request since motel
// started (the counts start from zero), as it does once its series is
// in the store. An app with no observer pushes nothing, whatever it
// counted while one named it: pushed unchanged, its counts would read
// as an app with no request at all, which an every-app `request_rate <`
// rule takes for a fact; unpushed, its series go stale in the store,
// and nothing reads them. An observe line put back starts them from
// zero (liveswap's Observe.observe), as a restart does. An observed
// app's motel_app_observed is pushed from the config's first push,
// before liveswap has reattached its release after a restart and with
// none deployed: the request thresholds' guard, whose app a push
// without its counts must not leave out.
func collectRequests(ch chan<- prometheus.Metric, s liveswap.AppSample) {
	if !s.Observed {
		return
	}
	app := s.App
	ch <- prometheus.MustNewConstMetric(appObservedDesc, prometheus.GaugeValue, 1, app)
	for _, r := range s.Requests {
		var total uint64
		for _, n := range r.ByClass {
			total += n
		}
		if total == 0 && (r.Release == "" || r.Release != s.Release) {
			continue
		}
		for i, n := range r.ByClass {
			ch <- prometheus.MustNewConstMetric(appRequestsDesc, prometheus.CounterValue, float64(n), app, r.Release, liveswap.RequestClasses[i])
		}
		buckets := make(map[float64]uint64, len(r.Buckets))
		for i, n := range r.Buckets {
			buckets[liveswap.ResponseBuckets[i]] = n
		}
		ch <- prometheus.MustNewConstHistogram(appResponseDesc, r.Count, r.SumSeconds, buckets, app, r.Release)
		for i, n := range r.Cache {
			if n > 0 {
				ch <- prometheus.MustNewConstMetric(appCacheDesc, prometheus.CounterValue, float64(n), app, r.Release, liveswap.CacheResults[i])
			}
		}
	}
}

// cgroupDir is the unit's directory under the cgroup root. cg is the
// manager's own report of the unit's cgroup; a path that would leave
// the root is no directory.
func (c appCollector) cgroupDir(cg string) (string, bool) {
	if cg == "" || !strings.HasPrefix(cg, "/") {
		return "", false
	}
	dir := filepath.Join(c.root, cg)
	if dir == filepath.Clean(c.root) || !strings.HasPrefix(dir, filepath.Clean(c.root)+string(filepath.Separator)) {
		return "", false
	}
	return dir, true
}

// maxCgroupFile bounds a read of one cgroup file: cpu.stat is a few
// lines, and memory.stat's inactive_file is among its first.
const maxCgroupFile = 4 << 10

func readSmall(path string) ([]byte, bool) {
	f, err := os.Open(path) //nolint:gosec // under the cgroup root, from the manager's report
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxCgroupFile)
	n, _ := f.Read(buf)
	return buf[:n], n > 0
}

func readCgroupUint(path string) (uint64, bool) {
	b, ok := readSmall(path)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(string(bytes.TrimSpace(b)), 10, 64)
	return v, err == nil
}

// readWorkingSet is memory.current less memory.stat's inactive_file:
// the page cache the kernel gives back before the cap is felt. An app
// that reads files under a MemoryMax= fills its cgroup with cache up to
// the cap and stays there with no pressure, so memory.current alone
// reads as an app at its limit. A memory.stat that cannot be read
// leaves memory.current as it is.
func readWorkingSet(dir string) (uint64, bool) {
	cur, ok := readCgroupUint(filepath.Join(dir, "memory.current"))
	if !ok {
		return 0, false
	}
	if inactive, ok := readStat(filepath.Join(dir, "memory.stat"), "inactive_file"); ok && inactive <= cur {
		cur -= inactive
	}
	return cur, true
}

// readCPUUsage is cpu.stat's usage_usec.
func readCPUUsage(path string) (uint64, bool) { return readStat(path, "usage_usec") }

// readStat is one key of a cgroup's flat keyed file.
func readStat(path, key string) (uint64, bool) {
	b, ok := readSmall(path)
	if !ok {
		return 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// diskAvailMetric is the box's disk series, which an operator's
// disk_fill rule reads (operatorrules.go).
const diskAvailMetric = "motel_disk_avail_bytes"

const diskAvailDescHelp = "Bytes a user can still write on the filesystem holding the path: one series per filesystem, under the first of motel's directories on it (the apps' root, Caddy's data, notify's state)."

var diskAvailDesc = prometheus.NewDesc(diskAvailMetric, diskAvailDescHelp, []string{"path"}, nil)

// diskCollector is the box's disk series: statfs at each gather of the
// directories notify's disk source measures, one series per device, as
// that source reads them (diskPoller).
type diskCollector struct {
	paths []string
	stat  func(path string) (diskNumbers, error)
}

func (c diskCollector) Describe(ch chan<- *prometheus.Desc) { ch <- diskAvailDesc }

func (c diskCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.stat
	if stat == nil {
		stat = statDisk
	}
	// Every path first, at once: a series keeps its name only while that
	// path is still on its device (diskNames.name).
	results := diskStats.statAll(c.paths, stat)
	type measured struct {
		path string
		n    diskNumbers
	}
	var got []measured
	devOf := map[string]uint64{}
	for i, p := range c.paths {
		r := results[i]
		if r.err != nil {
			continue // not measured is no sample: the rule's line keeps its last
		}
		got = append(got, measured{p, r.n})
		devOf[p] = r.n.dev
	}
	seen := map[uint64]bool{}
	for _, m := range got {
		if seen[m.n.dev] {
			continue
		}
		seen[m.n.dev] = true
		ch <- prometheus.MustNewConstMetric(diskAvailDesc, prometheus.GaugeValue, float64(m.n.avail), diskLabels.name(m.n.dev, m.path, c.paths, devOf))
	}
}

// diskStatWait is how long a gather, or a poll of notify's disk source,
// waits for the paths' statfs: a filesystem that hangs (a network mount
// whose server went away) must not hold back the push and the admin
// /metrics, and every motel series with them, nor notify's engine. A
// path not answered in time is not measured.
var diskStatWait = 2 * time.Second

// diskStat is one path's statfs, as it came back.
type diskStat struct {
	n   diskNumbers
	err error
}

// diskStats is the process's statfs calls of the disk series that have
// not come back: one per path at most, so a filesystem that hangs holds
// one goroutine, not one more at each gather. diskPollStats is notify's
// disk source's (diskPoller), apart, so that a gather's statfs of a
// path is never a poll's busy one.
var (
	diskStats     = &statsInFlight{busy: map[string]bool{}}
	diskPollStats = &statsInFlight{busy: map[string]bool{}}
)

type statsInFlight struct {
	mu   sync.Mutex
	busy map[string]bool
}

// errStatBusy is a path whose statfs has not come back since a gather
// before.
var errStatBusy = errors.New("the statfs of a gather before has not come back")

// statAll is each path's statfs, all started at once and waited for up
// to diskStatWait: a path not back by then, or whose statfs of a call
// before is still out (errStatBusy), has an error and no numbers.
func (s *statsInFlight) statAll(paths []string, stat func(string) (diskNumbers, error)) []diskStat {
	ctx, cancel := context.WithTimeout(context.Background(), diskStatWait)
	defer cancel()
	results := make([]<-chan diskStat, len(paths))
	for i, p := range paths {
		results[i] = s.start(p, stat)
	}
	out := make([]diskStat, len(paths))
	for i := range paths {
		// One back already is taken past the deadline too: a select
		// with both ready picks either.
		select {
		case out[i] = <-results[i]:
		default:
			select {
			case out[i] = <-results[i]:
			case <-ctx.Done():
				out[i].err = ctx.Err()
			}
		}
	}
	return out
}

// start runs stat(path) on its own goroutine, and returns where its
// answer comes; at once, with errStatBusy, while one before is out.
func (s *statsInFlight) start(path string, stat func(string) (diskNumbers, error)) <-chan diskStat {
	out := make(chan diskStat, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[path] {
		out <- diskStat{err: errStatBusy}
		return out
	}
	s.busy[path] = true
	go func() {
		n, err := stat(path)
		s.mu.Lock()
		delete(s.busy, path)
		s.mu.Unlock()
		out <- diskStat{n, err}
	}()
	return out
}

// diskLabels keeps the path a filesystem's series is named by for as
// long as that path is measured: a stat of it that fails once must not
// rename the series after the next path on the device, which would cut
// a disk_fill rule's history in two. The process's, as the per-device
// level of notify's disk source is.
var diskLabels = &diskNames{byDev: map[uint64]string{}}

type diskNames struct {
	mu    sync.Mutex
	byDev map[uint64]string
}

// name is the device's series' path: the one it had while that is
// still among paths and not measured on another device now (devOf: the
// paths measured at this gather), else p, measured now. A path names
// one device's series at most: two series of one name are a gather
// error (the push drops the second, the admin /metrics answers 500). A
// path moved to another filesystem (a disk mounted there) names that
// one's series, and the device it left is named by its next path.
func (n *diskNames) name(dev uint64, p string, paths []string, devOf map[string]uint64) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if had, ok := n.byDev[dev]; ok && slices.Contains(paths, had) {
		if d, now := devOf[had]; !now || d == dev {
			return had
		}
	}
	for d, had := range n.byDev {
		if had == p {
			delete(n.byDev, d)
		}
	}
	n.byDev[dev] = p
	return p
}
