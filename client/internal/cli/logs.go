package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/render"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// logs is `motel logs <app> [--lines] [--follow]`: GET
// /_motel/apps/<app>/logs, the app's own lines, for a person with a
// logs grant on the app or the box.
func (r *runner) logs() *cobra.Command {
	var t target
	var lines string
	var follow bool
	c := &cobra.Command{
		Use:   "logs <app>",
		Short: "An app's own lines, as the box's journal holds them; --follow reads on (experimental)",
		Long: "logs reads GET /_motel/apps/<app>/logs, for a person with a logs grant on the\n" +
			"app or the box: the app's newest lines, oldest first (--lines, " + strings.Join(wire.LogsLines, ", ") + ";\n" +
			"the box's default is " + wire.LogsLines[0] + "). Each line is its time (UTC) and its text, a\n" +
			"tab shown as spaces to the next stop of 8 and every other control character\n" +
			"and colour code shown escaped; a line before them, and wherever the release\n" +
			"or the day changes, names the day and the release.\n" +
			"A read sends at most that many lines or 64 KiB, and older lines are then not\n" +
			"sent: logs does not page back. <app> is a name, or the app's URL\n" +
			"(https://deploy.example.com/blog), which names the box too.\n\n" +
			"--follow then reads every 2 s for the lines after the last one read, until\n" +
			"ctrl-c (exit 130) or a read fails (that read's exit); a 429 or a busy box's\n" +
			"503 asking for a wait of a minute or less is waited out. Each read that sends\n" +
			"lines is a line in the box's journal, naming you; a follow beside an open\n" +
			"motel brings you to the 2 requests a second a box allows a person.\n\n" +
			"An app that keeps its output on the box (deploy_log_lines 0) has no line\n" +
			"sent: that is said on stderr, exit 0. --json prints the box's answer; with\n" +
			"--follow, one JSON object a line, each a line of the app's as the box sent\n" +
			"it, and nothing else on stdout, even on a failure once it runs: what is not a\n" +
			"line is said on stderr. It is asked with your own key only: with MOTEL_TOKEN\n" +
			"set, name the key with --key.\n\n" + experimental,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Nothing but lines goes to stdout in this mode, a failure's
			// object neither (Main).
			r.stream = follow && r.json
			app, err := client.ParseApp(args[0])
			if err != nil {
				return err
			}
			// The count is refused before the box is looked for, as every
			// argument is.
			if cmd.Flags().Changed("lines") && !slices.Contains(wire.LogsLines, lines) {
				return &client.Failure{Exit: client.ExitUsage, Message: fmt.Sprintf("--lines %s is not one of %s", termtext.Quote(lines), strings.Join(wire.LogsLines, ", "))}
			}
			c, err := r.client(cmd.Context(), &t, app.Box, "", "logs")
			if err != nil {
				return err
			}
			lr := &logsRun{r: r, c: c, app: app, lines: lines, out: &render.Logs{W: r.env.Stdout}}
			if follow {
				return lr.follow(cmd.Context())
			}
			return lr.once(cmd.Context())
		},
	}
	t.flags(c)
	c.Flags().StringVar(&lines, "lines", "", "the most lines a read sends: "+strings.Join(wire.LogsLines, ", ")+" (the box's default is "+wire.LogsLines[0]+")")
	c.Flags().BoolVar(&follow, "follow", false, "read on every 2 s for the lines after the last one read, until ctrl-c")
	return c
}

// logsRun is one logs command: the box asked, the app, the count asked
// for, and where the plain layout has got to.
type logsRun struct {
	r     *runner
	c     *client.Client
	app   client.App
	lines string
	out   *render.Logs
	// div is where the plain layout's dividers have got to.
	div model.LogsDividers
}

