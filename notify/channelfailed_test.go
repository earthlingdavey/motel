package notify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const metricsHead = `# HELP alertmanager_notifications_failed_total The total number of failed notifications.
# TYPE alertmanager_notifications_failed_total counter
`

func failedLine(integration, receiver, reason string, v int) string {
	if receiver == "" {
		return fmt.Sprintf("%s{integration=%q,reason=%q} %d\n", failedMetric, integration, reason, v)
	}
	return fmt.Sprintf("%s{integration=%q,receiver_name=%q,reason=%q} %d\n", failedMetric, integration, receiver, reason, v)
}

func TestParseFailed(t *testing.T) {
	body := metricsHead + failedLine("slack", "ch-ops", "clientError", 3) +
		`alertmanager_notifications_total{integration="slack"} 9` + "\n" +
		failedMetric + `{integration="webhook",receiver_name="odd \"name\"",reason="other"} 1` + "\n"
	got := parseFailed([]byte(body))
	if len(got) != 2 || got[0] != (failedSample{"slack", "ch-ops", "clientError", 3}) || got[1].receiver != `odd "name"` {
		t.Fatalf("samples = %+v", got)
	}
}

func TestParseFailedLineCutShort(t *testing.T) {
	body := failedMetric + `{integration="webhook",reason="other"} ` + "\n" + failedMetric + `{integration="webhook"} `
	if got := parseFailed([]byte(body)); len(got) != 0 {
		t.Fatalf("samples = %+v", got)
	}
}

func TestFailWatchRises(t *testing.T) {
	var w failWatch
	s := func(r string, v float64) failedSample { return failedSample{"webhook", r, "other", v} }
	if got := w.rises([]failedSample{s("a", 5), s("b", 0)}); len(got) != 0 {
		t.Fatalf("the first read is a baseline: %+v", got)
	}
	if got := w.rises([]failedSample{s("a", 5), s("b", 0)}); len(got) != 0 {
		t.Fatalf("nothing rose: %+v", got)
	}
	if got := w.rises([]failedSample{s("a", 6), s("b", 0), s("c", 2)}); len(got) != 2 {
		t.Fatalf("a rose, c appeared non-zero: %+v", got)
	}
	// Alertmanager restarted: its counters start over.
	if got := w.rises([]failedSample{s("a", 1), s("b", 0), s("c", 0)}); len(got) != 1 || got[0].receiver != "a" {
		t.Fatalf("after a restart: %+v", got)
	}
}

func TestScrapeFailedTellsTheOtherTeacherChannels(t *testing.T) {
	te := newTestEngine(t)
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = r
	_, _, a.receivers = render(r, testStateDir)
	te.setConfig(a, te.p, te.known, te.logger) // no goroutine: the test drains

	body := metricsHead + failedLine("slack", "event-app-blog-warning", "clientError", 0) + failedLine("webhook", "", "other", 0)
	read := func(context.Context) ([]byte, error) { return []byte(body), nil }
	te.scrapeFailed(context.Background(), read) // baseline
	if len(te.queue) != 0 {
		t.Fatal("the baseline posted")
	}
	// The blog receiver's slack config failed: ops is the only slack
	// channel in it, so pager (the other teacher channel) is told.
	body = metricsHead + failedLine("slack", "event-app-blog-warning", "clientError", 1) + failedLine("webhook", "", "other", 0)
	te.scrapeFailed(context.Background(), read)
	got := te.drain()
	if len(got) != 1 || got[0].Labels[labelChannel] != "pager" || got[0].Labels["suspects"] != "ops" || got[0].Labels["alertname"] != "ChannelFailed" {
		t.Fatalf("posts = %+v", got)
	}
	lines := te.lines("notify: channel failed")
	if len(lines) != 1 {
		t.Fatalf("lines = %+v", lines)
	}
	// Without receiver_name, every webhook channel is a suspect (pager,
	// students-bob): ops is told.
	body = metricsHead + failedLine("slack", "event-app-blog-warning", "clientError", 1) + failedLine("webhook", "", "other", 4)
	te.scrapeFailed(context.Background(), read)
	got = te.drain()
	if len(got) != 1 || got[0].Labels[labelChannel] != "ops" || !strings.Contains(got[0].Labels["suspects"], "pager") {
		t.Fatalf("posts = %+v", got)
	}
}

func TestSuspects(t *testing.T) {
	w := failWatch{
		kinds:    map[string]string{"ops": "slack", "pager": "webhook", "bob": "webhook"},
		channels: map[string][]string{"ch-pager": {"pager"}, "state-app-shop-warning": {"bob", "ops"}},
	}
	cases := []struct {
		s    failedSample
		want []string
	}{
		{failedSample{integration: "webhook", receiver: "ch-pager"}, []string{"pager"}},
		{failedSample{integration: "webhook", receiver: "state-app-shop-warning"}, []string{"bob"}},
		{failedSample{integration: "webhook"}, []string{"bob", "pager"}},
		{failedSample{integration: "discord", receiver: "state-app-shop-warning"}, nil},
	}
	for _, c := range cases {
		if got := w.suspects(c.s); !slices.Equal(got, c.want) {
			t.Errorf("suspects(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestHeartbeatFailureSuspectsNoChannel(t *testing.T) {
	_, _, recv := renderFull(t)
	w := failWatch{kinds: map[string]string{"pager": "webhook", "students-bob": "webhook", "ops": "slack"}, channels: recv}
	if got := w.suspects(failedSample{integration: "webhook", receiver: "heartbeat"}); len(got) != 0 {
		t.Fatalf("a failing heartbeat blamed %v", got)
	}
}
