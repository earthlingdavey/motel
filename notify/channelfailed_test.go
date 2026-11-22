package notify

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

func TestFailWatchRises(t *testing.T) {
	var w failWatch
	s := func(r string, v float64) failedSample { return failedSample{"webhook", r, "other", v} }
	if got := w.rises([]failedSample{s("a", 5), s("b", 0)}, false); len(got) != 0 {
		t.Fatalf("the first read is a baseline: %+v", got)
	}
	if got := w.rises([]failedSample{s("a", 5), s("b", 0)}, false); len(got) != 0 {
		t.Fatalf("nothing rose: %+v", got)
	}
	if got := w.rises([]failedSample{s("a", 6), s("b", 0), s("c", 2)}, false); len(got) != 2 {
		t.Fatalf("a rose, c appeared non-zero: %+v", got)
	}
	// A load the read gives no stamp of: the counters start over.
	if got := w.rises([]failedSample{s("a", 1), s("b", 0), s("c", 0)}, false); len(got) != 1 || got[0].receiver != "a" {
		t.Fatalf("after a restart: %+v", got)
	}
	// A load the read's stamp says came since: a's 1 is a failure since
	// it, though the last read was 1 too.
	if got := w.rises([]failedSample{s("a", 1), s("b", 0), s("c", 0)}, true); len(got) != 1 || got[0].receiver != "a" {
		t.Fatalf("after a load back to the same count: %+v", got)
	}
	// The first read, of a load after moteld started, is no baseline.
	var fresh failWatch
	if got := fresh.rises([]failedSample{s("a", 1), s("b", 0)}, true); len(got) != 1 || got[0].receiver != "a" {
		t.Fatalf("the first read after a load since the start: %+v", got)
	}
}

// loadedAt is Alertmanager's last config load as its /metrics has it.
func loadedAt(stamp float64) string {
	return fmt.Sprintf("# TYPE %s gauge\n%s %v\n", loadedMetric, loadedMetric, stamp)
}

// TestLoadSince: a stamp other than the last read's is a load since
// (an earlier one too: the wall clock stepped back between two loads),
// and the first read's is compared with moteld's start; a read without
// a stamp, or after reads that never had one, is none.
func TestLoadSince(t *testing.T) {
	start := time.Unix(1_794_000_000, 0)
	w := failWatch{started: start}
	at := func(stamp float64) amMetrics { return amMetrics{loaded: stamp} }
	if w.loadSince(at(1_793_999_999)) {
		t.Fatal("a load before moteld started is not the first read's")
	}
	if !w.loadSince(at(1_794_000_001)) {
		t.Fatal("a load after moteld started counts from 0 at the first read")
	}
	if (&failWatch{}).loadSince(at(1_794_000_001)) {
		t.Fatal("a watch with no start reads a baseline first")
	}
	w.prev, w.loaded = map[string]float64{}, 1_794_000_100
	for stamp, want := range map[float64]bool{0: false, 1_794_000_050: true, 1_794_000_100: false, 1_794_000_100.5: true} {
		if got := w.loadSince(at(stamp)); got != want {
			t.Errorf("last read's load %v, this read's %v: loadSince = %v", w.loaded, stamp, got)
		}
	}
	w.loaded = 0 // no read so far had a stamp
	if w.loadSince(at(1_794_000_100)) {
		t.Fatal("the first stamp after reads that never had one is no load since")
	}
}

// TestChannelFailedAcrossAConfigLoad: 1 before a config load, the load
// (0), and one failure before the next minute's read (1 again) is a
// rise: the read's load stamp is not the last read's. Compared with the
// last read alone it is none. A read without the stamp keeps the last
// one, so the load after it is still seen; so is one stamped earlier
// (the clock stepped back).
func TestChannelFailedAcrossAConfigLoad(t *testing.T) {
	te, _, body, read := holdEngine(t, fullBlock)
	line := func(v int) string { return metricsHead + failedLine("webhook", "event-warning-bob", "clientError", v) }
	for i, c := range []struct {
		body  string
		rises bool
	}{
		{line(1) + loadedAt(1_794_000_000), false},   // baseline: a failure before
		{line(1) + loadedAt(1_794_000_100.25), true}, // a load, then a failure
		{line(1) + loadedAt(1_794_000_100.25), false},
		{line(0) + loadedAt(1_794_000_200), false}, // a load, nothing since
		{line(1) + loadedAt(1_794_000_200), true},
		{line(1), false}, // a body without the stamp
		{line(1) + loadedAt(1_794_000_300), true},
		{line(1) + loadedAt(1_794_000_250), true}, // the clock stepped back
	} {
		te.advance(20 * time.Minute) // a key's bucket takes four an hour
		*body = c.body
		te.scrapeFailed(context.Background(), read)
		got := told(te.drain())
		if c.rises != (len(got) == 1 && got[0].Labels["suspects"] == "bob") || !c.rises && len(got) != 0 {
			t.Fatalf("read %d: posts %+v, want a ChannelFailed: %v", i, got, c.rises)
		}
	}
}

