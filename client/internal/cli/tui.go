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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/sshkey"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui"
	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// screenLong is what motel alone does: the root's help, above the
// experimental block.
const screenLong = "motel alone shows them on screens in the terminal, over the same reads the\n" +
	"commands make: the apps you may view sorted by what needs a look, an app's state\n" +
	"and deploys, a deploy's record laid out for reading a failure, who you are, the\n" +
	"box, its backups, its alerts and its rules. It only reads, well inside the box's\n" +
	"limit of 2 requests a second and 60 at once: up to 30 at once to start, then at\n" +
	"most 1.5 a second. It signs one token for ten minutes at a time, so a hardware\n" +
	"key is touched about every nine, and never asks for a passphrase or a PIN: a key\n" +
	"with a passphrase must be in the agent (ssh-add), and a hardware key that\n" +
	"requires its PIN cannot be used there yet. ? lists the keys; c shows the command\n" +
	"for a screen that reads the box, y the box's answer as JSON. It is asked with\n" +
	"your own key only, and takes no --json. With no arguments at all it prints this\n" +
	"help instead when stdout is not a terminal, and, with a line under it saying\n" +
	"why, when it finds no box or key to read with, or no screen to draw on."

// screen makes motel alone, the root, open the screens: with its own
// --box and --key, as every command that asks the box has, and the
// hidden --demo.
func (r *runner) screen(root *cobra.Command) {
	var t target
	var sample bool
	root.RunE = func(cmd *cobra.Command, args []string) (err error) {
		// A word cobra does not look at for a command, one after -- or
		// an empty one (a script's motel "$CMD"), is refused as cobra
		// refuses a word that is not a command, before anything else.
		if len(args) > 0 {
			return &client.Failure{Exit: client.ExitUsage, Message: cobraLine(fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath()))}
		}
		// motel with no arguments at all may be typed to see what it
		// can do: into a pipe or a file it prints its help, as before
		// there was a screen, and asks nothing. Any flag asks for the
		// screen, and fails as a command does.
		bare := cmd.Flags().NFlag() == 0
		if bare && !r.env.Terminal {
			return cmd.Help()
		}
		if r.json {
			return &client.Failure{Exit: client.ExitUsage, Message: "motel alone has no --json: it draws screens, and every command takes --json"}
		}
		// A closed terminal window (SIGHUP) ends it as SIGTERM does:
		// through the screen's event loop, which gives the terminal
		// back, and with an interrupt's exit code (errInterrupted:
		// Main's context does not carry it), whatever step it cut
		// short said that step failed (a box, a key or a passphrase
		// not found).
		ctx, stop := signal.NotifyContext(cmd.Context(), tuiSignals...)
		defer stop()
		defer func() {
			if ctx.Err() != nil {
				err = errInterrupted
			}
		}()
		// unopened is a step that stopped the screen before anything
		// was signed or sent. Typed alone, motel then prints its help,
		// and the line under it says what it did not find.
		unopened := func(err error) error {
			var f *client.Failure
			if !bare || ctx.Err() != nil || !errors.As(err, &f) {
				return err
			}
			if herr := cmd.Help(); herr != nil {
				return herr
			}
			if !strings.HasPrefix(f.Message, "the screen") {
				f = &client.Failure{Exit: f.Exit, Message: "the screen did not open: " + f.Message, Body: f.Body}
			}
			return f
		}
		// A start with no terminal to draw on (a CI job, cron, ssh
		// without -t) or a TERM the screen cannot draw with ends
		// here, with nothing signed or sent, as exit 2 says.
		if r.env.Draw != nil {
			if err := r.env.Draw(); err != nil {
				return unopened(screenFailure(err))
			}
		}
		o := tui.Options{Clock: tui.RealClock{}, Colour: r.env.Color, Tick: time.Second}
		// Wall-clock time: a monotonic reading stops while a laptop
		// sleeps, and ages and due times would leave the sleep out.
		wall := func() time.Time { return r.env.Now().Round(0) }
		s := &model.Session{Version: r.env.Version, Now: wall}
		// Where the line that says the first reads wait goes (sayAsking),
		// and what tells it a signature is being made or the client
		// pauses.
		errW := r.env.Stderr
		var notify func(func(bool))
		if sample {
			s.Host, s.Demo, s.Key = demo.Host, true, demo.Key
			s.TokenExpires = func() time.Time { return wall().Add(9 * time.Minute) }
			o.Client = &demo.Fake{Now: wall}
		} else {
			gt := &gate{w: r.env.Stderr}
			rr := *r
			rr.env.Stderr = gt
			c, err := rr.client(ctx, &t, nil, "", "the screen")
			if err != nil {
				return unopened(err)
			}
			pt := c.Cred.(*client.PersonToken)
			if sshkey.Passphrased(ctx, r.env.Run, pt.Key) {
				shown := termtext.From(strings.TrimSuffix(pt.Key.Shown, ".pub"))
				return unopened(&client.Failure{Exit: client.ExitUsage, Message: shown.String() + " is protected by a passphrase, and the agent does not hold it: the screen signs again about every nine minutes while it holds the terminal, where ssh-keygen could not ask for it. Add the key to the agent (ssh-add " + shown.String() + ") and run motel again."})
			}
			sig := forTUI(c, wall)
			boxSession(s, c, pt)
			s.TokenExpires = sig.Expires
			o.Client, o.Signing, o.Started, o.Resign = live{c}, sig.Notify, gt.Close, pt.Forget
			ps := &pauser{sleep: c.Sleep}
			c.Sleep = ps.Sleep
			errW = gt
			notify = func(f func(bool)) { sig.Notify(f); ps.Notify(f) }
		}
		s.Engine = model.NewEngine(wall())
		o.Session = s
		said := sayAsking(ctx, errW, "motel: asking "+termtext.From(s.Host).String()+" who you are and for its apps; the screen opens when it answers, or within a minute if it does not (ctrl-c quits)\n",
			func() <-chan time.Time { return o.Clock.After(askingAfter) }, notify)
		start := tui.Prime(ctx, o)
		said()
		if ctx.Err() != nil {
			// Interrupted at the first read (a touch not given): no
			// screen with nothing refreshing behind it.
			return errInterrupted
		}
		print, err := tui.Run(ctx, o, start)
		if err != nil {
			return runFailure(err)
		}
		return r.printed(print)
	}
	t.flags(root)
	root.Flags().BoolVar(&sample, "demo", false, "show sample data, with no box")
	_ = root.Flags().MarkHidden("demo")
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