// once is a tail: the newest lines.
func (l *logsRun) once(ctx context.Context) error {
	lg, ans, err := l.c.Logs(ctx, l.app, l.lines, "")
	if err != nil {
		return fail(err)
	}
	if err := l.known(lg); err != nil {
		return err
	}
	// With --json the box's answer is printed first, and what the client
	// says of it goes to stderr, as a follow's does.
	if l.r.json {
		if err := l.r.emit(ans.Body); err != nil {
			return err
		}
	}
	var f model.LogsFollow
	turn := f.Take(lg)
	switch {
	case turn.Off:
		return l.tell(l.off())
	case len(lg.Lines) == 0 && !turn.Older:
		return l.tell(l.none())
	case turn.Older:
		if err := l.say(l.older()); err != nil {
			return err
		}
	}
	if l.r.json {
		return nil
	}
	return l.out.Lines(l.layout(lg.Lines))
}

// follow reads the newest lines, then every LogsEvery the lines after
// the last one read (at once when a read stopped with lines left), until
// the context ends, a read fails, or the box says the app keeps its
// output on the box. A 429 and a busy box's 503 asking a minute or less
// (waitOut) are waited out, by the box's Retry-After (30 s for a 429
// with none), and the follow goes on.
// A 429 the client's own read waited out once (client.Get) is waited
// out here only when it stood.
func (l *logsRun) follow(ctx context.Context) error {
	if _, err := fmt.Fprintf(l.r.env.Stderr, "motel: following %s's lines, a read every %d s; ctrl-c stops.\n", l.app.Name, int(model.LogsEvery/time.Second)); err != nil {
		return err
	}
	var f model.LogsFollow
	var wait time.Duration
	taken, waiting := false, false
	for {
		if wait > 0 {
			if err := l.r.env.Sleep(ctx, wait); err != nil {
				return err
			}
		}
		lg, ans, err := l.c.Logs(ctx, l.app, l.lines, f.After())
		if err != nil {
			d, ok := waitOut(err)
			if !ok {
				return logsStopped(err, taken)
			}
			if !waiting {
				// The read's own sentence, the box's quoted in it (a 503's:
				// its reads busy, or moteld starting), then the wait.
				var cf *client.Failure
				errors.As(err, &cf)
				if _, err := fmt.Fprintf(l.r.env.Stderr, "motel: %s (following waits %d s, and asks again until it answers).\n", strings.TrimSuffix(cf.Message, "."), int(d/time.Second)); err != nil {
					return err
				}
			}
			waiting, wait = true, d
			continue
		}
		waiting = false
		if err := l.known(lg); err != nil {
			return logsStopped(err, taken)
		}
		turn := f.Take(lg)
		if turn.Off {
			return l.tell(l.off())
		}
		if !taken && len(lg.Lines) == 0 && !turn.Older {
			if err := l.tell(l.none() + "; following waits for its first"); err != nil {
				return err
			}
		}
		taken = true
		if turn.Gap {
			if err := l.say(model.LogsGapSays(l.app.Name, len(lg.Lines) == 0, turn.Older)); err != nil {
				return err
			}
		}
		if turn.Older {
			if err := l.say(l.older()); err != nil {
				return err
			}
		}
		if err := l.print(lg, ans); err != nil {
			return err
		}
		wait = model.LogsEvery
		if turn.Again {
			wait = 0
		}
	}
}

// known is nil when the box names what it keeps in a word this client
// knows; else the read could not be told, exit 3.
func (l *logsRun) known(lg wire.Logs) error {
	switch lg.Kept {
	case wire.KeptOn, wire.KeptOff:
		return nil
	}
	return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
		"%s answered about %s's lines in a word for what it keeps this client does not know (%s): it cannot tell whether they are sent", l.c.Box.Host, l.app.Name, lg.Kept)}
}

func (l *logsRun) off() string { return model.LogsOffSays(l.c.Box.Host, l.app.Name) }

func (l *logsRun) none() string { return model.LogsNoneSays(l.c.Box.Host, l.app.Name) }