// TestFirstReadCountsFromALoadAfterTheStart: Alertmanager started after
// moteld (setup's start, its socket's at motel's first post), and a
// failure before motel's first read is told; a load from before moteld
// started is a baseline, which an earlier moteld may have told of. The
// reload moteld's start asked for counts whatever its stamp says (the
// clock stepped back after the start), and a body without receiver
// names (no stamp read) stays a baseline.
func TestFirstReadCountsFromALoadAfterTheStart(t *testing.T) {
	for _, c := range []struct {
		stamp    float64
		reloaded bool
		names    bool
		want     int
	}{
		{1_794_000_030, false, true, 1},
		{1_793_999_000, false, true, 0},
		{1_793_999_000, true, true, 1},
		{1_794_000_030, true, false, 0},
	} {
		te, _, body, read := holdEngine(t, fullBlock)
		te.fail.started = time.Unix(1_794_000_000, 0)
		if c.reloaded {
			te.noteReload()
		}
		receiver := "event-warning-bob"
		if !c.names {
			receiver = ""
		}
		*body = metricsHead + failedLine("webhook", receiver, "clientError", 1) + loadedAt(c.stamp)
		te.scrapeFailed(context.Background(), read)
		if got := told(te.drain()); len(got) != c.want {
			t.Errorf("%+v: posts %+v, want %d", c, got, c.want)
		}
	}
}

func TestScrapeFailedTellsTheOtherOperatorChannels(t *testing.T) {
	te := newTestEngine(t)
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = r
	_, _, a.receivers = render(r, testStateDir)
	te.setConfig(a, te.p, te.known, te.logger) // no goroutine: the test drains

	body := metricsHead + failedLine("slack", "event-warning-ops", "clientError", 0) + failedLine("webhook", "", "other", 0)
	read := func(context.Context) ([]byte, error) { return []byte(body), nil }
	te.scrapeFailed(context.Background(), read) // baseline
	if len(te.queue) != 0 {
		t.Fatal("the baseline posted")
	}
	// ops's receiver for warning events failed, its slack config the
	// only one in it: pager, the other operator channel, takes
	// criticals alone: a ChannelFailed is a warning, so nobody is told,
	// and the journal says so.
	body = metricsHead + failedLine("slack", "event-warning-ops", "clientError", 1) + failedLine("webhook", "", "other", 0)
	te.scrapeFailed(context.Background(), read)
	if got := told(te.drain()); len(got) != 0 {
		t.Fatalf("a critical-only channel was told a warning: %+v", got)
	}
	lines := te.lines("monitor: channel failed")
	if len(lines) != 1 || lines[0].ContextMap()["suspects"] == nil {
		t.Fatalf("lines = %+v", lines)
	}
	// With pager's floor at warning, it is told.
	for i := range a.Channels {
		if a.Channels[i].Name == "pager" {
			a.Channels[i].Severity = SeverityWarning
		}
	}
	if a.cfg, err = a.resolve(testTrust(), testSecrets()); err != nil {
		t.Fatal(err)
	}
	te.setConfig(a, te.p, te.known, te.logger)
	body = metricsHead + failedLine("slack", "event-warning-ops", "clientError", 2) + failedLine("webhook", "", "other", 0)
	te.scrapeFailed(context.Background(), read)
	got := told(te.drain())
	if len(got) != 1 || got[0].Labels[labelChannel] != "pager" || got[0].Labels["suspects"] != "ops" || got[0].Labels["alertname"] != "ChannelFailed" {
		t.Fatalf("posts = %+v", got)
	}
	// vmalert's rule posts these labels too, and Alertmanager merges the
	// two only while they are the same: no occurred_at here.
	if _, ok := got[0].Labels[labelOccurredAt]; ok || len(got[0].Labels) != 6 {
		t.Fatalf("ChannelFailed's labels = %v", got[0].Labels)
	}
	// Without receiver_name, every webhook channel is a suspect (pager,
	// bob): ops is told.
	body = metricsHead + failedLine("slack", "event-warning-ops", "clientError", 2) + failedLine("webhook", "", "other", 4)
	te.scrapeFailed(context.Background(), read)
	got = told(te.drain())
	if len(got) != 1 || got[0].Labels[labelChannel] != "ops" || !strings.Contains(got[0].Labels["suspects"], "pager") {
		t.Fatalf("posts = %+v", got)
	}
}

