package notify

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// notifySetUp makes root look like a box where motel notify setup ran,
// with the store's data directory as Debian's package ships it, 0755
// (the fake install makes nothing).
func notifySetUp(t *testing.T, root string) {
	t.Helper()
	p := filepath.Join(root, socketPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, unitSocket, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, vmDataDir), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsSetupNeedsNotify(t *testing.T) {
	f := &fakeSystem{}
	err := metricsSetup(f, t.TempDir(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "run motel notify setup first") {
		t.Fatalf("err = %v", err)
	}
	if len(f.cmds) != 0 {
		t.Fatalf("ran %v before refusing", f.cmds)
	}
}

func TestMetricsSetupOrderAndFiles(t *testing.T) {
	root := t.TempDir()
	notifySetUp(t, root)
	f := &fakeSystem{}
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
		"systemctl enable --now motel-metrics-netns.service victoria-metrics.service motel-vm.socket motel-am-proxy.socket motel-vmalert.service",
		"systemctl restart motel-vmalert.service",
		"systemctl try-restart motel-vm.service motel-am-proxy.service",
		"systemctl try-reload-or-restart motel.service",
	}
	if !slices.Equal(f.cmds, want) {
		t.Fatalf("commands:\n%s\nwant\n%s", strings.Join(f.cmds, "\n"), strings.Join(want, "\n"))
	}
	for _, u := range metricsUnits() {
		b, err := os.ReadFile(filepath.Join(root, u.path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b, []byte("# Written by `motel metrics setup`")) {
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
	// Debian's 0755 would let every local uid read the series.
	if st, err := os.Stat(filepath.Join(root, vmDataDir)); err != nil {
		t.Error(err)
	} else if st.Mode().Perm() != 0o750 {
		t.Errorf("the store's data directory is %v, want 0750", st.Mode())
	}

	// Again, on a box set up: nothing installed, motel's rules kept,
	// the scrapes put back.
	if err := os.WriteFile(filepath.Join(dir, RulesName), []byte("rendered\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, scrapeName), []byte("edited\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	f1 := &fakeSystem{isInstalled: true}
	if err := metricsSetup(f1, root, &out); err != nil {
		t.Fatal(err)
	}
	if f1.cmds[0] != "systemctl mask victoria-metrics.service" || slices.ContainsFunc(f1.cmds, func(c string) bool { return strings.HasPrefix(c, "apt-get") }) {
		t.Fatalf("re-run: %v", f1.cmds)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, RulesName)); string(b) != "rendered\n" {
		t.Fatalf("setup replaced motel's rules: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, scrapeName)); !bytes.Equal(b, scrapeConfig) {
		t.Fatalf("setup left an edited scrape.yml: %q", b)
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

// TestMetricsUnitsAreTheMeasuredOnes holds the shipped units to what ran
// on hotserve-1 (hotserve1_stack_test.md), with this slice's paths and
// caps.
func TestMetricsUnitsAreTheMeasuredOnes(t *testing.T) {
	has := func(name string, unit []byte, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(string(unit), w) {
				t.Errorf("%s lacks %q", name, w)
			}
		}
	}
	has("the drop-in", unitVMDropIn, "ExecStart=\n", "-httpListenAddr=127.0.0.1:8428 ", "NetworkNamespacePath=/run/netns/motel-metrics",
		"MemoryMax=256M", "-memory.allowedBytes=96MiB", "-retentionPeriod=14d", "-promscrape.config="+DefaultMetricsStateDir+"/"+scrapeName,
		"Requires=motel-metrics-netns.service",
		// The store's start brings vmalert, which a stop of the store stopped.
		"Wants=motel-vmalert.service")
	// A namespace left behind must not fail every later start.
	has("the namespace unit", unitNetns, "ExecStartPre=-/usr/sbin/ip netns delete motel-metrics\nExecStart=/usr/sbin/ip netns add motel-metrics")
	has("motel-vm.socket", unitVMSocket, "ListenStream="+DefaultStoreSocket+"\n", "SocketGroup=motel", "SocketMode=0660")
	has("motel-vm.service", unitVMProxy, "NetworkNamespacePath=/run/netns/motel-metrics", "systemd-socket-proxyd --exit-idle-time=5min 127.0.0.1:8428",
		// Not root (the box audit of 2026-11-11).
		"DynamicUser=yes", "CapabilityBoundingSet=\n")
	// The cold-boot ordering cycle's fix.
	has("motel-am-proxy.socket", unitAMProxySocket, "DefaultDependencies=no", "After=sysinit.target motel-metrics-netns.service",
		"ListenStream=127.0.0.1:9093", "NetworkNamespacePath=/run/netns/motel-metrics", "WantedBy=multi-user.target")
	has("motel-am-proxy.service", unitAMProxy, "systemd-socket-proxyd --exit-idle-time=5min "+DefaultSocket, "Requires=motel-am-proxy.socket motel-am.socket",
		// Not root, and the motel group's files out of its view.
		"User=_victoria-metrics", "SupplementaryGroups=motel", "CapabilityBoundingSet=\n", "TemporaryFileSystem=/:ro",
		"BindReadOnlyPaths=/usr -/bin -/lib -/lib64 -/etc/ld.so.cache /run/motel-notify /run/systemd/notify\n")
	// Either one has systemd keep the host's root, and the view with it
	// (measured, systemd 257).
	for _, bad := range []string{"DynamicUser=", "ProtectSystem="} {
		if strings.Contains(string(unitAMProxy), "\n"+bad) {
			t.Errorf("motel-am-proxy.service has %s, which undoes TemporaryFileSystem=/", bad)
		}
	}
	has("motel-vmalert.service", unitVMAlert, "User=_victoria-metrics", "NetworkNamespacePath=/run/netns/motel-metrics",
		"-rule="+DefaultMetricsStateDir+"/"+RulesName+" ", "-configCheckInterval=1m", "-disableAlertgroupLabel",
		"-notifier.url=http://127.0.0.1:9093", "-remoteWrite.url=http://127.0.0.1:8428", "-remoteRead.url=http://127.0.0.1:8428", "MemoryMax=128M")
	if strings.Contains(string(unitVMDropIn), "0.0.0.0") || strings.Contains(string(unitVMDropIn), "-httpListenAddr=:") {
		t.Error("the drop-in listens beyond the namespace's loopback")
	}
	has("scrape.yml", scrapeConfig, `job_name: vmalert`, `"127.0.0.1:8880"`, `job_name: alertmanager`, `"127.0.0.1:9093"`)
}
