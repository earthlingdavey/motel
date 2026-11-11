package notify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
)

func init() {
	caddy.RegisterModule(MetricsStore{})
	httpcaddyfile.RegisterGlobalOption("metrics_store", parseMetricsStoreOption)
}

// The store's defaults: the socket motel-vm.socket listens on, and the
// directory setup makes for what the store and vmalert read.
const (
	DefaultStoreSocket     = "/run/motel-metrics/vm.sock"
	DefaultMetricsStateDir = "/var/lib/motel-metrics"
)

// cgroupRoot is where the per-app collector reads the units' cgroups.
const cgroupRoot = "/sys/fs/cgroup"

// MetricsStore is the `metrics_store` global option (DESIGN-metrics.md):
// motel's push to Debian's VictoriaMetrics, the rules it renders for
// vmalert, and `metrics` in GET /<app>. Not `metrics`, which is Caddy's
// own global option.
type MetricsStore struct {
	Store string `json:"store,omitempty"`

	// stateDir is where the rules are written: always
	// DefaultMetricsStateDir, which motel-vmalert.service's -rule names;
	// a test's directory in tests.
	stateDirOverride string

	logger *zap.Logger
	client *storeClient
	// reg is the config's metrics registry, which the push gathers.
	reg   prometheus.Gatherer
	rules []byte
}

// CaddyModule returns the Caddy module information.
func (MetricsStore) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "metrics_store",
		New: func() caddy.Module { return new(MetricsStore) },
	}
}

func (m *MetricsStore) store() string {
	if m.Store == "" {
		return DefaultStoreSocket
	}
	return m.Store
}

func (m *MetricsStore) stateDir() string {
	if m.stateDirOverride == "" {
		return DefaultMetricsStateDir
	}
	return m.stateDirOverride
}

// Provision checks the block, and wires the push, the collector and
// GET /<app>'s source into this config. It writes nothing and connects
// to nothing, so `motel validate` leaves the box as it was.
func (m *MetricsStore) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger()
	if m.Store != "" && !filepath.IsAbs(m.Store) {
		return fmt.Errorf("metrics_store: store %q is not an absolute path", m.Store)
	}
	m.client = newStoreClient(m.store())
	reg := ctx.GetMetricsRegistry()
	m.reg = prometheus.NewRegistry() // a context with no registry: an empty push
	if reg != nil {
		m.reg = reg
	}
	if err := thePusher.registerFailed(reg); err != nil {
		m.logger.Warn("metrics_store: motel_metrics_push_failed_total is not on the admin /metrics", zap.Error(err))
	}

	mod, err := ctx.AppIfConfigured("liveswap")
	switch {
	case errors.Is(err, caddy.ErrNotConfigured):
	case err != nil:
		return err
	default:
		ls, ok := mod.(*liveswap.App)
		if !ok {
			return fmt.Errorf("metrics_store: the liveswap app is a %T", mod)
		}
		if reg != nil {
			if err := reg.Register(appCollector{apps: ls, root: cgroupRoot}); err != nil {
				return fmt.Errorf("metrics_store: registering the per-app series: %w", err)
			}
		}
		ls.SetMetricsSource(newMetricsView(m.client, m.logger))
	}

	var n *App
	mod, err = ctx.AppIfConfigured("notify")
	switch {
	case errors.Is(err, caddy.ErrNotConfigured):
		m.logger.Warn("metrics_store: no notify block: vmalert is given no rule, so nothing the store sees is told to anyone; the store keeps the numbers, and GET /<app> reads them")
	case err != nil:
		return err
	default:
		var ok bool
		if n, ok = mod.(*App); !ok {
			return fmt.Errorf("metrics_store: the notify app is a %T", mod)
		}
	}
	m.rules = renderRules(n)
	return nil
}

// Start writes the rules and points the push at this config. Nothing
// here fails the config load: every failure is a journal line.
func (m *MetricsStore) Start() error {
	m.applyRules()
	thePusher.configure(liveStore{m: m, store: m.client, gather: m.reg, logger: m.logger})
	return nil
}

