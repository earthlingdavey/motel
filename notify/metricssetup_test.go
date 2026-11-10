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

// notifySetUp makes root look like a box where motel notify setup ran.
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
		t.Error("the first rules are not the fixed groups")
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
		"Requires=motel-metrics-netns.service")
	has("motel-vm.socket", unitVMSocket, "ListenStream="+DefaultStoreSocket+"\n", "SocketGroup=motel", "SocketMode=0660")
	has("motel-vm.service", unitVMProxy, "NetworkNamespacePath=/run/netns/motel-metrics", "systemd-socket-proxyd --exit-idle-time=5min 127.0.0.1:8428")
	// The cold-boot ordering cycle's fix.
	has("motel-am-proxy.socket", unitAMProxySocket, "DefaultDependencies=no", "After=sysinit.target motel-metrics-netns.service",
		"ListenStream=127.0.0.1:9093", "NetworkNamespacePath=/run/netns/motel-metrics", "WantedBy=multi-user.target")
	has("motel-am-proxy.service", unitAMProxy, "systemd-socket-proxyd --exit-idle-time=5min "+DefaultSocket, "Requires=motel-am-proxy.socket motel-am.socket")
	has("motel-vmalert.service", unitVMAlert, "User=_victoria-metrics", "NetworkNamespacePath=/run/netns/motel-metrics",
		"-rule="+DefaultMetricsStateDir+"/"+RulesName+" ", "-configCheckInterval=1m", "-disableAlertgroupLabel",
		"-notifier.url=http://127.0.0.1:9093", "-remoteWrite.url=http://127.0.0.1:8428", "-remoteRead.url=http://127.0.0.1:8428", "MemoryMax=128M")
	if strings.Contains(string(unitVMDropIn), "0.0.0.0") || strings.Contains(string(unitVMDropIn), "-httpListenAddr=:") {
		t.Error("the drop-in listens beyond the namespace's loopback")
	}
	has("scrape.yml", scrapeConfig, `job_name: vmalert`, `"127.0.0.1:8880"`, `job_name: alertmanager`, `"127.0.0.1:9093"`)
}
