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
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/sshkey"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui"
	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

func (r *runner) tui() *cobra.Command {
	var t target
	var sample bool
	c := &cobra.Command{
		Use:   "tui",
		Short: "Screens over the reads: the apps, an app and its deploys, a deploy's record (experimental)",
		Long: "tui shows the box on screens in the terminal, over the same reads the other\n" +
			"commands make: the apps you may view sorted by what needs a look, an app's\n" +
			"state and deploys, a deploy's record laid out for reading a failure, who you\n" +
			"are, and the box. It only reads, well inside the box's limit of 2 requests a\n" +
			"second and 60 at once: up to 30 at once to start, then at most 1.5 a second. It\n" +
			"signs one token for ten minutes at a time, so a hardware key is touched about\n" +
			"every nine, and never asks for a passphrase or a PIN: a key with a passphrase\n" +
			"must be in the agent (ssh-add), and a hardware key that requires its PIN cannot\n" +
			"be used here yet. ? lists the keys; c shows the command for a screen that reads\n" +
			"the box, y the box's answer as JSON. It is asked with your own key only.\n\n" + experimental,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if r.json {
				return &client.Failure{Exit: client.ExitUsage, Message: "tui has no --json: it draws screens, and every other command takes --json"}
			}
			// A closed terminal window (SIGHUP) ends it as SIGTERM does:
			// through the screen's event loop, which gives the terminal
			// back.
			ctx, stop := signal.NotifyContext(cmd.Context(), tuiSignals...)
			defer stop()
			o := tui.Options{Clock: tui.RealClock{}, Colour: r.env.Color, Tick: time.Second}
			// Wall-clock time: a monotonic reading stops while a laptop
			// sleeps, and ages and due times would leave the sleep out.
			wall := func() time.Time { return r.env.Now().Round(0) }
			s := &model.Session{Version: r.env.Version, Now: wall}
			if sample {
				s.Host, s.Demo, s.Key = demo.Host, true, demo.Key
				s.TokenExpires = func() time.Time { return wall().Add(9 * time.Minute) }
				o.Client = &demo.Fake{Now: wall}
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
					return &client.Failure{Exit: client.ExitUsage, Message: shown.String() + " is protected by a passphrase, and the agent does not hold it: the TUI signs again about every nine minutes while its screen holds the terminal, where ssh-keygen could not ask for it. Add the key to the agent (ssh-add " + shown.String() + ") and run motel tui again."}
				}
				sig := forTUI(c, wall)
				boxSession(s, c, pt)
				s.TokenExpires = sig.Expires
				o.Client, o.Signing, o.Started, o.Resign = live{c}, sig.Notify, gt.Close, pt.Forget
			}
			s.Engine = model.NewEngine(wall())
			o.Session = s
			start := tui.Prime(ctx, o)
			if err := ctx.Err(); err != nil {
				// Interrupted at the first read (a touch not given): no
				// screen with nothing refreshing behind it.
				return err
			}
			print, err := tui.Run(ctx, o, start)
			if err != nil {
				return runFailure(err)
			}
			if err := r.printed(print); err != nil {
				return err
			}
			return ctx.Err()
		},
	}
	t.flags(c)
	c.Flags().BoolVar(&sample, "demo", false, "show sample data, with no box")
	_ = c.Flags().MarkHidden("demo")
	return c
}

// boxSession is the box and key a live session shows: the host alone
// as its name (the token's aud, the title, whoami's rule), the address
// the client asks, :port and all, for the commands it prints, and the
// key the client signs with.
func boxSession(s *model.Session, c *client.Client, pt *client.PersonToken) {
	k := pt.Key
	s.Host, s.Addr = c.Box.Host, c.Box.Addr()
	s.Key = model.Key{Fingerprint: k.Public.Fingerprint(), Shown: k.Shown, Source: k.Source,
		Hardware: strings.HasPrefix(k.Public.Type, "sk-"), Line: k.Public.Line()}
}

// printed writes what P asked for. Into a pipe or a file, as P made it,
// byte for byte: the JSON as the box sent it is what the command's
// --json prints. On a terminal, the box's answer is printed made safe
// (it may hold C1 controls and bidi overrides, which JSON allows and a
// terminal acts on), and stderr says so and how to get the bytes.
func (r *runner) printed(p tui.Printed) error {
	out := p.Raw
	if r.env.Terminal && p.Shown != "" {
		out = p.Shown
		if _, err := io.WriteString(r.env.Stderr, "motel: the box's answer was made safe for the terminal; redirect stdout (> file, or | cat) for its bytes as sent\n"); err != nil {
			return err
		}
	}
	if out == "" {
		return nil
	}
	_, err := io.WriteString(r.env.Stdout, out)
	return err
}

