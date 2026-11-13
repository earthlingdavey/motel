package notify

import (
	"context"
	"encoding/json"
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

// appID is the Caddy app's ID and the global option's name: one global
// option yields one Caddy app (httpcaddyfile), so the store is a part of
// this app, not an app of its own.
const appID = "monitor"

// App is the `monitor` global option. Its notify lines are the
// channels, who the operator is, which identity's channels an app's
// events also reach, and where the box's secrets and the backup record
// are (DESIGN-notify.md); its metrics line is the store
// (DESIGN-metrics.md).
type App struct {
	Channels        []Channel `json:"channels,omitempty"`
	Operator        []string  `json:"operator,omitempty"`
	Binds           []Bind    `json:"binds,omitempty"`
	HeartbeatSecret string    `json:"heartbeat_secret,omitempty"`
	// DeploySucceeded sends deploy_succeeded, which #418's table has
	// off by default; nil for no line. A pointer, so that
	// `deploy_succeeded off` is a line in the JSON too (storeAlone).
	DeploySucceeded *bool `json:"deploy_succeeded,omitempty"`
	// AllowInsecureChannels admits http and private addresses for test
	// rigs whose receiver is a stub on the compose network. Warned at
	// every load.
	AllowInsecureChannels bool   `json:"allow_insecure_channels,omitempty"`
	SecretsFile           string `json:"secrets_file,omitempty"`
	// BackupRecord is the backup record's path; "off" reads none.
	BackupRecord string `json:"backup_record,omitempty"`
	// Metrics is the metrics line, nil for none (DESIGN-metrics.md,
	// "The metrics line").
	Metrics *MetricsStore `json:"metrics,omitempty"`

	logger *zap.Logger
	// notifies is whether this config runs notify: every block but a
	// metrics line alone (storeAlone). Set in Provision.
	notifies  bool
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
		ID:  appID,
		New: func() caddy.Module { return new(App) },
	}
}

// Provision checks the block against the apps liveswap configures and
// the box's secrets (resolve), and wires the store into this config
// when the block has a metrics line. It writes nothing and connects to
// nothing, so `motel validate` runs every refusal and leaves the box as
// it was. notify's part comes first: the store's rules are rendered
// from its receivers.
func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	ls, err := liveswapApp(ctx)
	if err != nil {
		return err
	}
	a.notifies = !a.storeAlone()
	if a.notifies {
		if err := a.provisionNotify(ctx, ls); err != nil {
			return err
		}
	}
	if a.Metrics != nil {
		var n *App
		if a.notifies {
			n = a
		}
		if err := a.Metrics.provision(ctx, ls, n, a.logger); err != nil {
			return err
		}
	}
	return nil
}

// storeAlone reports a block that is a metrics line and nothing else:
// the store keeps the numbers, and there is no notify to run (no
// engine, no Alertmanager config; DESIGN-metrics.md, "The metrics
// line"). Any other line is notify's, and makes the block notify's
// too, held to its refusals: an operator line among them. A block with
// no line at all is refused as notify's, for its missing operator line.
//
// A line is a field with a value in the app's JSON, which is what a
// JSON config writes and what the Caddyfile adapts to: every field is
// omitempty, so an empty list or string or a false bool is none, as no
// Caddyfile line writes one; the one field a line can set to its zero
// value is a pointer (deploy_succeeded off), so that it counts. So the test is the JSON with the metrics line
// taken out, never a list of fields a new one could be missing from.
func (a *App) storeAlone() bool {
	if a.Metrics == nil {
		return false
	}
	rest := *a
	rest.Metrics = nil
	b, err := json.Marshal(&rest)
	return err == nil && string(b) == "{}"
}

// deploySucceeded is the deploy_succeeded line: off when absent.
func (a *App) deploySucceeded() bool {
	return a.DeploySucceeded != nil && *a.DeploySucceeded
}

