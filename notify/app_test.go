package notify

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"

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
	notify {
		channel ops slack {secret.NOTIFY_OPS_URL}
		channel alice ntfy {secret.NOTIFY_ALICE_URL}
		operator ops
		` + binding + `
		secrets_file ` + secrets + `
		state_dir ` + state + `
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
	src := "{\n\tadmin off\n\tnotify {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tsecrets_file " + secrets + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
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
		return "{\n\tadmin off\n\tnotify {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n" + line +
			"\t\tstate_dir " + t.TempDir() + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
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
	src := "{\n\tadmin off\n\tnotify {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\tchannel spare json {secret.NOTIFY_SPARE_URL}\n\t\toperator ops spare\n\t\tsecrets_file " + secrets +
		"\n\t\tstate_dir " + filepath.Join(dir, "state") + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
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
	src := "{\n\tadmin off\n\tnotify {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tsecrets_file " + secrets +
		"\n\t\tstate_dir " + filepath.Join(dir, "state") + "\n\t}\n}\n:8080 {\n\trespond ok\n}\n"
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
