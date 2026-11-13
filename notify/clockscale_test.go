package notify

import (
	"bytes"
	"strings"
	"testing"
)

// The e2e build's division, run in every build: the shipped binary's
// files are the embedded ones as they are, and the metrics box's have
// every clock divided and nothing else changed.
func TestClockDivision(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1m", "10s"}, {"30s", "5s"}, {"5m", "50s"}, {"4m", "40s"},
		{"1h", "10m"}, {"4h", "40m"}, {"24h", "4h"}, {"10m", "100s"}, {"0s", "0s"},
	} {
		if got := scaleText(c.in, 6); got != c.want {
			t.Errorf("scaleText(%q, 6) = %q, want %q", c.in, got, c.want)
		}
		if got := scaleText(c.in, 1); got != c.in {
			t.Errorf("scaleText(%q, 1) = %q, want it unchanged", c.in, got)
		}
	}

	for name, b := range map[string][]byte{"the drop-in": unitVMDropIn, "vmalert's unit": unitVMAlert, "scrape.yml": scrapeConfig} {
		if !bytes.Equal(scaleUnit(b, 1), b) {
			t.Errorf("%s at a divisor of 1 is not the embedded file", name)
		}
	}
	va := string(scaleUnit(unitVMAlert, 6))
	if !strings.Contains(va, " -configCheckInterval=10s ") || !strings.Contains(va, " -evaluationInterval=10s ") {
		t.Errorf("vmalert's unit divided: %s", va)
	}
	if want := strings.NewReplacer("-configCheckInterval=1m", "-configCheckInterval=10s", "-evaluationInterval=1m", "-evaluationInterval=10s").Replace(string(unitVMAlert)); va != want {
		t.Errorf("vmalert's unit changed past its two clocks:\n%s", va)
	}
	if sc := string(scaleUnit(scrapeConfig, 6)); sc != strings.Replace(string(scrapeConfig), "scrape_interval: 30s", "scrape_interval: 5s", 1) {
		t.Errorf("scrape.yml divided:\n%s", sc)
	}
	di := scaleUnit(unitVMDropIn, 6)
	var exec []string
	for _, l := range strings.Split(string(di), "\n") {
		if strings.HasPrefix(l, "ExecStart=/") {
			exec = append(exec, l)
		}
	}
	if len(exec) != 1 || !strings.HasSuffix(exec[0], " -search.latencyOffset=5s") {
		t.Errorf("the drop-in's store command: %q", exec)
	}
	if strings.Count(string(di), "\n") != strings.Count(string(unitVMDropIn), "\n") {
		t.Errorf("the drop-in divided has other lines:\n%s", di)
	}
}

// The shipped build's clocks are the documented ones: the e2e file's
// divisor never reaches it.
func TestClockDivIsOne(t *testing.T) {
	if clockDiv != 1 {
		t.Skip("built with -tags e2e")
	}
	if pushEvery.String() != "30s" || repostEvery.String() != "1m0s" || stateGroupWait != "30s" || clockText("1m") != "1m" {
		t.Errorf("the shipped clocks moved: push %s, repost %s, group_wait %s", pushEvery, repostEvery, stateGroupWait)
	}
}
