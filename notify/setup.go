package notify

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
)

// The files `motel monitor setup` writes, which ship in the binary: the
// package installs nothing of notify's until root asks.
var (
	//go:embed units/motel-am.socket
	unitSocket []byte
	//go:embed units/10-motel.conf
	unitDropIn []byte
	//go:embed units/alertmanager.yml
	defaultConfig []byte
)

const (
	amPackage  = "prometheus-alertmanager"
	amService  = "prometheus-alertmanager.service"
	amSocket   = "motel-am.socket"
	amStorage  = "/var/lib/prometheus/alertmanager"
	dropInPath = "/etc/systemd/system/prometheus-alertmanager.service.d/10-motel.conf"
	socketPath = "/etc/systemd/system/motel-am.socket"
)

// system is what setup does to the host, so a test can watch it do so.
type system interface {
	run(name string, args ...string) error
	installed(pkg string) bool
	chown(path string, uid, gid int) error
	lookupUser(name string) (uid, gid int, err error)
	lookupGroup(name string) (gid int, err error)
	// loadedMonitor is the running motel's monitor app, as its admin API
	// answers after setup's reload: nil for a config with none, an error
	// when motel does not answer.
	loadedMonitor() ([]byte, error)
}

// isFile reports a regular file at p.
func isFile(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// maskSaid is a setup's line for its mask of service: the install's sake
// when it installs, and on a run that installs nothing (a re-run) the
// units it rewrites, which the service is stopped for until the unmask.
func maskSaid(service string, installs bool) string {
	if installs {
		return "masked " + service + ", so the install starts nothing"
	}
	return "masked " + service + ", so nothing starts it while its units are written"
}

// putBackOnFailure is deferred by a setup right after it masks service,
// before the stop. A setup masks and stops the daemon before any step
// that can fail (an install, a unit written to a full disk), and an
// error would leave a stack that worked masked and stopped until a
// later run got through. So on a re-run (motel's drop-in was in place
// when setup began) a failure unmasks the service and starts sockets:
// a connection that arrived while the service was masked fails its
// activation socket, which then starts nothing. daemons are started
// too if the service was running. Every step is tried, whatever the one
// before it did. A first run that fails before its own unmask is left
// masked: until the drop-in is written, Debian's unit listens beyond
// the box. One that fails after it is left as far as it got, every unit
// and drop-in already written, so what runs is motel's confined shape;
// the next run is a re-run (DESIGN-metrics.md, "Accepted residuals").
// then, if not nil, runs last: the store's setup gives Alertmanager back
// its listener in the namespace there.
func putBackOnFailure(sys system, say func(string, ...any), rerun bool, service string, sockets, daemons []string, then func() error, err *error) func() {
	if !rerun {
		return func() {}
	}
	start := sockets
	if len(daemons) > 0 && sys.run("systemctl", "is-active", "--quiet", service) == nil {
		start = append(slices.Clone(sockets), daemons...)
	}
	return func() {
		if *err == nil {
			return
		}
		steps := [][]string{{"daemon-reload"}, {"unmask", service}, append([]string{"start"}, start...)}
		var failed []string
		for _, s := range steps {
			if rerr := sys.run("systemctl", s...); rerr != nil {
				failed = append(failed, fmt.Sprintf("systemctl %s: %v", strings.Join(s, " "), rerr))
			}
		}
		if then != nil {
			if rerr := then(); rerr != nil {
				failed = append(failed, rerr.Error())
			}
		}
		if len(failed) > 0 {
			say("setup failed, and so did putting %s back as it was (%s): check that it is unmasked and running, and each unit named there", service, strings.Join(failed, "; "))
			return
		}
		say("setup failed; %s is unmasked again, and %s started", service, strings.Join(start, " and "))
	}
}

type hostSystem struct{ out io.Writer }

func (h hostSystem) run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed commands, fixed arguments
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
	}
	return nil
}

func (hostSystem) installed(pkg string) bool {
	out, err := exec.Command("dpkg-query", "-W", "-f=${Status}", pkg).Output() //nolint:gosec // a fixed package name
	return err == nil && strings.TrimSpace(string(out)) == "install ok installed"
}

func (hostSystem) chown(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }

func (hostSystem) lookupUser(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	return uid, gid, err
}

