package client

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// The reads, one a command. Each asks the box, takes the answer only
// when it is the one asked for, and returns it with the answer as
// received, whose body --json prints and the TUI's overlay shows. The
// commands and the TUI call these and ask the box in no other way, so
// a screen cannot read what no command reads.

// WhoAmI is `motelctl whoami`: GET /_motel/whoami.
func (c *Client) WhoAmI(ctx context.Context) (wire.WhoAmI, *Answer, error) {
	var me wire.WhoAmI
	ans, err := c.read(ctx, "/_motel/whoami", nil, &me)
	return me, ans, err
}

// Apps is `motelctl apps`: GET /_motel/apps.
func (c *Client) Apps(ctx context.Context) (wire.Apps, *Answer, error) {
	var list wire.Apps
	ans, err := c.read(ctx, "/_motel/apps", nil, &list)
	return list, ans, err
}

// AppNames is the names an apps list holds, in its order: the one place
// outside Read that knows the list's shape, so a screen never decodes
// it.
func AppNames(list wire.Apps) []string {
	out := make([]string, len(list.Apps))
	for i, e := range list.Apps {
		out[i] = e.Name
	}
	return out
}

// Status is `motelctl status <app> [--range]`: GET /<app>. rng is ""
// for the box's default.
func (c *Client) Status(ctx context.Context, app App, rng string) (wire.Status, *Answer, error) {
	var st wire.Status
	q := url.Values{}
	if rng != "" {
		if !slices.Contains(wire.MetricsRanges, rng) {
			return st, nil, usagef("--range %q is not one of %s", rng, strings.Join(wire.MetricsRanges, ", "))
		}
		q.Set("range", rng)
	}
	ans, err := c.read(ctx, app.Path, q, &st)
	if err == nil && st.App != app.Name {
		err = c.NotMotel(ans, "it is the status of another app")
	}
	return st, ans, err
}

// Deploy is `motelctl deploys <app> <version>`: GET
// /<app>?deploy=<version>, one deploy's whole record.
func (c *Client) Deploy(ctx context.Context, app App, version string) (wire.DeployResult, *Answer, error) {
	var d wire.DeployResult
	if err := CheckVersion(version); err != nil {
		return d, nil, err
	}
	ans, err := c.read(ctx, app.Path, url.Values{"deploy": {version}}, &d)
	if err == nil && d.Version != version {
		err = c.NotMotel(ans, "it is the record of another version")
	}
	return d, ans, err
}

// BoxStatus is `motelctl box status`: GET / on the box's host.
func (c *Client) BoxStatus(ctx context.Context) (wire.BoxStatus, *Answer, error) {
	var b wire.BoxStatus
	ans, err := c.read(ctx, "/", nil, &b)
	if err == nil && b.BoxWebhook != c.Box.Host {
		err = c.NotMotel(ans, "it names another host than the one asked")
	}
	return b, ans, err
}

// read is Get, then Read into v. An error is a *Failure, or the
// context's own when it ended; a signing error, which sent nothing,
// is a usage failure, as the commands have always said it.
func (c *Client) read(ctx context.Context, path string, q url.Values, v any) (*Answer, error) {
	ans, err := c.Get(ctx, path, q)
	if err != nil {
		var f *Failure
		switch {
		case errors.As(err, &f):
			return nil, f
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, err
		}
		return nil, &Failure{Exit: ExitUsage, Message: termtext.From(err.Error()).String()}
	}
	return ans, c.Read(ans, v)
}
