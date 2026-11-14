package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	te.fail.started = te.at // the stamps tests give are compared with it, not the host's clock
	te.logger = zap.New(core)
	te.post = te.p
	te.active = &App{}
	te.known = []string{"NOTIFY_OPS_URL=https://hooks.slack.com/services/T0/B0/secretpart"} // gitleaks:allow
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
	lines := te.lines("monitor: event")
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
	for _, l := range te.lines("monitor: event") {
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

// TestEachOneShotIsItsOwnAlert: two identical one-shots are two alerts
// to Alertmanager, apart in occurred_at alone, so each reaches the
// channels; two in one clock reading too.
func TestEachOneShotIsItsOwnAlert(t *testing.T) {
	te := newTestEngine(t)
	te.report(deployFailed())
	te.report(deployFailed()) // the same instant
	te.advance(2 * time.Minute)
	te.report(deployFailed())
	got := te.drain()
	if len(got) != 3 {
		t.Fatalf("posts = %d, want 3", len(got))
	}
	seen := map[string]bool{}
	for i, a := range got {
		v := a.Labels[labelOccurredAt]
		if v == "" || seen[v] {
			t.Fatalf("alert %d: occurred_at %q, seen %v", i, v, seen)
		}
		seen[v] = true
		if want := a.StartsAt.Format(occurredAtLayout); v != want {
			t.Errorf("alert %d: occurred_at %q, startsAt %q", i, v, want)
		}
		if !a.EndsAt.Equal(a.StartsAt.Add(oneShotLife)) {
			t.Errorf("alert %d: endsAt %v, startsAt %v", i, a.EndsAt, a.StartsAt)
		}
		// Through the filter, as a summary is, it is left whole.
		if r, _ := liveswap.NewRedactor(te.known, nil).Redact(v); r != v {
			t.Errorf("the filter masks occurred_at %q as %q", v, r)
		}
		// Apart from it, the labels and annotations are the first's.
		l := maps.Clone(a.Labels)
		delete(l, labelOccurredAt)
		l0 := maps.Clone(got[0].Labels)
		delete(l0, labelOccurredAt)
		if !maps.Equal(l, l0) || !maps.Equal(a.Annotations, got[0].Annotations) {
			t.Errorf("alert %d differs from the first beyond occurred_at: %v, %v", i, a.Labels, got[0].Labels)
		}
	}
	if got[0].Labels[labelOccurredAt] != "2026-11-10T12:00:00.000000000Z" ||
		got[1].Labels[labelOccurredAt] != "2026-11-10T12:00:00.000000001Z" ||
		got[2].Labels[labelOccurredAt] != "2026-11-10T12:02:00.000000000Z" {
		t.Errorf("occurred_at = %s, %s, %s", got[0].Labels[labelOccurredAt], got[1].Labels[labelOccurredAt], got[2].Labels[labelOccurredAt])
	}
	// A clock stepped back by more than a second is taken as it reads
	// (another key: the bucket's refill runs backwards too).
	te.advance(-time.Hour)
	ev := deployFailed()
	ev.Name = "rollback"
	te.report(ev)
	if got := te.drain(); len(got) != 1 || got[0].Labels[labelOccurredAt] != "2026-11-10T11:02:00.000000000Z" {
		t.Fatalf("after a step back: %+v", got)
	}
}

// attemptPoster records every attempt, failed ones too.
type attemptPoster struct {
	attempts [][]byte
	err      error
}

func (p *attemptPoster) postAlerts(_ context.Context, a []amAlert) error {
	b, _ := json.Marshal(a)
	p.attempts = append(p.attempts, b)
	return p.err
}

// TestRetriedOneShotIsTheSameAlert: the queue's retry of a one-shot
// posts the very alert of the first attempt, so it stays one.
func TestRetriedOneShotIsTheSameAlert(t *testing.T) {
	te := newTestEngine(t)
	p := &attemptPoster{err: &netErr{syscall.ECONNREFUSED}}
	te.post = p
	te.report(deployFailed())
	te.attempt(context.Background())
	te.advance(30 * time.Second)
	te.attempt(context.Background())
	p.err = nil
	te.advance(30 * time.Second)
	if again, _ := te.attempt(context.Background()); !again {
		t.Fatal("not delivered once Alertmanager answered")
	}
	if len(p.attempts) != 3 || !bytes.Equal(p.attempts[0], p.attempts[1]) || !bytes.Equal(p.attempts[0], p.attempts[2]) {
		t.Fatalf("attempts differ: %s", bytes.Join(p.attempts, []byte("\n")))
	}
	if !bytes.Contains(p.attempts[0], []byte(`"occurred_at":"2026-11-10T12:00:00.000000000Z"`)) {
		t.Fatalf("attempt = %s", p.attempts[0])
	}
}

// TestOnlyOneShotsCarryOccurredAt: a state alert's identity is stable
// across its re-posts and its resolve, and the mute's and the
// heartbeat's are fixed; none carries occurred_at.
func TestOnlyOneShotsCarryOccurredAt(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	te.mute("blog", true)
	te.tick()
	te.advance(61 * time.Second)
	te.tick() // a re-post, the mute's and a heartbeat again
	te.report(watchdog(false))
	te.mute("blog", false)
	te.advance(31 * time.Second)
	te.tick() // the resolve
	names := map[string]int{}
	for _, a := range te.drain() {
		names[a.Labels["alertname"]]++
		if _, ok := a.Labels[labelOccurredAt]; ok {
			t.Errorf("%s carries occurred_at: %v", a.Labels["alertname"], a.Labels)
		}
	}
	if names["watchdog_throttled"] < 3 || names[alertDeployMute] < 3 || names[alertWatchdog] < 2 {
		t.Fatalf("posted = %v", names)
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
	for _, l := range te.lines("monitor: event") {
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
	for _, l := range te.lines("monitor: event") {
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
	if len(te.lines("monitor: event")) != 0 {
		t.Fatal("the mute is not an event")
	}
}

// resolves are the posts that end a watchdog_throttled alert, in order.
func resolves(as []amAlert) []amAlert {
	var out []amAlert
	for _, a := range as {
		if a.Labels["alertname"] == "watchdog_throttled" && !a.EndsAt.IsZero() {
			out = append(out, a)
		}
	}
	return out
}

// TestRecoveryHeldUnderTheMute: Alertmanager 0.28.1 mutes a resolve
// under the inhibit rule as it does a firing alert, then drops it as
// sent, so a watchdog_* recovery whose hold ends while its app's deploy
// runs is held, kept alive, and posted once right behind the mute's end
// (the finding's first scenario: a second deploy inside the hold).
func TestRecoveryHeldUnderTheMute(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	te.drain()
	te.report(watchdog(false)) // a deploy's success ended the loop
	te.advance(10 * time.Second)
	te.mute("blog", true) // the next deploy, inside the hold
	te.drain()
	// About three minutes under the mute: never resolved, and kept alive
	// by the re-posts, so resolve_timeout does not end it unsaid.
	reposts := 0
	for range 6 {
		te.advance(31 * time.Second)
		te.tick()
		got := te.drain()
		if r := resolves(got); len(r) != 0 {
			t.Fatalf("resolved under the mute: %+v", r)
		}
		reposts += countAlert(got, "watchdog_throttled")
	}
	if reposts < 2 {
		t.Fatalf("re-posts while held = %d", reposts)
	}
	te.mute("blog", false)
	got := te.drain()
	if len(got) != 2 || got[0].Labels["alertname"] != "deploy_in_progress" || !got[0].EndsAt.Equal(te.at) ||
		len(resolves(got[1:])) != 1 || !got[1].EndsAt.Equal(te.at) {
		t.Fatalf("at the mute's end, want its end and then the resolve: %+v", got)
	}
	var states []string
	for _, l := range te.lines("monitor: event") {
		states = append(states, l.ContextMap()["state"].(string))
	}
	if strings.Join(states, ",") != "firing,resolved" {
		t.Fatalf("journal states = %v", states)
	}
	te.advance(time.Minute)
	te.tick()
	if n := countAlert(te.drain(), "watchdog_throttled"); n != 0 || len(te.states) != 0 {
		t.Fatalf("after the resolve: %d posts, table %v", n, te.states)
	}
}

// The second scenario: the recovery's hold ends while a deploy that
// fails still runs; its end, whatever it came to, lets it go.
func TestRecoveryHoldEndsInsideADeploy(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	te.mute("blog", true)
	te.report(watchdog(false)) // the old instance recovered
	te.drain()
	te.advance(31 * time.Second)
	te.tick()
	if r := resolves(te.drain()); len(r) != 0 {
		t.Fatalf("resolved under the mute: %+v", r)
	}
	te.report(deployFailed())
	te.mute("blog", false)
	if r := resolves(te.drain()); len(r) != 1 {
		t.Fatalf("resolves at the mute's end = %+v", r)
	}
}

// Fired again before the mute ends: no recovery was, so none is posted
// at its end, and the alert stays firing.
func TestRecoveryHeldUnderTheMuteThenFiringAgain(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	te.report(watchdog(false))
	te.mute("blog", true)
	te.advance(31 * time.Second)
	te.tick()
	te.report(watchdog(true))
	te.mute("blog", false)
	te.advance(time.Minute)
	te.tick()
	if r := resolves(te.drain()); len(r) != 0 {
		t.Fatalf("a recovery was posted over the alert firing again: %+v", r)
	}
	if st := te.states[watchdog(true).key()]; st == nil || !st.firing {
		t.Fatalf("the key: %+v", st)
	}
}

// Only what the inhibit rule mutes is held: another of the app's states
// (relaunch_refused is not watchdog_*) and another app's watchdog
// resolve after their hold, as before.
func TestMuteHoldsOnlyItsAppsWatchdog(t *testing.T) {
	te := newTestEngine(t)
	refused := watchdog(true)
	refused.Name = "relaunch_refused"
	shop := watchdog(true)
	shop.App = "shop"
	te.report(refused)
	te.report(shop)
	te.mute("blog", true)
	refused.Firing, shop.Firing = false, false
	te.report(refused)
	te.report(shop)
	te.drain()
	te.advance(31 * time.Second)
	te.tick()
	ended := map[string]bool{}
	for _, a := range te.drain() {
		if a.EndsAt.Equal(te.at) {
			ended[a.Labels["key"]] = true
		}
	}
	if !ended[refused.key()] || !ended[shop.key()] || len(ended) != 2 {
		t.Fatalf("ended under blog's mute: %v", ended)
	}
}

// The mute's end posts only what it held: a producer's state of the app
// whose hold has passed is the tick's, which writes external.json after
// it (persistExternal).
func TestMuteEndLeavesOtherStatesToTheTick(t *testing.T) {
	te := newTestEngine(t)
	ext := Event{Name: "queue_stuck", Kind: kindState, Severity: SeverityWarning, App: "blog", Source: sourceExternal + "cron", Summary: "x", Firing: true}
	te.report(ext)
	te.mute("blog", true)
	ext.Firing = false
	te.report(ext)
	te.drain()
	te.advance(31 * time.Second) // its hold passed between two ticks
	te.mute("blog", false)
	for _, a := range te.drain() {
		if a.Labels["alertname"] == "queue_stuck" {
			t.Fatalf("the mute's end posted another state: %+v", a)
		}
	}
	te.tick()
	if got := te.drain(); countAlert(got, "queue_stuck") != 1 {
		t.Fatalf("the tick's resolve = %+v", got)
	}
}

// A reload that drops an app whose deploy runs ends its mute before it
// resolves the app's keys: under the mute the resolve would never be
// sent.
func TestPruneEndsTheMuteBeforeTheResolve(t *testing.T) {
	te := newTestEngine(t)
	te.report(watchdog(true))
	te.mute("blog", true)
	te.drain()
	te.prune([]string{"shop"})
	got := te.drain()
	if len(got) != 2 || got[0].Labels["alertname"] != "deploy_in_progress" || got[0].EndsAt.IsZero() || len(resolves(got[1:])) != 1 {
		t.Fatalf("want the mute's end, then the resolve: %+v", got)
	}
}

// A restart mid-deploy: the deploy ended with the process, and its mute
// lives on in Alertmanager to resolve_timeout. The new process's first
// configure ends every app's, before anything it posts, so a recovery
// it tells (a standing one's, after its hold) is not sent under it; so
// does a configure after a reload took the block out, which forgot the
// mutes, and a reload's does not.
func TestFirstConfigureEndsTheLastProcessMutes(t *testing.T) {
	useStateDir(t, t.TempDir())
	useSocket(t, filepath.Join(t.TempDir(), "absent.sock"))
	e := newEngine()
	a := &App{BackupRecord: "off", cfg: &resolved{apps: []string{"blog", "shop"}}, logger: zap.NewNop()}
	p := &recPoster{}
	e.configure(a, p, nil, zap.NewNop())
	var first []amAlert
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		p.mu.Lock()
		if len(p.posts) > 0 {
			first = p.posts[0]
		}
		p.mu.Unlock()
		if first != nil {
			break
		}
	}
	ended := map[string]bool{}
	for _, m := range first {
		if m.Labels["alertname"] == "deploy_in_progress" && !m.EndsAt.IsZero() && m.EndsAt.Equal(m.StartsAt) {
			ended[m.Labels["app"]] = true
		}
	}
	if len(first) != 2 || !ended["blog"] || !ended["shop"] {
		t.Fatalf("the first post = %+v", first)
	}
	// A second config (a reload) ends none: a deploy running now is this
	// engine's. Its Alertmanager refuses, so what it queued stays.
	e.mute("blog", true)
	down := &recPoster{err: &netErr{syscall.ECONNREFUSED}}
	b := &App{BackupRecord: "off", cfg: &resolved{apps: []string{"blog", "shop"}}, logger: zap.NewNop()}
	e.configure(b, down, nil, zap.NewNop())
	mutesEnded := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return slices.ContainsFunc(e.queue, func(q *pending) bool { return q.key == "mute/" })
	}
	if mutesEnded() {
		t.Fatal("a reload ended the mutes")
	}
	// The block taken out by a reload (the last config's Stop) forgets
	// blog's mute with the table; one that brings it back ends it.
	e.release(b)
	e.release(a)
	c := &App{BackupRecord: "off", cfg: &resolved{apps: []string{"blog"}}, logger: zap.NewNop()}
	e.configure(c, down, nil, zap.NewNop())
	defer e.release(c)
	if !mutesEnded() {
		t.Fatal("the block brought back ended no mute")
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
	if l := te.lines("monitor: event"); strings.Contains(l[0].ContextMap()["summary"].(string), "secretpart") {
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
	lines := te.lines("monitor: delivery failed")
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

// TestDeliveryServerErrorIsRetried: a 5xx is Alertmanager's trouble,
// not a refusal of the alert.
func TestDeliveryServerErrorIsRetried(t *testing.T) {
	te := newTestEngine(t)
	te.p.err = &amStatusError{path: "/api/v2/alerts", code: 503, msg: "busy"}
	te.report(deployFailed())
	if again, wait := te.attempt(context.Background()); again || wait == 0 {
		t.Fatal("a 503 was given up at once")
	}
	te.p.err = nil
	if again, _ := te.attempt(context.Background()); !again || len(te.p.take()) != 1 {
		t.Fatal("not delivered once Alertmanager answered")
	}
}

func TestDeliveryRefusedIsFinal(t *testing.T) {
	te := newTestEngine(t)
	te.p.err = &amStatusError{path: "/api/v2/alerts", code: 400, msg: "bad"}
	te.report(deployFailed())
	if again, _ := te.attempt(context.Background()); !again {
		t.Fatal("a refusal was retried")
	}
	if len(te.lines("monitor: delivery failed")) != 1 || len(te.queue) != 0 {
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

// TestReleaseFallsBackToTheServingConfig: Caddy stops a new config whose
// reload failed after notify's Start, and the old one serves on.
func TestReleaseFallsBackToTheServingConfig(t *testing.T) {
	e := newEngine()
	old, nu := &App{}, &App{}
	p := &recPoster{}
	e.configure(old, p, nil, zap.NewNop())
	e.configure(nu, p, nil, zap.NewNop())
	e.release(nu)
	e.mu.Lock()
	active, running := e.active, e.running
	e.mu.Unlock()
	if active != old || !running {
		t.Fatalf("after the failed reload: active is old %v, running %v", active == old, running)
	}
	// A reload that succeeds: the new starts, the old stops; the new
	// keeps the engine.
	e.configure(nu, p, nil, zap.NewNop())
	e.release(old)
	if e.active != nu {
		t.Fatal("a successful reload's old Stop took the engine")
	}
	e.release(nu)
	if e.active != nil || e.running {
		t.Fatal("the last config's Stop left the engine running")
	}
}

// TestReleaseReappliesTheServingRender: a reload that fails after
// notify's Start leaves the candidate's render in the state directory
// and in Alertmanager; going back to the serving config puts its render
// back and reloads.
func TestReleaseReappliesTheServingRender(t *testing.T) {
	f, sock := startFakeAM(t)
	dir := t.TempDir()
	useStateDir(t, dir)
	useSocket(t, sock)
	mk := func(cfg string) *App {
		return &App{rendered: []byte(cfg), files: map[string]string{}, logger: zap.NewNop()}
	}
	e := newEngine()
	saved := theEngine
	theEngine = e
	t.Cleanup(func() { theEngine = saved })
	old, nu := mk("serving\n"), mk("candidate\n")
	am := newAMClient(sock)
	old.applyRender(am)
	e.configure(old, am, nil, zap.NewNop())
	nu.applyRender(am)
	e.configure(nu, am, nil, zap.NewNop())
	if got, _ := os.ReadFile(filepath.Join(dir, ConfigName)); string(got) != "candidate\n" {
		t.Fatalf("after the candidate's Start: %q", got)
	}
	f.mu.Lock()
	before := f.reloads
	f.mu.Unlock()
	e.release(nu)
	if got, _ := os.ReadFile(filepath.Join(dir, ConfigName)); string(got) != "serving\n" {
		t.Fatalf("after the failed reload the file is %q, want the serving config's", got)
	}
	f.mu.Lock()
	after := f.reloads
	f.mu.Unlock()
	if after != before+1 {
		t.Fatalf("reloads %d → %d: Alertmanager was not told", before, after)
	}
	e.release(old)
}

// TestReleaseAtAReloadSendsNothing: a reload that leaves no config
// running notify (the block taken out) gives Alertmanager the config
// that sends nothing and removes the URL files, so what it holds ends
// at resolve_timeout with no recovery on any channel; motel's own
// shutdown (no config running after it) leaves the render, which keeps
// what fired alive across a restart.
func TestReleaseAtAReloadSendsNothing(t *testing.T) {
	orig := configRunning
	t.Cleanup(func() { configRunning = orig })
	for _, reload := range []bool{true, false} {
		f, sock := startFakeAM(t)
		dir := t.TempDir()
		useStateDir(t, dir)
		useSocket(t, sock)
		a := &App{rendered: []byte("rendered\n"),
			files: map[string]string{urlFileName("NOTIFY_OPS_URL"): "https://ntfy.sh/ops"}, logger: zap.NewNop()}
		am := newAMClient(sock)
		a.applyRender(am)
		e := newEngine()
		e.configure(a, am, nil, zap.NewNop())
		f.mu.Lock()
		before := f.reloads
		f.mu.Unlock()
		configRunning = func() bool { return reload }
		e.release(a)
		cfg, _ := os.ReadFile(filepath.Join(dir, ConfigName))
		_, urlErr := os.Stat(filepath.Join(dir, urlFileName("NOTIFY_OPS_URL")))
		f.mu.Lock()
		after := f.reloads
		f.mu.Unlock()
		if reload {
			if string(cfg) != string(defaultConfig) || !os.IsNotExist(urlErr) || after != before+1 {
				t.Errorf("a reload without the block: config %q, url file %v, reloads %d → %d", cfg, urlErr, before, after)
			}
			continue
		}
		if string(cfg) != "rendered\n" || urlErr != nil || after != before {
			t.Errorf("a shutdown: config %q, url file %v, reloads %d → %d", cfg, urlErr, before, after)
		}
	}
}

// TestReleaseOfTheLastConfigClearsTheTable: with no config running
// notify, nothing can clear what the table holds, and a block added
// back later must not fire it again.
func TestReleaseOfTheLastConfigClearsTheTable(t *testing.T) {
	e := newEngine()
	a := &App{}
	e.configure(a, &recPoster{}, nil, zap.NewNop())
	e.report(watchdog(true))
	e.mute("blog", true)
	e.release(a)
	if len(e.states) != 0 || len(e.mutes) != 0 || len(e.buckets) != 0 {
		t.Fatalf("left: states %v mutes %v buckets %v", e.states, e.mutes, e.buckets)
	}
	b := &App{}
	e.configure(b, &recPoster{}, nil, zap.NewNop())
	defer e.release(b)
	e.mu.Lock()
	n := len(e.states)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("the old key came back with the block")
	}
}

// A standing report (liveswap's ReportStanding at Start) is what a
// detector told before: ignored for a key the table holds, taken as
// told for one it does not, after a motel restart emptied the table
// while Alertmanager kept the alert.
func TestStandingReports(t *testing.T) {
	standing := func(firing bool) Event {
		ev := watchdog(firing)
		ev.Standing = true
		return ev
	}
	t.Run("firing, the table empty: it fires", func(t *testing.T) {
		te := newTestEngine(t)
		te.report(standing(true))
		got := te.drain()
		if countAlert(got, "watchdog_throttled") != 1 || !got[0].EndsAt.IsZero() {
			t.Fatalf("posts = %+v", got)
		}
		if l := te.lines("monitor: event"); len(l) != 1 || l[0].ContextMap()["state"] != "firing" || l[0].ContextMap()["standing"] != true {
			t.Fatalf("journal = %+v", l)
		}
	})
	t.Run("the table holds it: ignored", func(t *testing.T) {
		te := newTestEngine(t)
		te.report(watchdog(true))
		te.report(watchdog(false)) // a recovery's hold under way
		te.drain()
		te.lines("monitor: event")
		te.advance(20 * time.Second)
		te.report(standing(true))
		te.report(standing(false))
		if got := te.drain(); len(got) != 0 {
			t.Fatalf("a standing report was posted: %+v", got)
		}
		te.advance(11 * time.Second) // the hold from the first clear, not from the standing one
		te.tick()
		if got := te.drain(); countAlert(got, "watchdog_throttled") != 1 || got[0].EndsAt.IsZero() {
			t.Fatalf("the recovery after the hold = %+v", got)
		}
	})
	t.Run("a recovery under way, the table empty: held, kept alive, resolved after the hold", func(t *testing.T) {
		te := newTestEngine(t)
		ev := standing(false)
		ev.Hold = 45 * time.Second
		te.report(ev)
		got := te.drain()
		if countAlert(got, "watchdog_throttled") != 1 || !got[0].EndsAt.IsZero() {
			t.Fatalf("not kept alive: %+v", got)
		}
		te.advance(44 * time.Second)
		te.tick()
		for _, a := range te.drain() {
			if a.Labels["alertname"] == "watchdog_throttled" && !a.EndsAt.IsZero() {
				t.Fatal("resolved inside the hold")
			}
		}
		te.advance(2 * time.Second)
		te.tick()
		if got := te.drain(); countAlert(got, "watchdog_throttled") != 1 || !got[0].EndsAt.Equal(te.at) {
			t.Fatalf("the resolve = %+v", got)
		}
		states := []string{}
		for _, l := range te.lines("monitor: event") {
			// Only the report itself was told before this process.
			states = append(states, fmt.Sprintf("%s:%v", l.ContextMap()["state"], l.ContextMap()["standing"]))
		}
		if strings.Join(states, ",") != "held:true,resolved:<nil>" {
			t.Fatalf("journal states = %v", states)
		}
	})
	t.Run("firing, then a recovery this process saw: its lines are not standing", func(t *testing.T) {
		te := newTestEngine(t)
		te.report(standing(true))
		te.drain()
		te.report(watchdog(false))
		te.advance(31 * time.Second)
		te.tick()
		te.drain()
		var states []string
		for _, l := range te.lines("monitor: event") {
			states = append(states, fmt.Sprintf("%s:%v", l.ContextMap()["state"], l.ContextMap()["standing"]))
		}
		if strings.Join(states, ",") != "firing:true,resolved:<nil>" {
			t.Fatalf("journal states = %v", states)
		}
	})
}
