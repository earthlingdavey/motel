package notify

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The files `motel monitor setup --metrics` writes, which ship in the
// binary: the package installs nothing of the store's until root asks.
var (
	//go:embed units/metrics/motel-metrics-netns.service
	unitNetns []byte
	//go:embed units/metrics/10-motel.conf
	unitVMDropIn []byte
	//go:embed units/metrics/motel-vm.socket
	unitVMSocket []byte
	//go:embed units/metrics/motel-vm.service
	unitVMProxy []byte
	//go:embed units/metrics/motel-am-metrics.socket
	unitAMMetricsSocket []byte
	//go:embed units/metrics/20-motel-metrics.conf
	unitAMMetricsDropIn []byte
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
	amNSSocket  = "motel-am-metrics.socket"
	vmalertUnit = "motel-vmalert.service"
	scrapeName  = "scrape.yml"
)

// The units of the shape before Alertmanager held its own listener in
// the namespace: a socket on 127.0.0.1:9093 there, and a proxy from it to
// Alertmanager's unix socket. Setup retires them, the socket first.
const (
	oldAMProxySocket  = "motel-am-proxy.socket"
	oldAMProxyService = "motel-am-proxy.service"
)

// oldAMProxyUnits are both, in the order setup retires them; the load's
// warning (LoadWarnings) names those still there from the same list.
var oldAMProxyUnits = []string{oldAMProxySocket, oldAMProxyService}

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
		{filepath.Join(unitDir, amNSSocket), unitAMMetricsSocket},
		{filepath.Join(unitDir, amService+".d", "20-motel-metrics.conf"), unitAMMetricsDropIn},
		{filepath.Join(unitDir, vmalertUnit), unitVMAlert},
	}
}