// applyRules writes this config's rules, with a journal line for each
// outcome: at Start, and when the pusher goes back to this config after
// a newer one was stopped by a failed reload, whose rules had replaced
// these.
func (m *MetricsStore) applyRules() {
	path := filepath.Join(m.stateDir(), RulesName)
	switch changed, err := writeRules(m.stateDir(), m.rules); {
	case errors.Is(err, errMetricsNotSetUp):
		m.logger.Warn("metrics_store: the metrics store is not set up on this box (run motel metrics setup as root); pushes fail until it is", zap.String("state_dir", m.stateDir()))
	case err != nil:
		m.logger.Error("metrics_store: the rules were not written", zap.String("rules", path), zap.Error(err))
	case changed:
		m.logger.Info("metrics_store: wrote the rules; vmalert reads them within a minute", zap.String("rules", path))
	}
}

// What a config with no metrics_store block says when it retires the
// rules a config with one wrote, at a reload (pusher.release) or at the
// boot of one (cmd/motel's gate, RetireRules).
const (
	RulesRetiredMessage    = "metrics_store: the block is gone: vmalert is given no rule; the store and vmalert still run until motel metrics setup's units are stopped"
	RulesNotRetiredMessage = "metrics_store: the block is gone, and its rules were not retired: vmalert still evaluates them"
)

// RetireRules writes the rules of no block, the two groups empty, into
// dir, the state directory setup made: what vmalert must read once no
// config has a metrics_store block. It reports whether the file
// changed; a box the store is not set up on has nothing to retire, and
// is no error. cmd/motel's gate calls it at `motel run` with a config
// that has no block, which builds no MetricsStore to do it (Copilot
// round 4 on #423); a reload's is pusher.release's.
func RetireRules(dir string) (bool, error) {
	changed, err := writeRules(dir, renderRules(nil))
	if errors.Is(err, errMetricsNotSetUp) {
		return false, nil
	}
	return changed, err
}

// retireRules is RetireRules when a reload takes the block away
// (pusher.release).
func (m *MetricsStore) retireRules() {
	path := filepath.Join(m.stateDir(), RulesName)
	switch changed, err := RetireRules(m.stateDir()); {
	case err != nil:
		m.logger.Error(RulesNotRetiredMessage, zap.String("rules", path), zap.Error(err))
	case changed:
		m.logger.Info(RulesRetiredMessage, zap.String("rules", path))
	}
}

// Stop lets go of the pusher, unless a newer config has taken it.
func (m *MetricsStore) Stop() error {
	thePusher.release(m)
	return nil
}

var errMetricsNotSetUp = errors.New("the metrics store is not set up on this box (run motel metrics setup as root)")

// writeRules writes the rule file into the state directory setup made.
func writeRules(dir string, rules []byte) (bool, error) {
	st, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, errMetricsNotSetUp
	}
	if err != nil {
		return false, err
	}
	if !st.IsDir() {
		return false, fmt.Errorf("%s is not a directory", dir)
	}
	return writeIfChanged(dir, RulesName, rules)
}

var (
	_ caddy.App         = (*MetricsStore)(nil)
	_ caddy.Provisioner = (*MetricsStore)(nil)
)

func parseMetricsStoreOption(d *caddyfile.Dispenser, existing any) (any, error) {
	if existing != nil {
		return nil, d.Err("the metrics_store option is given more than once")
	}
	m := new(MetricsStore)
	if err := m.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{Name: "metrics_store", Value: caddyconfig.JSON(m, nil)}, nil
}

// UnmarshalCaddyfile parses the global option (DESIGN-metrics.md, "The
// metrics_store block"):
//
//	metrics_store {
//	    store <socket path>
//	}
//
// An empty block, or the bare option, takes the default. There is no
// state_dir: vmalert's unit reads the rules from the one setup makes.
func (m *MetricsStore) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the option's name
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		name := d.Val()
		if name != "store" {
			return d.Errf("unknown metrics_store subdirective %q", name)
		}
		if m.Store != "" {
			return d.Err("metrics_store: store is given more than once")
		}
		if !d.NextArg() {
			return d.ArgErr()
		}
		v := d.Val()
		if d.NextArg() {
			return d.ArgErr()
		}
		if !filepath.IsAbs(v) {
			return d.Errf("metrics_store: store %q is not an absolute path", v)
		}
		m.Store = v
		if d.NextBlock(d.Nesting()) {
			return d.Err("metrics_store's lines take no block")
		}
	}
	return nil
}
