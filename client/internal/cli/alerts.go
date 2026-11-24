package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/render"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// monitor is `motel monitor`, the group of the monitor's reads: the
// server's twin is `moteld monitor`, whose words these take where it
// has the command (`rules`); `alerts`, which it has not, is the
// client's.
func (r *runner) monitor() *cobra.Command {
	m := &cobra.Command{
		Use:   "monitor",
		Short: "What the box's monitor holds (experimental)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return &client.Failure{Exit: client.ExitUsage, Message: "monitor needs a command: motel monitor alerts [--app <app>], or motel monitor rules [--app <app>]"}
		},
	}
	m.AddCommand(r.monitorAlerts(), r.monitorRules())
	return m
}

func (r *runner) monitorAlerts() *cobra.Command {
	var t target
	var appArg string
	c := &cobra.Command{
		Use:   "alerts [--app <app>]",
		Short: "What the box's Alertmanager holds: every alert and silence, or an app's alerts (experimental)",
		Long: "monitor alerts reads GET /_motel/alerts, for a viewer of the box: every alert\n" +
			"the box's Alertmanager holds, since when it fires, whether a silence or the\n" +
			"deploy mute holds it and until when, and every silence that is active or waits\n" +
			"to start, with its matchers, who made it and why. Silences are added and\n" +
			"expired as root on the box (moteld monitor silence).\n\n" +
			"monitor alerts --app <app> reads GET /_motel/apps/<app>/alerts, for a viewer of\n" +
			"the app or of the box: the alerts the box's Alertmanager holds about the app,\n" +
			"newest first, each with its severity, when it began, and whether a silence\n" +
			"holds it and until when, or a deploy of the app. Not the box's own alerts, and\n" +
			"not the silences. <app> is a name, or the app's URL\n" +
			"(https://deploy.example.com/blog), which names the box too. The box asks\n" +
			"Alertmanager at most every 5 s, so an answer can be that old. It exits 0 when\n" +
			"it read them, alerts or none, and when the box has no monitor (nothing is\n" +
			"raised there); 3 when the box could not read what its Alertmanager holds\n" +
			"(the box's journal says why), or said a word for it this client does not\n" +
			"know: --json then prints the client's own sentence. It is asked with your\n" +
			"own key only: with MOTEL_TOKEN set, name the key with --key.\n\n" + experimental,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("app") {
				return r.boxAlerts(cmd, &t)
			}
			app, err := client.ParseApp(appArg)
			if err != nil {
				return err
			}
			c, err := r.client(cmd.Context(), &t, app.Box, "", "monitor alerts")
			if err != nil {
				return err
			}
			a, ans, err := c.Alerts(cmd.Context(), app)
			if err != nil {
				return fail(err)
			}
			// Not knowing whether anything fires is could not tell, as for
			// any read the box could not answer.
			var unknown *client.Failure
			switch a.Monitor {
			case wire.MonitorOn, wire.MonitorOff:
			case wire.MonitorUnanswered:
				unknown = &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
					"%s could not tell %s's alerts: it could not read what its Alertmanager holds (the box's journal says why)", c.Box.Host, app.Name)}
			default:
				unknown = &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
					"%s answered about %s's alerts in a word for its monitor this client does not know (%s): it cannot tell what fires", c.Box.Host, app.Name, a.Monitor)}
			}
			switch {
			case unknown != nil:
				// Said once, as the client's own sentence: stderr's last
				// line, and --json's object.
				return unknown
			case r.json:
				return r.emit(ans.Body)
			}
			return render.WriteAlerts(r.env.Stdout, alertsView(a, app.Name, c.Box.Host))
		},
	}
	t.flags(c)
	c.Flags().StringVar(&appArg, "app", "", "the app: a name, or the app's URL")
	return c
}

// alertsView names the app and the host asked for. Every name,
// severity and state in a was held to its grammar (client's hold); a
// summary is the box's free text, labelled and capped.
func alertsView(a wire.Alerts, app, host string) render.Alerts {
	v := render.Alerts{Host: tx(host), App: tx(app), Monitor: tx(a.Monitor)}
	for _, al := range a.Alerts {
		v.Alerts = append(v.Alerts, render.Alert{Name: tx(al.Name), Severity: tx(al.Severity), State: tx(al.State),
			Since: al.StartsAt, Until: al.SilencedUntil, Inhibited: al.Inhibited, Summary: tx(al.Summary)})
	}
	return v
}
