package liveswap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/goleak"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
)

// The handler in the first moments of a start (#473): Caddy has
// provisioned the config and may have started the http app, but
// liveswap's Start has not yet installed the apps on their pooled
// state. These tests put the handler there by hand: the pooled app
// holds no spec and no sources, and the config holds the sources its
// Provision built.

// notInstalled is newTestHandler as a start leaves it before liveswap's
// Start: the pooled app as newManagedApp makes it, its collaborators
// the rig's fakes, and demo's sources on the config alone.
func notInstalled(t *testing.T) (*Handler, *testRig) {
	t.Helper()
	h, rig := newTestHandler(t)
	h.app.appVerifiers = map[string][]deploytrust.Verifier{"demo": rig.ma.verifiers}
	h.app.specs = map[string]*appSpec{"demo": rig.spec}
	rig.ma.spec, rig.ma.verifiers = nil, nil
	return h, rig
}

// install is what liveswap's Start does for demo: configure it with the
// spec and sources the config built.
func install(h *Handler, rig *testRig) {
	rig.ma.configure(h.app, h.app.specs["demo"], h.app.appVerifiers["demo"], zap.NewNop(), &fetchClients{}, nil)
}

// answer is one request served, for a goroutine: no t.Fatal off the
// test's own.
type answer struct {
	w    *httptest.ResponseRecorder
	err  error
	took time.Duration
}

func serveOne(h *Handler, req *http.Request) answer {
	w := httptest.NewRecorder()
	began := time.Now()
	err := h.ServeHTTP(w, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil }))
	return answer{w, err, time.Since(began)}
}

// demoGet is GET /demo with tok as the bearer ("" for none).
func demoGet(tok string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/demo", nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req
}

// TestDeployTokenNotRefusedBeforeStart is #473: a valid deploy token
// sent before liveswap's Start installed its app was the flat 401. It
// is answered once the app is installed, as late as the install.
func TestDeployTokenNotRefusedBeforeStart(t *testing.T) {
	h, rig := notInstalled(t)
	got := make(chan answer, 1)
	go func() { got <- serveOne(h, demoGet(appToken(t))) }()
	select {
	case a := <-got:
		t.Fatalf("a valid deploy token before Start was answered before the install: %d %s", a.w.Code, strings.TrimSpace(a.w.Body.String()))
	case <-time.After(60 * time.Millisecond):
	}
	install(h, rig)
	a := <-got
	if a.err != nil || a.w.Code != http.StatusOK {
		t.Fatalf("a valid deploy token, installed 60 ms later: %d %s (%v)", a.w.Code, strings.TrimSpace(a.w.Body.String()), a.err)
	}
	if a.took > 2*time.Second {
		t.Errorf("answered %v after it was sent, long after the install", a.took)
	}
}

// TestInstallWaitIs5s pins the bound: long enough for a Start that
// probes the sandbox on a busy box, short enough that a CI run is told
// to come back rather than held.
func TestInstallWaitIs5s(t *testing.T) {
	if installWait != 5*time.Second {
		t.Fatalf("installWait is %v, want 5s", installWait)
	}
}

// pendingTimer waits for the handler to set its timer on c, and returns
// when it fires.
func pendingTimer(t *testing.T, c *fakeClock) time.Time {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		c.mu.Lock()
		n := len(c.waiters)
		var at time.Time
		if n > 0 {
			at = c.waiters[0].at
		}
		c.mu.Unlock()
		if n > 0 {
			return at
		}
	}
	t.Fatal("the handler set no timer")
	return time.Time{}
}

