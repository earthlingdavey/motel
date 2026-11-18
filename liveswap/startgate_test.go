package liveswap

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The start gate (startgate.go, #489). Each test makes its own start
// state, so none of them opens the process's gate.

// gateRig is a gate on its own start state, and a real TCP listener
// behind it: a dialled connection completes in the kernel and waits in
// the backlog, as a client's does while the gate holds Accept.
type gateRig struct {
	g    *StartGate
	logs *observer.ObservedLogs
	raw  net.Listener
	ln   net.Listener
}

func newGateRig(t *testing.T, waitApps bool, maxWait time.Duration, names []string, cached func(string) bool) *gateRig {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	g := &StartGate{
		state: newStartState(), maxWait: maxWait, waitApps: waitApps,
		certNames: func(context.Context) []string { return names },
		cached:    cached,
		logger:    zap.New(core),
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	r := &gateRig{g: g, logs: logs, raw: raw, ln: g.WrapListener(raw)}
	t.Cleanup(func() {
		_ = r.ln.Close()
		// A test that never accepted started no opening: take its turn.
		// One that did ends at the latest at the cap, and the
		// package's leak check needs it gone.
		g.state.openOnce.Do(func() { close(g.state.open) })
		<-g.state.open
	})
	return r
}

func allCached(string) bool { return true }

// accept is one Accept, for a goroutine.
func (r *gateRig) accept() <-chan error {
	done := make(chan error, 1)
	go func() {
		c, err := r.ln.Accept()
		if c != nil {
			_ = c.Close()
		}
		done <- err
	}()
	return done
}

func (r *gateRig) dial(t *testing.T) {
	t.Helper()
	c, err := net.Dial("tcp", r.raw.Addr().String())
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
}

func held(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s: Accept returned (%v) while the gate should hold it", what, err)
	case <-time.After(50 * time.Millisecond):
	}
}

func released(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: Accept failed: %v", what, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s: Accept still held", what)
	}
}

func TestStartGateHoldsAcceptUntilTheAppsSettle(t *testing.T) {
	r := newGateRig(t, true, 10*time.Second, nil, allCached)
	r.dial(t)
	done := r.accept()
	held(t, done, "before the apps settled")
	r.g.state.settleApps()
	released(t, done, "once the apps settled")
	e := r.logs.FilterMessage("start gate open").All()
	if len(e) != 1 || e[0].ContextMap()["waited_for_apps"] != true {
		t.Fatalf("want one open line that says it waited for the apps, got %+v", r.logs.All())
	}
}

func TestStartGateWithoutLiveswapWaitsForNoApps(t *testing.T) {
	r := newGateRig(t, false, 10*time.Second, nil, allCached)
	r.dial(t)
	released(t, r.accept(), "a config without liveswap")
}

func TestStartGateWaitsForStoredCertificates(t *testing.T) {
	var loaded atomic.Bool
	r := newGateRig(t, true, 10*time.Second, []string{"a.test", "b.test"}, func(n string) bool {
		return n == "a.test" || loaded.Load()
	})
	r.g.state.settleApps()
	r.dial(t)
	done := r.accept()
	held(t, done, "while b.test's certificate is not cached")
	loaded.Store(true)
	released(t, done, "once every stored certificate is cached")
	e := r.logs.FilterMessage("start gate open").All()
	if len(e) != 1 || e[0].ContextMap()["certificates_waited_for"] != int64(2) {
		t.Fatalf("want one open line counting two certificates, got %+v", r.logs.All())
	}
}

func TestStartGateOpensAtItsCap(t *testing.T) {
	for _, c := range []struct {
		name     string
		settled  bool
		cappedOn string
		missing  []any
	}{
		{"apps never settle", false, "liveswap recovery", nil},
		{"a certificate never cached", true, "certificate cache", []any{"never.test"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newGateRig(t, true, 100*time.Millisecond, []string{"never.test"}, func(string) bool { return false })
			if c.settled {
				r.g.state.settleApps()
			}
			r.dial(t)
			began := time.Now()
			released(t, r.accept(), "at the cap")
			if took := time.Since(began); took < 90*time.Millisecond {
				t.Fatalf("opened after %v, before its 100ms cap", took)
			}
			e := r.logs.FilterMessageSnippet("at its cap").All()
			if len(e) != 1 {
				t.Fatalf("want one warning that it opened at its cap, got %+v", r.logs.All())
			}
			m := e[0].ContextMap()
			if m["capped_on"] != c.cappedOn {
				t.Fatalf("capped_on = %v, want %q", m["capped_on"], c.cappedOn)
			}
			got, said := m["certificates_not_cached"].([]any)
			if !slices.Equal(got, c.missing) || said != (c.missing != nil) {
				t.Fatalf("certificates_not_cached = %v (said: %v), want %v", m["certificates_not_cached"], said, c.missing)
			}
			if _, said := m["certificates_waited_for"]; said != c.settled {
				t.Fatalf("certificates_waited_for said: %v, want %v (never read when capped on the apps)", said, c.settled)
			}
		})
	}
}

