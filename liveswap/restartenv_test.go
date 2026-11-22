package liveswap

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The case of #657, step by step: an instance launched with one value
// of API_TOKEN, the env_file then changed (or the key taken out, the
// file gone, the spec pointed at another file) and nothing deployed,
// and moteld restarted: the new process reattaches to the running unit
// without a launch. The instance still holds, and logs, the value it
// was launched with. These values are made up.
const (
	launchedToken = "old-token-0123456789" // gitleaks:allow
	rotatedToken  = "new-token-9876543210" // gitleaks:allow
)

// restartedLogsRig is logsRig after a moteld restart: demo has never
// been launched by this process, its state names v1 on a unit that
// still runs, and the manager reports the environment the unit was
// launched with: buildEnv's, from the env_file as it was then
// (launched). change then does to the file or the spec what the
// operator did while the unit ran. The app's journal holds the line
// the instance wrote with its own value. ensureRunning is the
// reattach; its error is returned for the caller to check.
func restartedLogsRig(t *testing.T, launched string, change func(spec *appSpec)) (*Handler, *testPeople, *managedApp, error) {
	t.Helper()
	h, people, _, _ := logsRig(t, demoJournal(1, func(int) string { return "upstream said 401 for " + launchedToken }))
	ma := h.app.managed["demo"]
	spec := ma.spec
	must(t, os.WriteFile(spec.envFile, []byte(launched), 0o600))
	socket, releaseDir := spec.dirs.socket(recordedNonce), spec.dirs.release("v1")
	env, err := buildEnv(spec, "v1", socket, releaseDir)
	must(t, err)
	change(spec)
	r := ma.runner.(*fakeRunner)
	r.reattachOK, r.reattachEnv = true, env
	st := ma.store.(*fakeStore)
	st.state = appState{CurrentVersion: "v1", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v1." + recordedNonce + ".service"}}
	st.ok = true
	must(t, os.MkdirAll(releaseDir, 0o755))
	bindSocket(t, socket)
	return h, people, ma, ma.ensureRunning()
}

// TestReattachKnowsTheLaunchedValues (#657): after a moteld restart
// that reattaches to a running instance, every answer that carries the
// app's text is filtered with the values the instance was launched
// with, and redacted_env names the key: the logs read, the status's
// filter, and the filter a deploy's record is written through. Whatever
// the operator did to the env_file since the launch.
func TestReattachKnowsTheLaunchedValues(t *testing.T) {
	launched := "API_TOKEN=" + launchedToken + "\nOTHER=kept-value-0000\n"
	for _, c := range []struct {
		name   string
		change func(spec *appSpec)
	}{
		{"the value changed in the file", func(spec *appSpec) {
			must(t, os.WriteFile(spec.envFile, []byte("API_TOKEN="+rotatedToken+"\nOTHER=kept-value-0000\n"), 0o600))
		}},
		{"the key taken out of the file", func(spec *appSpec) {
			must(t, os.WriteFile(spec.envFile, []byte("OTHER=kept-value-0000\n"), 0o600))
		}},
		{"the file gone", func(spec *appSpec) {
			must(t, os.Remove(spec.envFile))
		}},
		{"the file unreadable", func(spec *appSpec) {
			// A directory where the file was: unreadable as root too,
			// which the test container runs as.
			must(t, os.Remove(spec.envFile))
			must(t, os.Mkdir(spec.envFile, 0o700))
		}},
		{"the spec naming another file", func(spec *appSpec) {
			other := filepath.Join(t.TempDir(), "other.env")
			must(t, os.WriteFile(other, []byte("API_TOKEN="+rotatedToken+"\n"), 0o600))
			spec.envFile = other
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, people, ma, err := restartedLogsRig(t, launched, c.change)
			must(t, err)
			w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
			got := decodeLogs(t, w)
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), launchedToken) || len(got.Lines) != 1 ||
				got.Lines[0].Text != "upstream said 401 for [redacted:API_TOKEN]" || !slices.Contains(got.RedactedEnv, "API_TOKEN") {
				t.Fatalf("the logs read: %d %s", w.Code, w.Body)
			}
			// The status's filter and the record's hold the same values:
			// the probe body, the log tail and a deploy's record pass
			// one or the other.
			line := "token " + launchedToken
			if out, keys := ma.redactorFor(ma.status()).Redact(line); out != "token [redacted:API_TOKEN]" || !slices.Contains(keys, "API_TOKEN") {
				t.Errorf("the status's filter: %q %v", out, keys)
			}
			rd, _ := ma.recordRedactor(ma.snapshot(), deployResult{Version: "v2"})
			if out, _ := rd.Redact(line); out != "token [redacted:API_TOKEN]" {
				t.Errorf("the record's filter: %q", out)
			}
		})
	}
}