// TestDeployTokenBeforeStartPastTheBound: a valid deploy token whose app
// no Start installs within installWait is told to come back, a 503 with
// Retry-After in the handler's error style and no Motel- header, and
// the journal says so; nothing of the request ran. The bound is the
// handler's own, on a fake clock: not answered a millisecond before
// it, answered at it. Once the app is
// installed the status is answered, at once.
func TestDeployTokenBeforeStartPastTheBound(t *testing.T) {
	h, rig := notInstalled(t)
	core, logs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	fc := newFakeClock()
	h.waitClock = fc
	deploy := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/demo", strings.NewReader(`{"url":"https://x/a.tgz","version":"v1"}`))
		req.Header.Set("Authorization", "Bearer "+appToken(t))
		return req
	}
	for _, req := range []*http.Request{demoGet(appToken(t)), deploy()} {
		got := make(chan answer, 1)
		go func() { got <- serveOne(h, req) }()
		if at := pendingTimer(t, fc); !at.Equal(fc.Now().Add(installWait)) {
			t.Fatalf("%s: the timer fires %v from now, want %v", req.Method, at.Sub(fc.Now()), installWait)
		}
		fc.Advance(installWait - time.Millisecond)
		select {
		case a := <-got:
			t.Fatalf("%s answered before the bound: %d %s", req.Method, a.w.Code, a.w.Body)
		case <-time.After(20 * time.Millisecond):
		}
		fc.Advance(time.Millisecond)
		a := <-got
		if a.err != nil || a.w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s before Start, past the bound: %d %s (%v)", req.Method, a.w.Code, a.w.Body, a.err)
		}
		const body = `{"error":"motel is starting and has not loaded this app yet; retry after 1 s"}` + "\n"
		hdr := a.w.Header()
		if a.w.Body.String() != body || hdr.Get("Retry-After") != "1" || hdr.Get("Content-Type") != "application/json" {
			t.Errorf("%s past the bound: %v %q", req.Method, hdr, a.w.Body)
		}
		wantNoMotelHeader(t, a.w)
	}
	if rig.fetch.lastReq.version != "" {
		t.Errorf("a deploy ran before the app was installed: %+v", rig.fetch.lastReq)
	}
	lines := logs.FilterMessage("deploy token verified before liveswap installed the app; answered 503").All()
	if len(lines) != 2 || lines[0].ContextMap()["app"] != "demo" || lines[0].ContextMap()["request_ended"] != false || lines[0].ContextMap()["waited"] != installWait {
		t.Errorf("journal: %v", logs.All())
	}

	install(h, rig)
	if a := serveOne(h, demoGet(appToken(t))); a.w.Code != http.StatusOK || a.took > time.Second {
		t.Errorf("after the install: %d %s in %v", a.w.Code, a.w.Body, a.took)
	}
}

// TestTrustIsOneRead: the handler takes an app's sources and whether it
// is installed in one read (trust), beside the install that sets both.
// No read sees the one without the other; under -race, a read outside
// specMu is reported here, where nothing else orders it.
func TestTrustIsOneRead(t *testing.T) {
	for round := range 20 {
		h, rig := notInstalled(t)
		stop := make(chan struct{})
		torn := make(chan string, 1)
		done, reading := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			rig.ma.trust()
			close(reading)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if vs, installed := rig.ma.trust(); installed != (len(vs) > 0) {
					select {
					case torn <- fmt.Sprintf("installed %v with %d sources", installed, len(vs)):
					default:
					}
				}
			}
		}()
		<-reading // a reader before the install as well as after it
		install(h, rig)
		close(stop)
		<-done
		select {
		case why := <-torn:
			t.Fatalf("round %d: a torn read: %s", round, why)
		default:
		}
	}
}

// TestStrangerBeforeStartIsTheFlat401: before Start, a request whose
// token does not verify is answered exactly as after it, status,
// headers and body, and at once: nothing waits before a token verified,
// so no stranger can make the box hold a connection open or learn that
// it has just started.
func TestStrangerBeforeStartIsTheFlat401(t *testing.T) {
	before, _ := notInstalled(t)
	after, _ := newTestHandler(t)
	for name, tok := range map[string]string{
		"no token":            "",
		"garbage":             "not-a-jwt",
		"a global source's":   globalToken(t), // not demo's
		"demo's, another aud": trusttest.Mint(t, appTestPriv, "other", nil),
	} {
		b := serveOne(before, demoGet(tok))
		if b.took > time.Second {
			t.Errorf("%s before Start: answered after %v", name, b.took)
		}
		a := serveOne(after, demoGet(tok))
		if b.w.Code != http.StatusUnauthorized {
			t.Errorf("%s before Start: %d %s", name, b.w.Code, b.w.Body)
		}
		sameAnswer(t, name+" before Start", b.w, a.w)
	}
}

