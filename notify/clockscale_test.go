package notify

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The e2e build's division, run in every build: the shipped binary's
// files are the embedded ones as they are, and the metrics box's have
// every clock divided and nothing else changed.
func TestClockDivision(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1m", "10s"}, {"30s", "5s"}, {"5m", "50s"}, {"4m", "40s"}, {"1d", "4h"},
		{"1h", "10m"}, {"4h", "40m"}, {"24h", "4h"}, {"10m", "100s"}, {"0s", "0s"}, {"1m5s", "10833ms"},
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
	// appended is the embedded file with a flag added to the one line
	// that starts with exec, and the replacements made.
	appended := func(b []byte, exec, flag string, r *strings.Replacer) string {
		lines := strings.Split(r.Replace(string(b)), "\n")
		n := 0
		for i, l := range lines {
			if strings.HasPrefix(l, exec) {
				lines[i], n = l+flag, n+1
			}
		}
		if n != 1 {
			t.Fatalf("%d lines start with %q", n, exec)
		}
		return strings.Join(lines, "\n")
	}
	none := strings.NewReplacer()
	if got, want := string(scaleUnit(unitVMAlert, 6)), appended(unitVMAlert, "ExecStart=/usr/bin/vmalert ", " -rule.evalDelay=5s",
		strings.NewReplacer("-configCheckInterval=1m", "-configCheckInterval=10s", "-evaluationInterval=1m", "-evaluationInterval=10s")); got != want {
		t.Errorf("vmalert's unit divided:\n%s\nwant:\n%s", got, want)
	}
	if got, want := string(scaleUnit(unitVMDropIn, 6)), appended(unitVMDropIn, "ExecStart=/usr/bin/victoria-metrics ", " -search.latencyOffset=5s", none); got != want {
		t.Errorf("the drop-in divided:\n%s\nwant:\n%s", got, want)
	}
	if got, want := string(scaleUnit(scrapeConfig, 6)), strings.Replace(string(scrapeConfig), "scrape_interval: 30s", "scrape_interval: 5s", 1); got != want {
		t.Errorf("scrape.yml divided:\n%s", got)
	}
}

// What the clocks must hold of one another at any divisor: the
// metrics-builder stage of e2e/Dockerfile runs this with -tags e2e too,
// and package init there runs every clockText literal at the divisor.
func TestClockInvariants(t *testing.T) {
	// A relaunch's window starts relaunchSkip after the cutover and its
	// first check is postDeployEvery after it: past it, the first window
	// is negative, which the store refuses.
	if relaunchSkip >= postDeployEvery {
		t.Errorf("relaunchSkip %s is not inside a check's spacing %s", relaunchSkip, postDeployEvery)
	}
	// storeLag reads past the store's latency offset as written.
	offset := 30 * time.Second
	if clockDiv != 1 {
		d, _ := time.ParseDuration(scaleText(storeLatencyOffset, clockDiv))
		offset = d
	}
	if storeLag <= offset {
		t.Errorf("storeLag %s does not read past the store's -search.latencyOffset %s", storeLag, offset)
	}
	// The counter guard's half a push is written whole, not truncated.
	if g := guardedIncrease("x", "app", "", time.Minute, 0); !strings.Contains(g, " + "+numberText(pushApart.Seconds())+"))") {
		t.Errorf("the guard does not carry half a push (%s): %s", pushApart, g)
	}
	// A minute's wait holds two pushes.
	if pushEvery*2 > time.Minute/clockDiv {
		t.Errorf("a push every %s is under two to a minute of %s", pushEvery, time.Minute/clockDiv)
	}
}

// The shipped build's clocks are the documented ones: the e2e file's
// divisor never reaches it.
func TestClockDivIsOne(t *testing.T) {
	if clockDiv != 1 {
		t.Skip("built with -tags e2e")
	}
	if pushEvery.String() != "30s" || repostEvery.String() != "1m0s" || relaunchSkip.String() != "40s" || stateGroupWait != "30s" || clockText("1m") != "1m" {
		t.Errorf("the shipped clocks moved: push %s, repost %s, relaunch skip %s, group_wait %s", pushEvery, repostEvery, relaunchSkip, stateGroupWait)
	}
}
