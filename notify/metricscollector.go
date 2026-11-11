package notify

import (
	"bufio"
	"bytes"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

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
		"The app's running release, as a label; always 1.", []string{"app", "release"}, nil)
	appMemoryDesc = prometheus.NewDesc("motel_app_memory_bytes",
		"The running unit's working set: its cgroup's memory.current less memory.stat's inactive_file.", []string{"app"}, nil)
	appMemoryMaxDesc = prometheus.NewDesc("motel_app_memory_max_bytes",
		"The running unit's MemoryMax=, when it has one.", []string{"app"}, nil)
	appCPUDesc = prometheus.NewDesc("motel_app_cpu_seconds_total",
		"CPU the running unit has used, from its cgroup's cpu.stat: one series per unit, since each new unit counts from zero.", []string{"app", "unit"}, nil)
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
	for _, d := range []*prometheus.Desc{appUpDesc, appReleaseDesc, appMemoryDesc, appMemoryMaxDesc, appCPUDesc} {
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
