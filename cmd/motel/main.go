// Command motel is the smallhours app server: Caddy with
// zero-downtime app deploys (liveswap), rate-limit hint enforcement
// (penaltybox), the config channel (box, being built: see
// box/DESIGN-box.md), notify (who motel tells, through Alertmanager:
// notify/DESIGN-notify.md) and HTTP caching (Souin + Otter storage)
// compiled in.
// It is distributed as its own binary and OS packages — see the repo
// README — and behaves exactly like a custom Caddy build: same CLI,
// same Caddyfile, same admin API.
package main

import (
	"os"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// Standard Caddy modules — this is what makes the binary equivalent
	// to a stock caddy distribution (http, tls automation, caddyfile
	// adapter, ...); upstream cmd/caddy/main.go does exactly this.
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	// motel modules. Importing liveswap also makes this process
	// non-dumpable before main runs (see liveswap/harden).
	_ "github.com/smallhoursorg/motel/box"
	_ "github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/notify"
	_ "github.com/smallhoursorg/motel/penaltybox"

	// HTTP cache (Souin) with in-memory Otter storage.
	_ "github.com/darkweak/souin/plugins/caddy"
	_ "github.com/darkweak/storages/otter/caddy"
)

// longDescription is `motel --help`'s text above the commands.
const longDescription = `motel is the smallhours app server: Caddy with zero-downtime app
deploys (liveswap), rate-limit hint enforcement (penaltybox), backups,
the box's config channel, monitor (who motel tells, and the numbers)
and HTTP caching compiled in. Its commands, Caddyfile and admin API are
Caddy's, under the name motel.

On a box, motel runs as motel.service from /etc/motel/Caddyfile, and
systemctl starts, stops and reloads it. With a box repository, change
the Caddyfile by a push: the next push replaces an edit made here.
Without one, after an edit:

	motel validate --config /etc/motel/Caddyfile
	systemctl reload motel

motel's own commands are listed below beside Caddy's; the motel
repository's README and docs/ describe them. The examples are Caddy's.
`

func main() {
	// Before Caddy's own validate and reload, whether a backup run would
	// see what the Caddyfile declares (backupcheck.go). Caddy's commands
	// cannot be registered twice, so this comes first.
	stop, args := gateArgs(os.Args[1:], os.Stderr)
	if stop {
		os.Exit(1)
	}
	os.Args = append(os.Args[:1], args...)
	// Provisioning cannot tell `motel validate` from a load; notify's
	// refusals of the box's secrets differ between the two
	// (notify/DESIGN-notify.md, "Secrets at load").
	if len(os.Args) > 1 {
		notify.SetCommand(os.Args[1])
	}
	// The binary's name in every usage line Caddy's CLI prints
	// (`motel monitor setup [--metrics]`, not `caddy monitor …`), and
	// motel's own description at the root's help in place of Caddy's,
	// which tells the reader to run `caddy run`. The root's examples are
	// Caddy's, fixed in its command, and still say `caddy`.
	caddy.CustomBinaryName = "motel"
	caddy.CustomLongDescription = longDescription
	caddycmd.Main()
}
