package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
)

func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "notify",
		Usage: "<command>",
		Short: "Who motel tells: send an event, test a channel, set up Alertmanager",
		Long: `Commands of the notify module (notify/DESIGN-notify.md in the motel
repository). motel detects; Debian's Alertmanager, on a unix socket
motel renders its config for, delivers.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.AddCommand(sendCommand(), testCommand(), setupCommand())
		},
	})
}

// adminFlags are how a command finds the running motel's admin API: the
// address, or the Caddyfile that names it (the starter's is
// unix//run/motel/admin.sock).
func adminFlags(c *cobra.Command) {
	c.Flags().String("config", "/etc/motel/Caddyfile", "the Caddyfile whose admin address to use")
	c.Flags().String("address", "", "the admin API's address, over the one the Caddyfile names")
}

func adminPost(c *cobra.Command, uri string, body any) ([]byte, error) {
	addr, _ := c.Flags().GetString("address")
	cfgFile, _ := c.Flags().GetString("config")
	if addr == "" {
		if _, err := os.Stat(cfgFile); err == nil {
			a, err := caddycmd.DetermineAdminAPIAddress("", nil, cfgFile, "caddyfile")
			if err != nil {
				return nil, err
			}
			addr = a
		}
	}
	if addr == "" {
		addr = "unix//run/motel/admin.sock"
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	h := http.Header{"Content-Type": {"application/json"}}
	resp, err := caddycmd.AdminAPIRequest(addr, http.MethodPost, uri, h, bytes.NewReader(b))
	if err != nil {
		return nil, err // AdminAPIRequest already says what the answer was
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

func sendCommand() *cobra.Command {
	var req SendRequest
	var details []string
	c := &cobra.Command{
		Use:   "send --source <name> --name <event> --summary <text> [flags]",
		Short: "Emit an event from a producer outside motel (cron, smartd, a script)",
		Long: `Sends one event to the running motel over its admin socket (root and the
motel user reach it), which checks it, stamps source external:<name>,
passes the text through the response filter, applies the flapping guard
and writes the notify: event line, as for its own events; then
Alertmanager routes it like any other.

A one-shot by default. With --state firing, a state alert that stays
until a --state resolved for the same name, subject and instance.

    motel notify send --source smartd --name disk_failing --severity critical \
        --instance /dev/sda --summary "SMART says /dev/sda is failing"
    motel notify send --source cron --name reboot_required --state firing \
        --summary "a kernel update waits for a reboot"

Refused: motel's own event names (watchdog_*, deploy_*, rollback,
backup_*, cert_failed, disk_low, Watchdog, ChannelFailed,
deploy_in_progress), a subject naming no app the box runs, a summary
over 200 characters or on more than one line, details over 2 KiB.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			for _, d := range details {
				k, v, ok := strings.Cut(d, "=")
				if !ok {
					return fmt.Errorf("--detail %q is not key=value", d)
				}
				if req.Details == nil {
					req.Details = map[string]string{}
				}
				req.Details[k] = v
			}
			if err := req.Check(); err != nil {
				return err
			}
			out, err := adminPost(c, "/notify/events", req)
			if err != nil {
				return err
			}
			var res struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(out, &res); err != nil || res.Key == "" {
				return fmt.Errorf("motel answered %q", strings.TrimSpace(string(out)))
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "sent %s\n", res.Key)
			return err
		},
	}
	f := c.Flags()
	f.StringVar(&req.Source, "source", "", "the producer's name (stamped external:<name>)")
	f.StringVar(&req.Subject, "subject", "box", "box, or app:<name> to reach the app's channels too")
	f.StringVar(&req.Name, "name", "", "the event's name, such as disk_failing")
	f.StringVar(&req.State, "state", "", "firing or resolved for a state alert; empty for a one-shot")
	f.StringVar(&req.Severity, "severity", SeverityWarning, "critical, warning or info")
	f.StringVar(&req.Instance, "instance", "", "what within the subject: a device, a unit")
	f.StringVar(&req.Summary, "summary", "", "one line, at most 200 characters, no secret")
	f.StringArrayVar(&details, "detail", nil, "key=value, repeatable, 2 KiB in all")
	adminFlags(c)
	return c
}

func testCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "test <channel>",
		Short: "Send a test notification to one channel and say whether it was delivered",
		Long: `Posts one NotifyTest alert routed to the named channel alone, then reads
Alertmanager's delivery counters for that channel's receiver for up to
15 seconds: delivered, failed (with Alertmanager's reason class), or
neither yet. It exits 1 unless delivered. The URL is never printed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			out, err := adminPost(c, "/notify/test", TestRequest{Channel: args[0]})
			if err != nil {
				return err
			}
			var res TestResult
			if err := json.Unmarshal(out, &res); err != nil || res.Channel == "" {
				return fmt.Errorf("motel answered %q", strings.TrimSpace(string(out)))
			}
			w := c.OutOrStdout()
			switch {
			case res.Delivered:
				_, err = fmt.Fprintf(w, "delivered to %s\n", res.Channel)
				return err
			case res.Failed != "":
				return fmt.Errorf("%s: Alertmanager could not deliver (%s); journalctl -u prometheus-alertmanager says more", res.Channel, res.Failed)
			default:
				return errors.New(res.Channel + ": " + res.Note)
			}
		},
	}
	adminFlags(c)
	return c
}

func setupCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Install and configure Debian's Alertmanager for motel (root)",
		Long: `Installs prometheus-alertmanager so that it never listens on a port:
masks its unit first, so the install starts nothing (Debian's would
listen on :9093 and 0.0.0.0:9094); installs it; writes the drop-in that
gives it motel's config and socket activation, the socket unit
(/run/motel-notify/am.sock, 0660 root:motel), and
/var/lib/motel-notify with a config that sends nothing; unmasks it,
leaves it to start on its socket's first connection, starts the socket
and reloads motel, which renders the notify block. Run again, it puts
the units back as the binary ships them.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return errors.New("motel notify setup installs a package and writes units: run it as root")
			}
			return setup(hostSystem{out: c.OutOrStdout()}, "/", c.OutOrStdout())
		},
	}
}
