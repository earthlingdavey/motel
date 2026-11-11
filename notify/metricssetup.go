package notify

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
)

func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "metrics",
		Usage: "<command>",
		Short: "The metrics store: set up Debian's VictoriaMetrics for motel",
		Long: `Commands of the metrics store (notify/DESIGN-metrics.md in the motel
repository). motel measures and pushes; Debian's VictoriaMetrics stores,
in a private network namespace behind a unix socket; vmalert evaluates
the rules motel renders, and posts to Alertmanager.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.AddCommand(&cobra.Command{
				Use:   "setup",
				Short: "Install and configure Debian's victoria-metrics for motel (root)",
				Long: `Installs victoria-metrics so that it never listens in the host's network
namespace: masks its unit first, so the install starts nothing (Debian's
would listen on 0.0.0.0:8428, unauthenticated); installs it (and
iproute2 if missing); writes the namespace unit, the drop-in that moves
the store into the namespace with its memory capped, the socket
/run/motel-metrics/vm.sock (0660 root:motel) and its proxy, the
in-namespace proxy to Alertmanager and vmalert's unit, and
/var/lib/motel-metrics with the scrapes and an empty rule file;
unmasks it, starts them all and reloads motel, which renders the rules
and pushes. Run motel notify setup first. Run again, it puts the units
back as the binary ships them.`,
				Args: cobra.NoArgs,
				RunE: func(c *cobra.Command, _ []string) error {
					if os.Geteuid() != 0 {
						return errors.New("motel metrics setup installs a package and writes units: run it as root")
					}
					return metricsSetup(hostSystem{out: c.OutOrStdout()}, "/", c.OutOrStdout())
				},
			})
		},
	})
}

// The files `motel metrics setup` writes, which ship in the binary: the
// package installs nothing of the store's until root asks.
var (
	//go:embed units/metrics/motel-metrics-netns.service
	unitNetns []byte
	//go:embed units/metrics/10-motel.conf
	unitVMDropIn []byte
	//go:embed units/metrics/motel-vm.socket
	unitVMSocket []byte
	//go:embed units/metrics/motel-vm.service
	unitVMProxy []byte
	//go:embed units/metrics/motel-am-proxy.socket
	unitAMProxySocket []byte
	//go:embed units/metrics/motel-am-proxy.service
	unitAMProxy []byte
	//go:embed units/metrics/motel-vmalert.service
	unitVMAlert []byte
	//go:embed units/metrics/scrape.yml
	scrapeConfig []byte
)

const (
	vmPackage   = "victoria-metrics"
	vmService   = "victoria-metrics.service"
	vmUser      = "_victoria-metrics"
	ipPackage   = "iproute2"
	unitDir     = "/etc/systemd/system"
	netnsUnit   = "motel-metrics-netns.service"
	vmSocket    = "motel-vm.socket"
	amProxySock = "motel-am-proxy.socket"
	vmalertUnit = "motel-vmalert.service"
	scrapeName  = "scrape.yml"
)

// metricsUnits are setup's unit files, each at its path under unitDir.
func metricsUnits() []struct {
	path string
	data []byte
} {
	return []struct {
		path string
		data []byte
	}{
		{filepath.Join(unitDir, vmService+".d", "10-motel.conf"), unitVMDropIn},
		{filepath.Join(unitDir, netnsUnit), unitNetns},
		{filepath.Join(unitDir, vmSocket), unitVMSocket},
		{filepath.Join(unitDir, "motel-vm.service"), unitVMProxy},
		{filepath.Join(unitDir, amProxySock), unitAMProxySocket},
		{filepath.Join(unitDir, "motel-am-proxy.service"), unitAMProxy},
		{filepath.Join(unitDir, vmalertUnit), unitVMAlert},
	}
}

// metricsSetup is `motel metrics setup`: Debian's victoria-metrics,
// installed so that it never listens in the host's namespace, even for a
// moment. Debian's unit starts on install on 0.0.0.0:8428 with no
// authentication (DESIGN-metrics.md, "Packaging and setup"), so the
// order is root's, as for notify's: mask, install, write the units and
// the state directory, unmask, start the namespace, the store, both
// sockets and vmalert, and have motel render. It needs notify's setup
// first: vmalert's outlet is Alertmanager's socket.
//
// root is a prefix for every path written (tests); the commands run as
// given. Each step says what it did on out.
func metricsSetup(sys system, root string, out io.Writer) (err error) {
	at := func(p string) string { return filepath.Join(root, p) }
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format+"\n", a...) }
	rerun := isFile(at(filepath.Join(unitDir, vmService+".d", "10-motel.conf")))

	if _, err := os.Stat(at(socketPath)); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s is not installed: run motel notify setup first (vmalert posts to Alertmanager's socket)", socketPath)
	} else if err != nil {
		return err
	}
	motelUID, _, err := sys.lookupUser("motel")
	if err != nil {
		return fmt.Errorf("the motel user: %w (is motel installed?)", err)
	}
	if err := sys.run("systemctl", "mask", vmService); err != nil {
		return err
	}
	say("masked %s, so the install starts nothing", vmService)
	defer putBackOnFailure(sys, say, rerun, vmService, []string{vmSocket}, []string{vmService, vmalertUnit}, &err)()
	// One installed by hand before setup runs on Debian's 0.0.0.0:8428,
	// and the mask does not stop it. Stopped here, it next starts in the
	// namespace, with the drop-in.
	if err := sys.run("systemctl", "stop", vmService); err != nil {
		return err
	}
	var pkgs []string
	for _, p := range []string{vmPackage, ipPackage} {
		if sys.installed(p) {
			say("%s is installed", p)
		} else {
			pkgs = append(pkgs, p)
		}
	}
	if len(pkgs) > 0 {
		if err := sys.run("apt-get", append([]string{"install", "-y", "--no-install-recommends"}, pkgs...)...); err != nil {
			return err
		}
		say("installed %v", pkgs)
	}
	vmGID, err := sys.lookupGroup(vmUser)
	if err != nil {
		return fmt.Errorf("the %s group, which %s makes: %w", vmUser, vmPackage, err)
	}

	for _, f := range metricsUnits() {
		if err := os.MkdirAll(filepath.Dir(at(f.path)), 0o755); err != nil { //nolint:gosec // G301: systemd's unit directories are world-readable
			return err
		}
		if err := writeFileAtomic(at(f.path), f.data, 0o644); err != nil {
			return err
		}
		say("wrote %s", f.path)
	}

	// The state directory: motel's, setgid _victoria-metrics, so what
	// motel writes is born readable by the store and vmalert and by no
	// one else.
	dir := at(DefaultMetricsStateDir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil { //nolint:gosec // G301: /var/lib, which exists on a box
		return err
	}
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := sys.chown(dir, motelUID, vmGID); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o750|fs.ModeSetgid); err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
		keep bool // kept if there: motel's own render
	}{{scrapeName, scrapeConfig, false}, {RulesName, renderRules(nil), true}}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if _, err := os.Lstat(p); f.keep && err == nil {
			continue
		}
		if err := writeFileAtomic(p, f.data, 0o640); err != nil {
			return err
		}
		if err := sys.chown(p, motelUID, vmGID); err != nil {
			return err
		}
		say("wrote %s", filepath.Join(DefaultMetricsStateDir, f.name))
	}

	for _, step := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "unmask", vmService},
		{"systemctl", "enable", "--now", netnsUnit, vmService, vmSocket, amProxySock, vmalertUnit},
		// A re-run's units are the ones just written: the store was
		// stopped above, so its start took the drop-in; vmalert and the
		// two proxies, which a push keeps alive, are restarted (the
		// proxies' sockets hold their listeners meanwhile).
		{"systemctl", "restart", vmalertUnit},
		{"systemctl", "try-restart", "motel-vm.service", "motel-am-proxy.service"},
	} {
		if err := sys.run(step[0], step[1:]...); err != nil {
			return err
		}
	}
	say("the store listens on %s only, its TCP in the motel-metrics namespace", DefaultStoreSocket)
	if err := sys.run("systemctl", "try-reload-or-restart", "motel.service"); err != nil {
		return err
	}
	say("reloaded motel, which renders the store's rules into %s and pushes", filepath.Join(DefaultMetricsStateDir, RulesName))
	return nil
}
