package liveswap

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sandboxViewScript is the one view probe, shared by the three lanes
// that read the sandbox from inside: this package's integration test,
// e2e/liveswap/systemd.sh and packaging/test/smoke.sh.
//
// Embedded rather than read from disk, and that is load-bearing. An
// embedded file is a build input, so editing the probe invalidates the
// test cache; a probe read at run time from outside this module is not
// tracked by `go test`, and both this test and the integration lane
// silently returned a cached pass when only the probe had changed.
// A missing file is now a compile error rather than a late failure.
//
//go:embed testdata/sandbox-view.sh
var sandboxViewScript string

// sandboxViewName is where the lanes install it inside a release dir.
const sandboxViewName = "sandbox-view.sh"

// sandboxViewKeys is every key the view probe emits. The view
// probe is shared by three lanes that assert overlapping subsets of it
// (the integration test below, e2e/liveswap/systemd.sh and
// packaging/test/smoke.sh), and a key quietly deleted from the script
// would turn each lane's expectation for it into a comparison against
// the empty string — which passes nothing and fails nothing. Pinning
// the set here fails that in `make test`, before any lane boots a
// container.
var sandboxViewKeys = []string{
	// The namespaces, and the uid the manager-socket check used.
	"pid", "uidmap", "nprocs", "uid",
	// The app's own dirs, and the rest of the liveswap root.
	"root_listing", "state", "apptmp", "current", "release", "root",
	// motel's own state, sockets and config.
	"motel_lib", "run_motel", "etc_motel", "admin_socket", "mgr_socket",
	// notify's Alertmanager socket and rendered state.
	"notify_run", "notify_socket", "notify_state",
	// The metrics store's socket and rendered rules.
	"metrics_run", "metrics_socket", "metrics_state",
	// The manager's process, via /proc.
	"mgr_root", "mgr_environ",
	// The rest of the host, which nothing bound.
	"abs_opt", "abs_srv", "abs_home", "abs_root", "abs_mnt", "abs_media",
	"abs_varlib", "abs_etcliveswap", "etc_listing", "varlib_listing",
	// The base view.
	"binsh", "usrbinenv", "hsbin", "etcssl", "sslprivate", "resolvconf", "dns",
	// The runtime environment.
	"cgroup", "cgroup_memory_max", "cgroup_memory_max_value", "cgroup_pids_max", "cgroup_cpu_max",
	"tmp", "home", "xdg_runtime", "nofile_soft", "nofile_hard",
	"core_soft", "core_hard", "seccomp", "io_uring_setup", "acme_token",
	"mqueue_owner", "shm_size_kb", "memtotal_kb", "shm_write", // sibling IPC (#311)
	// Vacuity guards, and the sentinel.
	"saw_mgr_pid", "saw_uid", "done",
}

// runSandboxView executes the view probe outside any sandbox, in a
// directory laid out the way it expects (<root>/<app>/releases/<v>),
// and returns the parsed view. Outside a unit the *values* are mostly
// meaningless — that is what the integration, e2e and smoke lanes are
// for — but the key set, the sentinel and the never-omit rule are all
// exercised, and so is the script actually running to completion.
func runSandboxView(t *testing.T, env ...string) map[string]string {
	t.Helper()
	root := t.TempDir()
	release := filepath.Join(root, "demo", "releases", "v1")
	shared := filepath.Join(root, "demo", "shared")
	for _, d := range []string{release, shared} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(release, sandboxViewName)
	if err := os.WriteFile(script, []byte(sandboxViewScript), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sourced, as every lane's ./server wrapper sources it. The path is
	// passed as an argument rather than interpolated into the shell
	// word, so a checkout under a path with a space still works.
	cmd := exec.Command("/bin/sh", "-c", `. "$1"`, "sh", script)
	cmd.Dir = release
	cmd.Env = append([]string{"HOME=" + shared, "PATH=" + os.Getenv("PATH")}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("view probe: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(shared, "view.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("view line %q is not k=v", line)
			continue
		}
		if _, dup := got[k]; dup {
			t.Errorf("view emits %q twice", k)
		}
		got[k] = v
	}
	return got
}

func TestSandboxViewProbeEmitsEveryKey(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the view probe reads /proc; it runs in the dev container")
	}
	got := runSandboxView(t)
	for _, k := range sandboxViewKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("view probe emitted no %q; a lane asserting it would compare against \"\"", k)
		}
	}
	for k := range got {
		if !slicesContains(sandboxViewKeys, k) {
			t.Errorf("view probe emitted unpinned key %q; add it to sandboxViewKeys and to the lanes that should assert it", k)
		}
	}
	if got["done"] != "1" {
		t.Errorf("done = %q, want 1: the lanes poll for the sentinel and would hang", got["done"])
	}
}

// TestSandboxViewProbeSkipsRatherThanOmits pins the rule the whole
// design rests on: a check whose input the lane did not supply reports
// "skipped" and still emits its key. Omitting it instead would let a
// lane that silently lost MGR_PID pass every /proc assertion by
// comparing "" against "".
func TestSandboxViewProbeSkipsRatherThanOmits(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the view probe reads /proc; it runs in the dev container")
	}
	without := runSandboxView(t)
	for _, k := range []string{"mgr_root", "mgr_environ", "dns"} {
		if without[k] != "skipped" {
			t.Errorf("with no input, %s = %q, want skipped", k, without[k])
		}
	}
	if without["saw_mgr_pid"] != "" {
		t.Errorf("saw_mgr_pid = %q, want empty: the guard must report what it was handed", without["saw_mgr_pid"])
	}
	// And with the input supplied, the check actually runs.
	with := runSandboxView(t, "MGR_PID=1", "PROBE_DNS_NAME=localhost")
	for _, k := range []string{"mgr_root", "mgr_environ"} {
		if with[k] != "open" && with[k] != "closed" {
			t.Errorf("with MGR_PID set, %s = %q, want open or closed", k, with[k])
		}
	}
	if with["saw_mgr_pid"] != "1" {
		t.Errorf("saw_mgr_pid = %q, want 1", with["saw_mgr_pid"])
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
