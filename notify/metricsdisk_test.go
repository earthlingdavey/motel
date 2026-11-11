package notify

import (
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The disk series: one per filesystem, under the first path on it; a
// path that cannot be measured has none.
func TestDiskCollector(t *testing.T) {
	diskLabels = &diskNames{byDev: map[uint64]string{}}
	c := diskCollector{
		paths: []string{"/var/lib/liveswap", "/var/lib/caddy", "/var/lib/motel-notify", "/gone"},
		stat: func(p string) (uint64, uint64, error) {
			switch p {
			case "/var/lib/liveswap", "/var/lib/caddy":
				return 1000, 1, nil
			case "/var/lib/motel-notify":
				return 50, 2, nil
			}
			return 0, 0, os.ErrNotExist
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
	if avail, _, err := statAvail(t.TempDir()); err != nil || avail == 0 {
		t.Errorf("statAvail of a temp dir: %d, %v", avail, err)
	}
}

// A path whose stat fails once leaves the series its name: the next
// path on the same filesystem is measured under it.
func TestDiskSeriesKeepsItsName(t *testing.T) {
	diskLabels = &diskNames{byDev: map[uint64]string{}}
	failing := false
	c := diskCollector{
		paths: []string{"/var/lib/liveswap", "/var/lib/caddy"},
		stat: func(p string) (uint64, uint64, error) {
			if p == "/var/lib/liveswap" && failing {
				return 0, 0, os.ErrPermission
			}
			return 1000, 1, nil
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
	c2 := diskCollector{paths: []string{"/srv/apps", "/var/lib/caddy"}, stat: func(string) (uint64, uint64, error) { return 5, 1, nil }}
	reg2 := prometheus.NewPedanticRegistry()
	reg2.MustRegister(c2)
	if err := testutil.GatherAndCompare(reg2, strings.NewReader(strings.ReplaceAll(strings.ReplaceAll(want, "/var/lib/liveswap", "/srv/apps"), "} 1000", "} 5"))); err != nil {
		t.Fatal(err)
	}
}
