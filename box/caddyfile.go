package box

import (
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"

	"github.com/smallhoursorg/motel/box/proof"
	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("box", parseGlobalOption)
	// Not RegisterHandlerDirective: that one takes an optional matcher
	// as the first argument, and a matcher on box_webhook would leave `/`
	// unserved while the file still read as having a webhook.
	httpcaddyfile.RegisterDirective("box_webhook", parseWebhookDirective)
	// Before liveswap_webhook, which is terminal on every path it sees.
	// RegisterDirectiveOrder anchors only on a standard directive and
	// inserts immediately before it; liveswap's anchor, reverse_proxy,
	// would put box_webhook after liveswap_webhook. redir is a standard
	// directive earlier than reverse_proxy (DESIGN-box.md, "The shape").
	httpcaddyfile.RegisterDirectiveOrder("box_webhook", httpcaddyfile.Before, "redir")
}

func parseGlobalOption(d *caddyfile.Dispenser, existing any) (any, error) {
	if existing != nil {
		return nil, d.Err("the box option is given more than once")
	}
	a := new(App)
	if err := a.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{
		Name:  "box",
		Value: caddyconfig.JSON(a, nil),
	}, nil
}

// UnmarshalCaddyfile parses the global option:
//
//	box {
//	    deploy_trust <preset> { ... }               # who may push (repeatable)
//	    signer <principal> <key-type> <base64>      # who may sign (repeatable)
//	    person <name> <key-type> <base64>           # a person's key (repeatable)
//	    grant <name> viewer|logs box|app <app> [from <prefix>...]  # what a person may read (repeatable)
//	}
//
// person and grant lines are read and checked here, so a bad one fails
// validate and the load; who is admitted is read from the installed
// file (DESIGN-box.md, "People and grants").
func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the option's name
	if d.NextArg() {
		return d.ArgErr() // everything is in the block
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "deploy_trust":
			tc, err := deploytrust.Parse(d)
			if err != nil {
				return err
			}
			a.DeployTrust = append(a.DeployTrust, tc)
		case "signer":
			args := d.RemainingArgs()
			// A block after the line is refused, as the walk refuses it:
			// NextBlock is true for one with lines in it, and an empty
			// `{ }` it consumes, moving the cursor off the last argument.
			if len(args) != 3 || d.NextBlock(d.Nesting()) || d.Val() != args[2] {
				return d.Err("signer takes <principal> <key-type> <base64>")
			}
			if _, err := proof.ParseSigner(args[0], args[1], args[2]); err != nil {
				return d.Err(err.Error())
			}
			a.Signers = append(a.Signers, SignerConfig{Principal: args[0], Type: args[1], Key: args[2]})
		case "person":
			args, err := lineArgs(d)
			if err != nil {
				return err
			}
			if _, err := access.ParsePerson(args); err != nil {
				return d.Err(err.Error())
			}
			a.People = append(a.People, PersonConfig{Name: args[0], Type: args[1], Key: args[2]})
		case "grant":
			args, err := lineArgs(d)
			if err != nil {
				return err
			}
			g, err := access.ParseGrant(args)
			if err != nil {
				return d.Err(err.Error())
			}
			a.Grants = append(a.Grants, grantConfig(g))
		default:
			return d.Errf("unknown box subdirective %q", d.Val())
		}
	}
	return nil
}

// lineArgs is a person or grant line's arguments, refusing a block
// after the line as the walk refuses one (lineTakesNoBlock). NextBlock
// is true for a block with lines in it, and an empty `{ }` it consumes,
// moving the cursor off the line's last token.
func lineArgs(d *caddyfile.Dispenser) ([]string, error) {
	kind := d.Val()
	args := d.RemainingArgs()
	last := kind
	if len(args) > 0 {
		last = args[len(args)-1]
	}
	if d.NextBlock(d.Nesting()) || d.Val() != last {
		return nil, d.Err(lineTakesNoBlock(kind))
	}
	return args, nil
}

// lineTakesNoBlock is both readers' refusal of a block after a person
// or grant line.
func lineTakesNoBlock(kind string) string { return kind + " line: it takes no block" }

func parseWebhookDirective(h httpcaddyfile.Helper) ([]httpcaddyfile.ConfigValue, error) {
	handler := new(Handler)
	if err := handler.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return h.NewRoute(nil, handler), nil
}

// UnmarshalCaddyfile parses `box_webhook`, which takes no argument, no
// matcher and no block: it answers on `/` of its site and passes every
// other path on.
func (*Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the directive's name
	if d.NextArg() {
		return d.Err("box_webhook takes no arguments and no matcher; it answers on / of its site")
	}
	if d.NextBlock(0) {
		return d.Err("box_webhook takes no block")
	}
	return nil
}
