package liveswap

// The start gate (#489): the listener wrapper liveswap_ready holds the
// first connections a start of moteld accepts until this process can
// answer them, so they wait a few tens of milliseconds instead of
// meeting one of two races at the moment the http app starts serving:
//
//   - liveswap has not picked the running apps back up: an app's
//     upstream has no socket yet, and reverse_proxy answers 503;
//   - the tls app has not yet put the certificates it already has in
//     storage into its cache: Caddy's http app serves before
//     automaticHTTPSPhase2 loads them, in goroutines, and a handshake
//     in between fails with "no certificate available"
//     (caddyserver/caddy#8431). With the listening socket closed
//     across a restart almost no connection lands in that millisecond;
//     with one kept open across it, every queued connection does.
//
// It opens once per process, when both are true or after
// startGateMaxWait, whichever comes first, and never closes again:
// a config loaded after that (a reload) passes straight through.
// HTTP/3 is not a net.Listener and is not held.

import (
	"context"
	"errors"
	"maps"
	"net"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

func init() { caddy.RegisterModule(StartGate{}) }

// startGateMaxWait bounds the hold: past it the gate opens whatever is
// still pending, and the connections meet today's races. The longest
// recovery seen on a box is about 120 ms (#489).
const startGateMaxWait = 2 * time.Second

// startState is one process's start as the gate waits on it.
type startState struct {
	appsOnce sync.Once
	apps     chan struct{} // closed when the first config's apps have settled
	openOnce sync.Once     // the gate's opening runs once
	open     chan struct{} // closed when the gate opens
	opened   atomic.Bool
}

func newStartState() *startState {
	return &startState{apps: make(chan struct{}), open: make(chan struct{})}
}

// processStart is this process's start. liveswap's Start settles it;
// every gate of every config reads it.
var processStart = newStartState()

// settleApps records that every app of the first config liveswap
// started has had its first recovery attempt return.
func (s *startState) settleApps() { s.appsOnce.Do(func() { close(s.apps) }) }

func (s *startState) appsSettled() bool {
	select {
	case <-s.apps:
		return true
	default:
		return false
	}
}

// StartGate is the caddy.listeners.liveswap_ready module. It has no
// settings:
//
//	servers {
//		listener_wrappers {
//			liveswap_ready
//		}
//	}
type StartGate struct {
	state    *startState
	maxWait  time.Duration
	waitApps bool
	// certNames are the names whose stored certificates the tls app
	// loads at a start, read once as the gate starts opening; cached
	// reports whether one is in the cache yet. Seams for the tests.
	certNames func(context.Context) []string
	cached    func(string) bool
	logger    *zap.Logger
}

func (StartGate) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.listeners.liveswap_ready",
		New: func() caddy.Module { return new(StartGate) },
	}
}

// Provision binds the gate to this process's start. It looks the
// liveswap app up with AppIfConfigured, so a config without one
// creates none and waits for no apps. The http app is not looked up
// here: this runs while it provisions.
func (g *StartGate) Provision(ctx caddy.Context) error {
	g.state, g.maxWait, g.logger = processStart, startGateMaxWait, ctx.Logger()
	switch _, err := ctx.AppIfConfigured("liveswap"); {
	case err == nil:
		g.waitApps = true
	case !errors.Is(err, caddy.ErrNotConfigured):
		return err
	}
	tlsApp, err := ctx.App("tls")
	if err != nil {
		return err
	}
	t := tlsApp.(*caddytls.TLS)
	// The cache itself, under its lock. Not HasCertificateForSubject:
	// it also reads the tls app's map of managed names, which Manage
	// writes, unlocked, while the servers already serve.
	g.cached = func(name string) bool { return len(caddytls.AllMatchingCertificates(name)) > 0 }
	g.certNames = func(c context.Context) []string { return storedCertNames(c, ctx, t) }
	return nil
}

// UnmarshalCaddyfile takes the bare name.
func (g *StartGate) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the name
	if d.NextArg() || d.NextBlock(0) {
		return d.Err("liveswap_ready takes no arguments or block")
	}
	return nil
}

// WrapListener holds ln's Accept until the gate opens. A listener
// wrapped after it opened is returned as it is.
func (g *StartGate) WrapListener(ln net.Listener) net.Listener {
	if g.state.opened.Load() {
		return ln
	}
	return &gatedListener{Listener: ln, g: g, closed: make(chan struct{})}
}