// tuiSignals end the TUI as SIGINT and SIGTERM do (main's): a closed
// terminal window sends SIGHUP.
var tuiSignals = []os.Signal{syscall.SIGHUP}

// runFailure is how the TUI's end is said: a panic, already one line
// and safe, exits 3; a screen that would not start, 2.
func runFailure(err error) error {
	var p *tui.Panic
	if errors.As(err, &p) {
		// The value quoted and cut (it may hold what a box sent), and the
		// functions it went through, innermost first, at most eight.
		return &client.Failure{Exit: client.ExitUnknown, Message: "the TUI failed: " + termtext.Quote(panicText(p.Value)).String() + " in " + shortStack(p.Stack)}
	}
	return &client.Failure{Exit: client.ExitUsage, Message: "the screen could not start: " + termtext.From(err.Error()).String()}
}

func panicText(v any) string {
	switch v := v.(type) {
	case error:
		return v.Error()
	case string:
		return v
	}
	return "a panic"
}

func shortStack(stack []byte) string {
	var fns []string
	for _, l := range strings.Split(string(stack), "\n") {
		if l == "" || strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "goroutine ") {
			continue
		}
		if i := strings.LastIndexByte(l, '('); i > 0 {
			l = l[:i]
		}
		if strings.HasPrefix(l, "runtime/debug.") || strings.HasPrefix(l, "panic") || strings.HasPrefix(l, "runtime.") {
			continue
		}
		fns = append(fns, l)
		if len(fns) == 8 {
			break
		}
	}
	return termtext.From(strings.Join(fns, " < ")).String()
}

// forTUI makes c the TUI's client, and is the only place one is made:
// each token lasts sshkey.MaxLife, so a session signs about every nine
// minutes; a 429 is handed back for the engine to wait out for every
// read. Its signer tells the title bar while it signs.
func forTUI(c *client.Client, now func() time.Time) *signer {
	pt := c.Cred.(*client.PersonToken)
	pt.Life = sshkey.MaxLife
	if k, ok := pt.Signer.(sshkey.Keygen); ok {
		// ssh-keygen kept off the terminal the screen holds: a key that
		// needs a passphrase or PIN fails at once, and says to use the
		// agent.
		k.NoTerminal = true
		pt.Signer = k
	}
	sig := &signer{inner: pt.Signer, now: now}
	pt.Signer = sig
	c.NoWait = true
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
		return model.Got[wire.Status]{}, argument(err)
	}
	v, ans, err := l.c.Status(ctx, a, "")
	return got(v, ans, err)
}

func (l live) Deploy(ctx context.Context, app, version string) (model.Got[wire.DeployResult], error) {
	// A version the box's filter hid is listed in the client's own
	// words, which name no deploy: nothing is sent, and this read
	// alone fails, saying so.
	if version == client.Filtered {
		return model.Got[wire.DeployResult]{}, &model.Failure{Kind: model.FailThis, Message: "the box's filter hid this deploy's version, so its record cannot be asked for"}
	}
	a, err := client.ParseApp(app)
	if err == nil {
		err = client.CheckVersion(version)
	}
	if err != nil {
		return model.Got[wire.DeployResult]{}, argument(err)
	}
	v, ans, err := l.c.Deploy(ctx, a, version)
	return got(v, ans, err)
}

func (l live) BoxStatus(ctx context.Context) (model.Got[wire.BoxStatus], error) {
	v, ans, err := l.c.BoxStatus(ctx)
	return got(v, ans, err)
}

func got[T any](v T, ans *client.Answer, err error) (model.Got[T], error) {
	if err != nil {
		return model.Got[T]{}, failure(err)
	}
	return model.Got[T]{V: v, Body: ans.Body}, nil
}

// argument is an argument the client refused before asking (an app or
// a version outside its grammar): that read's own failure, said on its
// screen, and never a stop of the session, as a failed signature is.
func argument(err error) error {
	var f *client.Failure
	if !errors.As(err, &f) {
		return err
	}
	return &model.Failure{Kind: model.FailThis, Message: f.Message}
}

// failure is a client failure as the TUI's engine reads it. A usage
// failure here is a token that could not be signed: the arguments were
// checked before (argument).
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
	case f.Exit == client.ExitUnknown && (f.Status == 0 || f.Status >= 500 && f.Status <= 599):
		m.Kind = model.FailNoAnswer
	case f.Exit == client.ExitUnknown:
		// The box answered, and the answer could not be used.
		m.Kind = model.FailUnusable
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
