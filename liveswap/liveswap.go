// Package liveswap turns Caddy into a zero-downtime deploy orchestrator
// for a single server. CI builds a tarball and POSTs a webhook with its
// URL and version; Caddy downloads it, runs an optional pre-start
// command (migrations), starts the new version as a systemd unit on its
// own unix socket, health-gates it, atomically cuts traffic over,
// then gracefully stops the old version. Part of motel, from
// smallhours.
//
// Three cooperating modules:
//   - liveswap (this file): the app module holding app definitions and
//     the per-app orchestration state
//   - http.handlers.liveswap_webhook (handler.go): the deploy trigger
//   - http.reverse_proxy.upstreams.liveswap (upstreams.go): routes
//     reverse_proxy traffic to the active version's socket
package liveswap

import (
	// Blank import: harden's init makes this process non-dumpable before
	// main — before os, fmt and every initializer that depends on them
	// (see liveswap/harden for the exact guarantee).
	_ "github.com/smallhoursorg/motel/liveswap/harden"

	"context"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/dustin/go-humanize"
	"github.com/smallhoursorg/motel/liveswap/backupdecl"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(App{})
}

// appPool holds every managedApp, keyed by app name, OUTSIDE any config
// instance. This is what makes app processes survive config reloads:
// Caddy provisions the new config (which takes a pool reference) before
// it cleans up the old one (which releases its reference), so the
// refcount never touches zero across a reload and Destruct only runs
// at real shutdown (where it leaves the unit running for reattach) or
// when an app is removed from the config (where it stops it).
var appPool = caddy.NewUsagePool()

func poolKey(name string) string { return "liveswap:app:" + name }

// App is the `liveswap` Caddy app module: the deploy orchestrator.
type App struct {
	// Root is the directory holding all app state on disk:
	// <root>/<app>/{releases/<version>/, shared/, tmp/, state.json}.
	// Default "/var/lib/liveswap".
	Root string `json:"root,omitempty"`

	// DeployTrust is the default set of deploy-auth trust sources for
	// all apps; each app may override it. A deploy request carries an
	// `Authorization: Bearer <JWT>` and is authorized if any source
	// verifies it (OIDC against an issuer's public JWKS, or a local
	// public key). No shared secret ever lives on the box. Every app
	// must resolve to at least one source — config load fails otherwise.
	DeployTrust []deploytrust.TrustConfig `json:"deploy_trust,omitempty"`

	// AllowInsecureHTTP permits artifact downloads over plain http.
	// Default false (https only). Exists for test rigs and LAN setups.
	AllowInsecureHTTP bool `json:"allow_insecure_http,omitempty"`

	// AllowTCPAdmin tells motel's own command gate (cmd/motel) that the
	// admin API is meant to listen on TCP though apps run: every app
	// shares the box's network namespace, so a TCP admin API is theirs
	// to reconfigure the server with, and validate and reload refuse
	// such a config without this. Exists for test rigs whose runner
	// drives the admin API over the network; a box keeps the starter's
	// `admin unix//run/motel/admin.sock`. Read by the gate alone.
	AllowTCPAdmin bool `json:"allow_tcp_admin,omitempty"`

	// ArtifactAllowlist declares where artifacts may be fetched from —
	// required: there is no "any origin" mode. Entries are host
	// ("artifacts.corp", single-tenant) or host+path prefix
	// ("github.com/your-org/"): on multi-tenant platforms the tenant
	// lives in the path, so a host-only rule would admit anyone's
	// artifacts. Governs the first hop only (GitHub asset URLs
	// redirect to S3); every hop must still be https unless
	// AllowInsecureHTTP. Apps may override.
	ArtifactAllowlist []string `json:"artifact_allowlist,omitempty"`

	// Apps defines the managed applications, keyed by name
	// ([a-z0-9-]). The name is the webhook path segment and the
	// argument to `dynamic liveswap <name>`.
	Apps map[string]*AppConfig `json:"apps,omitempty"`

	logger  *zap.Logger
	managed map[string]*managedApp
	// observed names the apps an observe handler of this config names
	// (Observe.observe), as keys: AppSample.Observed. Written while the
	// config provisions, and read meanwhile too: Caddy starts a load's
	// admin server before its apps, and its /metrics gathers this
	// config's registry, which notify's collector may join before the
	// HTTP app provisions its handlers. Hence a sync.Map, made at Provision.
	observed *sync.Map
	// notifier is the notify module's listener (SetNotifier), handed
	// to every app at Start; nil reports to nobody.
	notifier Notifier
	specs    map[string]*appSpec // built in Provision, installed at Start
	clients  *fetchClients
	started  bool     // Start ran: this config counts as live until Cleanup
	pooled   []string // pool keys this config instance holds references to
	// recoverCancel/recoverWG own the boot-recovery goroutines this
	// config started, so Cleanup can end them before rolling back or
	// releasing anything.
	recoverCancel context.CancelFunc
	recoverWG     *sync.WaitGroup // pointer: App values are copied by Caddy
	// metricsSource is the metrics store's (SetMetricsSource),
	// which adds `metrics` to GET /<app>; nil adds nothing.
	metricsSource MetricsSource
	// manager is the manager connection everything this config starts
	// talks to — its apps' runners, its unknown-app sweep, and (when
	// the seams below are nil) its capability measurement. Typed as
	// the interface, so a test installs a fake here and Start touches
	// no real socket; nil means the process-wide client. probeManager
	// and measureSandbox honour it, so that promise holds whether or
	// not the seams are set.
	//
	// managerProbe and sandboxProbe script the two OUTCOMES directly —
	// reachability, and the measured capability — for a test that
	// wants an answer rather than a connection to measure through.
	// Fields rather than the package vars they replace: a test scripts
	// a host on its own App instead of mutating global state and
	// restoring it in a t.Cleanup.
	//
	// processManager is the process-wide client itself — the one every
	// path above falls through to when nothing is installed, which is
	// what every real start does. nil means the real userManager; a
	// test installs a fake to reach that fall-through without a manager
	// to dial, which is the only way to assert the ordering inside it
	// (sharedManager). It carries the same promise as manager: set it
	// alone and nothing this App does touches a real socket, so
	// systemdConn honours it too — otherwise Start would measure
	// through the fake and then hand its apps and its sweep the real
	// one.
	manager         systemdConn
	managerProbe    func() error
	sandboxProbe    func(*zap.Logger) error
	processManager  managerClient
	allowlist       []artifactAllowEntry
	globalVerifiers []deploytrust.Verifier
	// appVerifiers is each app's resolved deploy_trust, built once in
	// Provision and installed by Start. The OIDC discovery/JWKS cache
	// lives on the verifier object, so the set Provision warms must be
	// the set the handler is later handed — not a fresh resolution.
	appVerifiers map[string][]deploytrust.Verifier
}

