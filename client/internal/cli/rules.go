package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/render"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// monitorRules is `motel monitor rules [--app <app>]`, the remote twin
// of `moteld monitor rules` (motelctl-spec, row 15), with --app as
// `monitor alerts` takes it.
func (r *runner) monitorRules() *cobra.Command {
	var t target
	var appArg string
	c := &cobra.Command{
		Use:   "rules [--app <app>]",
		Short: "The rules the box alerts on, and how close each is (experimental)",
		Long: "monitor rules reads GET /_motel/rules, for a viewer of the box: every alert the\n" +
			"box can raise, what it fires on, and how close it is: the number a check of\n" +
			"motel's last read (a disk, a backup's age, a watchdog's restarts), and for a\n" +
			"rule the metrics store evaluates, its value for each app or filesystem and\n" +
			"whether it is pending or firing, and since when. The box reads the store's\n" +
			"numbers at most every 30 s for everyone, as vmalert compared them (about a\n" +
			"minute old). It is `moteld monitor rules` from a laptop, without the store's\n" +
			"socket and queries, which are the box's alone.\n\n" +
			"monitor rules --app <app> reads GET /_motel/apps/<app>/rules, for a viewer of\n" +
			"the app or of the box: the rules that apply to the app, every app's and its\n" +
			"own, with its numbers alone. Not the box's own checks (its disks, backups and\n" +
			"certificates). <app> is a name, or the app's URL\n" +
			"(https://deploy.example.com/blog), which names the box too.\n\n" +
			"It exits 0 when it read them, the store's numbers among them or with no store\n" +
			"(no rule of the store's is listed then); 3 when the store did not answer the\n" +
			"box, or the box said a word for it this client does not know: --json then\n" +
			"prints the client's own sentence. It is asked with your own key only: with\n" +
			"MOTEL_TOKEN set, name the key with --key.\n\n" + experimental,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var app client.App
			if cmd.Flags().Changed("app") {
				a, err := client.ParseApp(appArg)
				if err != nil {
					return err
				}
				app = a
			}
			c, err := r.client(cmd.Context(), &t, app.Box, "", "monitor rules")
			if err != nil {
				return err
			}
			var rules wire.Rules
			var ans *client.Answer
			if app.Name == "" {
				rules, ans, err = c.BoxRules(cmd.Context())
			} else {
				rules, ans, err = c.Rules(cmd.Context(), app)
			}
			if err != nil {
				return fail(err)
			}
			of := "its rules"
			if app.Name != "" {
				of = app.Name + "'s rules"
			}
			// Not knowing the store's numbers is could not tell, as for any
			// read the box could not answer: a script would read a rule with
			// no number as one nowhere near firing.
			switch rules.Store {
			case wire.StoreOn, wire.StoreOff:
			case wire.StoreUnanswered:
				return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
					"%s could not tell how close %s are: its metrics store did not answer it in time", c.Box.Host, of)}
			default:
				return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
					"%s could not tell how close %s are: it says its store is %s, a word this client does not know", c.Box.Host, of, termtext.Quote(rules.Store))}
			}
			if r.json {
				return r.emit(ans.Body)
			}
			return render.WriteRules(r.env.Stdout, rulesView(rules, app.Name, c.Box.Host, r.env.Now()))
		},
	}
	t.flags(c)
	c.Flags().StringVar(&appArg, "app", "", "the app: a name, or the app's URL")
	return c
}

// rulesView is the rules as the screen lays them, whole, under a line
// that names the box (and the app) asked: the same rows, from the same
// code (model.RuleBlock), as `:rules`.
func rulesView(rules wire.Rules, app, host string, now time.Time) render.Rules {
	var v render.Rules
	add := func(l model.Line) { v.Lines = append(v.Lines, termtext.From(l.Text())) }
	add(append(model.Line{{T: termtext.From("store       ")}}, model.StoreSays(rules)...))
	if l := model.RulesFileSays(rules); l != nil {
		add(append(model.Line{{T: termtext.From("rules file  ")}}, l...))
	}
	switch {
	case len(rules.Alerts) == 0:
	case app == "":
		add(model.Line{{T: termtext.From(host + " alerts on these rules:")}})
	default:
		add(model.Line{{T: termtext.From("the rules of " + app + " on " + host + ", with its numbers alone:")}})
	}
	for _, a := range rules.Alerts {
		for _, l := range model.RuleBlock(a, now) {
			add(l)
		}
	}
	return v
}
