package notify

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
)

func init() {
	caddy.RegisterModule(App{})
}

// App is the `notify` global option: the channels, who the teacher is,
// which identity's channels an app's events also reach, and where
// Alertmanager is (DESIGN-notify.md).
type App struct {
	Channels        []Channel `json:"channels,omitempty"`
	Teacher         []string  `json:"teacher,omitempty"`
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

	logger *zap.Logger
	cfg    *resolved
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
	trust, err := appTrust(ctx)
	if err != nil {
		return err
	}
	secrets, err := readSecrets(a.secretsFile())
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	if a.cfg, err = a.resolve(trust, secrets); err != nil {
		return fmt.Errorf("notify: %w", err)
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
func appTrust(ctx caddy.Context) (map[string][]TrustSource, error) {
	mod, err := ctx.AppIfConfigured("liveswap")
	if errors.Is(err, caddy.ErrNotConfigured) {
		return map[string][]TrustSource{}, nil
	}
	if err != nil {
		return nil, err
	}
	ls, ok := mod.(*liveswap.App)
	if !ok {
		return nil, fmt.Errorf("notify: the liveswap app is a %T", mod)
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
	return out, nil
}

// maxSecretsFile is how much of the secrets file is read: the box's
// own cap on an env file (box/DESIGN-box.md, Caps) is far below it.
const maxSecretsFile = 1 << 20

// readSecrets is the NOTIFY_ values of the box tier's env file. A
// missing file is no values, so each secret a line names is refused by
// name (resolve), not as an I/O error.
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
		if strings.HasPrefix(k, "NOTIFY_") {
			out[k] = v
		}
	}
	return out, nil
}

// Start is a no-op until the renderer and the poster exist.
func (a *App) Start() error { return nil }

// Stop is a no-op.
func (a *App) Stop() error { return nil }

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)
