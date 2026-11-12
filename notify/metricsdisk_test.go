package notify

import (
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The disk series: one per filesystem, under the first path on it; a
// path that cannot be measured has none.
func TestDiskCollector(t *testing.T) {
	diskLabels = &diskNames{byDev: map[uint64]string{}}
	c := diskCollector{
		paths: []string{"/var/lib/liveswap", "/var/lib/caddy", "/var/lib/motel-notify", "/gone"},
		stat: func(p string) (diskNumbers, error) {
			switch p {
			case "/var/lib/liveswap", "/var/lib/caddy":
				return diskNumbers{avail: 1000, dev: 1}, nil
			case "/var/lib/motel-notify":
				return diskNumbers{avail: 50, dev: 2}, nil
			}
			return diskNumbers{}, os.ErrNotExist
		},
	}
	want := `# HELP motel_disk_avail_bytes ` + diskAvailDescHelp + `
# TYPE motel_disk_avail_bytes gauge
motel_disk_avail_bytes{path="/var/lib/liveswap"} 1000
motel_disk_avail_bytes{path="/var/lib/motel-notify"} 50
`
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	if n, err := statDisk(t.TempDir()); err != nil || n.avail == 0 {
		t.Errorf("statDisk of a temp dir: %+v, %v", n, err)
	}
}

// A path whose stat fails once leaves the series its name: the next
// path on the same filesystem is measured under it.
func TestDiskSeriesKeepsItsName(t *testing.T) {
	diskLabels = &diskNames{byDev: map[uint64]string{}}
	failing := false
	c := diskCollector{
		paths: []string{"/var/lib/liveswap", "/var/lib/caddy"},
		stat: func(p string) (diskNumbers, error) {
			if p == "/var/lib/liveswap" && failing {
				return diskNumbers{}, os.ErrPermission
			}
			return diskNumbers{avail: 1000, dev: 1}, nil
		},
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	want := `# HELP motel_disk_avail_bytes ` + diskAvailDescHelp + `
# TYPE motel_disk_avail_bytes gauge
motel_disk_avail_bytes{path="/var/lib/liveswap"} 1000
`
	for _, f := range []bool{false, true} {
		failing = f
		if err := testutil.GatherAndCompare(reg, strings.NewReader(want)); err != nil {
			t.Fatalf("failing %v: %v", f, err)
		}
	}
	// A path no longer measured (a reload moved the apps' root) names
	// the series no more.
	c2 := diskCollector{paths: []string{"/srv/apps", "/var/lib/caddy"}, stat: func(string) (diskNumbers, error) { return diskNumbers{avail: 5, dev: 1}, nil }}
	reg2 := prometheus.NewPedanticRegistry()
	reg2.MustRegister(c2)
	if err := testutil.GatherAndCompare(reg2, strings.NewReader(strings.ReplaceAll(strings.ReplaceAll(want, "/var/lib/liveswap", "/srv/apps"), "} 1000", "} 5"))); err != nil {
		t.Fatal(err)
	}
}

// A path that moves to another filesystem (a disk mounted on it) names
// that filesystem's series, and the one it left takes its next path's
// name: never two series of one name, which would fail the gather.
func TestDiskSeriesFollowsAMount(t *testing.T) {
	diskLabels = &diskNames{byDev: map[uint64]string{}}
	dev := map[string]uint64{"/var/lib/liveswap": 1, "/var/lib/caddy": 1}
	c := diskCollector{
		paths: []string{"/var/lib/liveswap", "/var/lib/caddy"},
		stat:  func(p string) (diskNumbers, error) { return diskNumbers{avail: 1000 * dev[p], dev: dev[p]}, nil },
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	head := `# HELP motel_disk_avail_bytes ` + diskAvailDescHelp + `
# TYPE motel_disk_avail_bytes gauge
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(head+`motel_disk_avail_bytes{path="/var/lib/liveswap"} 1000
`)); err != nil {
		t.Fatal(err)
	}
	dev["/var/lib/liveswap"] = 2
	want := head + `motel_disk_avail_bytes{path="/var/lib/caddy"} 1000
motel_disk_avail_bytes{path="/var/lib/liveswap"} 2000
`
	for range 2 {
		if err := testutil.GatherAndCompare(reg, strings.NewReader(want)); err != nil {
			t.Fatalf("after the mount: %v", err)
		}
	}
	// Its stat failing now leaves the moved path to its own filesystem:
	// the one it left keeps the name it took.
	c.stat = func(p string) (diskNumbers, error) {
		if p == "/var/lib/liveswap" {
			return diskNumbers{}, os.ErrPermission
		}
		return diskNumbers{avail: 1000, dev: 1}, nil
	}
	reg2 := prometheus.NewPedanticRegistry()
	reg2.MustRegister(c)
	if err := testutil.GatherAndCompare(reg2, strings.NewReader(head+`motel_disk_avail_bytes{path="/var/lib/caddy"} 1000
`)); err != nil {
		t.Fatal(err)
	}
}

// A statfs that hangs holds back no gather: the path has no sample
// until it comes back, and the gathers meanwhile start no other.
func TestDiskStatThatHangs(t *testing.T) {
	diskLabels = &diskNames{byDev: map[uint64]string{}}
	defer func(w time.Duration) { diskStatWait = w }(diskStatWait)
	diskStatWait = 50 * time.Millisecond
	release := make(chan struct{})
	var calls atomic.Int32
	c := diskCollector{
		paths: []string{"/srv/nfs", "/var/lib/caddy"},
		stat: func(p string) (diskNumbers, error) {
			if p == "/srv/nfs" {
				calls.Add(1)
				<-release
				return diskNumbers{avail: 7, dev: 2}, nil
			}
			return diskNumbers{avail: 1000, dev: 1}, nil
		},
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	head := `# HELP motel_disk_avail_bytes ` + diskAvailDescHelp + `
# TYPE motel_disk_avail_bytes gauge
`
	for range 3 {
		start := time.Now()
		if err := testutil.GatherAndCompare(reg, strings.NewReader(head+`motel_disk_avail_bytes{path="/var/lib/caddy"} 1000
`)); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("a gather waited %s", d)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d statfs of the hung path, want the one", n)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := testutil.GatherAndCompare(reg, strings.NewReader(head+`motel_disk_avail_bytes{path="/srv/nfs"} 7
motel_disk_avail_bytes{path="/var/lib/caddy"} 1000
`))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after it came back: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// statfs counts blocks in f_frsize, as df does; f_bsize is the
// preferred I/O size and need not be it.
func TestFsNumbersCountFragments(t *testing.T) {
	n := fsNumbers(syscall.Statfs_t{Bsize: 1 << 20, Frsize: 4096, Blocks: 1000, Bfree: 300, Bavail: 250}, 7)
	if n.avail != 250*4096 || n.dev != 7 || n.usedPct != 100*700.0/950 {
		t.Fatalf("numbers = %+v", n)
	}
	if n := fsNumbers(syscall.Statfs_t{Bsize: 512, Bavail: 10}, 1); n.avail != 5120 || n.usedPct != 0 {
		t.Fatalf("no f_frsize: %+v", n)
	}
}
