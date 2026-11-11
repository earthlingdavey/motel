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
