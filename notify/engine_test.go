package notify

import (
	"context"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap"
)

type recPoster struct {
	mu    sync.Mutex
	posts [][]amAlert
	err   error
}

func (p *recPoster) postAlerts(_ context.Context, a []amAlert) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.posts = append(p.posts, a)
	return nil
}

func (p *recPoster) take() []amAlert {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []amAlert
	for _, a := range p.posts {
		out = append(out, a...)
	}
	p.posts = nil
	return out
}

type testEngine struct {
	*engine
	t    *testing.T
	at   time.Time
	logs *observer.ObservedLogs
	p    *recPoster
}

func newTestEngine(t *testing.T) *testEngine {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	te := &testEngine{engine: newEngine(), t: t, at: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), logs: logs, p: &recPoster{}}
	te.now = func() time.Time { return te.at }
	te.logger = zap.New(core)
	te.post = te.p
	te.active = &App{}
	te.redact = liveswap.NewRedactor([]string{"NOTIFY_OPS_URL=https://hooks.slack.com/services/T0/B0/secretpart"}, nil) // gitleaks:allow
	return te
}

func (te *testEngine) advance(d time.Duration) { te.at = te.at.Add(d) }

// drain posts everything queued.
func (te *testEngine) drain() []amAlert {
	te.t.Helper()
	for {
		again, _ := te.attempt(context.Background())
		if !again {
			break
		}
	}
	return te.p.take()
}

func (te *testEngine) lines(msg string) []observer.LoggedEntry {
	return te.logs.FilterMessage(msg).TakeAll()
}

func deployFailed() Event {
	return Event{Name: "deploy_failed", Kind: kindEvent, Severity: SeverityWarning, App: "blog", Source: "liveswap",
		Summary: "deploy of v3 failed in phase health_check", Release: "v3", DeployedBy: "local:/etc/motel/alice.pub sub=alice"}
}

func watchdog(firing bool) Event {
	return Event{Name: "watchdog_throttled", Kind: kindState, Severity: SeverityCritical, App: "blog", Source: "liveswap",
		Summary: "blog is in a restart loop", Release: "v2", Firing: firing}
}

func TestOneShotJournalledThenPosted(t *testing.T) {
	te := newTestEngine(t)
	te.report(deployFailed())
	// The line is written before the post is even attempted.
	lines := te.lines("notify: event")
	if len(lines) != 1 || lines[0].ContextMap()["key"] != "liveswap/app:blog/deploy_failed" || lines[0].ContextMap()["state"] != "oneshot" {
		t.Fatalf("journal = %+v", lines)
	}
	got := te.drain()
	if len(got) != 1 {
		t.Fatalf("posts = %+v", got)
	}
	a := got[0]
	if a.Labels["app"] != "blog" || a.Labels["kind"] != "event" || a.Labels["deployed_by"] == "" || a.Labels["release"] != "v3" {
		t.Errorf("labels = %v", a.Labels)
	}
	if !a.EndsAt.Equal(te.at.Add(5 * time.Minute)) {
		t.Errorf("endsAt = %v, want now+5m", a.EndsAt)
	}
}

func TestDeploySucceededOffByDefault(t *testing.T) {
	te := newTestEngine(t)
	ev := Event{Name: "deploy_succeeded", Kind: kindEvent, Severity: SeverityInfo, App: "blog", Source: "liveswap", Summary: "deployed v4"}
	te.report(ev)
	if got := te.drain(); len(got) != 0 {
		t.Fatalf("posted with deploy_succeeded off: %+v", got)
	}
	te.deploySucceeded = true
	te.report(ev)
	if got := te.drain(); len(got) != 1 {
		t.Fatalf("posts with deploy_succeeded on = %d", len(got))
	}
}

func TestOneShotBucket(t *testing.T) {
	te := newTestEngine(t)
	for range 6 {
		te.report(deployFailed())
		te.advance(time.Second)
	}
	if got := te.drain(); len(got) != 4 {
		t.Fatalf("posted %d one-shots in six seconds, want 4", len(got))
	}
	sup := 0
	for _, l := range te.lines("notify: event") {
		if l.ContextMap()["suppressed"] != nil {
			sup++
		}
	}
	if sup != 2 {
		t.Fatalf("suppressed lines = %d", sup)
	}
	te.advance(15 * time.Minute)
	te.report(deployFailed())
	if got := te.drain(); len(got) != 1 {
		t.Fatalf("after a refill: %d", len(got))
	}
}