func TestSuspects(t *testing.T) {
	w := failWatch{
		kinds:    map[string]string{"ops": "slack", "pager": "webhook", "bob": "webhook"},
		channels: map[string][]string{"ch-pager": {"pager"}, "state-warning-bob": {"bob"}, "state-warning-ops": {"ops"}},
	}
	cases := []struct {
		s    failedSample
		want []string
	}{
		{failedSample{integration: "webhook", receiver: "ch-pager"}, []string{"pager"}},
		{failedSample{integration: "webhook", receiver: "state-warning-bob"}, []string{"bob"}},
		{failedSample{integration: "slack", receiver: "state-warning-ops"}, []string{"ops"}},
		{failedSample{integration: "webhook"}, []string{"bob", "pager"}},
		{failedSample{integration: "discord", receiver: "state-warning-bob"}, nil},
	}
	for _, c := range cases {
		if got := w.suspects(c.s); !slices.Equal(got, c.want) {
			t.Errorf("suspects(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestHeartbeatFailureSuspectsNoChannel(t *testing.T) {
	_, _, recv := renderFull(t)
	w := failWatch{kinds: map[string]string{"pager": "webhook", "bob": "webhook", "ops": "slack"}, channels: recv}
	if got := w.suspects(failedSample{integration: "webhook", receiver: "heartbeat"}); len(got) != 0 {
		t.Fatalf("a failing heartbeat blamed %v", got)
	}
}

// TestEachReceiversFailureNamesItsChannelAlone: with a receiver per
// channel, the failed counter of any receiver of the full block names
// that receiver's channel and no other, json and ntfy (one integration,
// webhook) among them.
func TestEachReceiversFailureNamesItsChannelAlone(t *testing.T) {
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	_, _, recv := render(r, testStateDir)
	w := failWatch{kinds: map[string]string{}, channels: recv}
	for name, ch := range r.channels {
		w.kinds[name] = channelKinds[ch.Kind]
	}
	n := 0
	for name, chans := range recv {
		if name == "heartbeat" {
			continue
		}
		if len(chans) != 1 {
			t.Fatalf("receiver %s holds %v", name, chans)
		}
		if got := w.suspects(failedSample{integration: w.kinds[chans[0]], receiver: name}); !slices.Equal(got, chans) {
			t.Errorf("a failure of %s names %v, want %v", name, got, chans)
		}
		n++
	}
	if n < 20 {
		t.Fatalf("only %d receivers", n)
	}
}

// TestScrapeFailedNamesOneWebhookChannel: bob (json) and pager (ntfy)
// are one integration, and both hear shop's criticals. bob's failure
// names bob alone, and ops, an operator channel that is not a suspect,
// is told; under a receiver both shared, the failure named both.
func TestScrapeFailedNamesOneWebhookChannel(t *testing.T) {
	te := newTestEngine(t)
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = r
	_, _, a.receivers = render(r, testStateDir)
	te.setConfig(a, te.p, te.known, te.logger)
	body := metricsHead + failedLine("webhook", "event-critical-bob", "clientError", 0) + failedLine("webhook", "event-critical-pager", "clientError", 0)
	read := func(context.Context) ([]byte, error) { return []byte(body), nil }
	te.scrapeFailed(context.Background(), read)
	body = metricsHead + failedLine("webhook", "event-critical-bob", "clientError", 1) + failedLine("webhook", "event-critical-pager", "clientError", 0)
	te.scrapeFailed(context.Background(), read)
	got := told(te.drain())
	if len(got) != 1 || got[0].Labels[labelChannel] != "ops" || got[0].Labels["suspects"] != "bob" {
		t.Fatalf("posts = %+v", got)
	}
	if len(te.untold) != 0 {
		t.Fatalf("a failure told to ops holds the heartbeat: %+v", te.untold)
	}
}

// amLines is one receiver's counters as Alertmanager's /metrics has
// them (a webhook's): deliveries is attempts that did not fail.
func amLines(receiver string, failed, requests, requestsFailed int) string {
	const integration = "webhook"
	return failedLine(integration, receiver, "clientError", failed) +
		fmt.Sprintf("%s{integration=%q,receiver_name=%q} %d\n", requestsMetric, integration, receiver, requests) +
		fmt.Sprintf("%s{integration=%q,receiver_name=%q} %d\n", requestsFailedMetric, integration, receiver, requestsFailed)
}

// holdEngine is an engine serving a block whose one operator channel,
// ops (ntfy), has no other channel to hear its failure, and read is
// Alertmanager's /metrics as *body says.
func holdEngine(t *testing.T, block string) (*testEngine, *App, *string, func(context.Context) ([]byte, error)) {
	t.Helper()
	te := newTestEngine(t)
	a := mustParse(t, block)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = r
	_, _, a.receivers = render(r, testStateDir)
	te.setConfig(a, te.p, te.known, te.logger)
	body := new(string)
	return te, a, body, func(context.Context) ([]byte, error) { return []byte(*body), nil }
}

const oneChannelBlock = `monitor {
	channel ops ntfy {secret.NOTIFY_OPS_URL}
	operator ops
	heartbeat {secret.NOTIFY_HC_URL}
}`

// counters is ops's two receivers in holdEngine's block that the test
// moves: its warning events' and its own (moteld monitor test's).
func counters(failed, events, eventsFailed, tests int) string {
	return metricsHead + amLines("event-warning-ops", failed, events, eventsFailed) +
		amLines("ch-ops", 0, tests, 0) + amLines("heartbeat", 0, 9, 0)
}

func heartbeats(te *testEngine) int {
	te.tick()
	return countAlert(te.drain(), alertWatchdog)
}

// TestHeartbeatHeldWhileAFailureIsToldToNoChannel: the one channel
// fails, and nobody can be told. After heartbeatHoldAfter with no
// delivery through it, motel stops posting Watchdog, so the check off
// the box reports the box down; a delivery through the channel (a test)
// resumes it.
func TestHeartbeatHeldWhileAFailureIsToldToNoChannel(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read) // baseline
	if heartbeats(te) != 1 {
		t.Fatal("no heartbeat before any failure")
	}
	*body = counters(1, 4, 1, 0)
	te.scrapeFailed(context.Background(), read)
	if got := told(te.drain()); len(got) != 0 {
		t.Fatalf("told someone: %+v", got)
	}
	l := te.lines("monitor: channel failed")
	if len(l) != 1 || len(l[0].ContextMap()["told"].([]interface{})) != 0 {
		t.Fatalf("lines = %+v", l)
	}
	// Not yet: Alertmanager sends a failed notification again at the
	// group's next flushes.
	for range 4 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
		if heartbeats(te) != 1 {
			t.Fatalf("held at %v", te.at)
		}
	}
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	held := te.lines("monitor: heartbeat held: a failed delivery no channel was told of; the heartbeat's check off the box will report this box down")
	if len(held) != 1 || !slices.Equal(stringsOf(held[0].ContextMap()["suspects"]), []string{"ops"}) {
		t.Fatalf("held lines = %+v", held)
	}
	for range 10 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
		if heartbeats(te) != 0 {
			t.Fatalf("a heartbeat while held, at %v", te.at)
		}
	}
	if len(te.lines("monitor: heartbeat held: a failed delivery no channel was told of; the heartbeat's check off the box will report this box down")) != 1 {
		t.Fatal("held said twice")
	}
	// The channel mended, and moteld monitor test delivers through it.
	*body = counters(1, 4, 1, 1)
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	if l := te.lines("monitor: heartbeat resumed"); len(l) != 1 || l[0].ContextMap()["delivered_through"] != "ch-ops" {
		t.Fatalf("resumed lines = %+v", l)
	}
	if heartbeats(te) != 1 {
		t.Fatal("no heartbeat after the delivery")
	}
}

