package notify

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	// The Caddyfile adapter and the liveswap option, as the motel
	// binary links them.
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	_ "github.com/smallhoursorg/motel/liveswap"
)

func writePubKey(t *testing.T, path string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// validate adapts a Caddyfile and provisions it without starting, as
// `motel validate` does.
func validate(t *testing.T, src string) error {
	t.Helper()
	adapter := caddyconfig.GetAdapter("caddyfile")
	cfgJSON, warn, err := adapter.Adapt([]byte(src), nil)
	if err != nil {
		return err
	}
	for _, w := range warn {
		t.Logf("adapt warning: %s", w.Message)
	}
	var cfg caddy.Config
	if err := caddy.StrictUnmarshalJSON(cfgJSON, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, cfgJSON)
	}
	return caddy.Validate(&cfg)
}

// useStateDir stands dir in for the state directory for one test.
func useStateDir(t *testing.T, dir string) {
	t.Helper()
	was := theStateDir
	theStateDir = dir
	t.Cleanup(func() { theStateDir = was })
}

// useSocket stands sock in for Alertmanager's socket for one test.
func useSocket(t *testing.T, sock string) {
	t.Helper()
	was := theSocket
	theSocket = sock
	t.Cleanup(func() { theSocket = was })
}

// TestValidateThroughCaddy holds Provision to its promise: the
// refusals reach `motel validate`, read the apps as liveswap built
// them, and touch nothing on disk.
func TestValidateThroughCaddy(t *testing.T) {
	dir := t.TempDir()
	alice := filepath.Join(dir, "alice.pub")
	carol := filepath.Join(dir, "carol.pub")
	writePubKey(t, alice)
	writePubKey(t, carol)
	secrets := filepath.Join(dir, "_box.env")
	if err := os.WriteFile(secrets, []byte("# box\nNOTIFY_OPS_URL=\"https://hooks.slack.com/services/T0/B0/x\"\nNOTIFY_ALICE_URL=\"https://ntfy.sh/alice\"\nOTHER=1\n"), 0o600); err != nil { // gitleaks:allow
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	useStateDir(t, state)
	file := func(binding string) string {
		return `{
	admin off
	liveswap {
		root ` + filepath.Join(dir, "liveswap") + `
		artifact_allowlist example.org
		app blog {
			command ./server
			deploy_trust local {
				public_key ` + alice + `
				audience blog
			}
		}
	}
	monitor {
		channel ops slack {secret.NOTIFY_OPS_URL}
		channel alice ntfy {secret.NOTIFY_ALICE_URL}
		operator ops
		` + binding + `
		secrets_file ` + secrets + `
	}
}
:8080 {
	respond ok
}
`
	}
	if err := validate(t, file("bind local:"+alice+" alice")); err != nil {
		t.Fatalf("a valid block was refused: %v", err)
	}
	err := validate(t, file("bind local:"+carol+" alice"))
	if err == nil || !strings.Contains(err.Error(), "no app's deploy_trust trusts this identity") {
		t.Fatalf("a binding to an identity no app trusts: err = %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("validate wrote the state dir: %v", err)
	}
}

func TestValidateWithoutLiveswap(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "_box.env")
	if err := os.WriteFile(secrets, []byte("NOTIFY_OPS_URL=https://ntfy.sh/ops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tsecrets_file " + secrets + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	if err := validate(t, src); err != nil {
		t.Fatal(err)
	}
	// The secret is missing once the file is gone: refused by name.
	if err := os.Remove(secrets); err != nil {
		t.Fatal(err)
	}
	err := validate(t, src)
	if err == nil || !strings.Contains(err.Error(), "secret NOTIFY_OPS_URL is not in the box's secrets") {
		t.Fatalf("err = %v", err)
	}
}

// TestValidateRefusesASecretsFileMotelCannotRead: validate runs as root,
// who reads anything; motel serves as its own uid. A file moved in with
// mv keeps root:root 0600, which a reload refuses and a restart drops
// every channel over, so validate refuses it, naming the mend. Only
// validate: a load reads the file as motel, which is the judgement.
func TestValidateRefusesASecretsFileMotelCannotRead(t *testing.T) {
	origUser := motelUser
	t.Cleanup(func() { motelUser = origUser; SetCommand("") })
	const motelUID = 4242
	group := os.Getegid()
	motelUser = func() (int, []int, bool) { return motelUID, []int{group + 1}, true }
	// Directories motel may enter, as /etc/motel/secrets is (2750
	// root:motel, motel's group): t.TempDir's are 0700.
	dir, err := os.MkdirTemp("", "secretsmode-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(dir, "_box.env")
	if err := os.WriteFile(secrets, []byte("NOTIFY_OPS_URL=https://ntfy.sh/ops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tsecrets_file " + secrets + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	SetCommand("validate")
	err = validate(t, src)
	if err == nil || !strings.Contains(err.Error(), "the motel user cannot read the box's secrets file "+secrets) ||
		!strings.Contains(err.Error(), " 0600)") || !strings.Contains(err.Error(), "chmod 0640") {
		t.Fatalf("0600, owned by another: err = %v", err)
	}
	// Group-readable, the group motel's: taken.
	motelUser = func() (int, []int, bool) { return motelUID, []int{group}, true }
	if err := os.Chmod(secrets, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := validate(t, src); err != nil {
		t.Fatalf("0640 in motel's group: %v", err)
	}
	// The file readable, its directory one motel may not enter: refused.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validate(t, src); err == nil || !strings.Contains(err.Error(), "it may not enter "+dir) {
		t.Fatalf("a directory motel may not enter: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Group-readable, another group's: refused.
	motelUser = func() (int, []int, bool) { return motelUID, []int{group + 1}, true }
	if err := validate(t, src); err == nil {
		t.Fatal("0640 in a group motel is not in was taken")
	}
	// Not validate: the load's own read judges.
	SetCommand("")
	if err := validate(t, src); err != nil {
		t.Fatalf("a load: %v", err)
	}
}

// TestEverySecretIsAKnownValue: a value no channel line names is still
// masked in what motel sends.
func TestEverySecretIsAKnownValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_box.env")
	if err := os.WriteFile(path, []byte("NOTIFY_OPS_URL=\"https://ntfy.sh/ops\"\nNOTIFY_SPARE_URL=\"https://ntfy.sh/spare-topic-7f3a\"\nOTHER=\"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets, err := readSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if secrets["NOTIFY_SPARE_URL"] == "" || secrets["OTHER"] == "" {
		t.Fatalf("secrets = %v", secrets)
	}
	te := newTestEngine(t)
	te.known = knownValues(secrets)
	ev := deployFailed()
	ev.Summary = "posting to https://ntfy.sh/spare-topic-7f3a failed"
	te.report(ev)
	if got := te.drain(); strings.Contains(got[0].Annotations["summary"], "spare-topic") {
		t.Fatalf("summary = %q", got[0].Annotations["summary"])
	}
}

// The box handler validates a push before root writes its
// secrets/_box/ into _box.env: `motel validate` takes the names the
// push carries as held, in validate alone and for the box's own file
// alone, and refuses every other missing one as before.
func TestValidateTakesAPushsSecretsAsHeld(t *testing.T) {
	if _, err := os.Stat(DefaultSecretsFile); err == nil {
		t.Skip("this machine has a box's secrets file")
	}
	t.Cleanup(func() { SetCommand("") })
	src := func(secretsFile string) string {
		line := ""
		if secretsFile != "" {
			line = "\t\tsecrets_file " + secretsFile + "\n"
		}
		return "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n" + line +
			"\t}\n}\n:8080 {\n\trespond ok\n}\n"
	}
	t.Setenv(PushedSecretsEnv, "NOTIFY_OTHER_URL,NOTIFY_OPS_URL")
	SetCommand("validate")
	if err := validate(t, src("")); err != nil {
		t.Fatalf("a secret the push carries was refused as missing: %v", err)
	}
	// The box's file however it is spelt, as the box compares it.
	if err := validate(t, src("/etc/motel//secrets/./_box.env")); err != nil {
		t.Fatalf("the box's file spelt otherwise: %v", err)
	}
	// Another file is not where root writes the push's secrets.
	err := validate(t, src(filepath.Join(t.TempDir(), "other.env")))
	if err == nil || !strings.Contains(err.Error(), "secret NOTIFY_OPS_URL is not in the box's secrets") {
		t.Fatalf("a custom secrets_file: err = %v", err)
	}
	// Not named: refused.
	t.Setenv(PushedSecretsEnv, "NOTIFY_OTHER_URL")
	if err := validate(t, src("")); err == nil || !strings.Contains(err.Error(), "secret NOTIFY_OPS_URL is not in the box's secrets") {
		t.Fatalf("a secret the push does not carry: err = %v", err)
	}
	// In any other command the variable is nobody's word: a reload of a
	// running config refuses the missing secret.
	t.Setenv(PushedSecretsEnv, "NOTIFY_OPS_URL")
	SetCommand("run")
	orig := configRunning
	configRunning = func() bool { return true }
	t.Cleanup(func() { configRunning = orig })
	if err := validate(t, src("")); err == nil {
		t.Fatal("a load took the variable's names as held")
	}
}

// The first load of `motel run` (a boot) drops a channel whose secret is
// missing and serves; validate, a reload (a config running) and a load
// told no command refuse it.
func TestBootDropsAMissingSecret(t *testing.T) {
	orig := configRunning
	t.Cleanup(func() { SetCommand(""); configRunning = orig })
	running := false
	configRunning = func() bool { return running }
	dir := t.TempDir()
	secrets := filepath.Join(dir, "_box.env")
	if err := os.WriteFile(secrets, []byte("NOTIFY_OPS_URL=https://ntfy.sh/ops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\tchannel spare json {secret.NOTIFY_SPARE_URL}\n\t\toperator ops spare\n\t\tsecrets_file " + secrets +
		"\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	for _, c := range []struct {
		cmd     string
		running bool
		ok      bool
	}{
		{"run", false, true},
		{"run", true, false},
		{"validate", false, false},
		{"", false, false},
	} {
		SetCommand(c.cmd)
		running = c.running
		err := validate(t, src)
		if c.ok && err != nil {
			t.Errorf("%q, running %v: refused: %v", c.cmd, c.running, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "secret NOTIFY_SPARE_URL is not in the box's secrets")) {
			t.Errorf("%q, running %v: err = %v", c.cmd, c.running, err)
		}
	}
}

// A secrets file a boot cannot parse drops every channel that reads it,
// and serves; validate and a reload refuse it as before.
func TestBootDropsTheChannelsOfAnUnusableSecretsFile(t *testing.T) {
	orig := configRunning
	t.Cleanup(func() { SetCommand(""); configRunning = orig })
	running := false
	configRunning = func() bool { return running }
	dir := t.TempDir()
	secrets := filepath.Join(dir, "_box.env")
	if err := os.WriteFile(secrets, []byte("NOTIFY_OPS_URL=https://ntfy.sh/ops\nnot a line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tsecrets_file " + secrets +
		"\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	for _, c := range []struct {
		cmd     string
		running bool
		ok      bool
	}{
		{"run", false, true},
		{"run", true, false},
		{"validate", false, false},
		{"", false, false},
	} {
		SetCommand(c.cmd)
		running = c.running
		err := validate(t, src)
		if c.ok && err != nil {
			t.Errorf("%q, running %v: refused: %v", c.cmd, c.running, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), secrets+":2: not KEY=VALUE")) {
			t.Errorf("%q, running %v: err = %v", c.cmd, c.running, err)
		}
	}
}

// TestNoHeartbeatIsWarned: a block with no heartbeat line loads (the
// Watchdog goes to the blackhole), and every load says so; so does a
// boot that drops the line for its secret; one with the line says
// nothing of it.
func TestNoHeartbeatIsWarned(t *testing.T) {
	// The secrets are given to resolveWith; the file only names them.
	secrets := filepath.Join(t.TempDir(), "_box.env")
	ops := Channel{Name: "ops", Kind: "ntfy", Secret: "NOTIFY_OPS_URL"}
	for _, c := range []struct {
		name, heartbeat string
		secrets         map[string]string
		want            int
	}{
		{"no line", "", map[string]string{"NOTIFY_OPS_URL": "https://ntfy.sh/ops"}, 1},
		{"the line, its secret missing at a boot", "NOTIFY_HC_URL", map[string]string{"NOTIFY_OPS_URL": "https://ntfy.sh/ops"}, 1},
		{"the line", "NOTIFY_HC_URL", map[string]string{"NOTIFY_OPS_URL": "https://ntfy.sh/ops", "NOTIFY_HC_URL": "https://hc-ping.com/x"}, 0},
	} {
		core, logs := observer.New(zap.WarnLevel)
		a := &App{Channels: []Channel{ops}, Operator: []string{"ops"}, HeartbeatSecret: c.heartbeat, SecretsFile: secrets, logger: zap.New(core)}
		r, _, err := a.resolveWith(map[string][]TrustSource{}, c.secrets, nil, true, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		a.warnAtLoad(r.heartbeat)
		if n := len(logs.FilterMessageSnippet("monitor: no heartbeat in this load").All()); n != c.want {
			t.Errorf("%s: %d warnings, want %d (%v)", c.name, n, c.want, logs.All())
		}
	}
}

// provision adapts a Caddyfile and provisions it without starting, as
// validate does, and returns the monitor app it built.
func provision(t *testing.T, src string) (*App, error) {
	t.Helper()
	cfgJSON, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(src), nil)
	if err != nil {
		return nil, err
	}
	var cfg caddy.Config
	if err := caddy.StrictUnmarshalJSON(cfgJSON, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, cfgJSON)
	}
	ctx, err := caddy.ProvisionContext(&cfg)
	if err != nil {
		return nil, err
	}
	mod, err := ctx.App(appID)
	if err != nil {
		t.Fatal(err)
	}
	return mod.(*App), nil
}

// The two blocks the monitor block replaced are refused at the adapt,
// each naming what to write instead.
func TestTheOldBlocksAreRefused(t *testing.T) {
	for src, want := range map[string]string{
		"{\n\tadmin off\n\tnotify {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n}\n": "the notify block is now `monitor { … }`, with the same lines but state_dir and alertmanager, whose paths are fixed",
		"{\n\tadmin off\n\tmetrics_store\n}\n":                                              "metrics_store is now a `metrics [<socket path>]` line in the monitor block",
		"{\n\tadmin off\n\tmetrics_store {\n\t\tstore /run/x/vm.sock\n\t}\n}\n":             "metrics_store is now a `metrics [<socket path>]` line in the monitor block",
		"{\n\tadmin off\n\tmonitor {\n\t\tmetrics\n\t}\n\tmonitor {\n\t\tmetrics\n\t}\n}\n": "the monitor option is given more than once",
	} {
		_, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(src), nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", src, err, want)
		}
	}
}

// A monitor block that is a metrics line alone is the store with no
// notify: the numbers kept, no rule, and nothing of notify built (no
// render, so nothing for Alertmanager; no engine at Start). Any other
// line makes the block notify's, held to its refusals; a block with no
// line at all is refused for its operator line, as notify's always was.
func TestAMetricsLineAloneIsTheStoreWithoutNotify(t *testing.T) {
	block := func(lines ...string) string {
		return "{\n\tadmin off\n\tmonitor {\n\t\t" + strings.Join(lines, "\n\t\t") + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	}
	a, err := provision(t, block("metrics /run/x/vm.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if a.notifies || a.cfg != nil || a.rendered != nil || a.Metrics == nil || a.Metrics.store() != "/run/x/vm.sock" {
		t.Fatalf("notifies %v, cfg %v, rendered %d bytes, metrics %+v; want the store alone", a.notifies, a.cfg, len(a.rendered), a.Metrics)
	}
	if strings.Contains(string(a.Metrics.rules), "alert:") {
		t.Errorf("the store alone renders alerting rules:\n%s", a.Metrics.rules)
	}

	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"an empty block", []string{""}, "no operator line"},
		{"an option alone", []string{"deploy_succeeded on"}, "no operator line"},
		{"metrics and an option", []string{"metrics", "secrets_file /tmp/x"}, "no operator line"},
		{"metrics and an option at its default", []string{"metrics", "deploy_succeeded off"}, "no operator line"},
		{"metrics and an empty path", []string{"metrics", `secrets_file ""`}, "secrets_file takes a path, not an empty string"},
		{"metrics and an operator line", []string{"metrics", "operator ops"}, "operator names ops, which no channel line defines"},
		{"metrics and a channel", []string{"metrics", "channel ops ntfy {secret.NOTIFY_OPS_URL}"}, "no operator line"},
		{"a relative socket in the JSON", nil, "is not an absolute path"},
	} {
		src := block(c.lines...)
		if c.lines == nil {
			// The Caddyfile refuses it first; JSON is held to it too.
			a := &App{Metrics: &MetricsStore{Store: "run/vm.sock"}}
			cfg := caddy.Config{AppsRaw: caddy.ModuleMap{appID: caddyconfig.JSON(a, nil)}, Admin: &caddy.AdminConfig{Disabled: true}}
			if err := caddy.Validate(&cfg); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v, want %q", c.name, err, c.want)
			}
			continue
		}
		if _, err := provision(t, src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// One app, both parts: Start points the engine and the pusher at this
// config, notify's first; Stop lets go of both. The store alone never
// takes the engine.
func TestStartAndStopBothParts(t *testing.T) {
	// The paths first: the release below may render or reload, and must
	// reach this test's, not a box's (cleanups run last first).
	dir := t.TempDir()
	useStateDir(t, filepath.Join(dir, "state")) // not made: Start finds no Alertmanager set up
	useSocket(t, filepath.Join(dir, "am.sock"))
	// Fresh ones, put back after: Provision and Start reach the
	// globals, and no other test's state is this one's to read or leave.
	savedEngine, savedPusher := theEngine, thePusher
	theEngine = newEngine()
	thePusher = newPusher(theEngine.report)
	t.Cleanup(func() {
		// A failed assertion may leave either running: stop both.
		thePusher.mu.Lock()
		m := thePusher.active
		thePusher.mu.Unlock()
		if m != nil {
			thePusher.release(m)
		}
		theEngine.mu.Lock()
		a := theEngine.active
		theEngine.mu.Unlock()
		if a != nil {
			theEngine.release(a)
		}
		theEngine, thePusher = savedEngine, savedPusher
	})
	secrets := filepath.Join(dir, "_box.env")
	if err := os.WriteFile(secrets, []byte("NOTIFY_OPS_URL=https://ntfy.sh/ops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	both := "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tsecrets_file " + secrets +
		"\n\t\tbackup_record off\n\t\tmetrics " + filepath.Join(dir, "vm.sock") + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	alone := "{\n\tadmin off\n\tmonitor {\n\t\tmetrics " + filepath.Join(dir, "vm.sock") + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
	for _, c := range []struct {
		name, src string
		engine    bool
	}{{"both", both, true}, {"the store alone", alone, false}} {
		a, err := provision(t, c.src)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		a.Metrics.stateDirOverride = filepath.Join(dir, "metrics")
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		theEngine.mu.Lock()
		engine := theEngine.active
		theEngine.mu.Unlock()
		thePusher.mu.Lock()
		pusher := thePusher.active
		thePusher.mu.Unlock()
		var want *App
		if c.engine {
			want = a
		}
		if engine != want || pusher != a.Metrics {
			t.Errorf("%s: after Start the engine serves %p, the pusher %p; want the engine %v, and the pusher this config's", c.name, engine, pusher, c.engine)
		}
		if err := a.Stop(); err != nil {
			t.Fatal(err)
		}
		theEngine.mu.Lock()
		engine = theEngine.active
		theEngine.mu.Unlock()
		thePusher.mu.Lock()
		pusher = thePusher.active
		thePusher.mu.Unlock()
		if engine != nil || pusher != nil {
			t.Errorf("%s: after Stop the engine serves %p, the pusher %p; want neither", c.name, engine, pusher)
		}
	}
}

// Every field of the app but Metrics is a notify line: set to any value
// a line can give it, the block is notify's. A field added later is
// held to it with no list to update; one that is not omitempty makes
// every metrics line alone notify's, which the test above refuses.
func TestEveryOtherFieldIsANotifyLine(t *testing.T) {
	if !(&App{Metrics: &MetricsStore{}}).storeAlone() {
		t.Fatal("a metrics line alone is not the store alone")
	}
	typ := reflect.TypeOf(App{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() || f.Name == "Metrics" {
			continue
		}
		a := &App{Metrics: &MetricsStore{}}
		v := reflect.ValueOf(a).Elem().Field(i)
		switch v.Kind() {
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		case reflect.String:
			v.SetString("x")
		case reflect.Bool:
			v.SetBool(true)
		case reflect.Pointer:
			v.Set(reflect.New(v.Type().Elem())) // its zero value: deploy_succeeded off
		default:
			t.Fatalf("field %s is a %s: give it a value here", f.Name, v.Kind())
		}
		if a.storeAlone() {
			t.Errorf("a metrics line beside %s is taken for the store alone", f.Name)
		}
	}
}