func TestStateFiresRepostsAndResolvesAfterHold(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	got := te.drain()
	if len(got) != 1 || !got[0].EndsAt.IsZero() {
		t.Fatalf("firing post = %+v", got)
	}
	if _, ok := got[0].Labels["release"]; ok {
		t.Errorf("a state alert carries release as a label: %v", got[0].Labels)
	}
	if got[0].Annotations["release"] != "v2" {
		t.Errorf("annotations = %v", got[0].Annotations)
	}
	te.advance(30 * time.Second)
	te.tick()
	for _, a := range te.drain() {
		if a.Labels["alertname"] == "watchdog_throttled" {
			t.Fatalf("re-posted after 30 s")
		}
	}
	te.advance(31 * time.Second)
	te.tick()
	if n := countAlert(te.drain(), "watchdog_throttled"); n != 1 {
		t.Fatalf("re-posts after a minute = %d", n)
	}
	// Clear: held for 30 s, then resolved with endsAt now.
	te.report(watchdog(false))
	if n := countAlert(te.drain(), "watchdog_throttled"); n != 0 {
		t.Fatal("the resolve was posted without the hold")
	}
	te.advance(29 * time.Second)
	te.tick()
	if n := countAlert(te.drain(), "watchdog_throttled"); n != 0 {
		t.Fatal("resolved inside the hold")
	}
	te.advance(2 * time.Second)
	te.tick()
	got = te.drain()
	var resolved *amAlert
	for i := range got {
		if got[i].Labels["alertname"] == "watchdog_throttled" {
			resolved = &got[i]
		}
	}
	if resolved == nil || !resolved.EndsAt.Equal(te.at) {
		t.Fatalf("resolve = %+v", resolved)
	}
	states := map[string]int{}
	for _, l := range te.lines("notify: event") {
		states[l.ContextMap()["state"].(string)]++
	}
	if states["firing"] != 1 || states["resolved"] != 1 {
		t.Fatalf("journal states = %v (re-posts are not events)", states)
	}
	if len(te.states) != 0 {
		t.Fatal("the key outlived its resolve")
	}
}

func TestStateRefireInsideHoldIsNoRecovery(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	te.report(watchdog(false))
	te.advance(10 * time.Second)
	te.report(watchdog(true))
	te.advance(time.Minute)
	te.tick()
	for _, a := range te.drain() {
		if a.Labels["alertname"] == "watchdog_throttled" && !a.EndsAt.IsZero() {
			t.Fatal("a recovery was posted for a condition that came back inside the hold")
		}
	}
}

func TestStateHoldFromTheDetector(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	ev := watchdog(false)
	ev.Hold = 2 * time.Minute // watchdog_grace 2m
	te.report(ev)
	te.drain()
	te.advance(time.Minute)
	te.tick()
	for _, a := range te.drain() {
		if a.Labels["alertname"] == "watchdog_throttled" && !a.EndsAt.IsZero() {
			t.Fatal("resolved before max(30s, watchdog_grace)")
		}
	}
	te.advance(61 * time.Second)
	te.tick()
	resolvedSeen := false
	for _, a := range te.drain() {
		if a.Labels["alertname"] == "watchdog_throttled" && !a.EndsAt.IsZero() {
			resolvedSeen = true
		}
	}
	if !resolvedSeen {
		t.Fatal("not resolved after the detector's hold")
	}
}

// TestStateFlappingGuard: an app that fails and recovers every five
// minutes for an hour gets at most four messages (two cycles), not
// twelve cycles' worth.
func TestStateFlappingGuard(t *testing.T) {
	te := newTestEngine(t)
	transitions := 0
	for range 12 {
		te.report(watchdog(true))
		te.advance(2 * time.Minute)
		te.tick()
		te.report(watchdog(false))
		te.advance(3 * time.Minute)
		te.tick()
	}
	for _, l := range te.lines("notify: event") {
		if l.ContextMap()["held"] == nil {
			transitions++
		}
	}
	// Four tokens to start, and four more over the hour's refill.
	if transitions > 8 {
		t.Fatalf("%d transitions in an hour of flapping", transitions)
	}
	if transitions < 4 {
		t.Fatalf("only %d transitions: the guard swallowed what it should pass", transitions)
	}
}

func TestBoxEventCarriesNoAppLabel(t *testing.T) {
	te := newTestEngine(t)
	te.report(Event{Name: "backup_stale", Kind: kindState, Severity: SeverityCritical, Instance: "blog", Source: "backup", Summary: "x", Firing: true})
	got := te.drain()
	if len(got) != 1 {
		t.Fatal(got)
	}
	if _, ok := got[0].Labels["app"]; ok || got[0].Labels["subject"] != "box" || got[0].Labels["instance"] != "blog" ||
		got[0].Labels["key"] != "backup/box/backup_stale/blog" {
		t.Fatalf("labels = %v", got[0].Labels)
	}
}

func TestMute(t *testing.T) {
	te := newTestEngine(t)
	te.mute("blog", true)
	got := te.drain()
	if len(got) != 1 || got[0].Labels["alertname"] != "deploy_in_progress" || !got[0].EndsAt.IsZero() || got[0].Labels["app"] != "blog" {
		t.Fatalf("mute = %+v", got)
	}
	te.advance(61 * time.Second)
	te.tick()
	if n := countAlert(te.drain(), "deploy_in_progress"); n != 1 {
		t.Fatalf("a long deploy's mute was not re-posted: %d", n)
	}
	te.mute("blog", false)
	got = te.drain()
	if len(got) != 1 || !got[0].EndsAt.Equal(te.at) {
		t.Fatalf("unmute = %+v", got)
	}
	if len(te.lines("notify: event")) != 0 {
		t.Fatal("the mute is not an event")
	}
}