// TestHeartbeatResumesAtADeliveryAfterALoad: the documented mend, the
// URL mended (a reload of Alertmanager, which starts its counts from 0)
// and then `moteld monitor test`, resumes the heartbeat at the next read.
// First with a read between the load and the test: the load alone
// resumes nothing (that read's 0 is any reader's baseline for the
// test's 1). Then the case that needs the load's stamp: the test read
// in the load's own read, its count no higher than ops's before the
// load (one earlier test).
func TestHeartbeatResumesAtADeliveryAfterALoad(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	const before, after = 1_794_000_000, 1_794_000_900
	*body = counters(0, 3, 0, 1) + loadedAt(before)
	te.scrapeFailed(context.Background(), read) // baseline
	*body = counters(1, 4, 1, 1) + loadedAt(before)
	te.scrapeFailed(context.Background(), read)
	for range 6 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
	}
	if !te.heartbeatHeld() {
		t.Fatal("not held")
	}
	*body = counters(0, 0, 0, 0) + loadedAt(after) // the mend's reload
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	if !te.heartbeatHeld() {
		t.Fatal("resumed by a load with no delivery since")
	}
	*body = counters(0, 0, 0, 1) + loadedAt(after) // moteld monitor test ops
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	if l := te.lines("monitor: heartbeat resumed"); len(l) != 1 || l[0].ContextMap()["delivered_through"] != "ch-ops" {
		t.Fatalf("resumed lines = %+v", l)
	}
	// And when the test is read in the load's own read.
	te, _, body, read = holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 1) + loadedAt(before)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 1) + loadedAt(before)
	te.scrapeFailed(context.Background(), read)
	for range 6 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
	}
	*body = counters(0, 0, 0, 1) + loadedAt(after)
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	if te.heartbeatHeld() || len(te.lines("monitor: heartbeat resumed")) != 1 {
		t.Fatalf("a test read in the load's own read: held %v", te.heartbeatHeld())
	}
}