// TestInstallWaitEndsWithTheRequest: a request that ends while it waits
// for the install stops waiting at once, is answered the 503 (nobody
// may be reading it), and leaves no goroutine behind.
func TestInstallWaitEndsWithTheRequest(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	h, _ := notInstalled(t)
	core, logs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan answer, 1)
	go func() { got <- serveOne(h, demoGet(appToken(t)).WithContext(ctx)) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case a := <-got:
		if a.w.Code != http.StatusServiceUnavailable || a.w.Header().Get("Retry-After") != "1" {
			t.Errorf("a request that ended while waiting: %d %v %s", a.w.Code, a.w.Header(), a.w.Body)
		}
	case <-time.After(installWait / 2):
		t.Fatal("the wait did not end with the request")
	}
	lines := logs.FilterMessage("deploy token verified before liveswap installed the app; answered 503").All()
	if len(lines) != 1 || lines[0].ContextMap()["request_ended"] != true {
		t.Errorf("journal: %v", logs.All())
	}
}

// TestReloadBeforeStartAnswersAtOnce: a reload provisions a config whose
// pooled apps are installed already, by the config still serving, so
// until the new config's Start the token is checked against the serving
// config's sources as before, and is answered at once; there is no
// window to wait out. Here the new config trusts a different key, which
// takes over only at its Start.
func TestReloadBeforeStartAnswersAtOnce(t *testing.T) {
	serving, rig := newTestHandler(t)
	reloaded := &App{
		managed:         map[string]*managedApp{"demo": rig.ma},
		globalVerifiers: serving.app.globalVerifiers,
		appVerifiers:    map[string][]deploytrust.Verifier{"demo": serving.app.globalVerifiers},
		specs:           map[string]*appSpec{"demo": rig.spec},
	}
	h := &Handler{app: reloaded, logger: zap.NewNop(), limiter: deploytrust.NewLimiter(rig.clock)}
	if a := serveOne(h, demoGet(appToken(t))); a.w.Code != http.StatusOK || a.took > time.Second {
		t.Fatalf("the serving config's token after a reload, before its Start: %d %s in %v", a.w.Code, a.w.Body, a.took)
	}
	if a := serveOne(h, demoGet(globalToken(t))); a.w.Code != http.StatusUnauthorized || a.took > time.Second {
		t.Fatalf("the new config's token before its Start: %d %s in %v", a.w.Code, a.w.Body, a.took)
	}
}

// personGet is GET target at the box's name with k's token.
func personGet(target string, k accesstest.Key) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://"+personBox+target, nil)
	req.Header.Set("Authorization", token(k))
	return req
}

// TestPersonBeforeStartReadsTheInstalledApp: a person's read of an app
// sent before liveswap's Start installed it is answered once the app
// is installed, with the installed app's state, as a deploy token's
// is. Before the install the pooled app holds no spec, and its status
// was a 200 saying an app that has deployed is idle with no release
// and no deploy; a deploy's record was a 503 with no Retry-After ("app
// has no configuration loaded").
func TestPersonBeforeStartReadsTheInstalledApp(t *testing.T) {
	for _, c := range []struct {
		target string
		code   int
		holds  string
	}{
		{"/demo", http.StatusOK, `"deploys":[{"version":"v1","status":"succeeded"}]`},
		{"/_motel/apps", http.StatusOK, `"newest_deploy":{"version":"v1","status":"succeeded"}`},
		{"/demo?deploy=v1", http.StatusOK, `"status":"succeeded"`},
	} {
		people := newTestPeople(t)
		h, rig := notInstalled(t)
		h.people = people.reg
		// What only an installed app can show: a deploy on its disk.
		must(t, writeDeployRecord(rig.spec.dirs, "v1", []byte(`{"version":"v1","status":"succeeded"}`)))
		got := make(chan answer, 1)
		go func() { got <- serveOne(h, personGet(c.target, people.alice)) }()
		select {
		case a := <-got:
			t.Fatalf("%s: a person's read before Start was answered before the install: %d %s", c.target, a.w.Code, strings.TrimSpace(a.w.Body.String()))
		case <-time.After(60 * time.Millisecond):
		}
		install(h, rig)
		a := <-got
		if a.err != nil || a.w.Code != c.code || !strings.Contains(a.w.Body.String(), c.holds) {
			t.Errorf("%s, installed 60 ms later: %d %s (%v), want %d holding %s", c.target, a.w.Code, strings.TrimSpace(a.w.Body.String()), a.err, c.code, c.holds)
		}
		if a.took > 2*time.Second {
			t.Errorf("%s: answered %v after it was sent, long after the install", c.target, a.took)
		}
	}
}

