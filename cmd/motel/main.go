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

func main() {
	// Before Caddy's own validate and reload, whether a backup run would
	// see what the Caddyfile declares (backupcheck.go). Caddy's commands
	// cannot be registered twice, so this comes first.
	if gate(os.Args[1:], os.Stderr) {
		os.Exit(1)
	}
	// Provisioning cannot tell `motel validate` from a load; notify's
	// refusals of the box's secrets differ between the two
	// (notify/DESIGN-notify.md, "Secrets at load").
	if len(os.Args) > 1 {
		notify.SetCommand(os.Args[1])
	}
	caddycmd.Main()
}
