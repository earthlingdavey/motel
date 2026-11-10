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
