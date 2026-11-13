package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.yaml.in/yaml/v3"
)

// notifySetUp makes root look like a box where Alertmanager's setup ran.
func notifySetUp(t *testing.T, root string) {
	t.Helper()
	p := filepath.Join(root, socketPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, unitSocket, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsSetupNeedsNotify(t *testing.T) {
	f := &fakeSystem{}
	err := metricsSetup(f, t.TempDir(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "/etc/systemd/system/motel-am.socket is missing: Alertmanager's setup has not run") {
		t.Fatalf("err = %v", err)
	}
	if len(f.cmds) != 0 {
		t.Fatalf("ran %v before refusing", f.cmds)
	}
}

func TestMetricsSetupOrderAndFiles(t *testing.T) {
	root := t.TempDir()
	notifySetUp(t, root)
	// A first run: Alertmanager's socket in the namespace does not listen
	// yet, so Alertmanager is restarted, once, to take it up, before the
	// namespace's own start (the restart starts it).
	f := &fakeSystem{failOn: "systemctl is-active --quiet " + amNSSocket}
	var out bytes.Buffer
	if err := metricsSetup(f, root, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl mask victoria-metrics.service",
		"systemctl stop victoria-metrics.service",
		"apt-get install -y --no-install-recommends victoria-metrics iproute2",
		"systemctl daemon-reload",
		"systemctl unmask victoria-metrics.service",
		"systemctl is-active --quiet motel-am-metrics.socket",
		"systemctl is-active --quiet prometheus-alertmanager.service",
		"systemctl restart prometheus-alertmanager.service",
		"systemctl enable --now motel-metrics-netns.service victoria-metrics.service motel-vm.socket motel-am-metrics.socket motel-vmalert.service",
		"systemctl restart motel-vmalert.service",
		"systemctl try-restart motel-vm.service",
		"systemctl try-reload-or-restart motel.service",
	}
	if !slices.Equal(f.cmds, want) {
		t.Fatalf("commands:\n%s\nwant\n%s", strings.Join(f.cmds, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out.String(), "restarted Alertmanager, once, so that it listens in the namespace (motel-am-metrics.socket)") {
		t.Errorf("setup did not say it restarted Alertmanager:\n%s", out.String())
	}
	// The packages it installed by name, not as a Go slice.
	if !strings.Contains(out.String(), "installed victoria-metrics iproute2\n") {
		t.Errorf("setup said:\n%s", out.String())
	}
	if !strings.HasPrefix(out.String(), "masked victoria-metrics.service, so the install starts nothing\n") {
		t.Errorf("a first run's mask: setup said\n%s", out.String())
	}
	for _, u := range metricsUnits() {
		b, err := os.ReadFile(filepath.Join(root, u.path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b, []byte("# Written by `motel monitor setup --metrics`")) {
			t.Errorf("%s = %q", u.path, b[:40])
		}
	}
	dir := filepath.Join(root, DefaultMetricsStateDir)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&fs.ModeSetgid == 0 || st.Mode().Perm() != 0o750 {
		t.Errorf("state dir mode = %v", st.Mode())
	}
	if f.owners[dir] != [2]int{986, 103} {
		t.Errorf("state dir owner = %v, want motel:_victoria-metrics", f.owners[dir])
	}
	for _, name := range []string{scrapeName, RulesName} {
		p := filepath.Join(dir, name)
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o640 || f.owners[p] != [2]int{986, 103} {
			t.Errorf("%s: %v %v, owner %v", name, st, err, f.owners[p])
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, RulesName)); !bytes.Equal(b, renderRules(nil)) {
		t.Error("the first rules are not the empty groups")
	}

	// Again, on a box set up: nothing installed, motel's rules kept,
	// the scrapes put back.
	if err := os.WriteFile(filepath.Join(dir, RulesName), []byte("rendered\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, scrapeName), []byte("edited\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Alertmanager listens in the namespace now: left as it runs.
	f1 := &fakeSystem{isInstalled: true}
	out.Reset()
	if err := metricsSetup(f1, root, &out); err != nil {
		t.Fatal(err)
	}
	if f1.cmds[0] != "systemctl mask victoria-metrics.service" || slices.ContainsFunc(f1.cmds, func(c string) bool { return strings.HasPrefix(c, "apt-get") }) {
		t.Fatalf("re-run: %v", f1.cmds)
	}
	// It installs nothing, so its mask is not said to be for an install
	// (the deploy of a8038b4 to hotserve-1).
	if !strings.HasPrefix(out.String(), "masked victoria-metrics.service, so nothing starts it while its units are written\nvictoria-metrics is installed\niproute2 is installed\n") ||
		strings.Contains(out.String(), "install starts nothing") {
		t.Errorf("a re-run's mask: setup said\n%s", out.String())
	}
	if slices.ContainsFunc(f1.cmds, func(c string) bool { return strings.Contains(c, amService) }) ||
		!strings.Contains(out.String(), "Alertmanager listens in the namespace already (motel-am-metrics.socket): left as it runs") {
		t.Fatalf("re-run with Alertmanager listening in the namespace: %v\n%s; want it left as it runs", f1.cmds, out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, RulesName)); string(b) != "rendered\n" {
		t.Fatalf("setup replaced motel's rules: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, scrapeName)); !bytes.Equal(b, scrapeConfig) {
		t.Fatalf("setup left an edited scrape.yml: %q", b)
	}

	// iproute2 alone to install: the mask is for the units, not for an
	// install of the store.
	fIP := &fakeSystem{missing: []string{ipPackage}}
	out.Reset()
	if err := metricsSetup(fIP, root, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "masked victoria-metrics.service, so nothing starts it while its units are written\nvictoria-metrics is installed\ninstalled iproute2\n") {
		t.Errorf("iproute2 alone missing: setup said\n%s", out.String())
	}
	if i := slices.IndexFunc(fIP.cmds, func(c string) bool { return strings.HasPrefix(c, "apt-get") }); i < 0 ||
		fIP.cmds[i] != "apt-get install -y --no-install-recommends iproute2" {
		t.Errorf("iproute2 alone missing: %v; want it installed alone", fIP.cmds)
	}
}

// A re-run that fails after the mask (here the units' reload) must not
// leave a store that worked masked and stopped; a first run that fails
// stays masked, since Debian's unit without the drop-in listens wide.
func TestMetricsSetupFailingPutsTheStoreBack(t *testing.T) {
	root := t.TempDir()
	notifySetUp(t, root)
	first := &fakeSystem{failOn: "systemctl daemon-reload"}
	if err := metricsSetup(first, root, &bytes.Buffer{}); err == nil {
		t.Fatal("the first run did not fail")
	}
	if slices.Contains(first.cmds, "systemctl unmask victoria-metrics.service") {
		t.Fatalf("a failed first run unmasked the store: %v", first.cmds)
	}
	// The drop-in is on disk now: this run is a re-run.
	f := &fakeSystem{isInstalled: true, failOn: "systemctl daemon-reload"}
	var out bytes.Buffer
	if err := metricsSetup(f, root, &out); err == nil {
		t.Fatal("the re-run did not fail")
	}
	want := []string{
		"systemctl mask victoria-metrics.service",
		"systemctl is-active --quiet victoria-metrics.service",
		"systemctl stop victoria-metrics.service",
		"systemctl daemon-reload", // fails
		"systemctl daemon-reload", // and again: the rest is still tried
		"systemctl unmask victoria-metrics.service",
		"systemctl start motel-vm.socket victoria-metrics.service motel-vmalert.service",
		// Past the retirement of the proxy shape: Alertmanager's socket
		// in the namespace is checked, here listening.
		"systemctl is-active --quiet motel-am-metrics.socket",
	}
	if !slices.Equal(f.cmds, want) {
		t.Fatalf("commands:\n%s\nwant\n%s", strings.Join(f.cmds, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out.String(), "systemctl daemon-reload: exit status 1") || !strings.Contains(out.String(), "check that it is unmasked and running") {
		t.Fatalf("setup did not say it put the store back: %s", out.String())
	}
	// The put-back getting through: a re-run whose install fails. And a
	// store that was not running is not started, only its socket.
	for _, c := range []struct{ failOn, start string }{
		{"apt-get", "systemctl start motel-vm.socket victoria-metrics.service motel-vmalert.service"},
		{"apt-get|systemctl is-active", "systemctl start motel-vm.socket"},
	} {
		f := &fakeSystem{failOn: c.failOn}
		out.Reset()
		if err := metricsSetup(f, root, &out); err == nil {
			t.Fatalf("failOn %q: the re-run did not fail", c.failOn)
		}
		n := len(f.cmds)
		if n < 3 || !slices.Equal(f.cmds[n-3:], []string{"systemctl daemon-reload", "systemctl unmask victoria-metrics.service", c.start}) {
			t.Errorf("failOn %q: commands %v", c.failOn, f.cmds)
		}
		if !strings.Contains(out.String(), "victoria-metrics.service is unmasked again") {
			t.Errorf("failOn %q: setup said %s", c.failOn, out.String())
		}
	}
}

// A box set up with the proxy shape (motel-am-proxy.socket on the
// namespace's 9093, its proxy to Alertmanager's unix socket): the
// re-run disables and removes both before the reload, so the port is
// free for Alertmanager's own socket, then restarts Alertmanager once.
// A failure after that gives Alertmanager its socket back in the
// put-back, so vmalert is not left with no outlet.
func TestMetricsSetupRetiresTheProxyShape(t *testing.T) {
	plant := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		notifySetUp(t, root)
		for _, p := range []string{
			filepath.Join(unitDir, vmService+".d", "10-motel.conf"),
			filepath.Join(unitDir, oldAMProxySocket),
			filepath.Join(unitDir, oldAMProxyService),
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, p), []byte("# the proxy shape's\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	notListening := "systemctl is-active --quiet " + amNSSocket
	root := plant(t)
	f := &fakeSystem{isInstalled: true, failOn: notListening}
	var out bytes.Buffer
	if err := metricsSetup(f, root, &out); err != nil {
		t.Fatal(err)
	}
	sock := slices.Index(f.cmds, "systemctl disable --now motel-am-proxy.socket")
	retire := slices.Index(f.cmds, "systemctl disable --now motel-am-proxy.service")
	if sock < 0 || retire < sock {
		t.Fatalf("commands: %v; want the proxy's socket disabled, then its service", f.cmds)
	}
	reload := slices.Index(f.cmds, "systemctl daemon-reload")
	restart := slices.Index(f.cmds, "systemctl restart "+amService)
	enable := slices.IndexFunc(f.cmds, func(c string) bool { return strings.HasPrefix(c, "systemctl enable --now ") })
	if retire < 0 || reload < retire || restart < reload || enable < restart {
		t.Fatalf("commands: %v; want the proxy's units disabled, then the reload, Alertmanager's restart and the enable", f.cmds)
	}
	n := 0
	for _, c := range acts(f.cmds) {
		if strings.Contains(c, amService) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Alertmanager in %d commands, want its one restart: %v", n, f.cmds)
	}
	for _, u := range oldAMProxyUnits {
		if _, err := os.Lstat(filepath.Join(root, unitDir, u)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there: %v", u, err)
		}
	}
	if !strings.Contains(out.String(), "retired motel-am-proxy.socket and motel-am-proxy.service") {
		t.Errorf("setup said:\n%s", out.String())
	}

	// The same upgrade failing at the enable: the put-back restarts
	// Alertmanager, its socket in the namespace not listening.
	root = plant(t)
	f = &fakeSystem{isInstalled: true, failOn: notListening + "|systemctl enable --now"}
	out.Reset()
	if err := metricsSetup(f, root, &out); err == nil {
		t.Fatal("the re-run did not fail")
	}
	n = len(f.cmds)
	if want := []string{"systemctl unmask victoria-metrics.service", "systemctl start motel-vm.socket victoria-metrics.service motel-vmalert.service",
		notListening, "systemctl is-active --quiet " + amService, "systemctl restart " + amService}; n < 5 || !slices.Equal(f.cmds[n-5:], want) {
		t.Errorf("put-back: %v; want it to end %v", f.cmds, want)
	}
	if !strings.Contains(out.String(), "victoria-metrics.service is unmasked again") {
		t.Errorf("setup said:\n%s", out.String())
	}

	// Failing before the proxy shape is retired (the install): the
	// proxy still holds the port, and Alertmanager is left as it runs.
	root = plant(t)
	f = &fakeSystem{failOn: notListening + "|apt-get"}
	if err := metricsSetup(f, root, &bytes.Buffer{}); err == nil {
		t.Fatal("the re-run did not fail")
	}
	if slices.ContainsFunc(f.cmds, func(c string) bool { return strings.Contains(c, amService) || strings.Contains(c, "motel-am-proxy") }) {
		t.Errorf("a failure before the retirement touched Alertmanager or the proxy: %v", f.cmds)
	}
	if !isFile(filepath.Join(root, unitDir, oldAMProxySocket)) {
		t.Error("a failure before the retirement removed the proxy's socket")
	}

	// A retirement cut short (the service's file left behind) is
	// finished by the next run (/code-review pass 1 on #435).
	root = plant(t)
	if err := os.Remove(filepath.Join(root, unitDir, oldAMProxySocket)); err != nil {
		t.Fatal(err)
	}
	f = &fakeSystem{isInstalled: true}
	out.Reset()
	if err := metricsSetup(f, root, &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.cmds, "systemctl disable --now motel-am-proxy.service") || slices.Contains(f.cmds, "systemctl disable --now motel-am-proxy.socket") ||
		isFile(filepath.Join(root, unitDir, oldAMProxyService)) || !strings.Contains(out.String(), "retired motel-am-proxy.service: ") {
		t.Errorf("the service left alone: %v\n%s; want it disabled and removed", f.cmds, out.String())
	}
}

// TestMetricsUnitsAreTheMeasuredOnes holds the shipped units to what ran
// on hotserve-1 (hotserve1_stack_test.md), with this slice's paths and
// caps; the store's proxy's user, the drop-in's chmod, the namespace
// unit's test for a leftover, its start's limit and its restart of a
// deaf Alertmanager, and Alertmanager's own socket in the namespace were
// measured in the e2e box first, then ran on hotserve-1 through a deploy
// and a cold boot; the delete of a leftover namespace in the e2e box alone
// (notify/DESIGN-metrics.md, "The shape").
func TestMetricsUnitsAreTheMeasuredOnes(t *testing.T) {
	has := func(name string, unit []byte, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(string(unit), w) {
				t.Errorf("%s lacks %q", name, w)
			}
		}
	}
	lacks := func(name string, unit []byte, keys map[string]string) {
		t.Helper()
		for _, line := range strings.Split(string(unit), "\n") {
			line = strings.TrimSpace(line)
			for key, why := range keys {
				if strings.HasPrefix(line, key) {
					t.Errorf("%s has %q, which %s", name, line, why)
				}
			}
		}
	}
	has("the drop-in", unitVMDropIn, "ExecStart=\n", "-httpListenAddr=127.0.0.1:8428 ", "NetworkNamespacePath=/run/netns/motel-metrics",
		"MemoryMax=256M", "-memory.allowedBytes=96MiB", "-retentionPeriod=14d", "-promscrape.config="+DefaultMetricsStateDir+"/"+scrapeName,
		"Requires=motel-metrics-netns.service",
		// The store's start brings vmalert, which a stop of the store stopped.
		"Wants=motel-vmalert.service",
		// Debian's 0755 would let every local uid read the series.
		"ExecStartPre=+/usr/bin/chmod 0750 /var/lib/victoria-metrics\n")
	// A namespace left behind must not fail every later start, and a
	// cold boot, which has none, must log no error for it.
	has("the namespace unit", unitNetns, "ExecStartPre=-/bin/sh -c 'test ! -e /run/netns/motel-metrics || exec /usr/sbin/ip netns delete motel-metrics'\nExecStart=/usr/sbin/ip netns add motel-metrics",
		// A oneshot has no start limit of its own, and Alertmanager's
		// start waits for this one.
		"TimeoutStartSec=30\n",
		// An Alertmanager that started while the namespace could not
		// holds no listener in it: the namespace's start restarts it.
		"ExecStartPost=-/usr/bin/systemctl --no-block try-restart prometheus-alertmanager.service\n",
		"Before=victoria-metrics.service motel-vmalert.service motel-am-metrics.socket motel-vm.service\n")
	has("motel-vm.socket", unitVMSocket, "ListenStream="+DefaultStoreSocket+"\n", "SocketGroup=motel", "SocketMode=0660")
	has("motel-vm.service", unitVMProxy, "NetworkNamespacePath=/run/netns/motel-metrics", "systemd-socket-proxyd --exit-idle-time=5min 127.0.0.1:8428",
		// Not root (the box audit of 2026-11-11).
		"DynamicUser=yes", "CapabilityBoundingSet=\n")
	// The cold-boot ordering cycle's fix, and the socket handed to
	// Alertmanager itself.
	has(amNSSocket, unitAMMetricsSocket, "DefaultDependencies=no", "Requires=motel-metrics-netns.service\n", "After=sysinit.target motel-metrics-netns.service\n",
		"Service=prometheus-alertmanager.service\n", "ListenStream=127.0.0.1:9093\n", "NetworkNamespacePath=/run/netns/motel-metrics\n", "WantedBy=multi-user.target")
	lacks(amNSSocket, unitAMMetricsSocket, map[string]string{
		// Alertmanager serves every listener it is handed and reads no
		// name; the order of the fds varies (measured, 0.28.1).
		"FileDescriptorName=": "names a socket Alertmanager does not read",
	})
	// After=, so that the socket listens before Alertmanager starts;
	// Wants=, so that a namespace that fails leaves notify on its unix
	// socket; PartOf=, so that the namespace's restart restarts it.
	has("Alertmanager's drop-in for the namespace", unitAMMetricsDropIn, "Wants=motel-am-metrics.socket\n", "After=motel-am-metrics.socket\n", "PartOf=motel-am-metrics.socket\n")
	lacks("Alertmanager's drop-in for the namespace", unitAMMetricsDropIn, map[string]string{
		// Measured: with the namespace failing, motel-am.socket hit its
		// trigger limit and notify stopped.
		"BindsTo=":  "takes notify down with the namespace",
		"Requires=": "keeps Alertmanager from starting while the namespace fails",
	})
	has("motel-vmalert.service", unitVMAlert, "User=_victoria-metrics", "NetworkNamespacePath=/run/netns/motel-metrics",
		"-rule="+DefaultMetricsStateDir+"/"+RulesName+" ", "-configCheckInterval=5s", "-rule.evalDelay=5s", "-disableAlertgroupLabel",
		"-notifier.url=http://127.0.0.1:9093", "-remoteWrite.url=http://127.0.0.1:8428", "-remoteRead.url=http://127.0.0.1:8428", "MemoryMax=128M",
		"After=motel-metrics-netns.service victoria-metrics.service motel-am-metrics.socket\n",
		// The namespace's start does not start Alertmanager's socket in
		// it again: vmalert's start does (measured, Copilot on #435).
		"Wants=motel-am-metrics.socket\n")
	if strings.Contains(string(unitVMDropIn), "0.0.0.0") || strings.Contains(string(unitVMDropIn), "-httpListenAddr=:") {
		t.Error("the drop-in listens beyond the namespace's loopback")
	}
	has("scrape.yml", scrapeConfig, `job_name: vmalert`, `"127.0.0.1:8880"`, `job_name: alertmanager`, `"127.0.0.1:9093"`)
}

// `motel monitor setup` is Alertmanager's setup alone. With --metrics
// on a box where it has not run, Alertmanager's runs first and the
// store's only once it has succeeded; where it has, Alertmanager's setup
// is not run (it would mask and stop it) and the store's runs alone, a
// re-run of it with its put-back armed, which restarts Alertmanager
// once, and only while its socket in the namespace is not listening.
func TestMonitorSetupRunsAlertmanagerOnlyWhereItIsNotSetUp(t *testing.T) {
	has := slices.Contains[[]string]
	root := t.TempDir()
	alone := &fakeSystem{}
	if err := runSetup(alone, root, &bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if !has(alone.cmds, "systemctl mask "+amService) || has(alone.cmds, "systemctl mask "+vmService) {
		t.Fatalf("without --metrics: %v; want Alertmanager's setup alone", alone.cmds)
	}
	if isFile(filepath.Join(root, unitDir, vmSocket)) {
		t.Fatal("without --metrics, the store's units were written")
	}

	both := &fakeSystem{}
	if err := runSetup(both, t.TempDir(), &bytes.Buffer{}, true); err != nil {
		t.Fatal(err)
	}
	am, vm := slices.Index(both.cmds, "systemctl mask "+amService), slices.Index(both.cmds, "systemctl mask "+vmService)
	reloads := 0
	for _, c := range both.cmds {
		if c == "systemctl try-reload-or-restart motel.service" {
			reloads++
		}
	}
	if am != 0 || vm <= am || reloads != 2 {
		t.Fatalf("with --metrics: %v; want Alertmanager's setup, then the store's, each reloading motel", both.cmds)
	}

	// Alertmanager's setup fails: the store's never starts.
	failed := &fakeSystem{failOn: "systemctl enable --now " + amSocket}
	if err := runSetup(failed, t.TempDir(), &bytes.Buffer{}, true); err == nil || has(failed.cmds, "systemctl mask "+vmService) {
		t.Fatalf("Alertmanager's setup failed: err %v, commands %v; want the error, and nothing of the store", err, failed.cmds)
	}

	// Alertmanager set up (root, from the first run above): --metrics
	// runs nothing of its setup, first run of the store's or a re-run;
	// the store's restarts it once to hand it its socket in the
	// namespace, and not when that socket listens.
	for _, c := range []struct {
		name      string
		listening bool
	}{{"its socket in the namespace not listening", false}, {"listening", true}} {
		var out bytes.Buffer
		storeOnly := &fakeSystem{isInstalled: true}
		if !c.listening {
			storeOnly.failOn = "systemctl is-active --quiet " + amNSSocket
		}
		if err := runSetup(storeOnly, root, &out, true); err != nil {
			t.Fatal(err)
		}
		var touched []string
		for _, a := range acts(storeOnly.cmds) {
			if strings.Contains(a, amService) || strings.Contains(a, amSocket) {
				touched = append(touched, a)
			}
		}
		want := []string{"systemctl restart " + amService}
		if c.listening {
			want = nil
		}
		if !slices.Equal(touched, want) {
			t.Fatalf("%s: with Alertmanager set up, --metrics ran %q of it: %v", c.name, touched, storeOnly.cmds)
		}
		if acts(storeOnly.cmds)[0] != "systemctl mask "+vmService || !strings.Contains(out.String(), "Alertmanager is set up") {
			t.Fatalf("%s: with Alertmanager set up: %v\n%s; want the store's setup alone, and a line saying why", c.name, storeOnly.cmds, out.String())
		}
	}
	again := &fakeSystem{isInstalled: true, failOn: "systemctl restart " + vmalertUnit}
	if err := runSetup(again, root, &bytes.Buffer{}, true); err == nil {
		t.Fatal("the failing re-run succeeded")
	}
	if acts(again.cmds)[0] != "systemctl mask "+vmService || has(again.cmds, "systemctl mask "+amService) ||
		!slices.ContainsFunc(again.cmds, func(c string) bool { return strings.HasPrefix(c, "systemctl start "+vmSocket) }) {
		t.Fatalf("a re-run that fails in the store's setup: %v; want the store's setup alone, put back", again.cmds)
	}
	// Without --metrics, Alertmanager's setup runs again: its re-run.
	amAgain := &fakeSystem{isInstalled: true}
	if err := runSetup(amAgain, root, &bytes.Buffer{}, false); err != nil || amAgain.cmds[0] != "systemctl mask "+amService || has(amAgain.cmds, "systemctl mask "+vmService) {
		t.Fatalf("motel monitor setup on a set-up box: %v, %v; want Alertmanager's setup alone", err, amAgain.cmds)
	}
}

// acts is what setup did to the host, its probes of systemd's state
// left out.
func acts(cmds []string) []string {
	return slices.DeleteFunc(slices.Clone(cmds), func(c string) bool {
		return strings.HasPrefix(c, "systemctl is-enabled ") || strings.HasPrefix(c, "systemctl is-active ")
	})
}

// A first Alertmanager setup that failed after it wrote its socket unit
// left the service masked or the socket not enabled: --metrics runs
// Alertmanager's setup again, its recovery, before the store's, and
// does not take the file for a working Alertmanager (Copilot round 1 on
// #428).
func TestMetricsSetupFinishesAnUnfinishedAlertmanagerSetup(t *testing.T) {
	for _, c := range []struct {
		name   string
		masked bool   // the mask link is still in place
		probe  string // the probe that fails
	}{
		{"masked", true, ""},
		{"the socket not enabled", false, "systemctl is-enabled"},
		{"the socket not listening", false, "systemctl is-active"},
	} {
		root := t.TempDir()
		first := &fakeSystem{failOn: "systemctl unmask " + amService}
		if err := runSetup(first, root, &bytes.Buffer{}, false); err == nil || !isFile(filepath.Join(root, socketPath)) {
			t.Fatalf("%s: the first setup: %v; want it failed after writing %s", c.name, err, socketPath)
		}
		if c.masked {
			if err := os.Symlink(os.DevNull, filepath.Join(root, unitDir, amService)); err != nil {
				t.Fatal(err)
			}
		}
		retry := &fakeSystem{isInstalled: true, failOn: c.probe}
		if err := runSetup(retry, root, &bytes.Buffer{}, true); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		done := acts(retry.cmds)
		am, unmask, vm := slices.Index(done, "systemctl mask "+amService), slices.Index(done, "systemctl unmask "+amService), slices.Index(done, "systemctl mask "+vmService)
		if am != 0 || unmask < 0 || vm <= unmask {
			t.Errorf("%s: --metrics after a failed first setup: %v; want Alertmanager's setup, whole, then the store's", c.name, done)
		}
	}
}

// /code-review pass 2 on #435. With notify's drop-in gone, Alertmanager
// would run Debian's command (:9093, 0.0.0.0:9094): --metrics runs
// Alertmanager's setup first, which writes it again, rather than the
// store's alone, which may restart Alertmanager. And an Alertmanager
// that is not running is started, not restarted, and setup says so.
func TestMetricsSetupNeedsNotifysDropIn(t *testing.T) {
	root := t.TempDir()
	if err := runSetup(&fakeSystem{}, root, &bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, dropInPath)); err != nil {
		t.Fatal(err)
	}
	f := &fakeSystem{isInstalled: true, failOn: "systemctl is-active --quiet " + amNSSocket + "|systemctl is-active --quiet " + amService}
	var out bytes.Buffer
	if err := runSetup(f, root, &out, true); err != nil {
		t.Fatal(err)
	}
	if acts(f.cmds)[0] != "systemctl mask "+amService || !isFile(filepath.Join(root, dropInPath)) {
		t.Fatalf("--metrics with notify's drop-in gone: %v; want Alertmanager's setup first, the drop-in written again", f.cmds)
	}
	if !strings.Contains(out.String(), "started Alertmanager, once, so that it listens in the namespace") || strings.Contains(out.String(), "restarted Alertmanager") {
		t.Errorf("Alertmanager not running: setup said\n%s\nwant it started, not restarted", out.String())
	}
}

// unitFlag is the value of flag on the one ExecStart= line of unit that
// runs exec.
func unitFlag(t *testing.T, unit []byte, exec, flag string) string {
	t.Helper()
	var vals []string
	for _, l := range strings.Split(string(unit), "\n") {
		if !strings.HasPrefix(l, "ExecStart="+exec+" ") {
			continue
		}
		for _, f := range strings.Fields(l) {
			if v, ok := strings.CutPrefix(f, flag+"="); ok {
				vals = append(vals, v)
			}
		}
	}
	if len(vals) != 1 {
		t.Fatalf("%s: %d %s flags, want 1", exec, len(vals), flag)
	}
	return vals[0]
}

// The store's offset, vmalert's evaluation delay, queryAt's own offset
// and the post-deploy check's lag are one clock: vmalert's help asks the
// delay to equal the offset, and a check must read past the offset, or
// the windows move off the cutover.
func TestStoreClocksAgree(t *testing.T) {
	offset := unitFlag(t, unitVMDropIn, "/usr/bin/victoria-metrics", "-search.latencyOffset")
	if delay := unitFlag(t, unitVMAlert, "/usr/bin/vmalert", "-rule.evalDelay"); offset != delay {
		t.Errorf("-search.latencyOffset %s and -rule.evalDelay %s differ", offset, delay)
	}
	if d, err := time.ParseDuration(offset); err != nil || d != storeLatencyOffset {
		t.Errorf("the drop-in's -search.latencyOffset %s is not queryAt's %s (%v)", offset, storeLatencyOffset, err)
	}
	if storeLag <= storeLatencyOffset {
		t.Errorf("storeLag %s does not read past the latency offset %s", storeLag, storeLatencyOffset)
	}
}

// scrapeJob is a job of scrape.yml, as the test reads it.
type scrapeJob struct {
	Name     string `yaml:"job_name"`
	Relabels []struct {
		Action       string   `yaml:"action"`
		SourceLabels []string `yaml:"source_labels"`
		Regex        string   `yaml:"regex"`
	} `yaml:"metric_relabel_configs"`
}

var plainName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// scrapeKept is each job's names that scrape.yml keeps: a keep of a
// literal alternation of names, or a drop of every name (`.+`). Any
// other shape is an error: the test cannot tell what it keeps.
func scrapeKept(conf []byte) (map[string][]string, error) {
	var f struct {
		Jobs []scrapeJob `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal(conf, &f); err != nil {
		return nil, err
	}
	kept := map[string][]string{}
	for _, j := range f.Jobs {
		if len(j.Relabels) != 1 || !slices.Equal(j.Relabels[0].SourceLabels, []string{"__name__"}) {
			return nil, fmt.Errorf("job %s: want one metric_relabel_configs entry on [__name__], have %+v", j.Name, j.Relabels)
		}
		switch r := j.Relabels[0]; {
		case r.Action == "drop" && r.Regex == ".+":
			kept[j.Name] = []string{}
		case r.Action == "keep":
			names := strings.Split(r.Regex, "|")
			for _, n := range names {
				if !plainName.MatchString(n) {
					return nil, fmt.Errorf("job %s: keep's regex %q is not an alternation of names", j.Name, r.Regex)
				}
			}
			slices.Sort(names)
			kept[j.Name] = names
		default:
			return nil, fmt.Errorf("job %s: %s of %q is neither a keep of names nor a drop of .+", j.Name, r.Action, r.Regex)
		}
	}
	return kept, nil
}

// storeRead is a series an expression reads: its name, and the job its
// selector names ("" for none).
type storeRead struct{ name, job string }

// labelLists take a parenthesised list of label names, not series.
var labelLists = map[string]bool{"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true}

var promWords = map[string]bool{"and": true, "or": true, "unless": true, "atan2": true, "default": true, "if": true, "ifnot": true, "offset": true, "bool": true, "keep_metric_names": true, "limit": true, "inf": true, "nan": true}

var matcherRe = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*("(?:[^"\\]|\\.)*")\s*$`)

// storeReads is every series name expr reads, by a scan of its tokens:
// a word followed by "(" is a function, one followed by by or without an
// aggregation, the lists after by, on and their kin are labels, and the
// rest are series, each with its selector's job. A bare selector names
// its series by __name__. What the scan cannot read is an error.
func storeReads(expr string) ([]storeRead, error) {
	isStart := func(c byte) bool { return c == '_' || c == ':' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
	isPart := func(c byte) bool { return isStart(c) || '0' <= c && c <= '9' }
	word := func(i int) (string, int) {
		j := i
		for j < len(expr) && isPart(expr[j]) {
			j++
		}
		return expr[i:j], j
	}
	space := func(i int) int {
		for i < len(expr) && strings.IndexByte(" \t\n", expr[i]) >= 0 {
			i++
		}
		return i
	}
	// selector reads the matchers from the "{" at i: the names __name__
	// gives (none: the word before it), the job, and the index past "}".
	selector := func(i int) (names []string, job string, end int, err error) {
		var parts []string
		start, quoted := i+1, false
		for end = i + 1; end < len(expr); end++ {
			switch c := expr[end]; {
			case quoted && c == '\\':
				end++
			case c == '"':
				quoted = !quoted
			case !quoted && (c == ',' || c == '}'):
				if p := strings.TrimSpace(expr[start:end]); p != "" {
					parts = append(parts, p)
				}
				start = end + 1
			}
			if !quoted && expr[end] == '}' {
				break
			}
		}
		if end >= len(expr) {
			return nil, "", 0, fmt.Errorf("%q: a selector does not close", expr)
		}
		for _, p := range parts {
			m := matcherRe.FindStringSubmatch(p)
			if m == nil {
				return nil, "", 0, fmt.Errorf("%q: matcher %q", expr, p)
			}
			val, err := strconv.Unquote(m[3])
			if err != nil {
				return nil, "", 0, fmt.Errorf("%q: matcher %q: %w", expr, p, err)
			}
			switch {
			case m[1] == "job" && m[2] == "=":
				job = val
			case m[1] == "job":
				return nil, "", 0, fmt.Errorf("%q: %s names no one job", expr, p)
			case m[1] == "__name__" && (m[2] == "=" || m[2] == "=~"):
				names = strings.Split(val, "|")
				for _, n := range names {
					if !plainName.MatchString(n) {
						return nil, "", 0, fmt.Errorf("%q: %s is not an alternation of names", expr, p)
					}
				}
			case m[1] == "__name__":
				return nil, "", 0, fmt.Errorf("%q: %s names no series", expr, p)
			}
		}
		return names, job, end + 1, nil
	}
	var out []storeRead
	for i := 0; i < len(expr); {
		switch c := expr[i]; {
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for ; j < len(expr) && expr[j] != c; j++ {
				if expr[j] == '\\' && c != '`' {
					j++
				}
			}
			if j >= len(expr) {
				return nil, fmt.Errorf("%q: a string does not close", expr)
			}
			i = j + 1
		case c == '#':
			return nil, fmt.Errorf("%q: a comment, which the scan does not read", expr)
		case c == '[':
			j := strings.IndexByte(expr[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("%q: a range does not close", expr)
			}
			i += j + 1
		case '0' <= c && c <= '9' || c == '.':
			for i < len(expr) && (isPart(expr[i]) || expr[i] == '.') {
				i++
			}
		case c == '{':
			names, job, end, err := selector(i)
			if err != nil {
				return nil, err
			}
			if len(names) == 0 {
				return nil, fmt.Errorf("%q: a selector with no name", expr)
			}
			for _, n := range names {
				out = append(out, storeRead{n, job})
			}
			i = end
		case isStart(c):
			w, j := word(i)
			k := space(j)
			next, _ := word(k)
			switch {
			case labelLists[w]:
				if k < len(expr) && expr[k] == '(' {
					e := strings.IndexByte(expr[k:], ')')
					if e < 0 {
						return nil, fmt.Errorf("%q: %s's list does not close", expr, w)
					}
					j = k + e + 1
				}
			case promWords[w], k < len(expr) && expr[k] == '(', next == "by" || next == "without":
			default:
				job := ""
				if j < len(expr) && expr[j] == '{' {
					names, sjob, end, err := selector(j)
					if err != nil {
						return nil, err
					}
					if len(names) > 0 {
						return nil, fmt.Errorf("%q: %s has a __name__ matcher too", expr, w)
					}
					job, j = sjob, end
				}
				out = append(out, storeRead{w, job})
			}
			i = j
		default:
			i++
		}
	}
	return out, nil
}

// readJob is the job a read reads from: the one its selector names, or
// the one its name's prefix belongs to. motel's push is job="motel".
func readJob(r storeRead) (string, error) {
	if r.job != "" {
		return r.job, nil
	}
	for prefix, job := range map[string]string{"vm_": "victoria-metrics", "vmalert_": "vmalert", "alertmanager_": "alertmanager", "motel_": "motel"} {
		if strings.HasPrefix(r.name, prefix) {
			return job, nil
		}
	}
	return "", fmt.Errorf("%s names no job, and no prefix tells it", r.name)
}

// queryRecorder is the store, answering every query with one series at
// 1, so that each reader runs its every query; it keeps the queries.
type queryRecorder struct {
	mu sync.Mutex
	qs []string
}

func (r *queryRecorder) answer(q string) []series {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.qs = append(r.qs, q)
	return []series{{labels: map[string]string{"class": "2xx"}, points: []point{{t: time.Now(), v: 1}}}}
}

func (r *queryRecorder) query(_ context.Context, q string) ([]series, error) { return r.answer(q), nil }
func (r *queryRecorder) queryAt(_ context.Context, q string, _ time.Time) ([]series, error) {
	return r.answer(q), nil
}

func (r *queryRecorder) queryRange(_ context.Context, q string, _, _ time.Time, _ time.Duration) ([]series, error) {
	return r.answer(q), nil
}

// motelsStoreQueries is every expression motel has the store evaluate:
// the rendered rules of a config with every kind of rule and ChannelFailed
// rules, the chain's check of vmalert, GET /<app>'s view over each range,
// and a post-deploy comparison.
func motelsStoreQueries(t *testing.T) []string {
	t.Helper()
	lines := []string{
		"rule errors_high threshold 5xx_share > 0.05 for 10m",
		"rule shop_slow threshold p95 > 1.5s for 15m app shop",
		"rule median threshold p50 > 200ms for 10m",
		"rule blog_quiet threshold request_rate < 0.1 for 1h app blog",
		"rule hits_low threshold cache_hit_share < 0.5 for 30m",
		"rule mem threshold memory_share > 0.8 for 5m",
		"rule busy threshold cpu_cores > 1.5 for 10m",
		"rule disk_filling disk_fill within 7d",
		"rule regress post_deploy {\n\t\tmax_5xx_share 0.02\n\t}",
	}
	for _, m := range thresholdMetricNames {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, " threshold "+m+" ") }) {
			t.Fatalf("no rule line here reads threshold %s: add one", m)
		}
	}
	var qs []string
	f := parseRules(t, renderRules(rulesApp(t, lines...)))
	for _, g := range f.Groups {
		if len(g.Rules) == 0 {
			t.Fatalf("group %s renders no rule here: its reads would go unseen", g.Name)
		}
		for _, r := range g.Rules {
			qs = append(qs, r.Expr)
		}
	}
	qs = append(qs, rulesQuery)
	rec := &queryRecorder{}
	v := newMetricsView(rec, zap.NewNop())
	for name, w := range viewWindows {
		if _, err := v.ask(context.Background(), "blog", name, w, time.Now()); err != nil {
			t.Fatalf("the view over %s: %v", name, err)
		}
	}
	if _, err := queryRelease(context.Background(), rec, "blog", "r2", time.Now(), 5*time.Minute, time.Minute, true, 0); err != nil {
		t.Fatal(err)
	}
	if len(rec.qs) < 3+2*7 {
		t.Fatalf("the view and the comparison asked %d queries: %q", len(rec.qs), rec.qs)
	}
	return append(qs, rec.qs...)
}

// keepDiff says how kept (scrape.yml's) and read (motel's readers')
// differ, job by job; "" when they are the same. `up` and scrape_* are
// the scraper's own, kept whatever the relabelling, and are not compared;
// a job nothing reads at all is said too.
func keepDiff(kept map[string][]string, reads []storeRead) (string, error) {
	read := map[string]map[string]bool{}
	for job := range kept {
		read[job] = nil
	}
	for _, r := range reads {
		job, err := readJob(r)
		if err != nil {
			return "", err
		}
		if job == "motel" {
			continue
		}
		if _, ok := kept[job]; !ok {
			return "", fmt.Errorf("%s is read from job %s, which scrape.yml does not scrape", r.name, job)
		}
		if read[job] == nil {
			read[job] = map[string]bool{}
		}
		if r.name != "up" && !strings.HasPrefix(r.name, "scrape_") {
			read[job][r.name] = true
		}
	}
	var b strings.Builder
	for _, job := range slices.Sorted(maps.Keys(kept)) {
		if read[job] == nil {
			fmt.Fprintf(&b, "job %s: scraped, and nothing motel runs reads it\n", job)
			continue
		}
		var unread, dropped []string
		for _, n := range kept[job] {
			if !read[job][n] {
				unread = append(unread, n)
			}
		}
		for _, n := range slices.Sorted(maps.Keys(read[job])) {
			if !slices.Contains(kept[job], n) {
				dropped = append(dropped, n)
			}
		}
		if len(unread) > 0 {
			fmt.Fprintf(&b, "job %s: kept, and nothing motel runs reads it: %s\n", job, strings.Join(unread, " "))
		}
		if len(dropped) > 0 {
			fmt.Fprintf(&b, "job %s: read, and dropped by scrape.yml: %s\n", job, strings.Join(dropped, " "))
		}
	}
	return b.String(), nil
}

// The store keeps only what motel reads from it (DESIGN-metrics.md,
// "What the store scrapes"): each job's kept names in the scrape.yml
// setup writes are exactly the names motel's own queries read from that
// job. A rule that comes to read a stack series fails this until
// scrape.yml keeps it; a series kept that no query reads fails it too.
func TestScrapeKeepsWhatMotelReads(t *testing.T) {
	kept, err := scrapeKept(scrapeConfig)
	if err != nil {
		t.Fatal(err)
	}
	var reads []storeRead
	for _, q := range motelsStoreQueries(t) {
		r, err := storeReads(q)
		if err != nil {
			t.Fatal(err)
		}
		reads = append(reads, r...)
	}
	diff, err := keepDiff(kept, reads)
	if err != nil {
		t.Fatal(err)
	}
	if diff != "" {
		t.Errorf("scrape.yml keeps what motel does not read, or drops what it does:\n%s"+
			"keep in notify/units/metrics/scrape.yml exactly what motel's queries read", diff)
	}
	// Either side changed alone is told, by name.
	for name, c := range map[string]struct {
		kept  map[string][]string
		reads []storeRead
		want  string
	}{
		"a series kept that nothing reads": {
			map[string][]string{"victoria-metrics": {"vm_free_disk_space_bytes", "vm_rows"}},
			[]storeRead{{"vm_free_disk_space_bytes", ""}},
			"job victoria-metrics: kept, and nothing motel runs reads it: vm_rows",
		},
		"a series read that is dropped": {
			map[string][]string{"vmalert": {}},
			[]storeRead{{"up", "vmalert"}, {"vmalert_alerts_sent_total", ""}},
			"job vmalert: read, and dropped by scrape.yml: vmalert_alerts_sent_total",
		},
		"a job nothing reads": {
			map[string][]string{"vmalert": {}},
			[]storeRead{{"motel_app_up", ""}},
			"job vmalert: scraped, and nothing motel runs reads it",
		},
	} {
		got, err := keepDiff(c.kept, c.reads)
		if err != nil || !strings.Contains(got, c.want) {
			t.Errorf("%s: keepDiff = %q, %v; want it to say %q", name, got, err, c.want)
		}
	}
}

// The scan of motel's expressions: every series, its job when its
// selector names one, nothing of a function, a label list or a string.
func TestStoreReads(t *testing.T) {
	for _, c := range []struct {
		expr string
		want []storeRead
	}{
		{`max(max_over_time(up{job="vmalert"}[3m]))`, []storeRead{{"up", "vmalert"}}},
		{`sum by (app, le) (increase(a_total{app="x",class=~"5xx|4xx"}[5m] offset 1d)) / b > bool 0.5 and on (app) group_left c`,
			[]storeRead{{"a_total", ""}, {"b", ""}, {"c", ""}}},
		{`{__name__=~"m1|m2",app="blog"}`, []storeRead{{"m1", ""}, {"m2", ""}}},
		{"label_replace(x, 'dst', '$1', \"src\", `(.*)`) atan2 y", []storeRead{{"x", ""}, {"y", ""}}},
		{"sum(x) keep_metric_names default 0 or y limit 5", []storeRead{{"x", ""}, {"y", ""}}},
		{`sum(increase(alertmanager_notifications_failed_total{integration="discord",receiver_name=~"a\\.b|c"}[5m])) > 0`,
			[]storeRead{{"alertmanager_notifications_failed_total", ""}}},
		// The request thresholds' guard (#442), as rendered: both series
		// are motel's push, neither a scraped job's.
		{"x and on (app) (" + pushedNow(`{app="blog"}`) + ")",
			[]storeRead{{"x", ""}, {"motel_app_observed", ""}, {"process_start_time_seconds", "motel"}}},
		{`scalar(min(tfirst_over_time(process_start_time_seconds{job="motel"}[7m]))) + 15`,
			[]storeRead{{"process_start_time_seconds", "motel"}}},
	} {
		got, err := storeReads(c.expr)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("storeReads(%s) = %v, %v; want %v", c.expr, got, err, c.want)
		}
	}
	for _, expr := range []string{`up{job=~"a|b"}`, `{__name__=~"vm_.*"}`, `{app="x"}`, `x{y="z"`, `x + 'open`, "x # a comment"} {
		if got, err := storeReads(expr); err == nil {
			t.Errorf("storeReads(%s) = %v; want an error", expr, got)
		}
	}
	if _, err := readJob(storeRead{"up", ""}); err == nil {
		t.Error(`readJob of up with no job: want an error`)
	}
}

// Debian's VictoriaMetrics accepts scrape.yml as setup writes it, with
// unknown fields refused (-promscrape.config.strictParse, its default).
func TestScrapeConfigPassesTheStoresCheck(t *testing.T) {
	vm := vmTool(t, "victoria-metrics")
	path := filepath.Join(t.TempDir(), scrapeName)
	if err := os.WriteFile(path, scrapeConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(vm, "-promscrape.config="+path, "-promscrape.config.dryRun", "-promscrape.config.strictParse", "-loggerLevel=ERROR").CombinedOutput()
	if err != nil {
		t.Errorf("victoria-metrics -promscrape.config.dryRun: %v\n%s", err, out)
	}
}