func (l *logsRun) older() string {
	n := l.lines
	if n == "" {
		n = wire.LogsLines[0]
	}
	s := model.LogsOlderSays(n) + ", and motel logs does not page back"
	// The largest count the box takes reaches further back.
	if most := model.LogsMostLines(); n != most {
		s += " (--lines " + most + " may send more, unless the 64 KiB was what stopped it)"
	}
	return s
}

// say is the client's own sentence s about the lines: on stderr with
// --json, where stdout holds the box's JSON alone; else on a divider
// among the lines, in their order.
func (l *logsRun) say(s string) error {
	if l.r.json {
		return l.tell(s)
	}
	l.div.Note()
	return l.out.Note(s)
}

// tell is the client's own sentence s on stderr: what is said instead
// of lines.
func (l *logsRun) tell(s string) error {
	_, err := fmt.Fprintf(l.r.env.Stderr, "motel: %s.\n", s)
	return err
}

// print writes a read's lines: with --json each as the box sent it,
// one a line (the answer's own JSON of it, its white space dropped and
// made safe as every --json is, termtext.JSON); else in the plain
// layout.
func (l *logsRun) print(lg wire.Logs, ans *client.Answer) error {
	if !l.r.json {
		return l.out.Lines(l.layout(lg.Lines))
	}
	var raw struct {
		Lines []json.RawMessage `json:"lines"`
	}
	// The body the client read decoded the same way: this cannot fail
	// but on a body that read and did not.
	if json.Unmarshal(ans.Body, &raw) != nil || len(raw.Lines) != len(lg.Lines) {
		return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf("%s answered with lines that could not be read again", l.c.Box.Host)}
	}
	for _, line := range raw.Lines {
		var b bytes.Buffer
		if json.Compact(&b, line) != nil {
			return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf("%s answered with lines that could not be read again", l.c.Box.Host)}
		}
		b.WriteByte('\n')
		if _, err := l.r.env.Stdout.Write(termtext.JSON(b.Bytes())); err != nil {
			return err
		}
	}
	return nil
}

// layout is the box's lines in the plain layout's view, after those
// written before: the TUI's logs screen lays them out by the same
// pieces (model.LogsDivider, LogsLine.Clock).
func (l *logsRun) layout(lines []wire.LogLine) []render.LogLine {
	shown := model.LogsLines(lines)
	out := make([]render.LogLine, len(shown))
	for i, s := range shown {
		d, _ := l.div.Before(s)
		out[i] = render.LogLine{Divider: d, Clock: s.Clock(), Text: s.Text}
	}
	return out
}

// waitOut is the wait for a read the box asked to be made again later:
// a 429 that stood after the client's own wait (its Retry-After, at
// most client.MaxRetryAfter as a 503's is, or model.NoRetryAfter with
// none), or a 503 that carries
// a wait (client.Logs: the box's, while it runs as many logs reads as it
// runs at once).
func waitOut(err error) (time.Duration, bool) {
	var f *client.Failure
	switch {
	case !errors.As(err, &f):
		return 0, false
	case f.Status == http.StatusTooManyRequests && f.RetryAfter > client.MaxRetryAfter:
		return 0, false
	case f.Status == http.StatusTooManyRequests && f.RetryAfter > 0, f.Status == http.StatusServiceUnavailable && f.RetryAfter > 0:
		return f.RetryAfter, true
	case f.Status == http.StatusTooManyRequests:
		return model.NoRetryAfter, true
	}
	return 0, false
}

// logsStopped is a read made while following that got no lines: the
// read's own failure, saying that following stopped once a read had
// been taken.
func logsStopped(err error, taken bool) error {
	err = fail(err)
	var f *client.Failure
	if !taken || !errors.As(err, &f) {
		return err
	}
	g := *f
	g.Message = strings.TrimSuffix(g.Message, ".") + " (following stopped)."
	return &g
}