// printed writes what P asked for. Into a pipe or a file, as P made it:
// the box's answer as the command's --json prints it (a pipe may end at
// a terminal too). On a terminal, the box's answer is printed indented
// and made safe, every line whole, and stderr says so and how to get
// what --json prints.
func (r *runner) printed(p tui.Printed) error {
	out := p.Raw
	if r.env.Terminal && p.Shown != "" {
		out = p.Shown
		if _, err := io.WriteString(r.env.Stderr, "motel: the box's answer was indented (at most 10 levels deep) and made safe for the terminal; redirect stdout (> file, or | cat) for what the command's --json prints\n"); err != nil {
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

// askingAfter is how long the first reads wait for the box before a
// line says so (sayAsking). An ordinary start waits about four round
// trips for the box (TCP, TLS, whoami, the list; five with TLS 1.2):
// well under a second near it, 1 to 1.25 s at 250 ms a round trip,
// 2.4 to 3 s at 600 ms (a satellite link). A box that does not answer
// holds each of the two reads for the client's 30 s, so the line comes
// with most of that minute still to wait.
var askingAfter = 3 * time.Second

// runFailure is how the TUI's end is said: a panic, already one line
// and safe, exits 3; a terminal that went away while the screen held it
// (a closed window's end of file, whether or not its SIGHUP came
// first) as an interrupt, nothing said; a screen that would not start, 2.
func runFailure(err error) error {
	if errors.Is(err, tui.ErrClosed) {
		return errInterrupted
	}
	var p *tui.Panic
	if errors.As(err, &p) {
		// The value quoted and cut (it may hold what a box sent), and the
		// functions it went through, innermost first, at most eight.
		return &client.Failure{Exit: client.ExitUnknown, Message: "the TUI failed: " + termtext.Quote(panicText(p.Value)).String() + " in " + shortStack(p.Stack)}
	}
	return screenFailure(err)
}

func screenFailure(err error) error {
	return &client.Failure{Exit: client.ExitUsage, Message: "the screen could not start: " + termtext.From(err.Error()).String()}
}

// CanDraw is Env.Draw on a real terminal. It asks the three things the
// screen's start needs, in this order: a terminfo entry for TERM, a
// character set of the locale that tcell can write (localeCharset;
// tcell's own ErrNoCharset when not), and /dev/tty, which the screen
// draws on whatever stdin and stdout are. A missing one fails here,
// before the first reads, and not once they have been made. (tcell's
// own NewDevTty would do for the last, but its Close before a Start
// closes nothing and fails.)
func CanDraw() error {
	if _, err := tcell.NewScreen(); err != nil {
		return err
	}
	if tcell.GetEncoding(localeCharset()) == nil {
		return tcell.ErrNoCharset
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return tty.Close()
}

// localeCharset is the character set tcell's screen takes from the
// locale when it starts, read as tcell v2.13.10 reads it on a Unix
// (getCharset, charset_unix.go), which it does not export: LC_ALL, else
// LC_CTYPE, else LANG, the first set; C and POSIX are US-ASCII; else,
// of the locale cut at its first @, what follows its first ., and
// UTF-8 when it has no . there. TestCharsetIsTcells holds it to tcell's
// Init.
func localeCharset() string {
	locale := os.Getenv("LC_ALL")
	if locale == "" {
		locale = os.Getenv("LC_CTYPE")
	}
	if locale == "" {
		locale = os.Getenv("LANG")
	}
	if locale == "POSIX" || locale == "C" {
		return "US-ASCII"
	}
	locale, _, _ = strings.Cut(locale, "@")
	_, set, found := strings.Cut(locale, ".")
	if !found {
		return "UTF-8"
	}
	return set
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
		return model.Got[wire.DeployResult]{}, &model.Failure{Kind: model.FailNotSent, Message: "the box's filter hid this deploy's version, so its record cannot be asked for"}
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

func (l live) Backups(ctx context.Context) (model.Got[wire.Backups], error) {
	v, ans, err := l.c.Backups(ctx)
	return got(v, ans, err)
}

func (l live) Alerts(ctx context.Context, app string) (model.Got[wire.Alerts], error) {
	a, err := client.ParseApp(app)
	if err != nil {
		return model.Got[wire.Alerts]{}, argument(err)
	}
	v, ans, err := l.c.Alerts(ctx, a)
	return got(v, ans, err)
}

func (l live) Series(ctx context.Context, app, rng string) (model.Got[wire.Series], error) {
	a, err := client.ParseApp(app)
	if err != nil {
		return model.Got[wire.Series]{}, argument(err)
	}
	v, ans, err := l.c.Series(ctx, a, rng)
	return got(v, ans, err)
}

func (l live) BoxAlerts(ctx context.Context) (model.Got[wire.BoxAlerts], error) {
	v, ans, err := l.c.BoxAlerts(ctx)
	return got(v, ans, err)
}

func (l live) Rules(ctx context.Context, app string) (model.Got[wire.Rules], error) {
	a, err := client.ParseApp(app)
	if err != nil {
		return model.Got[wire.Rules]{}, argument(err)
	}
	v, ans, err := l.c.Rules(ctx, a)
	return got(v, ans, err)
}

func (l live) BoxRules(ctx context.Context) (model.Got[wire.Rules], error) {
	v, ans, err := l.c.BoxRules(ctx)
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
	return &model.Failure{Kind: model.FailNotSent, Message: f.Message}
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

// sayAsking writes line to w once the first reads, before the screen
// opens, have waited askingAfter for the box (after gives each wait): a
// box that does not answer holds each for the client's 30 s, and the
// terminal would show nothing meanwhile. A signature's time is not
// counted, nor the client's own pause before it asks again (notify,
// when not nil, tells it one of them runs, and the wait starts again
// at its end): a hardware key's touch is ssh-keygen's to ask for, and
// the client waits a second before its retry of a 401, which the box
// answered at once. An ordinary start says nothing. The func it returns
// stops it, and returns once nothing more can be written.
func sayAsking(ctx context.Context, w io.Writer, line string, after func() <-chan time.Time, notify func(func(bool))) func() {
	ctx, cancel := context.WithCancel(ctx)
	signing, done := make(chan bool), make(chan struct{})
	if notify != nil {
		notify(func(on bool) {
			select {
			case signing <- on:
			case <-done:
			}
		})
	}
	go func() {
		defer close(done)
		wait := after()
		for {
			select {
			case <-ctx.Done():
				return
			case on := <-signing:
				wait = nil
				if !on {
					wait = after()
				}
			case <-wait:
				_, _ = io.WriteString(w, line)
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
		if notify != nil {
			notify(nil)
		}
	}
}

// pauser is the client's Sleep, telling sayAsking while it runs.
type pauser struct {
	sleep func(context.Context, time.Duration) error
	mu    sync.Mutex
	note  func(bool)
}

// Notify sets what is told; nil tells nothing.
func (p *pauser) Notify(f func(bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note = f
}

// Sleep sleeps as the client's Sleep, saying so before and after.
func (p *pauser) Sleep(ctx context.Context, d time.Duration) error {
	p.mu.Lock()
	note := p.note
	p.mu.Unlock()
	if note != nil {
		note(true)
		defer note(false)
	}
	return p.sleep(ctx, d)
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
