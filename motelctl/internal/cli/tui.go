package cli

import (
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

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
		Short: "Screens over the reads: apps, an app, a deploy's record, who you are, the box (experimental)",
		Long: "tui shows the box on screens in the terminal, over the same reads the other\n" +
			"commands make: the apps you may view sorted by what needs a look, an app's\n" +
			"state and deploys, a deploy's record laid out for reading a failure, who you\n" +
			"are, and the box. It only reads, at no more than 1.5 requests a second, and\n" +
			"signs one token for ten minutes at a time, so a hardware key is touched about\n" +
			"every nine. Each screen's keys are on its last line. It is asked with your\n" +
			"own key only.\n\n" + experimental,
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
				pt.Life = sshkey.MaxLife
				sig := &tui.Signer{Inner: pt.Signer, Now: r.env.Now}
				pt.Signer = sig
				c.NoWait, c.Notes = true, nil
				k := pt.Key
				s.Host = c.Box.Host
				s.Key = model.Key{Fingerprint: k.Public.Fingerprint(), Shown: k.Shown, Source: k.Source,
					Hardware: strings.HasPrefix(k.Public.Type, "sk-"), Line: k.Public.Line()}
				s.TokenExpires = sig.Expires
				o.Client, o.Signer, o.Started = model.Live{C: c}, sig, gt.Close
			}
			s.Engine = model.NewEngine(r.env.Now())
			o.Session = s
			start := tui.Prime(ctx, o)
			if err := tui.Run(ctx, o, start); err != nil {
				return &client.Failure{Exit: client.ExitUsage, Message: "the screen could not start: " + termtextFrom(err)}
			}
			return nil
		},
	}
	t.flags(c)
	c.Flags().BoolVar(&sample, "demo", false, "show sample data, with no box")
	_ = c.Flags().MarkHidden("demo")
	return c
}

func termtextFrom(err error) string { return termtext.From(err.Error()).String() }

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
