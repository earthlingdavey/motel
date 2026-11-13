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
		Use:   "add --comment <text> [--duration 2h] <matcher>...",
		Short: "Silence the alerts the matchers select, from now, for a while",
		Long: `Each matcher is one argument, amtool's grammar without quotes:
label=value, label!=value, label=~regex or label!~regex (a regex
matches the whole value). At least one is label=value with a value.

    motel monitor silence add --comment "replacing the disk" --duration 4h \
        alertname=disk_failing instance=/dev/sda

Refused: no matcher, or none of the label=value kind; matchers that
would select Watchdog, the heartbeat (pause the check off the box
instead); quotes around a value; a comment empty, on two lines or over
200 characters; a duration under 1m or over 24h. The silence's
createdBy is "motel monitor: <user>", the user from SUDO_USER, else
USER.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			req.Matchers, req.By = args, invokingUser()
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
		if strings.Contains(err.Error(), "permission denied") {
			addr, _ := c.Flags().GetString("address")
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
