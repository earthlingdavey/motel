package notify

import (
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("notify", parseGlobalOption)
}

func parseGlobalOption(d *caddyfile.Dispenser, existing any) (any, error) {
	if existing != nil {
		return nil, d.Err("the notify option is given more than once")
	}
	a := new(App)
	if err := a.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{
		Name:  "notify",
		Value: caddyconfig.JSON(a, nil),
	}, nil
}

// UnmarshalCaddyfile parses the global option (DESIGN-notify.md, "The
// notify block"):
//
//	notify {
//	    channel <name> <kind> {secret.NOTIFY_<NAME>} [severity <floor>]
//	    teacher <channel>...
//	    bind <identity> <channel>
//	    heartbeat {secret.NOTIFY_<NAME>}
//	    deploy_succeeded on|off
//	    allow_insecure_channels
//	    secrets_file <path>
//	    state_dir <path>
//	    alertmanager <unix socket path>
//	}
//
// The grammar is checked here, line by line; what needs the whole
// configuration (a teacher naming a channel, a binding no app trusts,
// a secret the box does not hold) is checked in Provision, so that
// `motel validate` refuses it with the rest.
func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the option's name
	if d.NextArg() {
		return d.ArgErr() // everything is in the block
	}
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
		case "teacher":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.Err("teacher names one or more channels")
			}
			a.Teacher = append(a.Teacher, args...)
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
			switch d.Val() {
			case "on":
				a.DeploySucceeded = true
			case "off":
				a.DeploySucceeded = false
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
		case "secrets_file", "state_dir", "alertmanager":
			name := d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			v := d.Val()
			if d.NextArg() {
				return d.ArgErr()
			}
			switch name {
			case "secrets_file":
				a.SecretsFile = v
			case "state_dir":
				a.StateDir = v
			default:
				a.Socket = v
			}
		default:
			return d.Errf("unknown notify subdirective %q", d.Val())
		}
		if d.NextBlock(d.Nesting()) {
			return d.Err("notify's lines take no block")
		}
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