// TestReattachRemembersOnlyWhatMotelDidNotPut: of the unit's
// environment, the pairs motel puts there itself are no known values —
// the inherited ones, HOME as the shared dir, SOCKET, and an inline env
// pair as the current spec expands it — so a diagnostic can still name
// them; every other pair is one. An inline value changed in the config
// since the launch is a known value until the next launch (the
// default's cost, which the owner is to confirm). A repeated key, an
// empty value and a value holding = are each a pair as the manager
// gave it.
func TestReattachRemembersOnlyWhatMotelDidNotPut(t *testing.T) {
	rig := newTestRig(t)
	spec := rig.spec
	spec.env = map[string]string{"DATA": "{shared_dir}/db", "WHERE": "{release_dir}/{version}", "MODE": "config-mode-now"}
	socket, releaseDir := spec.dirs.socket(recordedNonce), spec.dirs.release("v1")
	env := append(inheritedEnv(),
		"HOME="+spec.dirs.shared,
		"API_TOKEN="+launchedToken,
		"EMPTY=",
		"DSN=user=app password=launched-pass-01", // gitleaks:allow
		"API_TOKEN=repeated-value-0001",
		"DATA="+spec.dirs.shared+"/db",
		"WHERE="+releaseDir+"/v1",
		"MODE=config-mode-then",
		"SOCKET="+socket,
	)
	if !rig.ma.rememberRunningEnv(spec, "v1", socket, releaseDir, env) {
		t.Fatal("an environment with the instance's SOCKET was not remembered")
	}
	want := []string{"API_TOKEN=" + launchedToken, "EMPTY=", "DSN=user=app password=launched-pass-01", "API_TOKEN=repeated-value-0001", "MODE=config-mode-then"} // gitleaks:allow
	if !slices.Equal(rig.ma.secrets, want) {
		t.Fatalf("remembered %q, want %q", rig.ma.secrets, want)
	}
}

// TestReattachWithNoEnvironmentKnowsTheFileAsBefore: a unit whose
// environment did not read back with the instance's own SOCKET pair
// (the manager reported none) teaches the filter nothing, the reattach
// says so in the journal without a value, and the filter is as it was:
// the file as it is now, and every body withheld while it cannot be
// read.
func TestReattachWithNoEnvironmentKnowsTheFileAsBefore(t *testing.T) {
	for _, env := range [][]string{nil, {"API_TOKEN=" + launchedToken, "SOCKET=/elsewhere.sock"}} {
		rig := newTestRig(t)
		core, logs := observer.New(zap.WarnLevel)
		rig.ma.logger = zap.New(core)
		rig.spec.envFile = filepath.Join(t.TempDir(), "demo.env") // never written: unreadable
		rig.runner.reattachOK, rig.runner.reattachEnv = true, env
		rig.store.state = appState{CurrentVersion: "v1", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v1." + recordedNonce + ".service"}}
		rig.store.ok = true
		must(t, os.MkdirAll(rig.spec.dirs.release("v1"), 0o755))
		bindSocket(t, rig.spec.dirs.socket(recordedNonce))
		must(t, rig.ma.ensureRunning())
		if len(rig.ma.secrets) != 0 {
			t.Errorf("%q: remembered %q", env, rig.ma.secrets)
		}
		if got := logs.FilterMessageSnippet("environment was not read back").All(); len(got) != 1 || strings.Contains(got[0].Message, launchedToken) {
			t.Errorf("%q: warned %v", env, logs.All())
		}
		if r := rig.ma.redactorFor(rig.ma.status()); r.withhold == "" {
			t.Errorf("%q: the filter withholds nothing while the file is unreadable and the values unknown", env)
		}
	}
}

// reattachedRig is a rig after a moteld restart whose reattach read
// the unit's environment back (env, with the instance's SOCKET), the
// spec's env_file at envFile, which the test may leave unwritten.
func reattachedRig(t *testing.T, envFile string, env ...string) *testRig {
	t.Helper()
	rig := newTestRig(t)
	rig.spec.envFile = envFile
	socket := rig.spec.dirs.socket(recordedNonce)
	env = append(env, "SOCKET="+socket)
	rig.runner.reattachOK, rig.runner.reattachEnv = true, env
	rig.store.state = appState{CurrentVersion: "v1", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v1." + recordedNonce + ".service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v1"), 0o755))
	bindSocket(t, socket)
	must(t, rig.ma.ensureRunning())
	if !rig.ma.runningEnvRead {
		t.Fatal("the reattach did not read the environment")
	}
	return rig
}

// TestReattachKeepsWithholdingForABoxWrittenFile: a box-written env
// file's NOTIFY_ values are withheld from every unit (buildEnvFull), so
// the environment a reattach reads back cannot hold them: while such a
// file cannot be read, every body is withheld as before the reattach.
func TestReattachKeepsWithholdingForABoxWrittenFile(t *testing.T) {
	boxDir := t.TempDir()
	orig := boxSecretsDir
	boxSecretsDir = boxDir
	t.Cleanup(func() { boxSecretsDir = orig })
	rig := reattachedRig(t, filepath.Join(boxDir, "demo", "env"), "API_TOKEN="+launchedToken) // never written: unreadable
	if r := rig.ma.redactorFor(rig.ma.status()); r.withhold == "" {
		t.Fatal("a box-written file that cannot be read must withhold every body: its NOTIFY_ values are no unit's")
	}
}

// TestLaunchEndsWhatTheReattachStoodInFor: once the app is launched
// again, the filter is what it is in a moteld that never restarted: a
// reload naming another env_file that cannot be read withholds every
// body, though the values the reattach read stay known.
func TestLaunchEndsWhatTheReattachStoodInFor(t *testing.T) {
	dir := t.TempDir()
	rig := reattachedRig(t, filepath.Join(dir, "gone.env"), "API_TOKEN="+launchedToken)
	if r := rig.ma.redactorFor(rig.ma.status()); r.withhold != "" {
		t.Fatalf("after the reattach, an unreadable file withholds: %s", r.withhold)
	}
	rig.ma.rememberSecrets(rig.spec.envFile, nil) // a launch
	rig.spec.envFile = filepath.Join(dir, "other.env")
	r := rig.ma.redactorFor(rig.ma.status())
	if r.withhold == "" {
		t.Fatal("after a launch, a reload to a file that cannot be read must withhold as it always did")
	}
	if out, _ := NewRedactor(rig.ma.secrets, nil).Redact(launchedToken); out != "[redacted:API_TOKEN]" {
		t.Fatalf("the reattach's values are no longer known: %q", out)
	}
}