func stringsOf(v any) []string {
	var out []string
	switch s := v.(type) {
	case []string:
		return s
	case []interface{}:
		for _, x := range s {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// TestHeartbeatNotHeldWhenTheChannelDeliversAgain: a passing fault is
// delivered at a later flush, inside heartbeatHoldAfter, and no hold
// comes; nor for a failure some channel is told of, nor for the
// heartbeat's own receiver.
func TestHeartbeatNotHeldWhenTheChannelDeliversAgain(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	te.scrapeFailed(context.Background(), read)
	te.advance(time.Minute)
	*body = counters(1, 5, 1, 0) // the next flush delivered it
	te.scrapeFailed(context.Background(), read)
	for range 10 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
		if heartbeats(te) != 1 {
			t.Fatalf("held at %v after a delivery", te.at)
		}
	}
	if len(te.untold) != 0 || len(te.lines("monitor: heartbeat resumed")) != 0 {
		t.Fatalf("untold = %+v", te.untold)
	}

	// The heartbeat's own receiver failing: its check hears it already.
	*body = counters(1, 5, 1, 0) + amLines("heartbeat", 3, 12, 3)
	te.scrapeFailed(context.Background(), read)
	if len(te.untold) != 0 {
		t.Fatalf("the heartbeat's own failure holds it: %+v", te.untold)
	}

	// A second operator channel at warning hears ops' failure: no hold.
	te2, _, body2, read2 := holdEngine(t, `monitor {
	channel ops ntfy {secret.NOTIFY_OPS_URL}
	channel bob json {secret.NOTIFY_BOB_URL} severity warning
	operator ops bob
	heartbeat {secret.NOTIFY_HC_URL}
}`)
	*body2 = counters(0, 3, 0, 0)
	te2.scrapeFailed(context.Background(), read2)
	*body2 = counters(1, 4, 1, 0)
	te2.scrapeFailed(context.Background(), read2)
	if got := told(te2.drain()); len(got) != 1 || got[0].Labels[labelChannel] != "bob" || got[0].Labels["suspects"] != "ops" {
		t.Fatalf("posts = %+v", got)
	}
	if len(te2.untold) != 0 {
		t.Fatalf("a failure told to bob holds the heartbeat: %+v", te2.untold)
	}
}

// TestHeartbeatHoldEndsWithTheChannel: a reload whose config no longer
// has the failing channel ends its hold (nothing can deliver through
// it); one that keeps it keeps the hold, and the engine's emptying at
// the last config's stop ends it too.
func TestHeartbeatHoldEndsWithTheChannel(t *testing.T) {
	te, a, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	te.scrapeFailed(context.Background(), read)
	te.advance(heartbeatHoldAfter)
	te.scrapeFailed(context.Background(), read)
	if !te.heartbeatHeld() {
		t.Fatal("not held")
	}
	te.setConfig(a, te.p, te.known, te.logger) // the same block reloaded
	if !te.heartbeatHeld() {
		t.Fatal("a reload that keeps the channel ended the hold")
	}

	b := mustParse(t, `monitor {
	channel bob json {secret.NOTIFY_BOB_URL}
	operator bob
	heartbeat {secret.NOTIFY_HC_URL}
}`)
	b.cfg = mustResolve(t, b)
	_, _, b.receivers = render(b.cfg, testStateDir)
	te.setConfig(b, te.p, te.known, te.logger)
	if te.heartbeatHeld() {
		t.Fatal("a config without ops keeps its hold")
	}
	if l := te.lines("monitor: heartbeat resumed"); len(l) != 1 || l[0].ContextMap()["why"] == nil {
		t.Fatalf("resumed lines = %+v", l)
	}
}

func mustResolve(t *testing.T, a *App) *resolved {
	t.Helper()
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestHeartbeatNotHeldOnAGuess: a failure whose receiver is not this
// config's (the counter names none: a drop-in without the feature; or
// a name from before a reload) has suspects that are a guess and no
// delivery count by channel to end a hold by: nothing is recorded.
func TestHeartbeatNotHeldOnAGuess(t *testing.T) {
	for name, receiver := range map[string]string{"no receiver_name": "", "a receiver from before a reload": "event-box-warning"} {
		te, _, body, read := holdEngine(t, oneChannelBlock)
		*body = counters(0, 3, 0, 0) + failedLine("webhook", receiver, "other", 0)
		te.scrapeFailed(context.Background(), read)
		*body = counters(0, 3, 0, 0) + failedLine("webhook", receiver, "other", 2)
		te.scrapeFailed(context.Background(), read)
		if l := te.lines("monitor: channel failed"); len(l) != 1 {
			t.Fatalf("%s: the rise was not read: %+v", name, l)
		}
		if len(te.untold) != 0 {
			t.Errorf("%s: recorded %+v", name, te.untold)
		}
	}
}

// TestHeartbeatHoldNeedsAHeartbeatLine: with no heartbeat line there is
// no check off the box to hear a hold. The failure is recorded but
// nothing is held and no held line is written; a load that adds the
// line holds it at the next read, and one that takes the line away
// ends the hold.
func TestHeartbeatHoldNeedsAHeartbeatLine(t *testing.T) {
	const noHeartbeat = `monitor {
	channel ops ntfy {secret.NOTIFY_OPS_URL}
	operator ops
}`
	te, _, body, read := holdEngine(t, noHeartbeat)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	te.scrapeFailed(context.Background(), read)
	for range 7 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
	}
	if len(te.untold) != 1 || te.heartbeatHeld() || len(te.lines("monitor: heartbeat held: a failed delivery no channel was told of; the heartbeat's check off the box will report this box down")) != 0 {
		t.Fatalf("no heartbeat line: untold %+v, held %v", te.untold, te.heartbeatHeld())
	}

	with := mustParse(t, oneChannelBlock)
	with.cfg = mustResolve(t, with)
	_, _, with.receivers = render(with.cfg, testStateDir)
	te.setConfig(with, te.p, te.known, te.logger)
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	if !te.heartbeatHeld() || heartbeats(te) != 0 {
		t.Fatal("a load that adds the heartbeat line does not hold what stands")
	}

	without := mustParse(t, noHeartbeat)
	without.cfg = mustResolve(t, without)
	_, _, without.receivers = render(without.cfg, testStateDir)
	te.setConfig(without, te.p, te.known, te.logger)
	if te.heartbeatHeld() {
		t.Fatal("a load that takes the heartbeat line away keeps the hold")
	}
	if l := te.lines("monitor: heartbeat resumed"); len(l) != 1 || l[0].ContextMap()["why"] != "the config has no heartbeat line" {
		t.Fatalf("resumed lines = %+v", l)
	}
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	if te.heartbeatHeld() {
		t.Fatal("held again with no heartbeat line")
	}
}

// told is what was posted to channels: every alert but ChannelFailed's
// read-only copies, which readOnly picks out.
func told(posts []amAlert) []amAlert {
	return slices.DeleteFunc(slices.Clone(posts), func(a amAlert) bool { return a.Labels[labelChannel] == readOnlyChannel })
}

func readOnly(posts []amAlert) []amAlert {
	return slices.DeleteFunc(slices.Clone(posts), func(a amAlert) bool { return a.Labels[labelChannel] != readOnlyChannel })
}

// TestReadOnlyCopyNameIsNoChannel: no channel can be named the read-only
// copy's notify_channel, so no channel's route can be told it.
func TestReadOnlyCopyNameIsNoChannel(t *testing.T) {
	if channelNameRe.MatchString(readOnlyChannel) {
		t.Fatalf("%q is a channel name", readOnlyChannel)
	}
	if err := (Channel{Name: readOnlyChannel, Kind: "json", Secret: "NOTIFY_OPS_URL"}).check(); err == nil {
		t.Fatalf("a channel named %q passes check", readOnlyChannel)
	}
}

// TestReadOnlyCopyOnAOneChannelBox: ops, the one operator channel,
// fails. No channel can be told, and the read-only copy is posted all
// the same: ChannelFailed's six labels, notify_channel "-", the same
// summary the told copies carry, alive 15 minutes.
func TestReadOnlyCopyOnAOneChannelBox(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	te.scrapeFailed(context.Background(), read)
	got := te.drain()
	if len(got) != 1 {
		t.Fatalf("posts = %+v, want the read-only copy alone", got)
	}
	c := got[0]
	want := map[string]string{"alertname": alertChannelFailed, labelChannel: readOnlyChannel, "severity": SeverityWarning,
		"subject": "box", "integration": "webhook", "suspects": "ops"}
	if !maps.Equal(c.Labels, want) {
		t.Fatalf("labels = %v, want %v", c.Labels, want)
	}
	if !strings.Contains(c.Annotations["summary"], "failing channel: ops") || !c.StartsAt.Equal(te.at) || !c.EndsAt.Equal(te.at.Add(channelFailedLife)) {
		t.Fatalf("copy = %+v", c)
	}
	if l := te.lines("monitor: channel failed"); len(l) != 1 || len(l[0].ContextMap()["told"].([]interface{})) != 0 {
		t.Fatalf("journal: %+v (the copy is told to no channel)", l)
	}
}

const fourChannelBlock = `monitor {
	channel ops json {secret.NOTIFY_OPS_URL}
	channel pager json {secret.NOTIFY_PAGER_URL}
	channel alice json {secret.NOTIFY_ALICE_URL}
	channel bob json {secret.NOTIFY_BOB_URL}
	operator ops pager alice bob
	heartbeat {secret.NOTIFY_HC_URL}
}`

// TestReadOnlyCopyBesideTheAddressedOnes: four operator channels, ops
// fails: the three others are told, each its own copy, and the read-only
// copy is posted beside them with the same failure's labels and summary.
// Someone was told, so nothing is held and nothing keeps the copy alive:
// it ends 15 minutes after its post.
func TestReadOnlyCopyBesideTheAddressedOnes(t *testing.T) {
	te, _, body, read := holdEngine(t, fourChannelBlock)
	*body = metricsHead + amLines("event-warning-ops", 0, 3, 0)
	te.scrapeFailed(context.Background(), read)
	posted := te.at
	*body = metricsHead + amLines("event-warning-ops", 1, 4, 1)
	te.scrapeFailed(context.Background(), read)
	got := te.drain()
	var chans []string
	for _, a := range told(got) {
		chans = append(chans, a.Labels[labelChannel])
	}
	slices.Sort(chans)
	ro := readOnly(got)
	if !slices.Equal(chans, []string{"alice", "bob", "pager"}) || len(ro) != 1 {
		t.Fatalf("posts = %+v", got)
	}
	for _, a := range told(got) {
		if a.Labels["integration"] != ro[0].Labels["integration"] || a.Labels["suspects"] != ro[0].Labels["suspects"] ||
			a.Annotations["summary"] != ro[0].Annotations["summary"] || !a.StartsAt.Equal(ro[0].StartsAt) {
			t.Fatalf("the copy %+v is not the failure %+v", ro[0], a)
		}
	}
	if len(te.untold) != 0 {
		t.Fatalf("untold = %+v", te.untold)
	}
	for range 20 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
		if p := readOnly(te.drain()); len(p) != 0 {
			t.Fatalf("posted again at %v with no rise and no hold: %+v", te.at, p)
		}
	}
	if !ro[0].EndsAt.Equal(posted.Add(channelFailedLife)) {
		t.Fatalf("the copy ends %v, want 15 minutes after its post", ro[0].EndsAt)
	}
}

// TestReadOnlyCopyKeptWhileTheHoldStands: ops, the one channel, fails
// once. The hold records it, and at every read after it the read-only
// copy is posted again from the hold's start, so Alertmanager holds one
// alert with one start however long the hold stands, beyond the 15
// minutes a post lives. The hold itself keeps its timing: held at the
// first read heartbeatHoldAfter after the failure, the heartbeat no
// longer posted. A delivery through ops ends the record, and the copy
// with it: posted once more, resolved, and then no more.
func TestReadOnlyCopyKeptWhileTheHoldStands(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	te.tick()
	te.drain()
	*body = counters(1, 4, 1, 0)
	failed := te.at
	te.scrapeFailed(context.Background(), read)
	te.tick()
	te.drain()
	for i := 1; i <= 40; i++ {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
		te.tick()
		got := te.drain()
		ro := readOnly(got)
		if len(ro) != 1 || !ro[0].StartsAt.Equal(failed) || !ro[0].EndsAt.Equal(te.at.Add(channelFailedLife)) ||
			ro[0].Labels["suspects"] != "ops" || ro[0].Labels["integration"] != "webhook" || !strings.Contains(ro[0].Annotations["summary"], "failing channel: ops") {
			t.Fatalf("+%d min: read-only posts %+v, want one from %v", i, ro, failed)
		}
		if len(told(got)) != countAlert(got, alertWatchdog) {
			t.Fatalf("+%d min: told %+v", i, told(got))
		}
		held := te.at.Sub(failed) >= heartbeatHoldAfter
		if te.heartbeatHeld() != held || held && countAlert(got, alertWatchdog) != 0 {
			t.Fatalf("+%d min: held %v, heartbeats %d", i, te.heartbeatHeld(), countAlert(got, alertWatchdog))
		}
	}
	// The channel mended: moteld monitor test delivers through it.
	*body = counters(1, 4, 1, 1)
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	te.tick()
	got := te.drain()
	if ro := readOnly(got); len(ro) != 1 || !ro[0].StartsAt.Equal(failed) || !ro[0].EndsAt.Equal(te.at) || te.heartbeatHeld() || countAlert(got, alertWatchdog) != 1 {
		t.Fatalf("after the delivery: read-only %+v (want it resolved now), held %v", ro, te.heartbeatHeld())
	}
	for range 20 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
		if ro := readOnly(te.drain()); len(ro) != 0 {
			t.Fatalf("posted after the hold ended: %+v", ro)
		}
	}
}

