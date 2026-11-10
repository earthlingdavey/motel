package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyevents"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
)

func init() {
	caddy.RegisterModule(App{})
}

// App is the `notify` global option: the channels, who the operator is,
// which identity's channels an app's events also reach, and where
// Alertmanager is (DESIGN-notify.md).
type App struct {
	Channels        []Channel `json:"channels,omitempty"`
	Operator        []string  `json:"operator,omitempty"`
	Binds           []Bind    `json:"binds,omitempty"`
	HeartbeatSecret string    `json:"heartbeat_secret,omitempty"`
	// DeploySucceeded sends deploy_succeeded, which #418's table has
	// off by default.
	DeploySucceeded bool `json:"deploy_succeeded,omitempty"`
	// AllowInsecureChannels admits http and private addresses for test
	// rigs whose receiver is a stub on the compose network. Warned at
	// every load.
	AllowInsecureChannels bool   `json:"allow_insecure_channels,omitempty"`
	SecretsFile           string `json:"secrets_file,omitempty"`
	StateDir              string `json:"state_dir,omitempty"`
	Socket                string `json:"alertmanager,omitempty"`
	// BackupRecord is the backup record's path; "off" reads none.
	BackupRecord string `json:"backup_record,omitempty"`

	logger    *zap.Logger
	ls        *liveswap.App // this config's, or nil: ReportStanding at Start
	cfg       *resolved
	rendered  []byte              // alertmanager.yml, rendered in Provision
	files     map[string]string   // the URL files it names
	receivers map[string][]string // each receiver's channels
	known     []string            // every KEY=value of the secrets file, for the filter
	// diskPaths are the directories whose filesystems the disk source
	// measures: Caddy's data, the apps' root, notify's own state.
	diskPaths []string
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "notify",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision checks the block against the apps liveswap configures and
// the box's secrets (resolve). It writes nothing and connects to
// nothing, so `motel validate` runs every refusal and leaves the box as
// it was.
func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	trust, ls, err := appTrust(ctx)
	if err != nil {
		return err
	}
	secrets, err := readSecrets(a.secretsFile())
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	var dropped []error
	if a.cfg, dropped, err = a.resolveWith(trust, secrets, pushedSecrets(), dropsChannels()); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	for _, why := range dropped {
		// The reason names the line and the key, never a value.
		a.logger.Error("notify: a channel is dropped from this load: its secret cannot be used; motel validate and motel reload refuse the config until it is mended",
			zap.String("reason", why.Error()), zap.String("secrets_file", a.secretsFile()))
	}
	if len(dropped) > 0 && len(a.cfg.operator) == 0 {
		a.logger.Error("notify: no operator channel is left in this load: box events are journal lines only", zap.String("secrets_file", a.secretsFile()))
	}
	// Every value the file holds is a known value to the filter, named
	// by a channel line or not: a spare URL is as much a credential.
	a.known = knownValues(secrets)
	a.rendered, a.files, a.receivers = render(a.cfg, a.stateDir())
	theEngine.failures.register(ctx.GetMetricsRegistry())

	// The sources. liveswap's events reach the engine through its
	// Notifier; CertMagic's through Caddy's events app, subscribed by
	// name (never all events); the backup record and the disks are
	// polled by the engine.
	a.diskPaths = []string{caddy.AppDataDir(), a.stateDir()}
	if ls != nil {
		a.ls = ls
		ls.SetNotifier(lifecycleSink{theEngine})
		a.diskPaths = append([]string{ls.Root}, a.diskPaths...)
	}
	evApp, err := ctx.App("events")
	if err != nil {
		return fmt.Errorf("notify: the events app: %w", err)
	}
	events, ok := evApp.(*caddyevents.App)
	if !ok {
		return fmt.Errorf("notify: the events app is a %T", evApp)
	}
	for _, name := range []string{"cert_failed", "cert_obtained"} {
		if err := events.On(name, tlsHandler{theEngine}); err != nil {
			return fmt.Errorf("notify: subscribing to %s: %w", name, err)
		}
	}
	if a.AllowInsecureChannels {
		a.logger.Warn("notify: allow_insecure_channels is on: channel URLs may be http and private addresses, which Alertmanager reaches from the network namespace every app shares; for test rigs only")
	}
	return nil
}