// AppConfig defines one managed application.
type AppConfig struct {
	// Command starts the app, argv-style, with the release directory as
	// working directory. The app must listen on the unix socket at the
	// injected SOCKET path. Placeholders: {version}, {socket},
	// {release_dir}, {shared_dir}. Required.
	Command []string `json:"command,omitempty"`

	// PreStart runs to completion in the release directory before the
	// new version starts (migrations, asset warming). A non-zero exit
	// aborts the deploy while the old version keeps serving. Optional.
	PreStart []string `json:"pre_start,omitempty"`

	// Env sets extra environment variables. Values may use the same
	// placeholders as Command. Applied on top of env_file.
	Env map[string]string `json:"env,omitempty"`

	// EnvFile loads KEY=VALUE lines (e.g. /etc/liveswap/blog.env) into
	// the app's environment. Optional; missing file fails the deploy.
	EnvFile string `json:"env_file,omitempty"`

	// DeployTrust overrides the global deploy-auth trust sources for
	// this app (replaces, not appends).
	DeployTrust []deploytrust.TrustConfig `json:"deploy_trust,omitempty"`

	// ArtifactAllowlist overrides the global allowlist for this app
	// (same entry syntax; replaces, not appends).
	ArtifactAllowlist []string `json:"artifact_allowlist,omitempty"`

	// HealthPath is GET-probed on the new instance; 2xx = healthy.
	// "off" disables HTTP probing (the gate becomes "process stays
	// alive through the soak"). Default "/health".
	HealthPath string `json:"health_path,omitempty"`

	// HealthInterval is the time between probes. Default 5s.
	HealthInterval caddy.Duration `json:"health_interval,omitempty"`

	// HealthTimeout is the per-probe timeout. Default 2s.
	HealthTimeout caddy.Duration `json:"health_timeout,omitempty"`

	// Soak is how long the new instance must be continuously healthy
	// before traffic cuts over. 0 is no soak: traffic cuts over on the
	// first healthy probe, so it needs a HealthPath (Validate). A
	// pointer, as DeployLogLines, so that 0 stays apart from unset.
	// Default 15s.
	Soak *caddy.Duration `json:"soak,omitempty"`

	// Deadline bounds the whole health gate (and pre_start); a version
	// that cannot get healthy within it is stopped and the deploy
	// fails. Default 5m.
	Deadline caddy.Duration `json:"deadline,omitempty"`

	// Drain is the pause between cutover and SIGTERM to the old
	// version, letting in-flight requests finish. Default 5s.
	Drain caddy.Duration `json:"drain,omitempty"`

	// Grace is how long the old version gets between SIGTERM and
	// SIGKILL. Default 10s.
	Grace caddy.Duration `json:"grace,omitempty"`

	// Watchdog enables continuous supervision of the running instance:
	// a crash, or WatchdogFailures consecutive failed health probes,
	// restarts the current version (with backoff, within the
	// WatchdogRestarts/WatchdogWindow budget). "off" disables it.
	// Default "on".
	Watchdog string `json:"watchdog,omitempty"`

	// WatchdogFailures is how many consecutive health probes must fail
	// before the watchdog restarts the app; a single passing probe
	// resets the count. Default 3.
	WatchdogFailures int `json:"watchdog_failures,omitempty"`

	// WatchdogGrace is how long after every (re)start probe failures
	// are ignored, giving the app time to boot; it re-arms on each
	// watchdog restart. A process exit always counts, grace or not.
	// Default 30s.
	WatchdogGrace caddy.Duration `json:"watchdog_grace,omitempty"`

	// WatchdogRestarts is how many watchdog restarts (crash and health
	// triggered alike) may happen within WatchdogWindow. The budget is
	// a rate limiter, not a give-up point: when the window is full the
	// watchdog throttles until the oldest restart slides out, then
	// keeps trying. A successful deploy resets the budget. Default 5.
	WatchdogRestarts int `json:"watchdog_restarts,omitempty"`

	// WatchdogWindow is the sliding window the restart budget is
	// counted over. Default 10m.
	WatchdogWindow caddy.Duration `json:"watchdog_window,omitempty"`

	// DeployLogLines is how many of the app's last journal lines a
	// failed deploy's response carries (the pre_start's and the app's,
	// since the deploy began), on top of the exit status and the last
	// health probe's answer. The response goes to whoever deployed —
	// a CI log, and with a public repository a public one — so it
	// passes the response filter first, and 0 keeps the app's output
	// on the box entirely. Bounded at 8 KiB whatever the count.
	// Default 40, at most 1000.
	DeployLogLines *int `json:"deploy_log_lines,omitempty"`

	// Keep is how many release directories to retain on disk,
	// including the current one: the most recently deployed, a
	// rollback counting as a deploy of its version (state.go,
	// "Release order"). Default 5.
	Keep int `json:"keep,omitempty"`

	// MaxArtifactSize caps the artifact download, in bytes (the
	// Caddyfile accepts human forms like 100MB). Decompressed content
	// is additionally capped at 10x this. Default 100MB.
	MaxArtifactSize int64 `json:"max_artifact_size,omitempty"`

	// MaxArtifactEntries caps the number of entries (files, directories,
	// links) an artifact may contain. The byte cap does not bound what
	// extraction consumes — every entry costs an inode and most cost a
	// disk block — so this is what keeps one hostile artifact from
	// filling the disk for everything else on the box. A deploy warns
	// at 75% of it. Default 100000; a CI-built artifact is thousands.
	MaxArtifactEntries int `json:"max_artifact_entries,omitempty"`

	// Backup declares which paths under the app's shared dir are worth
	// backing up, and which of them are SQLite databases. It is a
	// declaration only: motel validates it and does nothing else
	// with it. Its paths take no placeholders. Optional.
	Backup *backupdecl.Config `json:"backup,omitempty"`
	// MemoryMax bounds the RAM every unit of the app may use (the
	// instance and the pre_start alike), in bytes; the Caddyfile
	// accepts 512MB. At it the kernel kills the process that asked, the
	// unit fails and the watchdog restarts the app; it gets no swap
	// under it. A running instance gets a reload's new value at once.
	// Default none.
	MemoryMax uint64 `json:"memory_max,omitempty"`

	// TasksMax bounds the threads and processes of every unit of the
	// app; past it a fork or thread fails with EAGAIN, which a `sh -c`
	// wrapped command dies on. The unit's sleeping sibling (#380) is
	// one of them, so the app itself gets one fewer, and the floor is
	// 2. Default none: the user manager's own, 15% of the kernel's
	// threads-max.
	TasksMax int `json:"tasks_max,omitempty"`

	// CPUQuota bounds the CPU time of every unit of the app, in percent
	// of one CPU (200 is two CPUs' worth); past it the app waits, it is
	// never killed. Default none.
	CPUQuota int `json:"cpu_quota,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "liveswap",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision applies defaults, resolves {env.*} placeholders in the
// static config, and binds each app definition to its long-lived
// managedApp in the pool.
func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	// Before the root's placeholders are resolved in place, below: the
	// check is about how the root is written.
	if err := backupNeedsLiteralRoot(a.Root, a.Apps); err != nil {
		return err
	}
	repl := caddy.NewReplacer()

	// ReplaceKnown resolves {env.*} now but leaves the deploy-time
	// placeholders ({version}, {socket}, ...) untouched for app.go.
	a.Root = repl.ReplaceKnown(a.Root, "")
	for i, e := range a.ArtifactAllowlist {
		a.ArtifactAllowlist[i] = repl.ReplaceKnown(e, "")
	}
	if a.Root == "" {
		a.Root = backupdecl.DefaultRoot
	}

	var err error
	if a.allowlist, err = parseAllowlist(a.ArtifactAllowlist); err != nil {
		return err
	}
	clients := &fetchClients{
		download: newDownloadClient(a.AllowInsecureHTTP),
		// The JWKS client fetches OIDC issuers' public keys over https
		// only (unless allow_insecure_http), with a bounded timeout.
		jwks: deploytrust.NewJWKSClient(a.AllowInsecureHTTP),
	}
	// Global trust sources back the unknown-app path: a request for an
	// app that does not exist is still authenticated (against the
	// global sources) before its 404, so the status never leaks app
	// names to unauthenticated callers, and no refusal waits on an
	// issuer; its time can still differ by the local work a name's
	// sources do, the residual DESIGN-threat-model.md accepts
	// ("Webhook endpoint"). The same wiring serves the box webhook (New).
	if a.globalVerifiers, err = deploytrust.Resolve(repl, a.DeployTrust, clients.jwks); err != nil {
		return err
	}

	// Build every spec first, then validate, and only then commit to the
	// process-global pool. Caddy calls Validate *after* Provision, but a
	// managedApp is shared across config loads — so if we mutated it here
	// and Validate then rejected the config, the rejected policy (its
	// deploy-auth verifiers included) would already be live on the pooled
	// object. Validate-before-commit keeps a bad reload from taking effect.
	a.managed = make(map[string]*managedApp, len(a.Apps))
	a.observed = new(sync.Map)
	specs := make(map[string]*appSpec, len(a.Apps))
	// One pass: a spec is built from its own app's config alone. This
	// used to need two — the sandbox's hidden set was derived from
	// EVERY app's env_file, so all of them had to have their {env.*}
	// placeholders resolved before the first spec was built — but the
	// view is deny-by-default now and derives nothing from siblings.
	for name, cfg := range a.Apps {
		if cfg == nil {
			cfg = new(AppConfig)
			a.Apps[name] = cfg
		}
		cfg.applyDefaults(repl)
		spec, err := a.buildSpec(name, cfg)
		if err != nil {
			return err
		}
		specs[name] = spec
	}
	if err := a.Validate(); err != nil {
		return err
	}
	warnEnvFileInView(a.logger, specs)
	warnEnvFileMode(a.logger, specs)
	warnUnboundTrust(a.logger, a.DeployTrust, a.Apps)
	// Take pool references now (so a reload never drops the refcount to
	// zero) but install nothing on the pooled apps until Start: Caddy
	// keeps the old config if any app's Start fails, and `validate`
	// never starts at all — neither may leave a rejected spec live on
	// an app that is still serving under the previous config.
	a.specs = specs
	a.clients = clients
	for name := range specs {
		val, _ := appPool.LoadOrStore(poolKey(name), newManagedApp(name))
		a.managed[name] = val.(*managedApp)
		a.pooled = append(a.pooled, poolKey(name))
	}
	// Begin each OIDC issuer's discovery and key fetch again in the
	// background, so this config picks up what the issuers now publish;
	// the keys the process already holds stay in use until the fetch
	// ends (deploytrust.Warm). The sets built here are the ones Start
	// installs (appVerifiers).
	a.appVerifiers = make(map[string][]deploytrust.Verifier, len(specs))
	sets := [][]deploytrust.Verifier{a.globalVerifiers}
	for name, spec := range specs {
		vs := deploytrust.Verifiers(spec.trust, clients.jwks)
		a.appVerifiers[name] = vs
		sets = append(sets, vs)
	}
	deploytrust.Warm(sets...)
	return nil
}

// liveStartedApps counts liveswap configs that have Started and not
// yet been Cleaned up. It is how Destruct tells "an app was removed by
// a reload that is now serving" (another started config is live: stop
// the units) from "this config never became the serving one" — a
// candidate rejected by another app's Start, `validate`, or process
// exit — where the units belong to whoever is or will be serving them.
var liveStartedApps atomic.Int32

// caddyExiting reports whether the whole process is shutting down (as
// opposed to a config unloading an app).
//
// Tests flip it through the seam below, which is an atomic rather than
// a plain var because App.Start's unknown-app sweep reads it from a
// goroutine nothing joins: a test restoring the seam can overlap a
// sweep still running from an earlier test, and a plain var leaves
// that undefined. nil = caddy.Exiting.
var caddyExitingSeam atomic.Pointer[func() bool]

func caddyExiting() bool {
	if f := caddyExitingSeam.Load(); f != nil {
		return (*f)()
	}
	return caddy.Exiting()
}

func (cfg *AppConfig) applyDefaults(repl *caddy.Replacer) {
	deploytrust.ResolvePlaceholders(repl, cfg.DeployTrust)
	cfg.EnvFile = repl.ReplaceKnown(cfg.EnvFile, "")
	for i, e := range cfg.ArtifactAllowlist {
		cfg.ArtifactAllowlist[i] = repl.ReplaceKnown(e, "")
	}
	for i, arg := range cfg.Command {
		cfg.Command[i] = repl.ReplaceKnown(arg, "")
	}
	for i, arg := range cfg.PreStart {
		cfg.PreStart[i] = repl.ReplaceKnown(arg, "")
	}
	for k, v := range cfg.Env {
		cfg.Env[k] = repl.ReplaceKnown(v, "")
	}
	if cfg.HealthPath == "" {
		cfg.HealthPath = "/health"
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = caddy.Duration(5 * time.Second)
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = caddy.Duration(2 * time.Second)
	}
	if cfg.Soak == nil {
		d := caddy.Duration(15 * time.Second)
		cfg.Soak = &d
	}
	if cfg.Deadline == 0 {
		cfg.Deadline = caddy.Duration(5 * time.Minute)
	}
	if cfg.Drain == 0 {
		cfg.Drain = caddy.Duration(5 * time.Second)
	}
	if cfg.Grace == 0 {
		cfg.Grace = caddy.Duration(10 * time.Second)
	}
	if cfg.Watchdog == "" {
		cfg.Watchdog = "on"
	}
	if cfg.WatchdogFailures == 0 {
		cfg.WatchdogFailures = 3
	}
	if cfg.WatchdogGrace == 0 {
		cfg.WatchdogGrace = caddy.Duration(30 * time.Second)
	}
	if cfg.WatchdogRestarts == 0 {
		cfg.WatchdogRestarts = 5
	}
	if cfg.WatchdogWindow == 0 {
		cfg.WatchdogWindow = caddy.Duration(10 * time.Minute)
	}
	if cfg.Keep == 0 {
		cfg.Keep = 5
	}
	if cfg.DeployLogLines == nil {
		n := 40
		cfg.DeployLogLines = &n
	}
	if cfg.MaxArtifactSize == 0 {
		cfg.MaxArtifactSize = 100_000_000
	}
	if cfg.MaxArtifactEntries == 0 {
		cfg.MaxArtifactEntries = 100_000
	}
}

// backupNeedsLiteralRoot refuses a root written with a placeholder
// when an app declares a backup. A declaration is written for a reader
// that is not this process, so it has to mean something from the
// Caddyfile alone: this process's environment is the only thing that
// resolves {env.*}, it may hold the ACME tokens, and it is not handed
// to anything else. Only the combination is refused; with no backup
// block, a placeholder root loads.
//
// The Caddyfile's own {$VAR} is out of reach here: the adapter
// substitutes it in the text before this module is given a root, so a
// root or a backup path written with one arrives as a literal and
// passes.
func backupNeedsLiteralRoot(root string, apps map[string]*AppConfig) error {
	if !strings.ContainsAny(root, "{}") {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(apps)) {
		if cfg := apps[name]; cfg != nil && cfg.Backup != nil {
			return fmt.Errorf("app %s declares a backup, but root %q is written with a placeholder ({ or }), which only motel's own environment can resolve and a backup does not have; write the root as a literal path", name, root)
		}
	}
	return nil
}

func (a *App) buildSpec(name string, cfg *AppConfig) (*appSpec, error) {
	healthPath := cfg.HealthPath
	if healthPath == "off" {
		healthPath = ""
	}
	allowlist := a.allowlist
	if len(cfg.ArtifactAllowlist) > 0 {
		var err error
		if allowlist, err = parseAllowlist(cfg.ArtifactAllowlist); err != nil {
			return nil, fmt.Errorf("app %s: %w", name, err)
		}
	}
	trust, err := deploytrust.Build(a.DeployTrust, cfg.DeployTrust)
	if err != nil {
		return nil, fmt.Errorf("app %s: %w", name, err)
	}
	return &appSpec{
		name:               name,
		command:            cfg.Command,
		preStart:           cfg.PreStart,
		env:                cfg.Env,
		envFile:            cfg.EnvFile,
		trust:              trust,
		healthPath:         healthPath,
		healthInterval:     time.Duration(cfg.HealthInterval),
		healthTimeout:      time.Duration(cfg.HealthTimeout),
		soak:               time.Duration(*cfg.Soak),
		deadline:           time.Duration(cfg.Deadline),
		drain:              time.Duration(cfg.Drain),
		grace:              time.Duration(cfg.Grace),
		watchdogOn:         cfg.Watchdog != "off",
		wdFailures:         cfg.WatchdogFailures,
		wdGrace:            time.Duration(cfg.WatchdogGrace),
		wdRestarts:         cfg.WatchdogRestarts,
		wdWindow:           time.Duration(cfg.WatchdogWindow),
		keep:               cfg.Keep,
		deployLogLines:     *cfg.DeployLogLines,
		maxArtifactSize:    cfg.MaxArtifactSize,
		maxArtifactEntries: cfg.MaxArtifactEntries,
		caps:               resourceCaps{memoryMax: cfg.MemoryMax, tasksMax: cfg.TasksMax, cpuQuota: cfg.CPUQuota},
		allowInsecure:      a.AllowInsecureHTTP,
		allowlist:          allowlist,
		dirs:               newAppDirs(a.Root, name),
	}, nil
}

// TrustSources is each app's effective deploy_trust (its own blocks, or
// the global ones it falls back to), as this config built them in
// Provision: for the notify module, which sends an app's events to the
// channels bound to an identity the app trusts, and refuses a binding
// no app trusts. Nil before Provision.
func (a *App) TrustSources() map[string][]deploytrust.Source {
	if a.specs == nil {
		return nil
	}
	out := make(map[string][]deploytrust.Source, len(a.specs))
	for name, spec := range a.specs {
		out[name] = slices.Clone(spec.trust)
	}
	return out
}

// MemoryCapped is whether each app has a memory_max, as this config
// built it: for the notify module, which refuses a memory_share rule
// naming an app with none (its share has nothing to divide by). Nil
// before Provision.
func (a *App) MemoryCapped() map[string]bool {
	if a.specs == nil {
		return nil
	}
	out := make(map[string]bool, len(a.specs))
	for name, spec := range a.specs {
		out[name] = spec.caps.memoryMax > 0
	}
	return out
}

// Releases are the release versions on disk for app, newest first
// (listReleases): what notify's `motel monitor silence add --release`
// takes. Nil for an app this config does not run, or when the
// directory cannot be read.
func (a *App) Releases(app string) []string {
	if _, ok := a.specs[app]; !ok {
		return nil
	}
	versions, _ := listReleases(newAppDirs(a.Root, app))
	return versions
}

// Validate enforces semantic invariants (runs after Provision has
// applied defaults, for JSON and Caddyfile config alike).
func (a *App) Validate() error {
	if !strings.HasPrefix(a.Root, "/") {
		return fmt.Errorf("root must be an absolute path, got %q", a.Root)
	}
	// Clean, not merely absolute: the containment checks below and in
	// sandbox.go compare paths lexically, so an uncleaned root like
	// /srv/liveswap/../liveswap would be measured against a spelling
	// the checks never see.
	if filepath.Clean(a.Root) != a.Root {
		return fmt.Errorf("root must be a clean path (no . , .. or doubled separators): %q resolves to %q", a.Root, filepath.Clean(a.Root))
	}
	if err := validateSandboxRoot(a.Root); err != nil {
		return err
	}
	for name, cfg := range a.Apps {
		if !appNameRe.MatchString(name) {
			return fmt.Errorf("app name %q must match %s", name, appNameRe)
		}
		if err := validateSocketPath(a.Root, name); err != nil {
			return err
		}
		for k := range cfg.Env {
			if !validEnvKey(k) {
				return fmt.Errorf("app %s: env key %q is not a valid environment variable name (must match %s)", name, k, envKeyRe)
			}
			if shellOwnedEnvKey(k) {
				return fmt.Errorf("app %s: env key %q %s", name, k, shellOwnedEnvKeyMsg)
			}
		}
		if len(cfg.Command) == 0 {
			return fmt.Errorf("app %s: command is required", name)
		}
		if len(a.DeployTrust) == 0 && len(cfg.DeployTrust) == 0 {
			return fmt.Errorf("app %s: no deploy_trust configured — declare who may deploy, e.g. a `deploy_trust github { audience ...; claim repository your-org/%s; claim ref refs/heads/main }` block or a `deploy_trust local { public_key ...; audience ... }` fallback (globally or per app)", name, name)
		}
		// Closed by default, deliberately without an "any origin"
		// escape hatch: a deploy webhook that fetches from anywhere is
		// an SSRF primitive, and on multi-tenant hosts anything short
		// of a tenant pin is theater.
		if len(a.ArtifactAllowlist) == 0 && len(cfg.ArtifactAllowlist) == 0 {
			return fmt.Errorf("app %s: artifact_allowlist is required — declare where artifacts may be fetched from, e.g. `artifact_allowlist github.com/your-org/` (host-only entries suit single-tenant hosts)", name)
		}
		if cfg.HealthPath != "off" && !strings.HasPrefix(cfg.HealthPath, "/") {
			return fmt.Errorf("app %s: health_path must start with / (or be \"off\"), got %q", name, cfg.HealthPath)
		}
		for field, d := range map[string]caddy.Duration{
			"health_interval": cfg.HealthInterval,
			"health_timeout":  cfg.HealthTimeout,
			"deadline":        cfg.Deadline,
			"grace":           cfg.Grace,
		} {
			if d <= 0 {
				return fmt.Errorf("app %s: %s must be positive, got %v", name, field, time.Duration(d))
			}
		}
		// Provision has set Soak by now; a config validated without
		// its defaults has none, which bounds nothing.
		var soak caddy.Duration
		if cfg.Soak != nil {
			soak = *cfg.Soak
		}
		if soak < 0 || cfg.Drain < 0 {
			return fmt.Errorf("app %s: soak and drain must not be negative", name)
		}
		// With health_path off the gate's one check is that the process
		// is alive, made once per health_interval through the soak; with
		// no soak that is a single look straight after the start.
		if cfg.Soak != nil && soak == 0 && cfg.HealthPath == "off" {
			return fmt.Errorf("app %s: soak 0 needs a health_path: with health_path off nothing is probed, so the gate would look once, straight after the process starts, and an app that crashes a moment later would take traffic; set a health_path or a soak above 0", name)
		}
		// The deadline bounds the whole gate, soak included: a soak
		// the deadline cannot contain is a deploy that can never pass.
		// Equal is allowed — the gate judges the soak on the tick at
		// the deadline — so a config that loaded before this check
		// and could deploy still does.
		if soak > cfg.Deadline {
			return fmt.Errorf("app %s: soak (%v) must not exceed deadline (%v), which bounds the whole health gate including the soak", name, time.Duration(soak), time.Duration(cfg.Deadline))
		}
		if cfg.Watchdog != "on" && cfg.Watchdog != "off" {
			return fmt.Errorf("app %s: watchdog must be \"on\" or \"off\", got %q", name, cfg.Watchdog)
		}
		if cfg.WatchdogFailures < 1 {
			return fmt.Errorf("app %s: watchdog_failures must be at least 1, got %d", name, cfg.WatchdogFailures)
		}
		if cfg.WatchdogRestarts < 1 {
			return fmt.Errorf("app %s: watchdog_restarts must be at least 1, got %d", name, cfg.WatchdogRestarts)
		}
		if cfg.WatchdogGrace < 0 {
			return fmt.Errorf("app %s: watchdog_grace must not be negative, got %v", name, time.Duration(cfg.WatchdogGrace))
		}
		if cfg.WatchdogWindow <= 0 {
			return fmt.Errorf("app %s: watchdog_window must be positive, got %v", name, time.Duration(cfg.WatchdogWindow))
		}
		if cfg.Keep < 1 {
			return fmt.Errorf("app %s: keep must be at least 1, got %d", name, cfg.Keep)
		}
		if n := cfg.DeployLogLines; n != nil && (*n < 0 || *n > 1000) {
			return fmt.Errorf("app %s: deploy_log_lines must be between 0 (off) and 1000, got %d", name, *n)
		}
		if cfg.MaxArtifactSize < 1 {
			return fmt.Errorf("app %s: max_artifact_size must be positive, got %d", name, cfg.MaxArtifactSize)
		}
		// The decompressed cap is decompressionRatioCap × this; past
		// here it would overflow to a negative budget that reads every
		// archive as empty.
		if cfg.MaxArtifactSize > math.MaxInt64/decompressionRatioCap {
			return fmt.Errorf("app %s: max_artifact_size must be at most %d, got %d", name, int64(math.MaxInt64/decompressionRatioCap), cfg.MaxArtifactSize)
		}
		if cfg.MaxArtifactEntries < 1 {
			return fmt.Errorf("app %s: max_artifact_entries must be positive, got %d", name, cfg.MaxArtifactEntries)
		}
		if cfg.Backup != nil {
			if err := cfg.Backup.Validate(); err != nil {
				return fmt.Errorf("app %s: backup %w", name, err)
			}
		}
		// The Caddyfile refuses these at parse; a JSON config reaches
		// here directly. 0 is unset for all three; the bounds are the
		// parser's (minMemoryMax, maxCPUQuota).
		if cfg.TasksMax < 0 || cfg.CPUQuota < 0 {
			return fmt.Errorf("app %s: tasks_max and cpu_quota must not be negative, got %d and %d", name, cfg.TasksMax, cfg.CPUQuota)
		}
		if cfg.TasksMax == 1 {
			// The sleeping sibling every unit carries (#380) is one of
			// the tasks: a cap of 1 refuses its fork and the shell dies
			// before the app runs.
			return fmt.Errorf("app %s: tasks_max must be at least 2, got 1; leave it out for the default", name)
		}
		if cfg.MemoryMax > 0 && cfg.MemoryMax < minMemoryMax {
			return fmt.Errorf("app %s: memory_max must be at least %s, got %d bytes", name, humanize.Bytes(minMemoryMax), cfg.MemoryMax)
		}
		if cfg.MemoryMax == infinityUSec {
			// systemd's "infinity" on the bus: a cap of nothing that the
			// handle would report as a cap, and a reattach as none.
			return fmt.Errorf("app %s: memory_max %d is systemd's infinity, not a cap; leave it out for no cap", name, cfg.MemoryMax)
		}
		if cfg.CPUQuota > maxCPUQuota {
			return fmt.Errorf("app %s: cpu_quota must be at most %d%%, got %d%%", name, maxCPUQuota, cfg.CPUQuota)
		}
	}
	// Cross-app, so it needs every app's config and comes after the
	// per-app loop: one app's env_file must not sit inside another
	// app's view.
	return validateEnvFileIsolation(a)
}

// Start recovers apps after a Caddy restart: each app that has a
// recorded current version but no live process is relaunched. Runs in
// the background so a slow app cannot stall config load; the health
// gate is a deploy gate, not a boot gate.
func (a *App) Start() error {
	// Apps run as transient units under this user's systemd manager.
	// Prove it answers before anything runs, with an error that says
	// what to fix; there is deliberately no fallback runner. Checked
	// here rather than in Provision so `motel validate` — which
	// provisions and cleans up without starting — works as any user.
	if len(a.managed) > 0 {
		if err := a.probeManager(); err != nil {
			return err
		}
		// Every app runs in the sandbox, so what this host can deliver
		// is settled here, on every start: the probe runs a throwaway
		// unit with the sandbox applied and checks the namespaces from
		// inside. A host that falls short fails the start — by design,
		// and documented as such — before any app is configured. The
		// error reaches the operator as the reason the server will not
		// come up, so it names the remedy: there is none in config.
		//
		// The measurement itself is cached on the manager connection,
		// so a reload that changes nothing about the manager does not
		// pay for a unit here; see measureSandbox and the cache it
		// reads, userManagerClient.cachedSandboxCapability.
		if err := a.measureSandbox(); err != nil {
			return fmt.Errorf("every app runs in the per-app sandbox and this host cannot deliver it: %w. Fix the host: there is no setting that runs an app without one", err)
		}
	}
	for name, ma := range a.managed {
		ma.configure(a, a.specs[name], a.appVerifiers[name], a.logger.Named(name), a.clients, a.systemdConn())
		ma.setNotifier(a.notifier)
		ma.startWatchdog()
	}
	a.started = true
	liveStartedApps.Add(1)
	// The one line a fresh install can look for in the journal: the
	// module is up, and with how many apps — zero is a valid, and on
	// a starter config the expected, answer.
	names := slices.Sorted(maps.Keys(a.managed))
	a.logger.Info("liveswap started", zap.Int("apps", len(names)), zap.Strings("app_names", names))
	// Units of apps no loaded config names have no managedApp to sweep
	// them (removed or renamed while motel was down): settle them
	// against the manager's own listing. Background, like recovery;
	// the sweep judges each app against the pool right before acting,
	// so neither a reload racing it nor a candidate config that later
	// fails to activate can lose an app someone still holds. Nothing
	// joins this goroutine — its own timeout bounds it — which is why
	// the two seams it reads (caddyExiting, appConfigured) are atomics
	// and not the plain vars they read like.
	conn := a.systemdConn()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), unknownAppSweepTimeout)
		defer cancel()
		if err := sweepUnknownApps(ctx, conn, a.Root, a.logger); err != nil {
			a.logger.Error("sweeping units of apps no longer configured", zap.Error(err))
		}
	}()
	// Recovery belongs to this config: Cleanup cancels and joins it
	// before rolling a rejected definition back or releasing pool
	// references, so a recovery that snapshotted a rejected spec can
	// never launch with it, and a removed app's final sweep runs after
	// its recovery, not before.
	ctx, cancel := context.WithCancel(context.Background())
	a.recoverCancel = cancel
	a.recoverWG = new(sync.WaitGroup)
	for name, ma := range a.managed {
		a.recoverWG.Add(1)
		go func(name string, ma *managedApp) {
			defer a.recoverWG.Done()
			ma.recover(ctx, a.logger.Named(name))
		}(name, ma)
	}
	return nil
}

// systemdConn returns the manager connection this config uses. Apps
// built by Provision share the process-wide client; a test may install
// its own so nothing it starts reaches a real manager.
func (a *App) systemdConn() systemdConn {
	if a.manager != nil {
		return a.manager
	}
	return a.sharedManager()
}

// managerClient is the process-wide manager client as probeManager and
// measureSandbox use it: a connection to measure over, the reachability
// proof that has to come first, and the cache the verdict is held in.
// userManagerClient (systemd_dbus.go) is the real one. It is an
// interface only so a test can reach the no-connection branch — the one
// every real start takes — without dialling a manager; measuring
// through the process-wide client for real would start a transient unit
// on whatever machine runs the tests.
type managerClient interface {
	systemdConn
	probe() error
	cachedSandboxCapability(measure func() error) error
}

// sharedManager returns the process-wide client, or the fake a test
// installed in its place. Distinct from systemdConn(): that one answers
// "which manager do this config's apps talk to", and an App with a
// scripted connection answers it with that connection. This one is the
// process-wide client as such — the probe and the cache live on it, and
// neither has a scripted equivalent.
func (a *App) sharedManager() managerClient {
	if a.processManager != nil {
		return a.processManager
	}
	return userManager
}

// probeManager fails loudly when the manager is unreachable, so Start
// reports what to fix rather than every deploy failing later. Called
// from Start rather than Provision so `motel validate` — which
// provisions and cleans up without starting — works as any user.
func (a *App) probeManager() error {
	if a.managerProbe != nil {
		return a.managerProbe()
	}
	if a.manager != nil {
		// An installed connection is reachable by construction. The
		// field exists so this App touches no real socket, and the
		// process-wide client below is exactly that socket.
		return nil
	}
	return a.sharedManager().probe()
}

// measureSandbox reports whether this host delivers the sandbox
// (sandbox.go), from the per-connection cache: nil, or the probe's
// reason it does not. The measurement starts a real unit, so the first
// caller after a dial pays for it and every later config load reads
// the cache.
func (a *App) measureSandbox() error {
	if a.sandboxProbe != nil {
		return a.sandboxProbe(a.logger)
	}
	if a.manager != nil {
		// Measure through the installed connection, uncached: the
		// cache belongs to the real client (it keeps a throwaway unit
		// off Caddy's reload path), and a scripted connection answers
		// at scripted speed.
		return a.probeSandboxOver(a.manager)
	}
	// probe() rather than get(): it proves the manager answers a real
	// request, and its error names the uid, the socket and the lingering
	// to enable. Start puts that reason verbatim into its refusal, so
	// a manager that went away between Start's probeManager and here must
	// not be reported as a sandbox problem with no remedy attached.
	mgr := a.sharedManager()
	if err := mgr.probe(); err != nil {
		return err
	}
	return mgr.cachedSandboxCapability(func() error {
		return a.probeSandboxOver(mgr)
	})
}

// probeSandboxOver runs the sandbox probe unit over conn, tearing the
// runner down with it.
func (a *App) probeSandboxOver(conn systemdConn) error {
	r := newSystemdRunner(conn, a.logger)
	defer r.close()
	return probeSandboxCapability(r)
}

// Stop intentionally does NOT stop app processes: on a config reload
// the old config is stopped after the new one starts, and the whole
// point of the pool is that apps keep serving across reloads. Real
// shutdown reaches the apps via Cleanup → pool release → Destruct.
func (a *App) Stop() error { return nil }

// Cleanup releases this config's pool references. When the last
// reference goes (shutdown, or an app removed from the config), the
// pool calls managedApp.Destruct: an app removed from the config is
// stopped; on process exit the units stay up for the next start.
func (a *App) Cleanup() error {
	if a.recoverCancel != nil {
		a.recoverCancel()
		// An attempt in flight is bounded by the runner's own
		// deadlines; on process exit it is not worth waiting for
		// (the next start's sweep settles anything it leaves).
		if !caddyExiting() {
			a.recoverWG.Wait()
		}
	}
	if a.started {
		a.started = false
		liveStartedApps.Add(-1)
		// A candidate Caddy rejected after our Start (another app's
		// Start failed) is cleaned up while the config it would have
		// replaced still holds the apps: give them back the serving
		// definition. A config replaced by a successful reload is not
		// the last writer, so this is a no-op for it.
		for name, ma := range a.managed {
			if refs, ok := appPool.References(poolKey(name)); ok && refs > 1 {
				if ma.rollbackConfig(a) {
					a.logger.Warn("config rejected after start; restored the serving definition", zap.String("app", name))
				}
			}
		}
	}
	var firstErr error
	for _, key := range a.pooled {
		if _, err := appPool.Delete(key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// managedApp returns the pool-backed state for a configured app, or
// nil if the name is not in this config.
func (a *App) managedApp(name string) *managedApp {
	return a.managed[name]
}

// Interface guards.
var (
	_ caddy.App          = (*App)(nil)
	_ caddy.Provisioner  = (*App)(nil)
	_ caddy.Validator    = (*App)(nil)
	_ caddy.CleanerUpper = (*App)(nil)
)