// TestReadOnlyCopyOfARiseInAHold: a rise of a failure whose hold stands
// posts its copy from the hold's start, not its own read's, so the two
// posts are one start to the people's reads.
func TestReadOnlyCopyOfARiseInAHold(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	failed := te.at
	te.scrapeFailed(context.Background(), read)
	te.drain()
	te.advance(7 * time.Minute)
	*body = counters(2, 5, 2, 0)
	te.scrapeFailed(context.Background(), read)
	copies := readOnly(te.drain())
	if len(copies) != 2 { // the hold's re-post, and the rise's own
		t.Fatalf("copies = %+v, want the hold's and the rise's", copies)
	}
	for _, c := range copies {
		if !c.StartsAt.Equal(failed) {
			t.Fatalf("a copy from %v, want the hold's start %v", c.StartsAt, failed)
		}
	}
}

// TestReadOnlyCopyEndsWithAChannelTheConfigDrops: a load whose config no
// longer has the failing channel ends its record, and resolves its copy.
func TestReadOnlyCopyEndsWithAChannelTheConfigDrops(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	failed := te.at
	te.scrapeFailed(context.Background(), read)
	te.drain()
	te.advance(time.Minute)
	b := mustParse(t, `monitor {
	channel pager ntfy {secret.NOTIFY_PAGER_URL}
	operator pager
	heartbeat {secret.NOTIFY_HC_URL}
}`)
	r, err := b.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	b.cfg = r
	_, _, b.receivers = render(r, testStateDir)
	te.setConfig(b, te.p, te.known, te.logger)
	ro := readOnly(te.drain())
	if len(te.untold) != 0 || len(ro) != 1 || ro[0].Labels["suspects"] != "ops" || !ro[0].StartsAt.Equal(failed) || !ro[0].EndsAt.Equal(te.at) {
		t.Fatalf("untold %+v, copies %+v: want the record gone and its copy resolved", te.untold, ro)
	}
}