// TestPersonBeforeStartPastTheBound: a person's read whose app no Start
// installs within installWait is told to come back, the deploy token's
// 503 with Retry-After, and the journal says so with the person's
// name. On a fake clock: not answered a millisecond before the bound,
// answered at it.
func TestPersonBeforeStartPastTheBound(t *testing.T) {
	for _, target := range []string{"/demo", "/demo?deploy=v1", "/_motel/apps"} {
		people := newTestPeople(t)
		h, _ := notInstalled(t)
		h.people = people.reg
		core, logs := observer.New(zap.InfoLevel)
		h.logger = zap.New(core)
		fc := newFakeClock()
		h.waitClock = fc
		got := make(chan answer, 1)
		go func() { got <- serveOne(h, personGet(target, people.alice)) }()
		if at := pendingTimer(t, fc); !at.Equal(fc.Now().Add(installWait)) {
			t.Fatalf("%s: the timer fires %v from now, want %v", target, at.Sub(fc.Now()), installWait)
		}
		fc.Advance(installWait - time.Millisecond)
		select {
		case a := <-got:
			t.Fatalf("%s answered before the bound: %d %s", target, a.w.Code, a.w.Body)
		case <-time.After(20 * time.Millisecond):
		}
		fc.Advance(time.Millisecond)
		a := <-got
		const body = `{"error":"motel is starting and has not loaded this app yet; retry after 1 s"}` + "\n"
		if a.err != nil || a.w.Code != http.StatusServiceUnavailable || a.w.Body.String() != body || a.w.Header().Get("Retry-After") != "1" {
			t.Errorf("%s before Start, past the bound: %d %v %q (%v)", target, a.w.Code, a.w.Header(), a.w.Body, a.err)
		}
		lines := logs.FilterMessage("a person's read came before liveswap installed the app; answered 503").All()
		if len(lines) != 1 || lines[0].ContextMap()["app"] != "demo" || lines[0].ContextMap()["via"] != "alice" || lines[0].ContextMap()["waited"] != installWait {
			t.Errorf("%s: journal: %v", target, logs.All())
		}
	}
}

// TestAppsListWaitsOnceForEveryApp: the list's wait is one bound for
// all the apps it names, not one each. Two apps, neither installed:
// the first is installed part of the way through, and the list is told
// to come back when installWait has passed since it was sent, on the
// one timer it set.
func TestAppsListWaitsOnceForEveryApp(t *testing.T) {
	people := newTestPeople(t)
	h, rig := notInstalled(t)
	h.people = people.reg
	h.app.managed["zeta"] = newManagedApp("zeta")
	core, logs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	fc := newFakeClock()
	h.waitClock = fc
	got := make(chan answer, 1)
	go func() { got <- serveOne(h, personGet("/_motel/apps", people.carol)) }()
	pendingTimer(t, fc)
	fc.Advance(installWait / 2)
	install(h, rig) // demo, the first by name
	select {
	case a := <-got:
		t.Fatalf("answered with zeta not installed: %d %s", a.w.Code, a.w.Body)
	case <-time.After(20 * time.Millisecond):
	}
	fc.mu.Lock()
	timers := len(fc.waiters)
	fc.mu.Unlock()
	if timers != 1 {
		t.Fatalf("%d timers pending, want the one the list set", timers)
	}
	fc.Advance(installWait / 2)
	a := <-got
	if a.err != nil || a.w.Code != http.StatusServiceUnavailable || a.w.Header().Get("Retry-After") != "1" {
		t.Fatalf("past the one bound: %d %s (%v)", a.w.Code, a.w.Body, a.err)
	}
	lines := logs.FilterMessage("a person's read came before liveswap installed the app; answered 503").All()
	if len(lines) != 1 || lines[0].ContextMap()["app"] != "zeta" || lines[0].ContextMap()["waited"] != installWait {
		t.Errorf("journal: %v", logs.All())
	}
}