// provisionNotify is Provision's notify part.
func (a *App) provisionNotify(ctx caddy.Context, ls *liveswap.App) error {
	trust := appTrust(ls)
	drop := dropsChannels()
	secrets, unusable := readSecrets(a.secretsFile())
	if unusable == nil && runningCommand() == "validate" {
		// validate runs as root, who reads any file; the server runs as
		// motel, and a file motel cannot read is refused at a reload and
		// drops every channel at a boot. So validate refuses it too.
		unusable = motelCannotRead(a.secretsFile())
	}
	switch {
	case unusable != nil && !drop:
		return fmt.Errorf("monitor: %w", unusable)
	case unusable != nil:
		// A boot serves: a file it cannot read or parse drops every line
		// that reads a secret, as a missing secret does ("Secrets at
		// load"), and the error is the file's, never a value.
		a.logger.Error("monitor: the box's secrets file cannot be used: every channel that reads a secret is dropped from this load; motel validate and motel reload refuse the config until it is mended",
			zap.String("secrets_file", a.secretsFile()), zap.Error(unusable))
		secrets = map[string]string{}
	}
	var dropped []error
	var err error
	if a.cfg, dropped, err = a.resolveWith(trust, secrets, pushedSecrets(), drop, unusable); err != nil {
		return fmt.Errorf("monitor: %w", err)
	}
	for _, why := range dropped {
		// The reason names the line and the key, never a value.
		a.logger.Error("monitor: a channel is dropped from this load: its secret cannot be used; motel validate and motel reload refuse the config until it is mended",
			zap.String("reason", why.Error()), zap.String("secrets_file", a.secretsFile()))
	}
	if len(dropped) > 0 && len(a.cfg.operator) == 0 {
		a.logger.Error("monitor: no operator channel is left in this load: box events are journal lines only", zap.String("secrets_file", a.secretsFile()))
	}
	// Every value the file holds is a known value to the filter, named
	// by a channel line or not: a spare URL is as much a credential.
	a.known = knownValues(secrets)
	a.rendered, a.files, a.receivers = render(a.cfg, theStateDir)
	theEngine.failures.register(ctx.GetMetricsRegistry())

	// The sources. liveswap's events reach the engine through its
	// Notifier; CertMagic's through Caddy's events app, subscribed by
	// name (never all events); the backup record and the disks are
	// polled by the engine.
	a.diskPaths = []string{caddy.AppDataDir(), theStateDir}
	if ls != nil {
		a.ls = ls
		ls.SetNotifier(lifecycleSink{e: theEngine, c: theComparator})
		a.diskPaths = append([]string{ls.Root}, a.diskPaths...)
	}
	evApp, err := ctx.App("events")
	if err != nil {
		return fmt.Errorf("monitor: the events app: %w", err)
	}
	events, ok := evApp.(*caddyevents.App)
	if !ok {
		return fmt.Errorf("monitor: the events app is a %T", evApp)
	}
	for _, name := range []string{"cert_failed", "cert_obtained"} {
		if err := events.On(name, tlsHandler{theEngine}); err != nil {
			return fmt.Errorf("monitor: subscribing to %s: %w", name, err)
		}
	}
	a.warnAtLoad(a.cfg.heartbeat)
	return nil
}

// warnAtLoad says, at every load (validate's too), what the block
// accepts but the operator should hear each time. heartbeat is the
// resolved config's, so a boot that drops the line for its secret says
// so too.
func (a *App) warnAtLoad(heartbeat string) {
	if a.AllowInsecureChannels {
		a.logger.Warn("monitor: allow_insecure_channels is on: channel URLs may be http and private addresses, which Alertmanager reaches from the network namespace every app shares; for test rigs only")
	}
	// Without a heartbeat the Watchdog goes to the blackhole: a box,
	// motel or Alertmanager that stops is told by no one (requirement 6).
	if heartbeat == "" {
		a.logger.Warn("monitor: no heartbeat in this load: nothing outside the box hears that it, motel or Alertmanager stopped; add heartbeat {secret.NOTIFY_<NAME>} with a dead-man check's ping URL, or mend its secret (notify/DESIGN-notify.md, \"Dead-man and uptime\")")
	}
}