// TestReadOnlyCopyCarriesTheLatestRise: while a hold stands, a later
// rise of the same failure through another receiver or for another
// reason is what the copy says from then on, the hold's re-posts too,
// so the read does not swap between two texts each minute.
func TestReadOnlyCopyCarriesTheLatestRise(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0) // event-warning-ops fails
	te.scrapeFailed(context.Background(), read)
	te.drain()
	te.advance(time.Minute)
	*body = metricsHead + amLines("event-warning-ops", 1, 4, 1) + amLines("ch-ops", 1, 1, 1) + amLines("heartbeat", 0, 9, 0)
	te.scrapeFailed(context.Background(), read) // and ch-ops: same suspects, another receiver
	te.drain()
	for range 3 {
		te.advance(time.Minute)
		te.scrapeFailed(context.Background(), read)
		ro := readOnly(te.drain())
		if len(ro) != 1 || !strings.Contains(ro[0].Annotations["summary"], "for receiver ch-ops") {
			t.Fatalf("the hold's re-post: %+v, want the latest rise's words", ro)
		}
	}
}

// TestReadOnlyCopyKeptWhileTheFailureGoesOn: four channels, ops failing
// at every read for two hours. The channels are told four times, then
// once a quarter-hour (the key's bucket); the read-only copy, told to
// no one, is posted at every rise, so it is never let lapse between two
// posts and stays one alert, from its first start.
func TestReadOnlyCopyKeptWhileTheFailureGoesOn(t *testing.T) {
	te, _, body, read := holdEngine(t, fourChannelBlock)
	*body = metricsHead + amLines("event-warning-ops", 0, 3, 0)
	te.scrapeFailed(context.Background(), read)
	var end time.Time
	toldPosts := 0
	for i := 1; i <= 120; i++ {
		te.advance(time.Minute)
		if !end.IsZero() && !te.at.Before(end) {
			t.Fatalf("+%d min: the copy lapsed at %v", i, end)
		}
		*body = metricsHead + amLines("event-warning-ops", i, 3+i, i)
		te.scrapeFailed(context.Background(), read)
		got := te.drain()
		toldPosts += len(told(got))
		ro := readOnly(got)
		if len(ro) != 1 {
			t.Fatalf("+%d min: read-only posts %+v", i, ro)
		}
		end = ro[0].EndsAt
	}
	if toldPosts > 3*(4+2*4+1) {
		t.Fatalf("the channels were told %d times: the bucket no longer bounds them", toldPosts)
	}
	if l := te.lines("monitor: channel failed"); len(l) != 120 {
		t.Fatalf("%d channel failed lines, want one a rise", len(l))
	}
}

