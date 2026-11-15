package box

import (
	"errors"
	"fmt"
	"maps"
	"slices"

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
// lines, are held for the same reason; nothing admits a person yet.
type App struct {
	DeployTrust []deploytrust.TrustConfig `json:"deploy_trust,omitempty"`
	Signers     []SignerConfig            `json:"signers,omitempty"`
	People      []PersonConfig            `json:"people,omitempty"`
	Grants      []GrantConfig             `json:"grants,omitempty"`

	verifiers []deploytrust.Verifier
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
	return a.provision(ctx.Logger())
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

// checkLines parses the person and grant lines again, as a config
// posted as JSON was never through the Caddyfile parser, and holds them
// to the rules across lines with the signers. This reader sees the box
// block alone: which apps the file runs and whether it has a monitor
// channel are the walk's to check (Walk).
func (a *App) checkLines(logger *zap.Logger, signers proof.Signers) error {
	var lines access.Lines
	for _, s := range signers {
		lines.Keys = append(lines.Keys, access.SignerKey(s.Principal, s.Type, s.B64, s.Key))
	}
	for _, p := range a.People {
		k, err := access.ParsePerson([]string{p.Name, p.Type, p.Key})
		if err != nil {
			return err
		}
		lines.Keys = append(lines.Keys, k)
	}
	for _, c := range a.Grants {
		g, err := access.ParseGrant(c.args())
		if err != nil {
			return err
		}
		lines.Grants = append(lines.Grants, g)
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

// Start is a no-op: the app is configuration box_webhook reads.
func (*App) Start() error { return nil }

// Stop is a no-op.
func (*App) Stop() error { return nil }

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)
