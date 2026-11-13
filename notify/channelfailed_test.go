package notify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
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
	if got := te.drain(); len(got) != 0 {
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
	got := te.drain()
	if len(got) != 1 || got[0].Labels[labelChannel] != "pager" || got[0].Labels["suspects"] != "ops" || got[0].Labels["alertname"] != "ChannelFailed" {
		t.Fatalf("posts = %+v", got)
	}
	// Without receiver_name, every webhook channel is a suspect (pager,
	// bob): ops is told.
	body = metricsHead + failedLine("slack", "event-warning-ops", "clientError", 2) + failedLine("webhook", "", "other", 4)
	te.scrapeFailed(context.Background(), read)
	got = te.drain()
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
	got := te.drain()
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
// moves: its warning events' and its own (motel monitor test's).
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
	if got := te.drain(); len(got) != 0 {
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
	// The channel mended, and motel monitor test delivers through it.
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
	if got := te2.drain(); len(got) != 1 || got[0].Labels[labelChannel] != "bob" || got[0].Labels["suspects"] != "ops" {
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
