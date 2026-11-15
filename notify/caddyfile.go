package notify

import (
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("monitor", parseGlobalOption)
	// The two blocks the monitor block replaced: refused by name, so
	// that a Caddyfile written for them says what to write instead of
	// Caddy's "unrecognized global option".
	httpcaddyfile.RegisterGlobalOption("notify", refusedOption("the notify block is now `monitor { … }`, with the same lines but state_dir and alertmanager, whose paths are fixed; to move a box onto it, docs/upgrading.md, \"From the notify and metrics_store blocks\""))
	httpcaddyfile.RegisterGlobalOption("metrics_store", refusedOption("metrics_store is now a `metrics [<socket path>]` line in the monitor block; to move a box onto it, docs/upgrading.md, \"From the notify and metrics_store blocks\""))
}

func parseGlobalOption(d *caddyfile.Dispenser, existing any) (any, error) {
	if existing != nil {
		return nil, d.Err("the monitor option is given more than once")
	}
	a := new(App)
	if err := a.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{
		Name:  appID,
		Value: caddyconfig.JSON(a, nil),
	}, nil
}

func refusedOption(msg string) func(*caddyfile.Dispenser, any) (any, error) {
	return func(d *caddyfile.Dispenser, _ any) (any, error) {
		d.Next() // the option's name, for the error's line
		return nil, d.Err(msg)
	}
}

