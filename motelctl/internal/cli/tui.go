package cli

// The one file that starts the TUI and the only place it meets the
// client package: it builds the client the TUI reads with (a token
// that lasts the longest a box takes, a 429 handed back), and gives the
// TUI that client as model.Client, whose methods are the client's
// reads, each a command's (TestTUIImports holds that no package of the
// TUI imports the client or a transport).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/client"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
	"github.com/smallhoursorg/motel/motelctl/internal/tui"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/demo"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

func (r *runner) tui() *cobra.Command {
	var t target
	var sample bool
	c := &cobra.Command{
		Use:   "tui",
		Short: "Screens over the reads: the apps, an app and its deploys, a deploy's record (experimental)",
		Long: "tui shows the box on screens in the terminal, over the same reads the other\n" +
			"commands make: the apps you may view sorted by what needs a look, an app's\n" +
			"state and deploys, and a deploy's record laid out for reading a failure. It\n" +
			"only reads, at no more than 1.5 requests a second, and signs one token for ten\n" +
			"minutes at a time, so a hardware key is touched about every nine. Each\n" +
			"screen's keys are on its last line. It is asked with your own key only, and\n" +
			"not with a key file that needs a passphrase the agent does not hold.\n\n" + experimental,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if r.json {
				return &client.Failure{Exit: client.ExitUsage, Message: "tui has no --json: each other command's --json is the box's answer"}
			}
			ctx := cmd.Context()
			o := tui.Options{Clock: tui.RealClock{}, Colour: r.env.Color, Tick: time.Second}
			s := &model.Session{Version: r.env.Version, Now: r.env.Now}
			if sample {
				s.Host, s.Demo, s.Key = demo.Host, true, demo.Key
				s.TokenExpires = func() time.Time { return r.env.Now().Add(9 * time.Minute) }
				o.Client = &demo.Fake{Now: r.env.Now}
			} else {
				gt := &gate{w: r.env.Stderr}
				rr := *r
				rr.env.Stderr = gt
				c, err := rr.client(ctx, &t, nil, "", "tui")
				if err != nil {
					return err
				}
				pt := c.Cred.(*client.PersonToken)
				if sshkey.Passphrased(ctx, r.env.Run, pt.Key) {
					shown := termtext.From(strings.TrimSuffix(pt.Key.Shown, ".pub"))
					return &client.Failure{Exit: client.ExitUsage, Message: shown.String() + " is protected by a passphrase, and the agent does not hold it: the TUI signs again about every nine minutes while its screen holds the terminal, where ssh-keygen could not ask for it. Add the key to the agent (ssh-add " + shown.String() + ") and run motelctl tui again."}
				}
				sig := forTUI(c, r.env.Now)
				k := pt.Key
				s.Host = c.Box.Host
				s.Key = model.Key{Fingerprint: k.Public.Fingerprint(), Shown: k.Shown, Source: k.Source,
					Hardware: strings.HasPrefix(k.Public.Type, "sk-"), Line: k.Public.Line()}
				s.TokenExpires = sig.Expires
				o.Client, o.Signing, o.Started = live{c}, sig.Notify, gt.Close
			}
			s.Engine = model.NewEngine(r.env.Now())
			o.Session = s
			start := tui.Prime(ctx, o)
			if err := ctx.Err(); err != nil {
				// Interrupted at the first read (a touch not given): no
				// screen with nothing refreshing behind it.
				return err
			}
			if err := tui.Run(ctx, o, start); err != nil {
				return &client.Failure{Exit: client.ExitUsage, Message: "the screen could not start: " + termtext.From(err.Error()).String()}
			}
			return ctx.Err()
		},
	}
	t.flags(c)
	c.Flags().BoolVar(&sample, "demo", false, "show sample data, with no box")
	_ = c.Flags().MarkHidden("demo")
	return c
}

// forTUI makes c the TUI's client, and is the only place one is made:
// each token lasts sshkey.MaxLife, so a session signs about every nine
// minutes; a 429 is handed back for the engine to wait out for every
// read; nothing is noted on stderr, which the screen holds. Its signer
// tells the title bar while it signs.
func forTUI(c *client.Client, now func() time.Time) *signer {
	pt := c.Cred.(*client.PersonToken)
	pt.Life = sshkey.MaxLife
	sig := &signer{inner: pt.Signer, now: now}
	pt.Signer = sig
	c.NoWait, c.Notes = true, nil
	return sig
}