// opening waits for the apps, then the certificates, or the cap, and
// opens the gate. Run once per process, by the first Accept.
func (g *StartGate) opening() {
	began := time.Now()
	deadline := time.NewTimer(g.maxWait)
	defer deadline.Stop()
	capped := ""
	if g.waitApps {
		select {
		case <-g.state.apps:
		case <-deadline.C:
			capped = "liveswap recovery"
		}
	}
	appsAfter := time.Since(began)
	var names, missing []string
	if capped == "" {
		ctx, cancel := context.WithDeadline(context.Background(), began.Add(g.maxWait))
		names = g.certNames(ctx)
		cancel()
		missing = names
		tick := time.NewTicker(time.Millisecond)
	wait:
		// One lookup a tick: the first name not yet cached, and the
		// ones after it as soon as it is.
		for len(missing) > 0 {
			if g.cached(missing[0]) {
				missing = missing[1:]
				continue
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				capped = "certificate cache"
				missing = g.uncached(missing)
				break wait
			}
		}
		tick.Stop()
	}
	g.state.opened.Store(true)
	close(g.state.open)
	fields := []zap.Field{
		zap.Duration("waited", time.Since(began)),
		zap.Bool("waited_for_apps", g.waitApps),
	}
	if capped != "liveswap recovery" {
		// Capped on the apps, the certificates were never read: say
		// nothing of them rather than that none was missing.
		fields = append(fields,
			zap.Duration("apps_settled_after", appsAfter),
			zap.Int("certificates_waited_for", len(names)))
	}
	if capped == "" {
		g.logger.Info("start gate open", fields...)
		return
	}
	fields = append(fields, zap.String("capped_on", capped))
	if capped == "certificate cache" {
		const shown = 10
		if len(missing) > shown {
			missing = append(missing[:shown:shown], "…")
		}
		fields = append(fields, zap.Strings("certificates_not_cached", missing))
	}
	g.logger.Warn("start gate open at its cap: the first connections may meet a 503 or a TLS error", fields...)
}

func (g *StartGate) uncached(names []string) []string {
	var out []string
	for _, n := range names {
		if !g.cached(n) {
			out = append(out, n)
		}
	}
	return out
}

type gatedListener struct {
	net.Listener
	g         *StartGate
	closed    chan struct{}
	closeOnce sync.Once
	passed    atomic.Bool
}

// Accept waits for the gate, or for the listener to close, first.
func (l *gatedListener) Accept() (net.Conn, error) {
	if !l.passed.Load() {
		l.g.state.openOnce.Do(func() { go l.g.opening() })
		select {
		case <-l.g.state.open:
			l.passed.Store(true)
		case <-l.closed:
			return nil, net.ErrClosed
		}
	}
	return l.Listener.Accept()
}

func (l *gatedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// storedCertNames is every name the tls app loads from storage at this
// start: managedNames, less those with nothing stored under any
// issuer. A name with nothing stored is being issued, which takes
// seconds and is not waited for.
func storedCertNames(ctx context.Context, cctx caddy.Context, t *caddytls.TLS) []string {
	httpApp, err := cctx.AppIfConfigured("http")
	if err != nil {
		return nil
	}
	var policies []policyView
	if t.Automation != nil {
		for _, ap := range t.Automation.Policies {
			policies = append(policies, policyView{subjects: ap.Subjects(), onDemand: ap.OnDemand})
		}
	}
	return storedNames(ctx, cctx.Storage(), managedNames(httpApp.(*caddyhttp.App).Servers, policies))
}

// policyView is what of an automation policy managedNames reads.
type policyView struct {
	subjects []string
	onDemand bool
}

// managedNames is every name the http app hands the tls app to manage,
// as automaticHTTPSPhase1 collects them (caddyhttp, Caddy v2.11.9):
// the host matchers of each server's routes, on a server with
// automatic HTTPS and its certificates on, less the names in skip and
// skip_certificates and those that qualify for no certificate. Less,
// too, a name whose automation policy (the first that matches it, as
// getAutomationPolicyForName picks) is on-demand: the tls app loads
// none of those at a start. A name with a placeholder Caddy cannot
// resolve without a request is left out; so would Caddy.
func managedNames(servers map[string]*caddyhttp.Server, policies []policyView) []string {
	repl := caddy.NewReplacer()
	set := map[string]struct{}{}
	for _, srv := range servers {
		auto := srv.AutoHTTPS
		if auto == nil {
			auto = new(caddyhttp.AutoHTTPSConfig)
		}
		if auto.Disabled || auto.DisableCerts {
			continue
		}
		for _, r := range srv.Routes {
			for _, ms := range r.MatcherSets {
				for _, m := range ms {
					hm, ok := m.(*caddyhttp.MatchHost)
					if !ok {
						continue
					}
					for _, h := range *hm {
						d, err := repl.ReplaceOrErr(h, true, false)
						if err != nil || slices.Contains(auto.Skip, d) || slices.Contains(auto.SkipCerts, d) ||
							!certmagic.SubjectQualifiesForCert(d) || onDemand(policies, d) {
							continue
						}
						set[strings.ToLower(d)] = struct{}{}
					}
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

func onDemand(policies []policyView, name string) bool {
	for _, p := range policies {
		if len(p.subjects) == 0 {
			return p.onDemand
		}
		for _, s := range p.subjects {
			if certmagic.MatchWildcard(name, s) {
				return p.onDemand
			}
		}
	}
	return false // the default policies are never on-demand
}

// storedNames is names less those with no certificate in storage under
// any issuer's directory. Unreadable storage is nothing stored.
func storedNames(ctx context.Context, st certmagic.Storage, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	issuers, err := st.List(ctx, "certificates", false)
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		for _, iss := range issuers {
			if st.Exists(ctx, certmagic.StorageKeys.SiteCert(path.Base(iss), n)) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

var (
	_ caddy.Provisioner     = (*StartGate)(nil)
	_ caddy.ListenerWrapper = (*StartGate)(nil)
	_ caddyfile.Unmarshaler = (*StartGate)(nil)
)