// appTrust is each app's deploy_trust, from liveswap's own provisioned
// config. With no liveswap block there are no apps, and every event is
// the box's. AppIfConfigured, not App: App would instantiate an empty
// liveswap, whose Start stops every app unit it does not know.
func appTrust(ctx caddy.Context) (map[string][]TrustSource, *liveswap.App, error) {
	mod, err := ctx.AppIfConfigured("liveswap")
	if errors.Is(err, caddy.ErrNotConfigured) {
		return map[string][]TrustSource{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	ls, ok := mod.(*liveswap.App)
	if !ok {
		return nil, nil, fmt.Errorf("notify: the liveswap app is a %T", mod)
	}
	out := map[string][]TrustSource{}
	for app, sources := range ls.TrustSources() {
		for _, s := range sources {
			out[app] = append(out[app], s)
		}
		if out[app] == nil {
			out[app] = []TrustSource{}
		}
	}
	return out, ls, nil
}

// maxSecretsFile is how much of the secrets file is read: the box's
// own cap on an env file (box/DESIGN-box.md, Caps) is far below it.
const maxSecretsFile = 1 << 20

// readSecrets is the box tier's env file: every value, for the filter,
// of which the channel lines name NOTIFY_ ones. A missing file is no
// values, so each secret a line names is refused by name (resolve), not
// as an I/O error.
func readSecrets(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // the configured secrets file
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the box's secrets: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSecretsFile+1))
	if err != nil {
		return nil, fmt.Errorf("reading the box's secrets: %w", err)
	}
	if len(data) > maxSecretsFile {
		return nil, fmt.Errorf("the box's secrets file %s is over %d bytes", path, maxSecretsFile)
	}
	vars, err := liveswap.ParseEnvLines(path, data)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(vars))
	for _, kv := range vars {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out, nil
}

// Start writes the rendered config into the state directory and has
// Alertmanager load it. Nothing here fails the config load: serving
// never waits on notify, and every failure is a journal line naming
// what to do.
func (a *App) Start() error {
	am := newAMClient(a.socket())
	a.applyRender(am)
	// Every value the box's secrets hold is a known value to the
	// filter, whatever line names it.
	theEngine.configure(a, am, a.known, a.logger)
	// What liveswap's watchdogs reported and has not ended, again: after
	// a motel restart the table is empty, and nothing else would report
	// a loop already under way or a refused relaunch (DESIGN-notify.md,
	// "Behaviour"). Pulled here, once the engine serves this config,
	// whichever app Caddy starts first.
	if a.ls != nil {
		a.ls.ReportStanding(lifecycleSink{theEngine})
	}
	return nil
}

// applyRender writes this config's render into the state directory and
// has Alertmanager load it, with a journal line for each outcome: at
// Start, and when the engine goes back to this config after a newer one
// was stopped by a failed reload, whose render had replaced this one's.
func (a *App) applyRender(am *amClient) {
	ctx, cancel := context.WithTimeout(context.Background(), amRequestTimeout)
	defer cancel()
	reloaded, err := apply(ctx, a.stateDir(), a.rendered, a.files, am)
	switch {
	case errors.Is(err, errNotSetUp):
		a.logger.Warn("notify: Alertmanager is not set up on this box (run motel notify setup as root); events are journal lines only until it is", zap.String("state_dir", a.stateDir()))
	case errors.Is(err, errUnanswered):
		a.logger.Warn("notify: "+errUnanswered.Error(), zap.String("state_dir", a.stateDir()), zap.Error(err))
	case err != nil:
		a.logger.Error("notify: Alertmanager's config was not updated", zap.String("state_dir", a.stateDir()), zap.Error(err))
	case reloaded:
		a.logger.Info("notify: Alertmanager reloaded the rendered config", zap.String("config", filepath.Join(a.stateDir(), ConfigName)))
	}
}

// Stop lets go of the engine, unless a newer config has taken it.
func (a *App) Stop() error {
	theEngine.release(a)
	return nil
}

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)

func (a *App) stateDir() string {
	if a.StateDir == "" {
		return DefaultStateDir
	}
	return a.StateDir
}

func (a *App) socket() string {
	if a.Socket == "" {
		return DefaultSocket
	}
	return a.Socket
}

func (a *App) backupRecord() string {
	switch a.BackupRecord {
	case "":
		return DefaultBackupRecord
	case "off":
		return ""
	}
	return a.BackupRecord
}

// knownValues is the filter's known values: every KEY=value of the
// secrets file.
func knownValues(secrets map[string]string) []string {
	out := make([]string, 0, len(secrets))
	for k, v := range secrets {
		out = append(out, k+"="+v)
	}
	return out
}