// live is model.Client over the client package: each method is the
// read its command makes, and each failure is said as the client says
// it, with what it means to the TUI's engine.
type live struct{ c *client.Client }

func (l live) WhoAmI(ctx context.Context) (model.Got[wire.WhoAmI], error) {
	v, ans, err := l.c.WhoAmI(ctx)
	return got(v, ans, err)
}

func (l live) Apps(ctx context.Context) (model.Got[[]string], error) {
	v, ans, err := l.c.Apps(ctx)
	return got(client.AppNames(v), ans, err)
}

func (l live) Status(ctx context.Context, app string) (model.Got[wire.Status], error) {
	a, err := client.ParseApp(app)
	if err != nil {
		return model.Got[wire.Status]{}, failure(err)
	}
	v, ans, err := l.c.Status(ctx, a, "")
	return got(v, ans, err)
}

func (l live) Deploy(ctx context.Context, app, version string) (model.Got[wire.DeployResult], error) {
	a, err := client.ParseApp(app)
	if err != nil {
		return model.Got[wire.DeployResult]{}, failure(err)
	}
	v, ans, err := l.c.Deploy(ctx, a, version)
	return got(v, ans, err)
}

func got[T any](v T, ans *client.Answer, err error) (model.Got[T], error) {
	if err != nil {
		return model.Got[T]{}, failure(err)
	}
	return model.Got[T]{V: v, Body: ans.Body}, nil
}

// failure is a client failure as the TUI's engine reads it.
func failure(err error) error {
	var f *client.Failure
	if !errors.As(err, &f) {
		return err // the context's own end
	}
	m := &model.Failure{Message: f.Message, RetryAfter: f.RetryAfter}
	switch {
	case f.Exit == client.ExitRefused:
		m.Kind = model.FailSignedOut
	case f.Exit == client.ExitCertificate:
		m.Kind = model.FailCertificate
	case f.Exit == client.ExitUsage:
		m.Kind = model.FailNotSigned
	case f.Status == http.StatusTooManyRequests:
		m.Kind = model.FailSlowed
	case f.Exit == client.ExitForbidden:
		m.Kind = model.FailForbidden
	case f.Exit == client.ExitNotFound:
		m.Kind = model.FailNotFound
	case f.Exit == client.ExitUnknown && (f.Status == 0 || f.Status >= 500):
		m.Kind = model.FailNoAnswer
	default:
		m.Kind = model.FailThis
	}
	return m
}

// signer tells the screen while a token is being signed, and says when
// the token last signed ends.
type signer struct {
	inner sshkey.Signer
	now   func() time.Time
	mu    sync.Mutex
	note  func(bool)
	at    time.Time
}

// Expires is when the token last signed stops being taken: sshkey's
// times for a token of MaxLife made when signing began, as the
// client's PersonToken makes them.
func (s *signer) Expires() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at.IsZero() {
		return time.Time{}
	}
	_, exp := sshkey.TimesLasting(s.at, sshkey.MaxLife)
	return exp
}

// Notify sets what is told; nil tells nothing.
func (s *signer) Notify(f func(bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note = f
}

// Sign signs with inner, saying so before and after.
func (s *signer) Sign(ctx context.Context, key sshkey.Key, namespace string, message []byte) ([]byte, error) {
	s.mu.Lock()
	note, now := s.note, s.now()
	s.mu.Unlock()
	if note != nil {
		note(true)
		defer note(false)
	}
	out, err := s.inner.Sign(ctx, key, namespace, message)
	if err == nil {
		s.mu.Lock()
		s.at = now
		s.mu.Unlock()
	}
	return out, err
}

// gate is stderr until the screen takes the terminal, and nothing
// after: ssh-keygen's own prompt would be drawn over the screen, whose
// title bar says it instead.
type gate struct {
	w   io.Writer
	mu  sync.Mutex
	off bool
}

// Close shuts the gate.
func (g *gate) Close() {
	g.mu.Lock()
	g.off = true
	g.mu.Unlock()
}

func (g *gate) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.off {
		return len(p), nil
	}
	return g.w.Write(p)
}
