package liveswap

import (
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

func init() {
	caddy.RegisterModule(Upstreams{})
}

// Upstreams is the reverse_proxy dynamic upstream source:
//
//	reverse_proxy {
//	    dynamic liveswap <app>
//	}
//
// GetUpstreams reads the app's active socket from an atomic — the
// deploy pipeline's promote step swaps that value, which makes the
// cutover instantaneous and config-reload-free while keeping every
// reverse_proxy feature (websockets, h2, streaming, load-balancer
// retries) intact.
type Upstreams struct {
	// App names the liveswap app whose active version receives traffic.
	App string `json:"app,omitempty"`

	ma *managedApp
}

// CaddyModule returns the Caddy module information.
func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.liveswap",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

// Provision resolves the app reference.
func (u *Upstreams) Provision(ctx caddy.Context) error {
	if u.App == "" {
		return fmt.Errorf("an app name is required: dynamic liveswap <app>")
	}
	appModule, err := ctx.App("liveswap")
	if err != nil {
		return err
	}
	u.ma = appModule.(*App).managedApp(u.App)
	if u.ma == nil {
		return fmt.Errorf("unknown liveswap app %q (define it in the liveswap global options)", u.App)
	}
	return nil
}

// GetUpstreams returns the active version's address, or an error (a
// 502 with a clear log line) when nothing has been deployed yet. It
// tells the app's liveswap_observe, if the route has one, which release
// the request went to.
func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	sock := u.ma.activeSocket.Load()
	markUpstream(r, u.ma, sock)
	if sock == nil {
		return nil, fmt.Errorf("liveswap app %q has no running version yet (deploy one via the webhook)", u.App)
	}
	// The pinned name under proxy/, never the app-writable one (socketRef).
	addr, err := sock.dial()
	if err != nil {
		return nil, fmt.Errorf("liveswap app %q: %w", u.App, err)
	}
	// "unix/" + an absolute path is Caddy's unix//abs/path spelling.
	return []*reverseproxy.Upstream{
		{Dial: "unix/" + addr},
	}, nil
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)