// liveswapApp is this config's liveswap app, nil for no liveswap block.
// AppIfConfigured, not App: App would instantiate an empty liveswap,
// whose Start stops every app unit it does not know.
func liveswapApp(ctx caddy.Context) (*liveswap.App, error) {
	mod, err := ctx.AppIfConfigured("liveswap")
	if errors.Is(err, caddy.ErrNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ls, ok := mod.(*liveswap.App)
	if !ok {
		return nil, fmt.Errorf("monitor: the liveswap app is a %T", mod)
	}
	return ls, nil
}

// appTrust is each app's deploy_trust, from liveswap's own provisioned
// config. With no liveswap block (ls nil) there are no apps, and every
// event is the box's.
func appTrust(ls *liveswap.App) map[string][]TrustSource {
	out := map[string][]TrustSource{}
	if ls == nil {
		return out
	}
	for app, sources := range ls.TrustSources() {
		for _, s := range sources {
			out[app] = append(out[app], s)
		}
		if out[app] == nil {
			out[app] = []TrustSource{}
		}
	}
	return out
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
// Alertmanager load it, then writes the store's rules and points the
// push at this config. Nothing here fails the config load: serving
// never waits on notify or the store, and every failure is a journal
// line naming what to do. notify's engine comes first, so that the
// store's chain events reach an engine serving this config.
func (a *App) Start() error {
	if a.notifies {
		a.startNotify()
	}
	if a.Metrics != nil {
		a.Metrics.start()
	}
	return nil
}

// startNotify is Start's notify part.
func (a *App) startNotify() {
	am := newAMClient(theSocket)
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
		a.ls.ReportStanding(lifecycleSink{e: theEngine})
	}
}

// applyRender writes this config's render into the state directory and
// has Alertmanager load it, with a journal line for each outcome: at
// Start, and when the engine goes back to this config after a newer one
// was stopped by a failed reload, whose render had replaced this one's.
func (a *App) applyRender(am *amClient) {
	ctx, cancel := context.WithTimeout(context.Background(), amRequestTimeout)
	defer cancel()
	reloaded, err := apply(ctx, theStateDir, a.rendered, a.files, am)
	switch {
	case errors.Is(err, errNotSetUp):
		a.logger.Warn("monitor: Alertmanager is not set up on this box (run motel monitor setup as root); events are journal lines only until it is", zap.String("state_dir", theStateDir))
	case errors.Is(err, errUnanswered):
		a.logger.Warn("monitor: "+errUnanswered.Error(), zap.String("state_dir", theStateDir), zap.Error(err))
	case err != nil:
		a.logger.Error("monitor: Alertmanager's config was not updated", zap.String("state_dir", theStateDir), zap.Error(err))
	case reloaded:
		a.logger.Info("monitor: Alertmanager reloaded the rendered config", zap.String("config", filepath.Join(theStateDir, ConfigName)))
	}
}

// applyNothing writes the config that sends nothing, setup's own, into
// this config's state directory, removes the URL files, and has
// Alertmanager load it: a reload left no config that runs notify
// (engine.release).
func (a *App) applyNothing() {
	ctx, cancel := context.WithTimeout(context.Background(), amRequestTimeout)
	defer cancel()
	reloaded, err := apply(ctx, theStateDir, defaultConfig, map[string]string{}, newAMClient(theSocket))
	config := zap.String("config", filepath.Join(theStateDir, ConfigName))
	switch {
	case errors.Is(err, errNotSetUp):
	case errors.Is(err, errUnanswered):
		// The file is written: Alertmanager reads it at its next start,
		// and motel's next load asks again; until then it runs the last
		// render.
		a.logger.Warn("monitor: no loaded config has the monitor block's channels, and Alertmanager did not answer the reload of the config that sends nothing: until it loads it, what it holds may reach the channels as a recovery that did not happen", config, zap.Error(err))
	case err != nil:
		a.logger.Error("monitor: no loaded config has the monitor block's channels, and Alertmanager was not given the config that sends nothing: what it holds may reach the channels as a recovery that did not happen", zap.String("state_dir", theStateDir), zap.Error(err))
	case reloaded:
		a.logger.Info("monitor: no loaded config has the monitor block's channels: Alertmanager was given the config that sends nothing, so what it held ends without a message", config)
	default:
		// Not running (it reads the file when it starts), or already on it.
		a.logger.Info("monitor: no loaded config has the monitor block's channels: the config that sends nothing is in place for Alertmanager", config)
	}
}

// Stop lets go of the pusher and the engine, unless a newer config has
// taken them. The pusher first: what it clears as it stops reaches an
// engine still serving this config.
func (a *App) Stop() error {
	if a.Metrics != nil {
		thePusher.release(a.Metrics)
		theComparator.release(a.Metrics)
	}
	if a.notifies {
		theEngine.release(a)
	}
	return nil
}

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)

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
