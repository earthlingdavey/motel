package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/liveswap/backupdecl"
	"github.com/smallhoursorg/motel/notify"
)

// liveswapDefaultRoot is the liveswap root of a config that names none
// (backupdecl.DefaultRoot). A variable only so a test can stand another
// in: whether this machine has one is not the test's business.
var liveswapDefaultRoot = backupdecl.DefaultRoot

// refuseRunByHand refuses a `moteld run` by root whose config has a
// liveswap block with a root root does not own: on a box, root starting
// a second moteld beside moteld.service, whose motel user owns the root
// (packaging/postinstall.sh). Such a run takes the service's admin
// socket before it fails on the ports, and liveswap's start can retire
// the sockets the service's apps are routed through
// (liveswap/DESIGN-liveswap.md, Accepted residuals). It runs first at
// `run`, before the gate writes anything, and says what to do instead.
// Any other user goes on: it cannot unlink the admin socket
// (/run/motel is 0750 motel:motel) or reach the motel user's manager,
// and a service user that can write a root it does not own (a group's,
// an ACL's) starts as before. So do a config with no liveswap block, a
// root that does not exist yet (a first start), and root's own root, as
// in a container that runs moteld as root (notify.NotOwner). A config
// that is not JSON is Caddy's to refuse.
func refuseRunByHand(cfg []byte, stderr io.Writer) (stop bool) {
	var c struct {
		Apps struct {
			Liveswap *struct {
				Root string `json:"root"`
			} `json:"liveswap"`
		} `json:"apps"`
	}
	if euid() != 0 || json.Unmarshal(cfg, &c) != nil || c.Apps.Liveswap == nil {
		return false
	}
	// As liveswap's Provision reads it, from this environment, which the
	// envfiles have joined.
	root := caddy.NewReplacer().ReplaceKnown(c.Apps.Liveswap.Root, "")
	if root == "" {
		root = liveswapDefaultRoot
	}
	why := notify.NotOwner(root, 0)
	if why == "" {
		return false
	}
	_, _ = fmt.Fprintf(stderr, "Error: this `moteld run` is refused, and nothing was changed: the liveswap root %s. On a box, moteld runs as moteld.service: systemctl starts and restarts it. To check a config, run `moteld validate --config <file>`; to see why moteld will not start, `journalctl -u moteld`.\n", why)
	return true
}
