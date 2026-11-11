package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "alertmanager-0.28.1-metrics.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestParseAMMetricsRealBody reads what Debian's Alertmanager 0.28.1
// served on hotserve-1: with receiver-name-in-metrics, a failed counter
// pre-made at zero for each receiver and reason of the integrations the
// config uses (there, only webhook: the operator's channel is ntfy), and
// the one attempt `motel monitor test` made to ch-operator.
func TestParseAMMetricsRealBody(t *testing.T) {
	m, err := parseAMMetrics(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	receivers := map[string]bool{}
	for _, s := range m.failed {
		receivers[s.receiver] = true
		if s.integration != "webhook" || s.receiver == "" || s.reason == "" {
			t.Fatalf("a series that is not the webhook's, or lacks receiver_name or reason: %+v", s)
		}
		if s.value != 0 {
			t.Fatalf("no notification had failed: %+v", s)
		}
	}
	for _, want := range []string{"ch-operator", "event-box-info", "state-box-critical", "fallback"} {
		if !receivers[want] {
			t.Errorf("no series for receiver %s (have %d receivers)", want, len(receivers))
		}
	}
	if m.requests["ch-operator"] != 1 || m.requestsFailed["ch-operator"] != 0 {
		t.Fatalf("ch-operator: requests %v failed %v", m.requests["ch-operator"], m.requestsFailed["ch-operator"])
	}
	c, err := countsFor(fixture(t), "ch-operator")
	if err != nil || !c.delivered(counts{}) {
		t.Fatalf("counts = %+v, %v", c, err)
	}
}

func TestParseAMMetricsFormats(t *testing.T) {
	cases := []struct {
		name string
		body string
		want failedSample
	}{
		{"untyped, no HELP or TYPE",
			failedMetric + `{integration="webhook",receiver_name="ch-a",reason="other"} 3` + "\n",
			failedSample{"webhook", "ch-a", "other", 3}},
		{"counter with HELP and TYPE",
			"# HELP " + failedMetric + " x\n# TYPE " + failedMetric + " counter\n" +
				failedMetric + `{integration="slack",receiver_name="ch-b",reason="clientError"} 2` + "\n",
			failedSample{"slack", "ch-b", "clientError", 2}},
		{"escaped label value",
			failedMetric + `{integration="webhook",receiver_name="odd \"name\" \\ x\ny",reason="other"} 1` + "\n",
			failedSample{"webhook", "odd \"name\" \\ x\ny", "other", 1}},
		{"a timestamp after the value",
			failedMetric + `{integration="webhook",receiver_name="ch-c",reason="other"} 4 1794326137000` + "\n",
			failedSample{"webhook", "ch-c", "other", 4}},
		{"no receiver_name (the feature off)",
			failedMetric + `{integration="discord",reason="serverError"} 5` + "\n",
			failedSample{"discord", "", "serverError", 5}},
		{"a float in exponent form",
			failedMetric + `{integration="webhook",receiver_name="ch-d",reason="other"} 1e+00` + "\n",
			failedSample{"webhook", "ch-d", "other", 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := parseAMMetrics([]byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if len(m.failed) != 1 || m.failed[0] != c.want {
				t.Fatalf("failed = %+v, want %+v", m.failed, c.want)
			}
		})
	}
}

// TestParseAMMetricsRefusesWhatIsNotAReading: a body cut short (the
// client caps it), a malformed line, a body without the failed counter.
// Each is no reading, never a partial one.
func TestParseAMMetricsRefusesWhatIsNotAReading(t *testing.T) {
	full := fixture(t)
	i := strings.Index(string(full), failedMetric+"{")
	cuts := map[string]string{
		"cut after the label set":   string(full[:i]) + failedMetric + `{integration="webhook",reason="other"} `,
		"cut inside a label":        string(full[:i]) + failedMetric + `{integration="webh`,
		"cut mid-file, mid-line":    string(full[:len(full)/2]),
		"a value that is no number": failedMetric + `{integration="webhook",reason="other"} many` + "\n",
		"no failed counter at all":  "# HELP alertmanager_build_info x\nalertmanager_build_info{version=\"0.28.1\"} 1\n",
		"empty":                     "",
	}
	for name, body := range cuts {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAMMetrics([]byte(body)); err == nil {
				t.Fatal("accepted")
			}
			if _, err := countsFor([]byte(body), "ch-operator"); err == nil {
				t.Fatal("countsFor accepted")
			}
		})
	}
	if _, err := parseAMMetrics([]byte("")); !errors.Is(err, errNoFailedCounter) {
		t.Fatalf("an empty body: %v", err)
	}
}

func TestCountsForSumsIntegrations(t *testing.T) {
	body := failedMetric + `{integration="webhook",receiver_name="event-app-blog-warning",reason="other"} 1` + "\n" +
		failedMetric + `{integration="discord",receiver_name="event-app-blog-warning",reason="clientError"} 2` + "\n" +
		requestsMetric + `{integration="webhook",receiver_name="event-app-blog-warning"} 5` + "\n" +
		requestsMetric + `{integration="discord",receiver_name="event-app-blog-warning"} 2` + "\n" +
		requestsFailedMetric + `{integration="discord",receiver_name="event-app-blog-warning"} 2` + "\n" +
		requestsMetric + `{integration="webhook",receiver_name="other"} 9` + "\n"
	c, err := countsFor([]byte(body), "event-app-blog-warning")
	if err != nil {
		t.Fatal(err)
	}
	if c.requests != 7 || c.requestsFailed != 2 || c.failed != 3 || c.reason == "" {
		t.Fatalf("counts = %+v", c)
	}
}

// TestScrapeFailedKeepsTheBaselineOverABadRead: a read that is no
// reading neither posts nor moves the baseline, so a rise across it is
// still seen, and nothing that had not risen is taken for a rise.
func TestScrapeFailedKeepsTheBaselineOverABadRead(t *testing.T) {
	te := newTestEngine(t)
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = r
	_, _, a.receivers = render(r, testStateDir)
	te.setConfig(a, te.p, te.known, te.logger)
	line := func(v string) string {
		return failedMetric + `{integration="slack",receiver_name="event-app-blog-warning",reason="clientError"} ` + v + "\n"
	}
	bodies := []string{
		line("2"),      // baseline: two failures before motel looked
		"",             // Alertmanager restarting: an empty answer
		line("2")[:40], // cut short
		line("3"),      // one more failure since the baseline
	}
	for i, b := range bodies {
		body := b
		te.scrapeFailed(context.Background(), func(context.Context) ([]byte, error) { return []byte(body), nil })
		got := te.drain()
		switch i {
		case 0, 1, 2:
			if len(got) != 0 {
				t.Fatalf("read %d posted %+v", i, got)
			}
		case 3:
			if len(got) != 1 || got[0].Labels["alertname"] != alertChannelFailed || got[0].Labels["suspects"] != "ops" {
				t.Fatalf("the rise across the bad reads: %+v", got)
			}
		}
	}
	if n := len(te.lines("monitor: channel failed")); n != 1 {
		t.Fatalf("channel failed lines = %d", n)
	}
}
