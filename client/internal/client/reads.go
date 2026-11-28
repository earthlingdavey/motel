package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The reads, one a command. Each asks the box, takes the answer only
// when it is the one asked for, and returns it with the answer as
// received, whose body --json prints and the TUI's overlay shows. The
// commands and the TUI call these and ask the box in no other way, so
// a screen cannot read what no command reads.

// WhoAmI is `motel whoami`: GET /_motel/whoami.
func (c *Client) WhoAmI(ctx context.Context) (wire.WhoAmI, *Answer, error) {
	var me wire.WhoAmI
	ans, err := c.read(ctx, "/_motel/whoami", nil, &me)
	return me, ans, err
}

// Apps is `motel apps`: GET /_motel/apps.
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

// Status is `motel status <app> [--range]`: GET /<app>. rng is ""
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

// Deploy is `motel deploys <app> <version>`: GET
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

// BoxStatus is `motel box status`: GET / on the box's host.
func (c *Client) BoxStatus(ctx context.Context) (wire.BoxStatus, *Answer, error) {
	var b wire.BoxStatus
	ans, err := c.read(ctx, "/", nil, &b)
	if err == nil && b.BoxWebhook != c.Box.Host {
		err = c.NotMotel(ans, "it names another host than the one asked")
	}
	return b, ans, err
}

// Backups is `motel backup status`: GET /_motel/backups, what the box
// says of its own backups (`motel-backup status --json` as any account
// on the box looks), for a viewer of the box.
func (c *Client) Backups(ctx context.Context) (wire.Backups, *Answer, error) {
	var b wire.Backups
	ans, err := c.read(ctx, "/_motel/backups", nil, &b)
	return b, ans, err
}

// Alerts is `motel monitor alerts --app <app>`: GET
// /_motel/apps/<app>/alerts, the app's alerts as the box's Alertmanager
// holds them, for a person whose grants cover the app. The route is on
// the box's own name, whatever path an app URL names.
func (c *Client) Alerts(ctx context.Context, app App) (wire.Alerts, *Answer, error) {
	var a wire.Alerts
	ans, err := c.read(ctx, "/_motel/apps/"+app.Name+"/alerts", nil, &a)
	return a, ans, err
}

// Series is `motel metrics <app> [--range]`: GET
// /_motel/apps/<app>/series, the app's numbers over a window as fixed
// panels of points, for a person whose grants cover the app. rng is ""
// for the box's default. The route is on the box's own name, whatever
// path an app URL names. An answer about another window than the one
// --range named is not moteld's; with none named, the box's default is
// taken, whichever of the set it is.
func (c *Client) Series(ctx context.Context, app App, rng string) (wire.Series, *Answer, error) {
	var s wire.Series
	q := url.Values{}
	if rng != "" {
		if !slices.Contains(wire.SeriesRanges, rng) {
			return s, nil, usagef("--range %q is not one of %s", rng, strings.Join(wire.SeriesRanges, ", "))
		}
		q.Set("range", rng)
	}
	ans, err := c.read(ctx, "/_motel/apps/"+app.Name+"/series", q, &s)
	if err == nil && rng != "" && s.Window != rng {
		err = c.NotMotel(ans, "it is of another window than the one asked")
	}
	return s, ans, err
}

// Logs is `motel logs <app> [--lines] [--follow]`: GET
// /_motel/apps/<app>/logs, the app's own lines from the box's journal,
// for a person with a logs grant on the app or the box. lines is one of
// wire.LogsLines, or "" for the box's default; after is "" for a tail
// (the newest lines), or a cursor an earlier answer gave, for the lines
// after it. Both are refused before anything is sent when they are not
// of the box's grammars (url.Values writes the cursor's ; escaped, as
// the box requires). A 503 is not waited out here: its Retry-After, the
// box's when it runs as many logs reads as it runs at once, is on the
// failure for a follower to wait out (RetryAfter), as a 429's is; only
// on a 503 that says why in JSON and asks for a wait of at most a
// minute, as the box's does (1 s), so that a page of another server's,
// or its maintenance window, is not waited out.
func (c *Client) Logs(ctx context.Context, app App, lines, after string) (wire.Logs, *Answer, error) {
	var l wire.Logs
	q := url.Values{}
	if lines != "" {
		if !slices.Contains(wire.LogsLines, lines) {
			return l, nil, usagef("--lines %q is not one of %s", lines, strings.Join(wire.LogsLines, ", "))
		}
		q.Set("lines", lines)
	}
	if after != "" {
		if !wire.LogsCursor.MatchString(after) {
			return l, nil, usagef("the cursor to read after is not one a box gives")
		}
		q.Set("after", after)
	}
	ans, err := c.read(ctx, "/_motel/apps/"+app.Name+"/logs", q, &l)
	var f *Failure
	var said wire.Error
	if errors.As(err, &f) && ans != nil && ans.Code == http.StatusServiceUnavailable && decodeObject(ans, &said) == nil && said.Error != "" {
		if secs, ok := retryAfter(ans.Header, c.Now()); ok && secs <= int64(MaxRetryAfter/time.Second) {
			g := *f
			g.RetryAfter = time.Duration(min(max(secs, 1), 3600)) * time.Second
			err = &g
		}
	}
	return l, ans, err
}

// BoxAlerts is `motel monitor alerts`: GET /_motel/alerts, what the box's
// Alertmanager holds (every alert, every silence that has not ended),
// for a viewer of the box.
func (c *Client) BoxAlerts(ctx context.Context) (wire.BoxAlerts, *Answer, error) {
	var b wire.BoxAlerts
	ans, err := c.read(ctx, "/_motel/alerts", nil, &b)
	return b, ans, err
}

// Rules is `motel monitor rules --app <app>`: GET
// /_motel/apps/<app>/rules, the rules the box alerts on that apply to
// the app, each with the app's numbers alone, for a person whose grants
// cover the app. An answer that names another app, on a rule or a
// reading, is not about the app asked for.
func (c *Client) Rules(ctx context.Context, app App) (wire.Rules, *Answer, error) {
	var r wire.Rules
	ans, err := c.read(ctx, "/_motel/apps/"+app.Name+"/rules", nil, &r)
	if err == nil {
		for _, a := range r.Alerts {
			// A box's own (its subject, a reading of a filesystem or a
			// backup) is not an app's either.
			other := a.Subject != "app" || a.App != "" && a.App != app.Name || len(a.Apps) > 0
			for _, rd := range a.Readings {
				other = other || rd.Instance != "" || rd.App == "" || rd.App != app.Name
			}
			if other {
				err = c.NotMotel(ans, "it holds rules or numbers not of this app")
				break
			}
		}
	}
	return r, ans, err
}

// BoxRules is `motel monitor rules`: GET /_motel/rules, every rule the
// box alerts on, each with every number it judges (an app's, a
// filesystem's), for a viewer of the box.
func (c *Client) BoxRules(ctx context.Context) (wire.Rules, *Answer, error) {
	var r wire.Rules
	ans, err := c.read(ctx, "/_motel/rules", nil, &r)
	return r, ans, err
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