// UnmarshalCaddyfile parses the global option (DESIGN-notify.md, "The
// monitor block"; DESIGN-metrics.md, "The metrics line"):
//
//	monitor {
//	    channel <name> <kind> {secret.NOTIFY_<NAME>} [severity <floor>]
//	    operator <channel>...
//	    bind <identity> <channel>
//	    heartbeat {secret.NOTIFY_<NAME>}
//	    deploy_succeeded on|off
//	    allow_insecure_channels
//	    secrets_file <path>
//	    backup_record <path>|off
//	    metrics [<socket path>]
//	    rule <name> <kind> <parameters>...
//	}
//
// Every line but metrics and rule is notify's: who hears. metrics turns
// on the store (DESIGN-metrics.md), at its default socket or the
// absolute path given; a rule line is one of the operator's rules over
// its numbers (operatorrules.go), and needs the metrics line. The grammar is checked here, line by line; what needs
// the whole configuration (an operator line naming a channel, a binding
// no app trusts, a secret the box does not hold) is checked in
// Provision, so that `motel validate` refuses it with the rest.
func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the option's name
	if d.NextArg() {
		return d.ArgErr() // everything is in the block
	}
	var rules []Rule
	var ruleWithoutMetrics error // the first rule line's, if no metrics line comes
	for d.NextBlock(0) {
		switch d.Val() {
		case "channel":
			args := d.RemainingArgs()
			if len(args) != 3 && len(args) != 5 {
				return d.Err("channel takes <name> <kind> {secret.NOTIFY_<NAME>} [severity critical|warning|info]")
			}
			ch := Channel{Name: args[0], Kind: args[1], Severity: SeverityInfo}
			key, err := secretRef(args[2])
			if err != nil {
				return d.Errf("channel %s: %v", args[0], err)
			}
			ch.Secret = key
			if len(args) == 5 {
				if args[3] != "severity" {
					return d.Errf("channel %s: unknown option %q (only severity follows the URL)", args[0], args[3])
				}
				ch.Severity = args[4]
			}
			if err := ch.check(); err != nil {
				return d.Err(err.Error())
			}
			a.Channels = append(a.Channels, ch)
		case "operator":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.Err("operator names one or more channels")
			}
			a.Operator = append(a.Operator, args...)
		case "bind":
			args := d.RemainingArgs()
			if len(args) != 2 {
				return d.Err(`bind takes <identity> <channel>; quote an identity with claims: bind "oidc:<issuer> repository=org/app" <channel>`)
			}
			if _, err := parseIdentity(args[0]); err != nil {
				return d.Errf("bind %q: %v", args[0], err)
			}
			a.Binds = append(a.Binds, Bind{Identity: args[0], Channel: args[1]})
		case "heartbeat":
			if !d.NextArg() {
				return d.ArgErr()
			}
			key, err := secretRef(d.Val())
			if err != nil {
				return d.Errf("heartbeat: %v", err)
			}
			a.HeartbeatSecret = key
			if d.NextArg() {
				return d.ArgErr()
			}
		case "deploy_succeeded":
			if !d.NextArg() {
				return d.ArgErr()
			}
			on := d.Val() == "on"
			switch d.Val() {
			case "on", "off":
				a.DeploySucceeded = &on
			default:
				return d.Errf("deploy_succeeded takes on or off, not %q", d.Val())
			}
			if d.NextArg() {
				return d.ArgErr()
			}
		case "allow_insecure_channels":
			if d.NextArg() {
				return d.ArgErr()
			}
			a.AllowInsecureChannels = true
		case "state_dir":
			// Not a line: Alertmanager reads the directory its drop-in
			// names, which setup makes, so a directory of the line's own
			// would be written and never read.
			return d.Errf("state_dir is not a monitor line: the state directory is fixed at %s, the one motel monitor setup makes and Alertmanager reads; remove the line", DefaultStateDir)
		case "alertmanager":
			// Not a line either: motel-am.socket, which setup writes,
			// is where Alertmanager listens, so another path reaches
			// nothing.
			return d.Errf("alertmanager is not a monitor line: Alertmanager's socket is fixed at %s, the one motel monitor setup's motel-am.socket listens on; remove the line", DefaultSocket)
		case "secrets_file", "backup_record":
			name := d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			v := d.Val()
			if d.NextArg() {
				return d.ArgErr()
			}
			if v == "" {
				// An empty value would be no line in the JSON (omitempty):
				// the default, read silently, and beside a metrics line
				// the store alone (storeAlone).
				return d.Errf("%s takes a path, not an empty string; leave the line out for the default", name)
			}
			switch name {
			case "secrets_file":
				a.SecretsFile = v
			default:
				a.BackupRecord = v
			}
		case "metrics":
			if a.Metrics != nil {
				return d.Err("metrics is given more than once")
			}
			m := new(MetricsStore)
			if d.NextArg() {
				v := d.Val()
				if d.NextArg() {
					return d.ArgErr()
				}
				if !filepath.IsAbs(v) {
					return d.Errf("metrics: the store's socket %q is not an absolute path", v)
				}
				m.Store = v
			}
			a.Metrics = m
		case "rule":
			if ruleWithoutMetrics == nil {
				ruleWithoutMetrics = d.Err(errRuleNeedsMetrics.Error())
			}
			at := *d // the rule's line, for a refusal of the lines together
			r, err := parseRule(d)
			if err != nil {
				return err
			}
			rules = append(rules, r)
			if err := checkRules(rules, nil); err != nil {
				return at.Err(err.Error())
			}
		default:
			return d.Errf("unknown monitor subdirective %q", d.Val())
		}
		if d.NextBlock(d.Nesting()) {
			return d.Err("the monitor block's lines take no block")
		}
	}
	if len(rules) > 0 {
		if a.Metrics == nil {
			return ruleWithoutMetrics
		}
		a.Metrics.Rules = rules
	}
	return nil
}

// secretRef reads `{secret.NOTIFY_<NAME>}`, the only form a URL takes in
// the Caddyfile: a channel URL is a credential (a Slack or Discord hook
// is one by itself), and the Caddyfile is a commit anyone who can read
// the repository reads. The value is the box's, in the secrets file.
func secretRef(tok string) (string, error) {
	key, ok := strings.CutPrefix(tok, "{secret.")
	if ok {
		key, ok = strings.CutSuffix(key, "}")
	}
	if !ok {
		return "", errLiteralURL
	}
	if !strings.HasPrefix(key, "NOTIFY_") || !secretKeyRe.MatchString(key) {
		return "", errSecretName
	}
	return key, nil
}
