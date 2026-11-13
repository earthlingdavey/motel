package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
)

func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "monitor",
		Usage: "<command>",
		Short: "Who motel tells, and the numbers: send an event, test a channel, silence an alert, set up Alertmanager and the store",
		Long: `Commands of the monitor block (notify/DESIGN-notify.md and
notify/DESIGN-metrics.md in the motel repository). motel detects;
Debian's Alertmanager, on a unix socket motel renders its config for,
delivers. With the block's metrics line, motel pushes its numbers to
Debian's VictoriaMetrics, whose vmalert evaluates the rules motel
renders.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.AddCommand(sendCommand(), testCommand(), setupCommand(), silenceCommand())
		},
	})
	// The commands monitor replaced: hidden, and refused with the new
	// spelling, as the Caddyfile's old blocks are.
	movedCommand("notify", func(args []string) string {
		return "motel notify is now motel monitor: motel monitor " + shellWords(args)
	})
	movedCommand("metrics", func([]string) string {
		return "motel metrics setup is now motel monitor setup --metrics, which sets up Alertmanager, then the store"
	})
}

// shellWords is args as a shell would read them back: each that holds
// anything but a plain word's characters single-quoted, so that the new
// spelling of `--summary "a b"` can be pasted as it is.
func shellWords(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if a == "" || strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:=@%+,") != "" {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}

// movedCommand registers name as a hidden command that runs nothing and
// exits non-zero, saying what to run instead.
func movedCommand(name string, moved func(args []string) string) {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  name,
		Usage: "",
		Short: "Moved to motel monitor",
		CobraFunc: func(cmd *cobra.Command) {
			cmd.Hidden = true
			cmd.DisableFlagParsing = true
			cmd.Args = cobra.ArbitraryArgs
			cmd.RunE = func(_ *cobra.Command, args []string) error {
				return errors.New(strings.TrimSpace(moved(args)))
			}
		},
	})
}

// defaultAdmin is the starter's admin address (packaging/Caddyfile).
const defaultAdmin = "unix//run/motel/admin.sock"

// adminFlags are how a command finds the running motel's admin API: the
// starter's address, another, or a Caddyfile that names one. Not the
// box's Caddyfile by default: reading it adapts the whole file, which
// logs, and the admin address is the starter's on every box.
func adminFlags(c *cobra.Command) {
	c.Flags().String("address", defaultAdmin, "the admin API's address")
	c.Flags().String("config", "", "a Caddyfile whose admin address to use instead")
}

// adminAddr is the admin API's address the flags name.
func adminAddr(c *cobra.Command) (string, error) {
	addr, _ := c.Flags().GetString("address")
	if cfgFile, _ := c.Flags().GetString("config"); cfgFile != "" {
		return caddycmd.DetermineAdminAPIAddress("", nil, cfgFile, "caddyfile")
	}
	return addr, nil
}

func adminPost(c *cobra.Command, uri string, body any) ([]byte, error) {
	addr, err := adminAddr(c)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	h := http.Header{"Content-Type": {"application/json"}}
	resp, err := caddycmd.AdminAPIRequest(addr, http.MethodPost, uri, h, bytes.NewReader(b))
	if err != nil {
		return nil, apiRefusal(err)
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
and writes the monitor: event line, as for its own events; then
Alertmanager routes it like any other.

A one-shot by default. With --state firing, a state alert that stays
until a --state resolved for the same name, subject and instance.

    motel monitor send --source smartd --name disk_failing --severity critical \
        --instance /dev/sda --summary "SMART says /dev/sda is failing"
    motel monitor send --source cron --name reboot_required --state firing \
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
			var res SendResult
			if err := json.Unmarshal(out, &res); err != nil || res.Key == "" {
				return fmt.Errorf("motel answered %q", strings.TrimSpace(string(out)))
			}
			return sendAnswer(c.OutOrStdout(), res)
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

// sendAnswer is what `motel monitor send` says of motel's answer. An
// event that goes nowhere but the journal is an error (exit 1), naming
// setup; a resolve that found nothing firing under its key is not one
// (a producer may resolve whatever it last said), but it says so.
func sendAnswer(w io.Writer, res SendResult) error {
	if res.Undelivered != "" {
		return fmt.Errorf("journalled as %s, and delivered nowhere: %s", res.Key, res.Undelivered)
	}
	if res.ResolvedNothing {
		_, err := fmt.Fprintf(w, "resolved nothing: no state fires under %s (a resolve matches the firing's --source, --subject, --name and --instance, and a restart of motel forgets the state)\n", res.Key)
		return err
	}
	_, err := fmt.Fprintf(w, "sent %s\n", res.Key)
	return err
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
	var metrics bool
	c := &cobra.Command{
		Use:   "setup [--metrics]",
		Short: "Install and configure Debian's Alertmanager, and with --metrics its VictoriaMetrics, for motel (root)",
		Long: `Installs prometheus-alertmanager so that it never listens on a port:
masks its unit first, so the install starts nothing (Debian's would
listen on :9093 and 0.0.0.0:9094); installs it; writes the drop-in that
gives it motel's config and socket activation, the socket unit
(/run/motel-notify/am.sock, 0660 root:motel), and
/var/lib/motel-notify with a config that sends nothing; unmasks it,
leaves it to start on its socket's first connection, starts the socket
and reloads motel, which renders the monitor block's channels.

With --metrics, the store, which posts to Alertmanager: on a
box where Alertmanager's setup has not run to its end (motel-am.socket
not enabled and listening, or the service masked), Alertmanager's setup
above runs first, and the store's only once it
has succeeded; where it is, only the store's setup runs. The store's: installs
victoria-metrics so that it never listens in the host's network
namespace: masks its unit first, so the install starts nothing
(Debian's would listen on 0.0.0.0:8428, unauthenticated); installs it
(and iproute2 if missing); writes the namespace unit, the drop-in that
moves the store into the namespace with its memory capped, the socket
/run/motel-metrics/vm.sock (0660 root:motel) and its proxy,
Alertmanager's second socket (127.0.0.1:9093 inside the namespace) with
the drop-in that hands it to Alertmanager, and vmalert's unit, and
/var/lib/motel-metrics with the scrapes and an empty rule file;
retires the proxy an earlier setup put on that port; unmasks the store;
restarts Alertmanager once, unless that socket already listens (it
takes a socket up only when it starts: what it holds in memory goes);
starts them all and reloads motel, which renders the rules and pushes.

Run again, each puts its units back as the binary ships them:
motel monitor setup Alertmanager's (stopping it for the while: what it
holds in memory goes), motel monitor setup --metrics the store's.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return errors.New("motel monitor setup installs packages and writes units: run it as root")
			}
			return runSetup(hostSystem{out: c.OutOrStdout()}, "/", c.OutOrStdout(), metrics)
		},
	}
	c.Flags().BoolVar(&metrics, "metrics", false, "set up the metrics store (and Alertmanager first, where it is not set up)")
	return c
}

// runSetup is `motel monitor setup`: Alertmanager's setup; with
// metrics, the store's, after Alertmanager's only where that has not
// run to its end (amSetUp). A re-run with metrics does not run a working
// Alertmanager's setup: it masks and stops Alertmanager, which loses the
// alerts it holds in memory and what waits in a group (the store's
// restarts it once, only to hand it its socket in the namespace, and not
// again once it has it). Each setup keeps
// its own refusals and its own put-back on a failed re-run, and the
// first that fails stops the rest.
func runSetup(sys system, root string, out io.Writer, metrics bool) error {
	if metrics && amSetUp(sys, root) {
		_, _ = fmt.Fprintf(out, "Alertmanager is set up (%s enabled and listening): its setup is not run again (motel monitor setup does that)\n", amSocket)
		return metricsSetup(sys, root, out)
	}
	if err := setup(sys, root, out); err != nil {
		return err
	}
	if !metrics {
		return nil
	}
	return metricsSetup(sys, root, out)
}

// amSetUp reports an Alertmanager setup that ran to its end: the socket
// unit and the drop-in written, the service not masked, and the socket enabled and
// active. A first setup writes the socket unit before it unmasks the
// service and enables the socket, so the file alone would take one that
// failed between for a working Alertmanager, masked and deaf (Copilot
// round 1 on #428); then Alertmanager's setup runs again, which is its
// recovery, and touches nothing that works.
func amSetUp(sys system, root string) bool {
	// The drop-in too: without it Alertmanager runs Debian's command,
	// on :9093 and 0.0.0.0:9094, and the store's setup may restart it
	// (/code-review pass 2 on #435).
	if !isFile(filepath.Join(root, socketPath)) || !isFile(filepath.Join(root, dropInPath)) {
		return false
	}
	// `systemctl mask` is a link to /dev/null where the unit would be.
	if dest, err := os.Readlink(filepath.Join(root, unitDir, amService)); err == nil && dest == os.DevNull {
		return false
	}
	return sys.run("systemctl", "is-enabled", "--quiet", amSocket) == nil &&
		sys.run("systemctl", "is-active", "--quiet", amSocket) == nil
}

// apiRefusal is the admin API's own words for a refusal, which
// AdminAPIRequest wraps as `caddy responded with error: HTTP 422:
// {"error":"…"}`.
func apiRefusal(err error) error {
	msg := err.Error()
	i := strings.Index(msg, "{")
	if i < 0 {
		return err
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(msg[i:])), &body) != nil || body.Error == "" {
		return err
	}
	return errors.New(body.Error)
}