// TestReadOnlyCopyAloneIsNoErrorLine: a post that tells no channel (the
// one channel failing) and that Alertmanager never takes is given up at
// debug, as the hold's re-posts are; one that tells channels stays an
// error line.
func TestReadOnlyCopyAloneIsNoErrorLine(t *testing.T) {
	for _, c := range []struct {
		block string
		body  func(int) string
		level zapcore.Level
	}{
		{oneChannelBlock, func(i int) string { return counters(i, 3+i, i, 0) }, zap.DebugLevel},
		{fourChannelBlock, func(i int) string { return metricsHead + amLines("event-warning-ops", i, 3+i, i) }, zap.ErrorLevel},
	} {
		te, _, body, read := holdEngine(t, c.block)
		*body = c.body(0)
		te.scrapeFailed(context.Background(), read)
		te.p.err = &netErr{syscall.ECONNREFUSED}
		*body = c.body(1)
		te.scrapeFailed(context.Background(), read)
		for range 7 {
			te.attempt(context.Background())
			te.advance(time.Minute)
		}
		te.attempt(context.Background())
		var lines []observer.LoggedEntry
		for _, l := range te.lines("monitor: delivery failed") {
			if strings.HasPrefix(l.ContextMap()["key"].(string), "notify/box/channel_failed/ops") {
				lines = append(lines, l)
			}
		}
		if len(lines) == 0 || lines[0].Level != c.level {
			t.Fatalf("delivery failed lines = %+v, want the first at %v", lines, c.level)
		}
	}
}

// TestReadOnlyCopyOfAnotherIntegrationEnds: a load changes the held
// channel's kind (ntfy to slack), and it fails again: the copy of the
// old integration is another alert, so it is resolved as the record
// moves to the new one, and the read is left one row.
func TestReadOnlyCopyOfAnotherIntegrationEnds(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	te.scrapeFailed(context.Background(), read)
	te.drain()
	b := mustParse(t, strings.Replace(oneChannelBlock, "channel ops ntfy", "channel ops slack", 1))
	r, err := b.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	b.cfg = r
	_, _, b.receivers = render(r, testStateDir)
	te.setConfig(b, te.p, te.known, te.logger)
	te.advance(time.Minute)
	*body = counters(1, 4, 1, 0) + failedLine("slack", "event-warning-ops", "clientError", 1)
	te.scrapeFailed(context.Background(), read)
	// The last post of each alert is what Alertmanager holds.
	last := map[string]amAlert{}
	for _, c := range readOnly(te.drain()) {
		last[c.Labels["integration"]] = c
	}
	if len(last) != 2 || !last["webhook"].EndsAt.Equal(te.at) || !last["slack"].EndsAt.After(te.at) {
		t.Fatalf("last posts %+v: want the webhook copy resolved and the slack one live", last)
	}
}

// TestReadOnlyCopyAfterALoadThatAddsAChannel: a hold stands for ops; a
// load adds pager, which takes warnings, and changes ops's kind (ntfy to
// slack). ops fails again: pager is told, the hold's record stays (the
// accepted row on a load that adds a channel), and the record takes the
// rise's integration and words: the old integration's copy is resolved,
// and the record's re-posts are the slack one, from the record's start.
func TestReadOnlyCopyAfterALoadThatAddsAChannel(t *testing.T) {
	te, _, body, read := holdEngine(t, oneChannelBlock)
	*body = counters(0, 3, 0, 0)
	te.scrapeFailed(context.Background(), read)
	*body = counters(1, 4, 1, 0)
	failed := te.at
	te.scrapeFailed(context.Background(), read)
	te.drain()
	b := mustParse(t, `monitor {
	channel ops slack {secret.NOTIFY_OPS_URL}
	channel pager ntfy {secret.NOTIFY_PAGER_URL}
	operator ops pager
	heartbeat {secret.NOTIFY_HC_URL}
}`)
	r, err := b.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	b.cfg = r
	_, _, b.receivers = render(r, testStateDir)
	te.setConfig(b, te.p, te.known, te.logger)
	te.advance(time.Minute)
	*body = counters(1, 4, 1, 0) + failedLine("slack", "event-warning-ops", "clientError", 1)
	te.scrapeFailed(context.Background(), read)
	got := te.drain()
	if tl := told(got); len(tl) != 1 || tl[0].Labels[labelChannel] != "pager" {
		t.Fatalf("told %+v, want pager", tl)
	}
	last := map[string]amAlert{}
	for _, c := range readOnly(got) {
		last[c.Labels["integration"]] = c
	}
	if len(last) != 2 || !last["webhook"].EndsAt.Equal(te.at) || !last["slack"].EndsAt.After(te.at) || !last["slack"].StartsAt.Equal(failed) {
		t.Fatalf("last posts %+v: want the webhook copy resolved and the slack one live from %v", last, failed)
	}
	te.advance(time.Minute)
	te.scrapeFailed(context.Background(), read)
	ro := readOnly(te.drain())
	if len(ro) != 1 || ro[0].Labels["integration"] != "slack" || !ro[0].StartsAt.Equal(failed) {
		t.Fatalf("the record's re-post: %+v, want the slack copy from %v", ro, failed)
	}
	// ops delivers again: the record ends, and with its copy the copy told
	// to pager, which the read folded under it, is resolved too, so the
	// read is left nothing of the failure.
	te.advance(time.Minute)
	*body = counters(1, 4, 1, 1) + failedLine("slack", "event-warning-ops", "clientError", 1)
	te.scrapeFailed(context.Background(), read)
	got = te.drain()
	ended := map[string]bool{}
	for _, a := range got {
		if a.Labels["alertname"] == alertChannelFailed && a.EndsAt.Equal(te.at) {
			ended[a.Labels[labelChannel]+"/"+a.Labels["integration"]] = true
		}
	}
	if len(te.untold) != 0 || !ended["-/slack"] || !ended["pager/slack"] || len(ended) != 2 {
		t.Fatalf("ended %v (untold %d): want the read-only copy and pager's resolved", ended, len(te.untold))
	}
}
