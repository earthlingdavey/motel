package cli

import (
	"fmt"
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

// metrics is `motel metrics <app> [--range]`, the motelctl spec's
// command (row 18, `motelctl metrics <app> --range <r>`): the read the
// metrics screen draws, its JSON for a script, and its numbers in
// words for a person.
func (r *runner) metrics() *cobra.Command {
	var t target
	var rng string
	c := &cobra.Command{
		Use:   "metrics <app>",
		Short: "An app's numbers over time: memory, cpu, requests, 5xx share, p95 (experimental)",
		Long: "metrics reads GET /_motel/apps/<app>/series, for a viewer of the app or of the\n" +
			"box: the app's numbers over a window, as the box's metrics store holds them,\n" +
			"a point a step (1h: a minute; 24h: 5 minutes; 7d: 30; 14d: an hour), the newest\n" +
			"of the last whole step. It prints each one's lowest, highest and newest value\n" +
			"and when, and how many steps have a point; motel alone draws them (g on an\n" +
			"app's screen), and --json prints the points. The request numbers are there\n" +
			"only for an app behind observe. <app> is a name, or the app's URL\n" +
			"(https://deploy.example.com/blog), which names the box too. The box asks its\n" +
			"store at most every 30 s for an app and window, so an answer can be that old.\n" +
			"It exits 0 when it read them, and when the box keeps no numbers (it has no\n" +
			"metrics line); 3 when the box could not ask its store, or said a word for it\n" +
			"this client does not know: --json then prints the client's own sentence. It is\n" +
			"asked with your own key only: with MOTEL_TOKEN set, name the key with --key.\n\n" + experimental,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := client.ParseApp(args[0])
			if err != nil {
				return err
			}
			// The window is refused before the box is looked for, as every
			// argument is.
			if cmd.Flags().Changed("range") && !slices.Contains(wire.SeriesRanges, rng) {
				return &client.Failure{Exit: client.ExitUsage, Message: fmt.Sprintf("--range %q is not one of %s", termtext.From(rng), strings.Join(wire.SeriesRanges, ", "))}
			}
			c, err := r.client(cmd.Context(), &t, app.Box, "", "metrics")
			if err != nil {
				return err
			}
			s, ans, err := c.Series(cmd.Context(), app, rng)
			if err != nil {
				return fail(err)
			}
			switch s.Store {
			case wire.StoreOn, wire.StoreOff:
			case wire.StoreUnanswered:
				return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
					"%s could not tell %s's numbers: its metrics store did not answer it", c.Box.Host, app.Name)}
			default:
				return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
					"%s answered about %s's numbers in a word for its store this client does not know (%s): it cannot tell what the store holds", c.Box.Host, app.Name, s.Store)}
			}
			if r.json {
				return r.emit(ans.Body)
			}
			return render.WriteSeries(r.env.Stdout, seriesView(s, app.Name, c.Box.Host))
		},
	}
	t.flags(c)
	c.Flags().StringVar(&rng, "range", "", "the window: "+strings.Join(wire.SeriesRanges, ", ")+" (the box's default is "+wire.SeriesRanges[0]+")")
	return c
}

// seriesView is what the command says of s: the store's word and the
// window were held to their grammars (client's holdSeries), a panel's
// name and unit to a word's; every value is the client's own writing of
// a number.
func seriesView(s wire.Series, app, host string) render.Series {
	v := render.Series{Host: tx(host), App: tx(app), Window: tx(s.Window), Store: tx(s.Store), Step: time.Duration(s.StepSeconds) * time.Second}
	// The request panels' absence is said as observe's only beside the
	// app's memory or cpu, every panel known: with no point of the app at
	// all, or a panel the box's filter may have renamed, no cause is.
	ran := false
	for _, p := range s.Panels {
		if (p.Name == wire.PanelMemory || p.Name == wire.PanelCPU) && len(p.Points) > 0 {
			ran = true
		}
	}
	v.NoRequests = s.Store == wire.StoreOn && ran
	if s.End != nil {
		v.End = *s.End
	}
	steps := 0
	if v.Step > 0 && s.End != nil {
		steps = int(model.SeriesSpan(s.Window)/v.Step) + 1
	}
	at := func(pt [2]float64) time.Time { return time.Unix(int64(pt[0]), 0) }
	for _, p := range s.Panels {
		label, known := model.SeriesLabel(p.Name, p.Unit)
		// A panel this client does not know may be one the box's filter
		// renamed: no cause is said for one absent then.
		if p.Name == wire.PanelRequests && known || !known {
			v.NoRequests = false
		}
		pn := render.SeriesPanel{Label: label, Name: tx(p.Name), Unit: tx(p.Unit), Known: known, Points: len(p.Points), Steps: steps}
		if known && len(p.Points) > 0 {
			lo, hi := p.Points[0], p.Points[0]
			for _, pt := range p.Points {
				if pt[1] < lo[1] {
					lo = pt
				}
				if pt[1] > hi[1] {
					hi = pt
				}
			}
			newest := p.Points[len(p.Points)-1]
			pn.Min = render.SeriesPoint{V: model.SeriesValue(p.Name, lo[1], false), T: at(lo)}
			pn.Max = render.SeriesPoint{V: model.SeriesValue(p.Name, hi[1], false), T: at(hi)}
			pn.Newest = render.SeriesPoint{V: model.SeriesValue(p.Name, newest[1], false), T: at(newest)}
		}
		v.Panels = append(v.Panels, pn)
	}
	return v
}
