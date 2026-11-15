package box

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/box/proof"
	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

func init() {
	caddy.RegisterModule(App{})
}

// App is the `box` global option (DESIGN-box.md, "The shape"): who may
// push this box's configuration — `deploy_trust`, liveswap's grammar
// and verifier — and who may sign it — the `signer` lines.
//
// The verifiers are what box_webhook authenticates against. The
// signers are held here only so that a bad line fails config load, and
// with it `motel validate`; the signer list the box trusts is read
// from the installed file's raw tokens (Walk), never from the running
// configuration (step 10). People and Grants, the person and grant
// lines, are held for the same reason: the people the box admits are
// the registry's, which is read from the installed file too.
type App struct {
	DeployTrust []deploytrust.TrustConfig `json:"deploy_trust,omitempty"`
	Signers     []SignerConfig            `json:"signers,omitempty"`
	People      []PersonConfig            `json:"people,omitempty"`
	Grants      []GrantConfig             `json:"grants,omitempty"`

	verifiers []deploytrust.Verifier
	// registry is the people this load admits (Access), nil for none.
	registry *access.Registry
}

// SignerConfig is one `signer <principal> <key-type> <base64>` line.
type SignerConfig struct {
	Principal string `json:"principal"`
	Type      string `json:"type"`
	Key       string `json:"key"`
}

// PersonConfig is one `person <name> <key-type> <base64>` line.
type PersonConfig struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Key  string `json:"key"`
}

// GrantConfig is one `grant <name> <role> box|app <app> [from
// <prefix>...]` line: Scope is "box" or "app", App the app's name for
// "app", From the list as written.
type GrantConfig struct {
	Name  string   `json:"name"`
	Role  string   `json:"role"`
	Scope string   `json:"scope"`
	App   string   `json:"app,omitempty"`
	From  []string `json:"from,omitempty"`
}

// grantConfig is a parsed grant line as the app's JSON holds it.
func grantConfig(g access.Grant) GrantConfig {
	args := g.Args()
	c := GrantConfig{Name: args[0], Role: args[1], Scope: args[2], App: g.App}
	if len(g.From) > 0 {
		c.From = args[len(args)-len(g.From):]
	}
	return c
}