func TestHeartbeat(t *testing.T) {
	te := newTestEngine(t)
	te.tick()
	got := te.drain()
	if len(got) != 1 || got[0].Labels["alertname"] != "Watchdog" || !got[0].EndsAt.Equal(te.at.Add(4*time.Minute)) {
		t.Fatalf("heartbeat = %+v", got)
	}
	te.advance(30 * time.Second)
	te.tick()
	if n := countAlert(te.drain(), "Watchdog"); n != 0 {
		t.Fatal("two heartbeats inside a minute")
	}
}

func TestSummaryRedactedAndCut(t *testing.T) {
	te := newTestEngine(t)
	ev := deployFailed()
	ev.Summary = "posting to https://hooks.slack.com/services/T0/B0/secretpart failed " + strings.Repeat("x", 300) // gitleaks:allow
	te.report(ev)
	got := te.drain()
	s := got[0].Annotations["summary"]
	if strings.Contains(s, "secretpart") || len([]rune(s)) > maxSummary {
		t.Fatalf("summary = %q", s)
	}
	if l := te.lines("notify: event"); strings.Contains(l[0].ContextMap()["summary"].(string), "secretpart") {
		t.Fatal("the journal line holds the secret")
	}
}

func TestDeliveryRetriedThenGivenUp(t *testing.T) {
	te := newTestEngine(t)
	te.p.err = &netErr{syscall.ECONNREFUSED}
	te.report(deployFailed())
	before := testutil.ToFloat64(te.failures.c)
	again, wait := te.attempt(context.Background())
	if again || wait != 2*time.Second {
		t.Fatalf("first failure: again %v wait %v", again, wait)
	}
	te.advance(4 * time.Minute)
	if again, _ := te.attempt(context.Background()); again {
		t.Fatal("given up before five minutes")
	}
	// Alertmanager back inside the window: delivered.
	te.p.err = nil
	if again, _ := te.attempt(context.Background()); !again || len(te.p.take()) != 1 {
		t.Fatal("not delivered once Alertmanager answered")
	}
	// Down for longer than the window: a line and the counter.
	te.p.err = &netErr{syscall.ECONNREFUSED}
	te.report(Event{Name: "rollback", Kind: kindEvent, Severity: SeverityWarning, App: "blog", Source: "liveswap", Summary: "rolled back to v1"})
	te.attempt(context.Background())
	te.advance(5 * time.Minute)
	te.attempt(context.Background())
	lines := te.lines("notify: delivery failed")
	if len(lines) != 1 || lines[0].Level != zap.ErrorLevel || lines[0].ContextMap()["key"] != "liveswap/app:blog/rollback" {
		t.Fatalf("delivery failed lines = %+v", lines)
	}
	if after := testutil.ToFloat64(te.failures.c); after != before+1 {
		t.Fatalf("counter %v → %v", before, after)
	}
	if len(te.queue) != 0 {
		t.Fatal("a given-up post stayed queued")
	}
}

func TestDeliveryRefusedIsFinal(t *testing.T) {
	te := newTestEngine(t)
	te.p.err = &amStatusError{path: "/api/v2/alerts", code: 400, msg: "bad"}
	te.report(deployFailed())
	if again, _ := te.attempt(context.Background()); !again {
		t.Fatal("a refusal was retried")
	}
	if len(te.lines("notify: delivery failed")) != 1 || len(te.queue) != 0 {
		t.Fatal("a refused post was not given up")
	}
}

func TestInactiveEngineDropsEvents(t *testing.T) {
	te := newTestEngine(t)
	te.active = nil
	te.report(deployFailed())
	te.mute("blog", true)
	te.tick()
	if len(te.queue) != 0 {
		t.Fatal("an engine no config runs took events")
	}
}

// TestRunPostsThroughTheGoroutine runs the real loop against a fake
// Alertmanager on a socket.
func TestRunPostsThroughTheGoroutine(t *testing.T) {
	f, sock := startFakeAM(t)
	e := newEngine()
	a := &App{}
	e.configure(a, newAMClient(sock), nil, zap.NewNop())
	e.report(deployFailed())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := 0
		for _, p := range f.posts {
			if strings.Contains(string(p), "deploy_failed") {
				n++
			}
		}
		f.mu.Unlock()
		if n == 1 {
			e.release(a)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.release(a)
	t.Fatal("the goroutine did not post the event")
}

type netErr struct{ errno syscall.Errno }

func (e *netErr) Error() string { return "dial unix: " + e.errno.Error() }
func (e *netErr) Unwrap() error { return e.errno }

func countAlert(as []amAlert, name string) int {
	n := 0
	for _, a := range as {
		if a.Labels["alertname"] == name {
			n++
		}
	}
	return n
}
