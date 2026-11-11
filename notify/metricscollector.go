package notify

import (
	"bufio"
	"bytes"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

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
		appRequestsDesc, appResponseDesc, appCacheDesc} {
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
		collectRequests(ch, s.App, s.Requests)
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
// response yet has none: an app with no observer pushes nothing.
func collectRequests(ch chan<- prometheus.Metric, app string, rs []liveswap.ReleaseRequests) {
	for _, r := range rs {
		var total uint64
		for _, n := range r.ByClass {
			total += n
		}
		if total == 0 {
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
	stat  func(path string) (avail uint64, dev uint64, err error)
}

func (c diskCollector) Describe(ch chan<- *prometheus.Desc) { ch <- diskAvailDesc }

func (c diskCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.stat
	if stat == nil {
		stat = statAvail
	}
	seen := map[uint64]bool{}
	for _, p := range c.paths {
		avail, dev, err := stat(p)
		if err != nil || seen[dev] {
			continue // not measured is no sample: the rule's line keeps its last
		}
		seen[dev] = true
		ch <- prometheus.MustNewConstMetric(diskAvailDesc, prometheus.GaugeValue, float64(avail), p)
	}
}

// statAvail is what a user can still write on the filesystem holding
// path, as df's Avail, and the device it is on.
func statAvail(path string) (uint64, uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, 0, err
	}
	return fs.Bavail * uint64(fs.Bsize), st.Dev, nil //nolint:gosec // a block size is positive
}
