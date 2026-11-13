package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func silenceCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "silence <command>",
		Short: "Silence alerts in Alertmanager for a while, and end a silence (root)",
		Long: `Writes silences through the running motel's admin socket (root and the
motel user reach it), which checks each one, writes it to Alertmanager
and leaves a monitor: silence line in the journal. To list silences,
ask Alertmanager itself (docs/monitor.md, "Silence an alert").`,
	}
	c.AddCommand(silenceAddCommand(), silenceExpireCommand())
	return c
}

func silenceAddCommand() *cobra.Command {
	var req SilenceRequest
	var asJSON bool
	c := &cobra.Command{
		Use:   "add [--alert <name>] [--app <name>] [--release <version>] --comment <text> [--duration 2h]",
		Short: "Silence one of motel's alerts, an app's, or both, from now, for a while",
		Long: `motel builds the silence from names it knows: --alert is one of the
alerts motel raises on this box, --app one of the apps it runs,
--release one of the app's releases on disk. Each is an exact match;
give --alert, --app or both. --release needs --app and an alert whose
release is on disk: rollback, deploy_succeeded or
post_deploy_regressed (a failed or refused deploy's release is not:
silence deploy_failed by --alert and --app).

    motel monitor silence add --comment "resizing the volume" --duration 4h \
        --alert disk_low
    motel monitor silence add --comment "v42 is slow, a fix is coming" \
        --alert post_deploy_regressed --app shop --release v42

Refused: neither --alert nor --app; an alert motel does not raise here
(Watchdog, the heartbeat, among them: pause the check off the box
instead), an app the box does not run or a release not on disk, each
answered with the names it takes; a pair that would match nothing (a
box alert with --app); a comment empty, over 200 characters or with a
control character; a duration under 1m or over 24h. The comment is masked as a summary is.
The silence's createdBy is "motel monitor: <user>", the user from
SUDO_USER, else USER.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			req.By = invokingUser()
			return silenceRun(c, asJSON, func() (SilenceResult, error) {
				if err := req.Check(); err != nil {
					return SilenceResult{}, err
				}
				return silencePost(c, "/notify/silences", req, true)
			}, func(w io.Writer, res SilenceResult) error {
				_, err := fmt.Fprintf(w, "silence %s added; it ends at %s\n", res.ID, res.EndsAt)
				return err
			})
		},
	}
	f := c.Flags()
	f.StringVar(&req.Alert, "alert", "", "one of the alerts motel raises on this box, such as disk_low")
	f.StringVar(&req.App, "app", "", "one of the apps this box runs")
	f.StringVar(&req.Release, "release", "", "one release of the app on disk (with --app and --alert rollback, deploy_succeeded or post_deploy_regressed)")
	f.StringVar(&req.Comment, "comment", "", "why, in one line: whoever finds the silence reads it")
	f.StringVar(&req.Duration, "duration", defaultSilence.String(), "how long, from 1m to 24h")
	f.BoolVar(&asJSON, "json", false, `print {"id":…,"ends_at":…}, or {"error":…}`)
	adminFlags(c)
	return c
}

func silenceExpireCommand() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "expire <id>",
		Short: "End a silence now",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			req := ExpireRequest{ID: args[0], By: invokingUser()}
			return silenceRun(c, asJSON, func() (SilenceResult, error) {
				if err := req.Check(); err != nil {
					return SilenceResult{}, err
				}
				return silencePost(c, "/notify/silences/expire", req, false)
			}, func(w io.Writer, res SilenceResult) error {
				_, err := fmt.Fprintf(w, "silence %s expired\n", res.ID)
				return err
			})
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, `print {"id":…}, or {"error":…}`)
	adminFlags(c)
	return c
}

// silenceRun prints run's answer as text, or as JSON with --json. An
// error with --json is {"error":…} on stdout and exit 1, with cobra's
// own line silenced.
func silenceRun(c *cobra.Command, asJSON bool, run func() (SilenceResult, error), text func(io.Writer, SilenceResult) error) error {
	res, err := run()
	w := c.OutOrStdout()
	if !asJSON {
		if err != nil {
			return err
		}
		return text(w, res)
	}
	if err != nil {
		c.SilenceErrors = true
		if werr := json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); werr != nil {
			return werr
		}
		return err
	}
	return json.NewEncoder(w).Encode(res)
}

func silencePost(c *cobra.Command, uri string, body any, add bool) (SilenceResult, error) {
	out, err := adminPost(c, uri, body)
	if err != nil {
		// The dial's refusal alone, which AdminAPIRequest words
		// `performing request: …`; a route's own answer (its 502 from
		// Alertmanager's socket) is passed on as it is.
		if msg := err.Error(); strings.HasPrefix(msg, "performing request:") && strings.Contains(msg, "permission denied") {
			addr, _ := adminAddr(c)
			return SilenceResult{}, fmt.Errorf("the admin socket (%s) admits root and the motel user: run motel monitor silence as root", addr)
		}
		return SilenceResult{}, err
	}
	var res SilenceResult
	if json.Unmarshal(out, &res) != nil || res.ID == "" || add && res.EndsAt == "" {
		return SilenceResult{}, fmt.Errorf("motel answered %q", strings.TrimSpace(string(out)))
	}
	return res, nil
}

// invokingUser is who runs the command, for the silence's createdBy:
// the user sudo ran it for, else USER, else the uid's name. It is what
// the caller's environment says, from a caller the admin socket
// already admits as root or motel.
func invokingUser() string {
	for _, v := range []string{os.Getenv("SUDO_USER"), os.Getenv("USER")} {
		if byRe.MatchString(v) {
			return v
		}
	}
	if u, err := user.Current(); err == nil && byRe.MatchString(u.Username) {
		return u.Username
	}
	return "uid" + strconv.Itoa(os.Getuid())
}