func (hostSystem) lookupGroup(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// loadedWait bounds setup's read of the running config: Caddy's admin
// client has no deadline of its own, and a motel holding a load would
// otherwise keep setup waiting after its work is done.
const loadedWait = 10 * time.Second

func (hostSystem) loadedMonitor() ([]byte, error) {
	type answer struct {
		monitor []byte
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		resp, err := caddycmd.AdminAPIRequest(defaultAdmin, http.MethodGet, "/config/apps", nil, nil)
		if err != nil {
			done <- answer{nil, err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var apps map[string]json.RawMessage
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&apps); err != nil {
			done <- answer{nil, err}
			return
		}
		if m := apps[appID]; len(m) > 0 && string(m) != "null" {
			done <- answer{m, nil}
			return
		}
		done <- answer{nil, nil}
	}()
	select {
	case a := <-done:
		return a.monitor, a.err
	case <-time.After(loadedWait):
		return nil, fmt.Errorf("no answer within %s", loadedWait)
	}
}

// loadedSays is what setup says once it has reloaded motel, by what the
// running config's monitor block holds (sys.loadedMonitor): it renders
// or pushes, or it has no such line and setup says what to add, or motel
// does not answer.
func loadedSays(sys system, metrics bool) string {
	raw, err := sys.loadedMonitor()
	if err != nil {
		if metrics {
			return "motel did not answer on its admin socket, " + defaultAdmin + " (" + err.Error() + "): it is not running, or its Caddyfile sets another admin address or none; once it runs a monitor block with a metrics line, it renders the store's rules and pushes"
		}
		return "motel did not answer on its admin socket, " + defaultAdmin + " (" + err.Error() + "): it is not running, or its Caddyfile sets another admin address or none; once it runs a monitor block with channels, it renders them into " + filepath.Join(DefaultStateDir, ConfigName)
	}
	var a App
	has := raw != nil && json.Unmarshal(raw, &a) == nil
	switch {
	case metrics && has && a.Metrics != nil:
		return "reloaded motel, which renders the store's rules into " + filepath.Join(DefaultMetricsStateDir, RulesName) + " and pushes"
	case metrics:
		return "reloaded motel, whose config has no monitor block with a metrics line: nothing is pushed until there is one (docs/monitor.md); add it, then systemctl reload motel"
	case has && !a.storeAlone():
		return "reloaded motel, which renders the monitor block's channels into " + filepath.Join(DefaultStateDir, ConfigName)
	}
	return "reloaded motel, whose config has no monitor block with channels: Alertmanager sends nothing until there is one (docs/monitor.md); add it, then systemctl reload motel"
}

// setup is `motel monitor setup`: Debian's Alertmanager, installed so
// that it never listens on a port, even for a moment. Debian's unit
// starts on install on :9093 and gossips on 0.0.0.0:9094, and a host
// with no firewall would put both on the internet; a `Depends` cannot
// close that window, since apt may configure the dependency, and start
// it, before motel's own scripts run (DESIGN-notify.md, "Packaging and
// setup"). So the order is root's, here: mask the unit, install,
// write the drop-in, the socket and the state directory with a config
// Alertmanager can start from, unmask, leave the service to socket
// activation, start the socket, and have motel render.
//
// root is a prefix for every path written (tests); the commands run as
// given. Each step says what it did on out.
func setup(sys system, root string, out io.Writer) (err error) {
	at := func(p string) string { return filepath.Join(root, p) }
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format+"\n", a...) }
	rerun := isFile(at(dropInPath))

	motelUID, _, err := sys.lookupUser("motel")
	if err != nil {
		return fmt.Errorf("the motel user: %w (is motel installed?)", err)
	}
	// Asked before the mask, so that the mask's line names its reason.
	installs := !sys.installed(amPackage)
	if err := sys.run("systemctl", "mask", amService); err != nil {
		return err
	}
	say("%s", maskSaid(amService, installs))
	// Its socket starts it again at the next connection, once unmasked.
	defer putBackOnFailure(sys, say, rerun, amService, []string{amSocket}, nil, nil, &err)()
	// One installed by hand before setup runs on Debian's :9093 and
	// 0.0.0.0:9094, and the mask does not stop it; nor would the socket
	// be handed to a service already running. Stopped here, it next
	// starts from its socket, with the drop-in.
	if err := sys.run("systemctl", "stop", amService); err != nil {
		return err
	}
	if !installs {
		say("%s is installed", amPackage)
	} else {
		if err := sys.run("apt-get", "install", "-y", "--no-install-recommends", amPackage); err != nil {
			return err
		}
		say("installed %s", amPackage)
	}
	promGID, err := sys.lookupGroup("prometheus")
	if err != nil {
		return fmt.Errorf("the prometheus group, which %s makes: %w", amPackage, err)
	}
	promUID, _, err := sys.lookupUser("prometheus")
	if err != nil {
		return fmt.Errorf("the prometheus user, which %s makes: %w", amPackage, err)
	}

	for _, f := range amUnits() {
		if err := os.MkdirAll(filepath.Dir(at(f.path)), 0o755); err != nil { //nolint:gosec // G301: systemd's unit directories are world-readable
			return err
		}
		if err := writeFileAtomic(at(f.path), f.data, 0o644); err != nil {
			return err
		}
		say("wrote %s", f.path)
	}

	// The state directory: motel's, setgid prometheus, so what motel
	// writes is born readable by Alertmanager and by no one else.
	dir := at(DefaultStateDir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil { //nolint:gosec // G301: /var/lib, which exists on a box
		return err
	}
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := sys.chown(dir, motelUID, promGID); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o750|fs.ModeSetgid); err != nil {
		return err
	}
	cfg := filepath.Join(dir, ConfigName)
	if _, err := os.Lstat(cfg); errors.Is(err, fs.ErrNotExist) {
		if err := writeFileAtomic(cfg, defaultConfig, 0o640); err != nil {
			return err
		}
		if err := sys.chown(cfg, motelUID, promGID); err != nil {
			return err
		}
		say("wrote %s, a config that sends nothing, until motel renders the monitor block's channels", filepath.Join(DefaultStateDir, ConfigName))
	}
	// Its mode is the drop-in's, set at every start (Debian ships 0755).
	if err := os.MkdirAll(at(amStorage), 0o750); err != nil {
		return err
	}
	if err := sys.chown(at(amStorage), promUID, promGID); err != nil {
		return err
	}

	for _, step := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "unmask", amService},
		// Started only by its socket: enabled as Debian leaves it, it
		// would also start at boot before anything asked.
		{"systemctl", "disable", amService},
		{"systemctl", "enable", "--now", amSocket},
	} {
		if err := sys.run(step[0], step[1:]...); err != nil {
			return err
		}
	}
	say("Alertmanager listens on %s only, started by its first connection", DefaultSocket)
	if err := sys.run("systemctl", "try-reload-or-restart", "motel.service"); err != nil {
		return err
	}
	say("%s", loadedSays(sys, false))
	return nil
}

func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
