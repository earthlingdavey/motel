// Package model is the TUI with no library under it: what the screens
// read from a box, when they read it, and what they show, as views of
// termtext.Text. It reads the box only through Client, whose methods
// are the client package's reads, the ones the motelctl commands call,
// so the TUI can show nothing a command does not read
// (TestActionsAreCommands holds the table of them). It imports no
// net/http and no terminal library (TestTUIImports).
package model

import (
	"context"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/client"
)

// Client is one method per command the TUI reads with. Each returns
// the box's value and its body as received, for the JSON overlay. An
// error is a *client.Failure, or the context's own when it ended.
type Client interface {
	WhoAmI(ctx context.Context) (Got[wire.WhoAmI], error)
	// Apps is the names the box lists, through client.AppNames: the
	// TUI never reads the list's shape itself.
	Apps(ctx context.Context) (Got[[]string], error)
	Status(ctx context.Context, app string) (Got[wire.Status], error)
	Deploy(ctx context.Context, app, version string) (Got[wire.DeployResult], error)
	BoxStatus(ctx context.Context) (Got[wire.BoxStatus], error)
}

// Got is one answer: the value, the body as received, and when it
// came (stamped by the session on the client's clock).
type Got[T any] struct {
	V    T
	Body []byte
	At   time.Time
}

// Live is Client over the client package.
type Live struct {
	C *client.Client
}

// WhoAmI asks as `motelctl whoami` does.
func (l Live) WhoAmI(ctx context.Context) (Got[wire.WhoAmI], error) {
	v, ans, err := l.C.WhoAmI(ctx)
	return got(v, ans, err)
}

// Apps asks as `motelctl apps` does.
func (l Live) Apps(ctx context.Context) (Got[[]string], error) {
	v, ans, err := l.C.Apps(ctx)
	return got(client.AppNames(v), ans, err)
}

// Status asks as `motelctl status <app>` does. The name came from the
// box's own list, which the client held to the app grammar, or from
// the person; ParseApp holds it again.
func (l Live) Status(ctx context.Context, app string) (Got[wire.Status], error) {
	a, err := client.ParseApp(app)
	if err != nil {
		return Got[wire.Status]{}, err
	}
	v, ans, err := l.C.Status(ctx, a, "")
	return got(v, ans, err)
}

// Deploy asks as `motelctl deploys <app> <version>` does.
func (l Live) Deploy(ctx context.Context, app, version string) (Got[wire.DeployResult], error) {
	a, err := client.ParseApp(app)
	if err != nil {
		return Got[wire.DeployResult]{}, err
	}
	v, ans, err := l.C.Deploy(ctx, a, version)
	return got(v, ans, err)
}

// BoxStatus asks as `motelctl box status` does.
func (l Live) BoxStatus(ctx context.Context) (Got[wire.BoxStatus], error) {
	v, ans, err := l.C.BoxStatus(ctx)
	return got(v, ans, err)
}

func got[T any](v T, ans *client.Answer, err error) (Got[T], error) {
	if err != nil {
		return Got[T]{}, err
	}
	return Got[T]{V: v, Body: ans.Body}, nil
}
