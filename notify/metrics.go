package notify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/caddyserver/caddy/v2"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
)

// The store's defaults: the socket motel-vm.socket listens on, and the
// directory setup makes for what the store and vmalert read.
const (
	DefaultStoreSocket     = "/run/motel-metrics/vm.sock"
	DefaultMetricsStateDir = "/var/lib/motel-metrics"
)

// cgroupRoot is where the per-app collector reads the units' cgroups.
const cgroupRoot = "/sys/fs/cgroup"

// MetricsStore is the monitor block's `metrics` line (DESIGN-metrics.md,
// "The metrics line"): motel's push to Debian's VictoriaMetrics, the
// rules it renders for vmalert, and `metrics` in GET /<app>. A part of
// the monitor app (App.Metrics), not a Caddy app of its own: one global
// option yields one app.
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

// provision checks the line, and wires the push, the collector and
// GET /<app>'s source into this config: App.Provision's, after notify's
// part, with this config's liveswap (nil for none) and the block's
// notify, nil for a metrics line alone. It writes nothing and connects
// to nothing, so `motel validate` leaves the box as it was.
func (m *MetricsStore) provision(ctx caddy.Context, ls *liveswap.App, n *App, logger *zap.Logger) error {
	m.logger = logger
	if m.Store != "" && !filepath.IsAbs(m.Store) {
		return fmt.Errorf("monitor: metrics: the store's socket %q is not an absolute path", m.Store)
	}
	m.client = newStoreClient(m.store())
	reg := ctx.GetMetricsRegistry()
	m.reg = prometheus.NewRegistry() // a context with no registry: an empty push
	if reg != nil {
		m.reg = reg
	}
	if err := thePusher.registerFailed(reg); err != nil {
		m.logger.Warn("monitor metrics: motel_metrics_push_failed_total is not on the admin /metrics", zap.Error(err))
	}
	if ls != nil {
		if reg != nil {
			if err := reg.Register(appCollector{apps: ls, root: cgroupRoot}); err != nil {
				return fmt.Errorf("monitor: metrics: registering the per-app series: %w", err)
			}
		}
		ls.SetMetricsSource(newMetricsView(m.client, m.logger))
	}
	if n == nil {
		m.logger.Warn(StoreAloneMessage)
	}
	m.rules = renderRules(n)
	return nil
}

// StoreAloneMessage is what a monitor block that is a metrics line alone
// says at every load: no channel, so vmalert is given no rule.
// Alertmanager keeps the last config notify rendered (its Stop releases
// only the engine), so a rule that fired would still reach the old
// channels (Copilot on #423).
const StoreAloneMessage = "monitor metrics: the monitor block has no channel, only its metrics line: vmalert is given no rule, so nothing the store sees is told to anyone; the store keeps the numbers, and GET /<app> reads them"

// start writes the rules and points the push at this config:
// App.Start's, after notify's engine. Nothing here fails the config
// load: every failure is a journal line.
func (m *MetricsStore) start() {
	m.applyRules()
	thePusher.configure(liveStore{m: m, store: m.client, gather: m.reg, logger: m.logger})
}

// applyRules writes this config's rules, with a journal line for each
// outcome: at Start, and when the pusher goes back to this config after
// a newer one was stopped by a failed reload, whose rules had replaced
// these.
func (m *MetricsStore) applyRules() {
	path := filepath.Join(m.stateDir(), RulesName)
	switch changed, err := writeRules(m.stateDir(), m.rules); {
	case errors.Is(err, errMetricsNotSetUp):
		m.logger.Warn("monitor metrics: "+errMetricsNotSetUp.Error()+"; pushes fail until it is", zap.String("state_dir", m.stateDir()))
	case err != nil:
		m.logger.Error("monitor metrics: the rules were not written", zap.String("rules", path), zap.Error(err))
	case changed:
		m.logger.Info("monitor metrics: wrote the rules; vmalert reads them within a minute", zap.String("rules", path))
	}
}

// What a config with no metrics line says when it retires the rules a
// config with one wrote, at a reload (pusher.release) or at the boot of
// one (cmd/motel's gate, RetireRules).
const (
	RulesRetiredMessage    = "monitor metrics: the metrics line is gone: vmalert is given no rule; the store and vmalert, which motel monitor setup --metrics started, run until their units are stopped"
	RulesNotRetiredMessage = "monitor metrics: the metrics line is gone, and its rules were not retired: vmalert still evaluates them"
)

// RetireRules writes the rules of no notify, the two groups empty, into
// dir, the state directory setup made: what vmalert must read once no
// config has a metrics line. It reports whether the file changed; a box
// the store is not set up on has nothing to retire, and is no error.
// cmd/motel's gate calls it at `motel run` with a config that has no
// metrics line, which builds no MetricsStore to do it (Copilot round 4
// on #423); a reload's is pusher.release's.
func RetireRules(dir string) (bool, error) {
	changed, err := writeRules(dir, renderRules(nil))
	if errors.Is(err, errMetricsNotSetUp) {
		return false, nil
	}
	return changed, err
}

// retireRules is RetireRules when a reload takes the metrics line away
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

var errMetricsNotSetUp = errors.New("the metrics store is not set up on this box (run motel monitor setup --metrics as root)")

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