// One opening releases every listener the gate wraps; one wrapped
// after it opened is not wrapped at all.
func TestStartGateOpensOnceForEveryListener(t *testing.T) {
	r := newGateRig(t, true, 10*time.Second, nil, allCached)
	raw2, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	ln2 := r.g.WrapListener(raw2)
	defer func() { _ = ln2.Close() }()
	r.dial(t)
	c2, err := net.Dial("tcp", raw2.Addr().String())
	must(t, err)
	defer func() { _ = c2.Close() }()
	done := r.accept()
	done2 := make(chan error, 1)
	go func() {
		c, err := ln2.Accept()
		if c != nil {
			_ = c.Close()
		}
		done2 <- err
	}()
	held(t, done, "the first listener")
	held(t, done2, "the second listener")
	r.g.state.settleApps()
	released(t, done, "the first listener")
	released(t, done2, "the second listener")
	if n := r.logs.FilterMessage("start gate open").Len(); n != 1 {
		t.Fatalf("the gate opened %d times, want once", n)
	}

	raw3, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = raw3.Close() }()
	if ln3 := r.g.WrapListener(raw3); ln3 != raw3 {
		t.Fatalf("a listener wrapped after the gate opened must come back as it is, got %T", ln3)
	}
}

// A listener closed while its Accept is held (a config stopped before
// the gate opened) ends that Accept.
func TestStartGateCloseEndsAHeldAccept(t *testing.T) {
	r := newGateRig(t, true, 200*time.Millisecond, nil, allCached)
	done := r.accept()
	held(t, done, "before the close")
	_ = r.ln.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("a held Accept on a closed listener must end with net.ErrClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing the listener left its Accept held")
	}
}

func TestStartGateCaddyfile(t *testing.T) {
	for in, ok := range map[string]bool{
		"liveswap_ready":                     true,
		"liveswap_ready 2s":                  false,
		"liveswap_ready {\n\tmax_wait 2s\n}": false,
	} {
		err := new(StartGate).UnmarshalCaddyfile(caddyfile.NewTestDispenser(in))
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok = %v", in, err, ok)
		}
	}
}

// The first config's apps settle the start once each one's first
// recovery attempt has returned.
func TestRecoverAppsSettlesTheStart(t *testing.T) {
	rig := newTestRig(t) // nothing recorded: the first attempt is the last
	a := &App{logger: zap.NewNop(), managed: map[string]*managedApp{"demo": rig.ma}, recoverWG: new(sync.WaitGroup)}
	s := newStartState()
	a.recoverApps(context.Background(), s)
	a.recoverWG.Wait()
	waitUntil(t, "the start to settle", s.appsSettled)
}

// A config whose recovery ended before its first attempt — a reload
// replaced it within a start's first moments — does not settle the
// start: the gate waits for the config that replaced it.
func TestRecoverAppsCancelledBeforeAnAttemptDoesNotSettle(t *testing.T) {
	rig := newTestRig(t)
	a := &App{logger: zap.NewNop(), managed: map[string]*managedApp{"demo": rig.ma}, recoverWG: new(sync.WaitGroup)}
	s := newStartState()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.recoverApps(ctx, s)
	a.recoverWG.Wait()
	time.Sleep(50 * time.Millisecond) // the settling goroutine's turn
	if s.appsSettled() {
		t.Fatal("a config that made no attempt settled the start")
	}
	b := &App{logger: zap.NewNop(), managed: map[string]*managedApp{"demo": rig.ma}, recoverWG: new(sync.WaitGroup)}
	b.recoverApps(context.Background(), s)
	b.recoverWG.Wait()
	waitUntil(t, "the replacing config to settle the start", s.appsSettled)
}