// metricsSetup is the store's part of `motel monitor setup --metrics`,
// after Alertmanager's (runSetup): Debian's victoria-metrics, installed
// so that it never listens in the host's namespace, even for a moment. Debian's unit starts on install on 0.0.0.0:8428 with no
// authentication (DESIGN-metrics.md, "Packaging and setup"), so the
// order is root's, as for notify's: mask, install, write the units and
// the state directory, retire the proxy shape's units, unmask, restart
// Alertmanager once unless its socket in the namespace already listens,
// start the namespace, the store, both sockets and vmalert, and have
// motel render. It refuses unless Alertmanager's setup has run:
// vmalert's outlet is Alertmanager.
//
// root is a prefix for every path written (tests); the commands run as
// given. Each step says what it did on out.
func metricsSetup(sys system, root string, out io.Writer) (err error) {
	at := func(p string) string { return filepath.Join(root, p) }
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format+"\n", a...) }
	rerun := isFile(at(filepath.Join(unitDir, vmService+".d", "10-motel.conf")))

	if _, err := os.Stat(at(socketPath)); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s is missing: Alertmanager's setup has not run, and vmalert's outlet is Alertmanager", socketPath)
	} else if err != nil {
		return err
	}
	motelUID, _, err := sys.lookupUser("motel")
	if err != nil {
		return fmt.Errorf("the motel user: %w (is motel installed?)", err)
	}
	// Which packages are there, asked before the mask, so that the mask's
	// line names its reason: the store's install, or none (a re-run, or
	// iproute2 alone missing).
	var pkgs, have []string
	for _, p := range []string{vmPackage, ipPackage} {
		if sys.installed(p) {
			have = append(have, p)
		} else {
			pkgs = append(pkgs, p)
		}
	}
	if err := sys.run("systemctl", "mask", vmService); err != nil {
		return err
	}
	say("%s", maskSaid(vmService, slices.Contains(pkgs, vmPackage)))
	// Set when the proxy shape's port is free (every unit is written by
	// then): from there on a failure may leave vmalert no outlet unless
	// Alertmanager takes up its socket in the namespace.
	retired := false
	defer putBackOnFailure(sys, say, rerun, vmService, []string{vmSocket}, []string{vmService, vmalertUnit}, func() error {
		if !retired {
			return nil
		}
		return amListenInNamespace(sys, say)
	}, &err)()
	// One installed by hand before setup runs on Debian's 0.0.0.0:8428,
	// and the mask does not stop it. Stopped here, it next starts in the
	// namespace, with the drop-in.
	if err := sys.run("systemctl", "stop", vmService); err != nil {
		return err
	}
	for _, p := range have {
		say("%s is installed", p)
	}
	if len(pkgs) > 0 {
		if err := sys.run("apt-get", append([]string{"install", "-y", "--no-install-recommends"}, pkgs...)...); err != nil {
			return err
		}
		say("installed %s", strings.Join(pkgs, " "))
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

	// The proxy shape's socket holds 127.0.0.1:9093 in the namespace,
	// which Alertmanager's own socket binds next: it goes first, before
	// the reload, with its proxy.
	// Each by its own file, so a retirement cut short (a remove that
	// failed) is finished by the next run. The put-back's restart is
	// armed once the port is free: with no proxy socket, or once it is
	// stopped, not while it still carries vmalert's posts.
	retired = !isFile(at(filepath.Join(unitDir, oldAMProxySocket)))
	var gone []string
	for _, u := range oldAMProxyUnits {
		if !isFile(at(filepath.Join(unitDir, u))) {
			continue
		}
		if err := sys.run("systemctl", "disable", "--now", u); err != nil {
			return err
		}
		retired = true
		if err := os.Remove(at(filepath.Join(unitDir, u))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		gone = append(gone, u)
	}
	if len(gone) > 0 {
		say("retired %s: Alertmanager listens in the namespace itself", strings.Join(gone, " and "))
	}
	for _, step := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "unmask", vmService},
	} {
		if err := sys.run(step[0], step[1:]...); err != nil {
			return err
		}
	}
	// Before the namespace's own start: a first run starts it within
	// Alertmanager's restart, whose start waits for it.
	if err := amListenInNamespace(sys, say); err != nil {
		return err
	}
	for _, step := range [][]string{
		{"systemctl", "enable", "--now", netnsUnit, vmService, vmSocket, amNSSocket, vmalertUnit},
		// A re-run's units are the ones just written: the store was
		// stopped above, so its start took the drop-in; vmalert and the
		// store's proxy, which a push keeps alive, are restarted (the
		// proxy's socket holds its listener meanwhile).
		{"systemctl", "restart", vmalertUnit},
		{"systemctl", "try-restart", "motel-vm.service"},
	} {
		if err := sys.run(step[0], step[1:]...); err != nil {
			return err
		}
	}
	say("the store listens on %s only, its TCP in the motel-metrics namespace", DefaultStoreSocket)
	if err := sys.run("systemctl", "try-reload-or-restart", "motel.service"); err != nil {
		return err
	}
	say("%s", loadedSays(sys, true))
	return nil
}

// amListenInNamespace restarts Alertmanager, once, unless its socket in
// the metrics namespace (amNSSocket) is active. Alertmanager is handed
// its sockets only when it starts, and systemd refuses to start a socket
// whose service runs, so a running Alertmanager takes the socket up only
// by a restart: one `systemctl restart`, whose transaction starts the
// socket (and the namespace) before Alertmanager (the drop-in's Wants=
// and After=). That is a first run, an upgrade from the proxy shape, and
// an Alertmanager left on its unix socket alone; a re-run with the socket
// listening leaves it as it runs. A restart loses what it holds in
// memory: motel posts each firing state again within a minute.
func amListenInNamespace(sys system, say func(string, ...any)) error {
	if sys.run("systemctl", "is-active", "--quiet", amNSSocket) == nil {
		say("Alertmanager listens in the namespace already (%s): left as it runs", amNSSocket)
		return nil
	}
	// Not running (its socket starts it at the first connection), the
	// restart is a start, and nothing it held is lost: said so.
	did := "restarted"
	if sys.run("systemctl", "is-active", "--quiet", amService) != nil {
		did = "started"
	}
	if err := sys.run("systemctl", "restart", amService); err != nil {
		return err
	}
	say("%s Alertmanager, once, so that it listens in the namespace (%s) beside its unix socket", did, amNSSocket)
	return nil
}