// slowClock is a clock a second later each time it is read, which
// keeps the waits it is asked for and never fires them.
type slowClock struct {
	mu     sync.Mutex
	now    time.Time
	waits  []time.Duration
	asked  chan struct{}
	notify sync.Once
}

func (c *slowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

func (c *slowClock) Sleep(time.Duration) {}

func (c *slowClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	c.notify.Do(func() { close(c.asked) })
	return make(chan time.Time)
}

// TestPersonWaitIsWhatIsLeftOfTheBound: the timer a person's read sets
// is for what is left of installWait since the read began, not a whole
// installWait from whenever the first app that is not installed is
// reached: the bound is on the read. On a clock a second later at each
// reading, the read begins at one reading and sets its timer at the
// next, for a second less than installWait.
func TestPersonWaitIsWhatIsLeftOfTheBound(t *testing.T) {
	people := newTestPeople(t)
	h, rig := notInstalled(t)
	h.people = people.reg
	sc := &slowClock{now: time.Unix(1_700_000_000, 0), asked: make(chan struct{})}
	h.waitClock = sc
	got := make(chan answer, 1)
	go func() { got <- serveOne(h, personGet("/demo", people.alice)) }()
	select {
	case <-sc.asked:
	case a := <-got:
		t.Fatalf("answered with no timer set: %d %s", a.w.Code, a.w.Body)
	case <-time.After(5 * time.Second):
		t.Fatal("the handler set no timer")
	}
	sc.mu.Lock()
	waits := append([]time.Duration(nil), sc.waits...)
	sc.mu.Unlock()
	if len(waits) != 1 || waits[0] != installWait-time.Second {
		t.Errorf("the timer is for %v, want %v: installWait less the second since the read began", waits, installWait-time.Second)
	}
	install(h, rig)
	if a := <-got; a.err != nil || a.w.Code != http.StatusOK {
		t.Errorf("after the install: %d %s (%v)", a.w.Code, a.w.Body, a.err)
	}
}

// TestPersonNotAllowedBeforeStartDoesNotWait: only a read Decide
// allowed waits for an install. Before Start a stranger's 401, a
// person's 403 for an app no grant of theirs covers, a box viewer's 404
// for a name that is no app, whoami, and the list of a person who may
// view no app are answered at once, and set no timer.
func TestPersonNotAllowedBeforeStartDoesNotWait(t *testing.T) {
	people := newTestPeople(t)
	h, _ := notInstalled(t)
	h.people = people.reg
	fc := newFakeClock()
	h.waitClock = fc
	for _, c := range []struct {
		what   string
		target string
		key    accesstest.Key
		code   int
	}{
		{"a stranger", "/demo", people.stranger, http.StatusUnauthorized},
		{"a person with no grant on the app", "/demo", people.dave, http.StatusForbidden},
		{"a box viewer, a name that is no app", "/nothere", people.carol, http.StatusNotFound},
		{"whoami", "/_motel/whoami", people.alice, http.StatusOK},
		{"the list of a person who views no app", "/_motel/apps", people.dave, http.StatusOK},
	} {
		got := make(chan answer, 1)
		go func() { got <- serveOne(h, personGet(c.target, c.key)) }()
		select {
		case a := <-got:
			if a.err != nil || a.w.Code != c.code {
				t.Errorf("%s before Start: %d %s (%v), want %d", c.what, a.w.Code, strings.TrimSpace(a.w.Body.String()), a.err, c.code)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s before Start was made to wait", c.what)
		}
		fc.mu.Lock()
		timers := len(fc.waiters)
		fc.mu.Unlock()
		if timers != 0 {
			t.Fatalf("%s set a timer", c.what)
		}
	}
}