// args are the line's arguments, as access.ParseGrant takes them.
func (c GrantConfig) args() []string {
	args := []string{c.Name, c.Role, c.Scope}
	if c.Scope == "app" {
		args = append(args, c.App)
	}
	if len(c.From) > 0 {
		args = append(append(args, "from"), c.From...)
	}
	return args
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "box",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision checks every signer as the walk does (proof.ParseSigner on
// each line, then the set: one key, one principal), and every person
// and grant line with the signers (access.CheckLines, which warns of
// what it takes with a doubt), and builds the verifiers
// (deploytrust.New, which refuses an empty set at config load), then
// warns about a deploy_trust block wider than it reads.
func (a *App) Provision(ctx caddy.Context) error {
	if err := a.provision(ctx.Logger()); err != nil {
		return err
	}
	a.registry = a.loadRegistry(ctx.Logger(), registryFile())
	return nil
}

// Access is the people this load admits, for both webhooks: nil when
// it admits no one.
func (a *App) Access() *access.Registry { return a.registry }

// registryPath is the installed file the registry is read from; a test
// stands another in (setRegistryFile). An atomic, as the tests that
// provision an App run beside one another.
var registryPath atomic.Pointer[string]

func registryFile() string {
	if p := registryPath.Load(); p != nil {
		return *p
	}
	return installedFile
}

// loadRegistry is the registry of the installed file at path, read as
// the walk reads it (DESIGN-box.md, "Reading the signed file"), never
// from the running configuration: an admin-socket load cannot add a
// person. It is built here, at provision, before any app starts, so a
// person's first request after a start finds it (unlike a deploy
// token's sources, which liveswap installs at Start: issue #473). A
// file that cannot be read or that the walk refuses admits no one, and
// the journal says why at each load; deploys are not touched. A file
// whose box block has no person and no grant line admits no one either
// (Walk hands out no key for it). What the walk took with a doubt and
// this module's own reading could not see (people with no monitor
// channel) is said here too.
func (a *App) loadRegistry(logger *zap.Logger, path string) *access.Registry {
	declares := len(a.People) > 0 || len(a.Grants) > 0
	none := func(why string, err error) *access.Registry {
		level := logger.Info
		if declares {
			level = logger.Warn
		}
		level("box: no person is admitted: "+why, zap.String("file", path), zap.String("error", proof.Bound(err.Error())))
		return nil
	}
	b, err := readFile(path, proof.MaxCaddyfile, true)
	if errors.Is(err, fs.ErrNotExist) {
		return none("the box runs no installed Caddyfile", err)
	}
	if err != nil {
		return none("the installed Caddyfile cannot be read", err)
	}
	shape, err := Walk(b)
	if err != nil {
		return none("the installed Caddyfile is not one the box can speak for", err)
	}
	reg := access.NewRegistry(shape.Host, shape.Access)
	if reg.Keys() == 0 {
		return nil
	}
	mine, _ := access.CheckLines(a.linesOrEmpty(), nil)
	for _, w := range shape.AccessWarnings {
		if !slices.Contains(mine, w) {
			logger.Warn(w, zap.String("option", "box"), zap.String("file", path))
		}
	}
	logger.Info("box: people admitted", zap.String("box", shape.Host), zap.Int("keys", reg.Keys()))
	return reg
}

// linesOrEmpty is the block's own lines as provision read them, or none
// if they do not parse (provision has refused such a block already).
func (a *App) linesOrEmpty() access.Lines {
	signers := make(proof.Signers, 0, len(a.Signers))
	for _, s := range a.Signers {
		sg, err := proof.ParseSigner(s.Principal, s.Type, s.Key)
		if err != nil {
			return access.Lines{}
		}
		signers = append(signers, sg)
	}
	l, err := a.lines(signers)
	if err != nil {
		return access.Lines{}
	}
	return l
}

func (a *App) provision(logger *zap.Logger) error {
	if len(a.Signers) == 0 {
		return errors.New("box: no signer; the box applies only commits signed by a key its Caddyfile lists")
	}
	signers := make(proof.Signers, 0, len(a.Signers))
	for _, s := range a.Signers {
		sg, err := proof.ParseSigner(s.Principal, s.Type, s.Key)
		if err != nil {
			return fmt.Errorf("box: %w", err)
		}
		signers = append(signers, sg)
	}
	if _, err := signers.AllowedSigners(); err != nil {
		return fmt.Errorf("box: %w", err)
	}
	if err := a.checkLines(logger, signers); err != nil {
		return fmt.Errorf("box: %w", err)
	}
	vs, err := deploytrust.New(a.DeployTrust, false)
	if err != nil {
		return fmt.Errorf("box: %w", err)
	}
	a.verifiers = vs
	// The warning reads a block as it will verify, placeholders
	// resolved; New resolved its own copy, so this is another.
	resolved := slices.Clone(a.DeployTrust)
	for i := range resolved {
		resolved[i].Claims = maps.Clone(resolved[i].Claims)
	}
	deploytrust.ResolvePlaceholders(caddy.NewReplacer(), resolved)
	for i, tc := range resolved {
		deploytrust.WarnUnboundSource(logger, tc, zap.String("option", "box"), zap.Int("deploy_trust", i))
	}
	return nil
}

// checkLines holds the person and grant lines to the rules across lines
// with the signers. This reader sees the box block alone: which apps
// the file runs and whether it has a monitor channel are the walk's to
// check (Walk).
func (a *App) checkLines(logger *zap.Logger, signers proof.Signers) error {
	lines, err := a.lines(signers)
	if err != nil {
		return err
	}
	warnings, err := access.CheckLines(lines, nil)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		logger.Warn(w, zap.String("option", "box"))
	}
	return nil
}

// lines parses the person and grant lines again, as a config posted as
// JSON was never through the Caddyfile parser: the signers' keys first,
// then the people's, then the grants, each in file order.
func (a *App) lines(signers proof.Signers) (access.Lines, error) {
	var lines access.Lines
	for _, s := range signers {
		lines.Keys = append(lines.Keys, access.SignerKey(s.Principal, s.Type, s.B64, s.Key))
	}
	for _, p := range a.People {
		k, err := access.ParsePerson([]string{p.Name, p.Type, p.Key})
		if err != nil {
			return access.Lines{}, err
		}
		lines.Keys = append(lines.Keys, k)
	}
	for _, c := range a.Grants {
		// The scope decides which of the two the line is: a JSON grant
		// whose app disagrees with it would be checked as one and kept
		// as the other.
		if (c.Scope == "box") != (c.App == "") {
			return access.Lines{}, fmt.Errorf("grant %q: a grant on the box names no app, and a grant on an app names one (scope %q, app %q)", c.Name, c.Scope, c.App)
		}
		g, err := access.ParseGrant(c.args())
		if err != nil {
			return access.Lines{}, err
		}
		lines.Grants = append(lines.Grants, g)
	}
	return lines, nil
}

// Start is a no-op: the app is configuration box_webhook reads.
func (*App) Start() error { return nil }

// Stop is a no-op.
func (*App) Stop() error { return nil }

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
	_ access.Source     = (*App)(nil)
)