// recover's firstDone is called once: with true when the first attempt
// returns, whatever it returned, and not again after a retry; with
// false when recovery ends before making one.
func TestRecoverCallsFirstDoneOnce(t *testing.T) {
	recordV7 := func(rig *testRig) {
		bindSocket(t, rig.spec.dirs.socket(recordedNonce))
		rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
		rig.store.ok = true
		must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	}
	t.Run("a reattach", func(t *testing.T) {
		rig := newTestRig(t)
		recordV7(rig)
		rig.runner.reattachOK = true
		var calls atomic.Int32
		rig.ma.recover(context.Background(), zap.NewNop(), func(attempted bool) {
			if !attempted || !activeSocketIs(rig) {
				t.Errorf("firstDone(%v) was called before the attempt routed the app", attempted)
			}
			calls.Add(1)
		})
		if n := calls.Load(); n != 1 {
			t.Fatalf("firstDone called %d times, want once", n)
		}
	})
	t.Run("a transient failure, then retries", func(t *testing.T) {
		rig := newTestRig(t)
		recordV7(rig)
		rig.runner.reattachErrs = []error{errTest, errTest}
		rig.runner.reattachOK = true
		var calls atomic.Int32
		first := make(chan struct{})
		done := make(chan struct{})
		go func() {
			rig.ma.recover(context.Background(), zap.NewNop(), func(attempted bool) {
				if !attempted {
					t.Error("firstDone(false) after a first attempt")
				}
				calls.Add(1)
				close(first)
			})
			close(done)
		}()
		select {
		case <-first: // before any retry: the clock has not moved
		case <-time.After(2 * time.Second):
			t.Fatal("firstDone was not called after a first attempt that failed")
		}
		advanceUntil(t, rig, recoveryBackoffFloor, "the reattach on the third attempt", func() bool { return activeSocketIs(rig) })
		<-done
		if n := calls.Load(); n != 1 {
			t.Fatalf("firstDone called %d times across three attempts, want once", n)
		}
	})
	t.Run("ended before the first attempt", func(t *testing.T) {
		rig := newTestRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls atomic.Int32
		rig.ma.recover(ctx, zap.NewNop(), func(attempted bool) {
			if attempted {
				t.Error("firstDone(true) with no attempt made")
			}
			calls.Add(1)
		})
		if n := calls.Load(); n != 1 {
			t.Fatalf("firstDone called %d times, want once", n)
		}
	})
}

func TestManagedNames(t *testing.T) {
	t.Setenv("GATE_TEST_HOST", "env.test")
	srv := func(auto *caddyhttp.AutoHTTPSConfig, hosts ...string) *caddyhttp.Server {
		hm := caddyhttp.MatchHost(hosts)
		path := caddyhttp.MatchPath{"/x"}
		return &caddyhttp.Server{AutoHTTPS: auto, Routes: caddyhttp.RouteList{
			{MatcherSets: caddyhttp.MatcherSets{{&path, &hm}}},
		}}
	}
	servers := map[string]*caddyhttp.Server{
		"srv0": srv(&caddyhttp.AutoHTTPSConfig{Skip: []string{"skip.test"}, SkipCerts: []string{"skipcert.test"}},
			"A.Test", "skip.test", "skipcert.test", "{env.GATE_TEST_HOST}", "{http.request.host}",
			"x.demand.test", "192.0.2.1", "*.wild.test", "localhost"),
		"srv1":    srv(nil, "b.test"),
		"off":     srv(&caddyhttp.AutoHTTPSConfig{Disabled: true}, "off.test"),
		"nocerts": srv(&caddyhttp.AutoHTTPSConfig{DisableCerts: true}, "nocerts.test"),
	}
	policies := []policyView{{subjects: []string{"*.demand.test"}, onDemand: true}, {subjects: []string{"b.test"}}}
	want := []string{"*.wild.test", "192.0.2.1", "a.test", "b.test", "env.test", "localhost"}
	if got := managedNames(servers, policies); !slices.Equal(got, want) {
		t.Fatalf("managedNames = %v, want %v", got, want)
	}
	// A catch-all policy decides every name no earlier policy matched.
	catchAll := []policyView{{subjects: []string{"b.test"}}, {onDemand: true}}
	if got := managedNames(map[string]*caddyhttp.Server{"s": srv(nil, "a.test", "b.test")}, catchAll); !slices.Equal(got, []string{"b.test"}) {
		t.Fatalf("under an on-demand catch-all, managedNames = %v, want [b.test]", got)
	}
}

func TestStoredNames(t *testing.T) {
	ctx := context.Background()
	st := &certmagic.FileStorage{Path: t.TempDir()}
	if got := storedNames(ctx, st, []string{"a.test"}); got != nil {
		t.Fatalf("with nothing stored, storedNames = %v, want none", got)
	}
	for _, k := range []string{
		certmagic.StorageKeys.SiteCert("acme-v02.api.letsencrypt.org-directory", "a.test"),
		certmagic.StorageKeys.SiteCert("local", "b.test"),
		certmagic.StorageKeys.SiteCert("local", "*.wild.test"),
		certmagic.StorageKeys.SitePrivateKey("local", "nocert.test"),
	} {
		must(t, st.Store(ctx, k, []byte("x")))
	}
	got := storedNames(ctx, st, []string{"a.test", "b.test", "*.wild.test", "nocert.test", "none.test"})
	if want := []string{"a.test", "b.test", "*.wild.test"}; !slices.Equal(got, want) {
		t.Fatalf("storedNames = %v, want %v", got, want)
	}
}
