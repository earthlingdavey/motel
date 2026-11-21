package box

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/motel/box/proof"
	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

// The public halves of keys of every type a person or signer line may
// name, made once for these tests; no private key is kept.
var lineKeys = func() map[string]string {
	marshal := func(pub any) string {
		k, err := ssh.NewPublicKey(pub)
		if err != nil {
			panic(err)
		}
		return base64.StdEncoding.EncodeToString(k.Marshal())
	}
	ed := func() ed25519.PublicKey {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		return pub
	}
	ec := func(c elliptic.Curve) *ecdsa.PublicKey {
		priv, err := ecdsa.GenerateKey(c, rand.Reader)
		if err != nil {
			panic(err)
		}
		return &priv.PublicKey
	}
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	// A hardware key's public half as OpenSSH writes it (PROTOCOL.u2f).
	skEdOf := func(pub ed25519.PublicKey) string {
		return base64.StdEncoding.EncodeToString(ssh.Marshal(struct{ Type, Key, Application string }{"sk-ssh-ed25519@openssh.com", string(pub), "ssh:"}))
	}
	skEd := skEdOf(ed())
	// ED1 and SKED1 are one key in two forms, as are ED3 and SKED3. The
	// ECDSA keys are for the refusals: a person's key is Ed25519 for now.
	ed1, ed3 := ed(), ed()
	point, err := ec(elliptic.P256()).Bytes()
	if err != nil {
		panic(err)
	}
	skEC := base64.StdEncoding.EncodeToString(ssh.Marshal(struct{ Type, Curve, Key, Application string }{"sk-ecdsa-sha2-nistp256@openssh.com", "nistp256", string(point), "ssh:"}))
	return map[string]string{
		// Ed25519 points of small order (libsodium's blocklist): the
		// identity, the point of order 2, the identity as an sk key.
		"SO1":  base64.StdEncoding.EncodeToString(ssh.Marshal(struct{ Type, Key string }{"ssh-ed25519", "\x01" + strings.Repeat("\x00", 31)})),
		"SO2":  base64.StdEncoding.EncodeToString(ssh.Marshal(struct{ Type, Key string }{"ssh-ed25519", "\xec" + strings.Repeat("\xff", 30) + "\x7f"})),
		"SKSO": base64.StdEncoding.EncodeToString(ssh.Marshal(struct{ Type, Key, Application string }{"sk-ssh-ed25519@openssh.com", "\x01" + strings.Repeat("\x00", 31), "ssh:"})),
		"ED1":  marshal(ed1), "SKED1": skEdOf(ed1), "ED2": marshal(ed()), "ED3": marshal(ed3), "SKED3": skEdOf(ed3), "ED4": marshal(ed()),
		"SKED": skEd, "SKED4": skEdOf(ed()), "SKEC": skEC,
		"P256": marshal(ec(elliptic.P256())), "P384": marshal(ec(elliptic.P384())), "P521": marshal(ec(elliptic.P521())),
		"RSA": marshal(&r.PublicKey),
	}
}()

// linesFile is a whole box Caddyfile with lines in its box block after
// two signer lines (alice and bob, ed25519), running the apps example
// and blog, with no monitor block. Each @NAME@ is a key of lineKeys.
func linesFile(t *testing.T, lines string) []byte {
	t.Helper()
	in := `{
	box {
		deploy_trust local {
			public_key @PUB@
			audience box1
		}
		signer alice@example.com ssh-ed25519 @ED1@
		signer bob ssh-ed25519 @ED2@
` + lines + `
	}

	liveswap {
		app example {
			command node server.js
		}
		app blog {
			command node server.js
		}
	}
}

deploy.example.com {
	liveswap_webhook
	box_webhook
}
`
	// The module's reader provisions deploy_trust, which reads the key.
	pairs := []string{"@PUB@", localTrust(t).PublicKey}
	for name, key := range lineKeys {
		pairs = append(pairs, "@"+name+"@", key)
	}
	return []byte(strings.NewReplacer(pairs...).Replace(in))
}

// readModule is `moteld validate`'s reading of the box block: the
// Caddyfile adapter, which runs the module's parser, then the box
// app's Provision. It returns what Provision warned about the lines.
func readModule(t *testing.T, in []byte) ([]string, error) {
	t.Helper()
	cfg, _, err := adaptRaw(in)
	if err != nil {
		return nil, err
	}
	var c struct {
		Apps struct {
			Box json.RawMessage `json:"box"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	var a App
	if err := json.Unmarshal(c.Apps.Box, &a); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.WarnLevel)
	if err := a.provision(zap.New(core)); err != nil {
		return nil, err
	}
	var warnings []string
	for _, e := range logs.All() {
		warnings = append(warnings, e.Message)
	}
	return warnings, nil
}

// prefixes is n distinct from entries, each after a space.
func prefixes(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, " 10.0.%d.0/24", i)
	}
	return b.String()
}

// adaptRaw is adapt without its K1/K2 template, which a real key's
// base64 may hold.
func adaptRaw(in []byte) ([]byte, []caddyconfig.Warning, error) {
	return caddyconfig.GetAdapter("caddyfile").Adapt(in, map[string]any{"filename": "Caddyfile"})
}

// noChannel is the walk's warning for this file, which has no monitor
// block: the module cannot see that block, so it is left out where the
// two readers' warnings are compared.
const noChannel = "people are declared and no monitor channel is"

// TestLinesBothReaders feeds one table of person and grant lines to
// both readers, the module's (adapter and Provision: `moteld validate`
// and the load) and the walk's (a push, GET /, the applier, describe),
// and fails if they disagree on any row: taken or refused, and the
// message.
func TestLinesBothReaders(t *testing.T) {
	for name, c := range map[string]struct {
		lines string
		// want is the refusal both readers name, "" for taken.
		want string
		// warn is a warning both readers give, when taken.
		warn string
		// walkOnly is the walk's refusal of a line the module takes:
		// what only the whole file shows (DESIGN-box.md, "People and
		// grants").
		walkOnly string
	}{
		// Taken.
		"a person of each type": {lines: `
		person carol ssh-ed25519 @ED3@
		person carol sk-ssh-ed25519@openssh.com @SKED@
		grant carol viewer box`},
		"the design's lines": {lines: `
		person alice@example.com sk-ssh-ed25519@openssh.com @SKED@
		person bob@example.com   ssh-ed25519 @ED4@

		grant bob@example.com   viewer app example
		grant bob@example.com   viewer app blog from 198.51.100.0/24 2001:db8:5::/48
		grant alice@example.com viewer box`},
		"a signer granted, no person line": {lines: "\t\tgrant bob viewer app example"},
		"a lone address":                   {lines: "\t\tgrant bob viewer box from 198.51.100.7 2001:db8::1"},
		"sixteen prefixes":                 {lines: "\t\tgrant bob viewer box from" + prefixes(16)},
		"a name of 256 bytes":              {lines: "\t\tperson " + strings.Repeat("n", 256) + " ssh-ed25519 @ED3@\n\t\tgrant " + strings.Repeat("n", 256) + " viewer box"},
		"a quoted argument":                {lines: "\t\tgrant \"bob\" viewer \"box\""},
		"a person with no grant": {lines: "\t\tperson carol ssh-ed25519 @ED3@",
			warn: "person carol has no grant: their key is told only who they are here (/_motel/whoami)"},
		"a box grant and an app grant": {lines: "\t\tgrant bob viewer box\n\t\tgrant bob viewer app blog",
			warn: "grant bob viewer app blog: the box grant already covers it"},
		"a logs grant on an app":                 {lines: "\t\tgrant bob logs app blog"},
		"a logs grant on the box":                {lines: "\t\tgrant bob logs box from 203.0.113.0/24"},
		"viewer from anywhere, logs from a list": {lines: "\t\tgrant bob viewer app blog\n\t\tgrant bob logs app blog from 203.0.113.0/24"},
		"logs on the box covers viewer on an app": {lines: "\t\tgrant bob viewer app blog\n\t\tgrant bob logs box",
			warn: "grant bob viewer app blog: the logs grant on the box already covers it"},
		"an ssh-rsa signer": {lines: "\t\tsigner carol ssh-rsa @RSA@\n\t\tgrant bob viewer box",
			warn: "signer carol: an ssh-rsa key signs commits but cannot sign a person's token; to reach this box over HTTPS, add a person line for carol with an ed25519 key (ssh-keygen -t ed25519)"},

		// Refused, one line at a time.
		"a person's ssh-rsa key": {lines: "\t\tperson carol ssh-rsa @RSA@",
			want: "person carol: a person's key is ssh-ed25519 or its sk- form, not ssh-rsa (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"},
		// ECDSA is deferred, each type refused by name by both readers.
		"a person's ecdsa-sha2-nistp256 key": {lines: "\t\tperson carol ecdsa-sha2-nistp256 @P256@\n\t\tgrant carol viewer box",
			want: "person carol: a person's key is an Ed25519 key for now, and this release does not take ecdsa-sha2-nistp256 yet (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"},
		"a person's ecdsa-sha2-nistp384 key": {lines: "\t\tperson carol ecdsa-sha2-nistp384 @P384@",
			want: "person carol: a person's key is an Ed25519 key for now, and this release does not take ecdsa-sha2-nistp384 yet"},
		"a person's ecdsa-sha2-nistp521 key": {lines: "\t\tperson carol ecdsa-sha2-nistp521 @P521@",
			want: "person carol: a person's key is an Ed25519 key for now, and this release does not take ecdsa-sha2-nistp521 yet"},
		"a person's sk-ecdsa key": {lines: "\t\tperson carol sk-ecdsa-sha2-nistp256@openssh.com @SKEC@",
			want: "person carol: a person's key is an Ed25519 key for now, and this release does not take sk-ecdsa-sha2-nistp256@openssh.com yet"},
		"an unknown type":  {lines: "\t\tperson carol ssh-dss @ED3@", want: `person carol: key type "ssh-dss" is not one of`},
		"the wrong type":   {lines: "\t\tperson carol sk-ssh-ed25519@openssh.com @ED3@", want: `person carol: the key is "ssh-ed25519", not sk-ssh-ed25519@openssh.com`},
		"not base64":       {lines: "\t\tperson carol ssh-ed25519 !!!!", want: "person carol: the key is not base64"},
		"not a key":        {lines: "\t\tperson carol ssh-ed25519 AAAA", want: "person carol: the key does not parse as an SSH public key"},
		"a person, 2 args": {lines: "\t\tperson carol ssh-ed25519", want: "person line: it takes <name> <key-type> <base64>"},
		"a person, 4 args": {lines: "\t\tperson carol ssh-ed25519 @ED3@ x", want: "person line: it takes <name> <key-type> <base64>"},
		"the identity point": {lines: "\t\tperson mallory ssh-ed25519 @SO1@",
			want: "person mallory: the key is not a usable ed25519 key: its point has small order, so anyone can sign for it"},
		"the point of order 2": {lines: "\t\tperson mallory ssh-ed25519 @SO2@",
			want: "person mallory: the key is not a usable ed25519 key"},
		"the identity as an sk key": {lines: "\t\tperson mallory sk-ssh-ed25519@openssh.com @SKSO@",
			want: "person mallory: the key is not a usable ed25519 key"},
		"a person, no args": {lines: "\t\tperson", want: "person line: it takes <name> <key-type> <base64>"},
		"a person, a block": {lines: "\t\tperson carol ssh-ed25519 @ED3@ {\n\t\t\tx\n\t\t}", want: "person line: it takes no block"},
		"a person, an empty block": {lines: "\t\tperson carol ssh-ed25519 @ED3@ {\n\t\t}\n\t\tgrant carol viewer box",
			want: "person line: it takes no block"},
		"a bad name":            {lines: "\t\tperson c*rol ssh-ed25519 @ED3@", want: `person name "c*rol" may only contain letters, digits and . _ @ + -`},
		"a name of 257":         {lines: "\t\tperson " + strings.Repeat("n", 257) + " ssh-ed25519 @ED3@", want: `person name "` + strings.Repeat("n", 64) + `"... is longer than 256 bytes`},
		"a runtime placeholder": {lines: "\t\tgrant {env.WHO} viewer box", want: `grant name "{env.WHO}" may only contain`},
		"grant deployer":        {lines: "\t\tgrant bob deployer box", want: "grant bob deployer: deployer is held by a deploy_trust block, not granted"},
		"grant signer":          {lines: "\t\tgrant bob signer box", want: "grant bob signer: signer is held by a signer line (signer bob <key-type> <base64>); this release takes it from no grant"},
		"grant root":            {lines: "\t\tgrant bob root box", want: "grant bob root: root is held by root's key on the box, not granted"},
		"grant unknown role":    {lines: "\t\tgrant bob admin box", want: `grant bob "admin": unknown role; a grant gives viewer or logs`},
		"grant no scope":        {lines: "\t\tgrant bob viewer", want: "grant line: it takes <name> <role> box|app <app> [from <prefix>...]"},
		"grant bad scope":       {lines: "\t\tgrant bob viewer all", want: `grant bob viewer "all": the scope is box or app <app>`},
		"grant app no name":     {lines: "\t\tgrant bob viewer app", want: "grant bob viewer app: app takes the app's name"},
		"grant bad app":         {lines: "\t\tgrant bob viewer app Blog", want: `grant bob viewer app "Blog": not an app name`},
		"grant a word after":    {lines: "\t\tgrant bob viewer box please", want: `grant bob viewer box: "please" after the scope`},
		"grant from nothing":    {lines: "\t\tgrant bob viewer box from", want: "grant bob viewer box: from takes 1 to 16 addresses or prefixes"},
		"seventeen prefixes": {lines: "\t\tgrant bob viewer box from" + prefixes(17),
			want: "grant bob viewer box: from lists 17 addresses or prefixes; a grant takes at most 16"},
		"host bits":        {lines: "\t\tgrant bob viewer box from 198.51.100.7/24", want: "grant bob viewer box: 198.51.100.7/24 has host bits set; write 198.51.100.0/24"},
		"slash zero":       {lines: "\t\tgrant bob viewer box from 0.0.0.0/0", want: "grant bob viewer box: 0.0.0.0/0 would admit every address; leave from out instead"},
		"a zone":           {lines: "\t\tgrant bob viewer box from fe80::1%eth0", want: `grant bob viewer box: "fe80::1%eth0" has a zone; an address on a grant has none`},
		"a name for host":  {lines: "\t\tgrant bob viewer box from office.example.com", want: `grant bob viewer box: "office.example.com" is not an address or a prefix`},
		"a grant, a block": {lines: "\t\tgrant bob viewer box {\n\t\t\tx\n\t\t}", want: "grant line: it takes no block"},
		"a signer of small order": {lines: "\t\tsigner mallory ssh-ed25519 @SO1@",
			want: "signer mallory: the key is not a usable ed25519 key: its point has small order, so anyone can sign for it"},
		"a signer of small order, as an sk key": {lines: "\t\tsigner mallory sk-ssh-ed25519@openssh.com @SKSO@",
			want: "signer mallory: the key is not a usable ed25519 key: its point has small order, so anyone can sign for it"},
		"a grant, an empty block": {lines: "\t\tgrant bob viewer box {\n\t\t}\n\t\tgrant alice@example.com viewer box",
			want: "grant line: it takes no block"},

		// Refused across lines.
		"a person with a signer's key": {lines: "\t\tperson carol ssh-ed25519 @ED1@",
			want: "signer alice@example.com and person carol are the same key"},
		"a signer after a person, one key": {lines: "\t\tperson carol ssh-ed25519 @ED3@\n\t\tsigner dave ssh-ed25519 @ED3@",
			want: "signer dave and person carol are the same key"},
		"two people, one key": {lines: "\t\tperson carol ssh-ed25519 @ED3@\n\t\tperson dave ssh-ed25519 @ED3@",
			want: "person carol and person dave are the same key"},
		"a signer's key in its sk form": {lines: "\t\tperson carol sk-ssh-ed25519@openssh.com @SKED1@",
			want: "signer alice@example.com and person carol are the same key, written as ssh-ed25519 and as sk-ssh-ed25519@openssh.com"},
		"an Ed25519 key in both forms, two people": {lines: "\t\tperson carol ssh-ed25519 @ED3@\n\t\tperson dave sk-ssh-ed25519@openssh.com @SKED3@",
			want: "person carol and person dave are the same key, written as ssh-ed25519 and as sk-ssh-ed25519@openssh.com"},
		"an address twice": {lines: "\t\tgrant bob viewer box from 198.51.100.7 198.51.100.7/32",
			want: "grant bob viewer box: from lists 198.51.100.7 twice"},
		"a grant for no one": {lines: "\t\tgrant carol viewer box", want: "grant carol viewer box: no person or signer line declares carol"},
		"a grant for an ssh-rsa signer": {lines: "\t\tsigner carol ssh-rsa @RSA@\n\t\tgrant carol viewer box",
			want: "grant carol viewer box: carol has no key a person's token can use (an ssh-rsa signer key signs commits only); add a person line with an ed25519 key"},
		"two grants on one scope": {lines: "\t\tgrant bob viewer app blog\n\t\tgrant bob viewer app blog from 10.0.0.0/8",
			want: "grant bob viewer app blog: bob already has a viewer grant on app blog; one per role and scope"},
		"two logs grants on one scope": {lines: "\t\tgrant bob logs box from 10.0.0.0/8\n\t\tgrant bob logs box",
			want: "grant bob logs box: bob already has a logs grant on the box; one per role and scope"},
		"grant logs bad app": {lines: "\t\tgrant bob logs app Blog", want: `grant bob logs app "Blog": not an app name`},

		// The module's parser sees the box block alone.
		"a grant on an app the file does not run": {lines: "\t\tgrant bob viewer app shop",
			walkOnly: "grant bob viewer app shop: the liveswap block runs no app shop"},
		"a logs grant on an app the file does not run": {lines: "\t\tgrant bob logs app shop",
			walkOnly: "grant bob logs app shop: the liveswap block runs no app shop"},
	} {
		t.Run(name, func(t *testing.T) {
			in := linesFile(t, c.lines)
			modWarn, modErr := readModule(t, in)
			shape, walkErr := Walk(in)
			if c.walkOnly != "" {
				if modErr != nil {
					t.Fatalf("the module refused: %v", modErr)
				}
				if walkErr == nil || !strings.Contains(walkErr.Error(), "the Caddyfile has a bad "+c.walkOnly) {
					t.Fatalf("the walk: got %v, want %q", walkErr, c.walkOnly)
				}
				return
			}
			if c.want != "" {
				if modErr == nil || !strings.Contains(modErr.Error(), c.want) {
					t.Errorf("the module: got %v, want %q", modErr, c.want)
				}
				var r *Refusal
				if !errors.As(walkErr, &r) || !strings.Contains(walkErr.Error(), "the Caddyfile has a bad "+c.want) {
					t.Errorf("the walk: got %v, want %q", walkErr, c.want)
				}
				return
			}
			if modErr != nil || walkErr != nil {
				t.Fatalf("refused: the module %v, the walk %v", modErr, walkErr)
			}
			walkWarn := slices.DeleteFunc(slices.Clone(shape.AccessWarnings), func(w string) bool { return strings.HasPrefix(w, noChannel) })
			if len(shape.AccessWarnings) != len(walkWarn)+1 {
				t.Errorf("the walk did not warn of no monitor channel: %q", shape.AccessWarnings)
			}
			slices.Sort(modWarn)
			slices.Sort(walkWarn)
			if !slices.Equal(modWarn, walkWarn) {
				t.Errorf("the readers warn differently:\nmodule %q\nwalk   %q", modWarn, walkWarn)
			}
			if c.warn != "" && !slices.ContainsFunc(walkWarn, func(w string) bool { return strings.HasPrefix(w, c.warn) }) {
				t.Errorf("no warning %q: %q", c.warn, walkWarn)
			}
			if c.warn == "" && len(walkWarn) != 0 {
				t.Errorf("warned: %q", walkWarn)
			}
		})
	}
}

// TestLinesNoneReadAsBefore is the file without a person or a grant
// line: the module's JSON has neither key, and the walk gives no
// warning and nothing describe would print, even beside an ssh-rsa
// signer and with no monitor block.
func TestLinesNoneReadAsBefore(t *testing.T) {
	in := linesFile(t, "\t\tsigner carol ssh-rsa @RSA@")
	cfg, _, err := adaptRaw(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), `"people"`) || strings.Contains(string(cfg), `"grants"`) {
		t.Errorf("the module's JSON: %s", cfg)
	}
	if w, err := readModule(t, in); err != nil || len(w) != 0 {
		t.Errorf("the module: %q, %v", w, err)
	}
	shape, err := Walk(in)
	if err != nil {
		t.Fatal(err)
	}
	if shape.Access.Declared() || len(shape.AccessWarnings) != 0 || shape.Access.Keys != nil || shape.Access.Grants != nil {
		t.Errorf("the walk: %+v", shape)
	}
	var b strings.Builder
	describeAccess(&b, shape)
	if b.Len() != 0 {
		t.Errorf("describe adds %q", b.String())
	}
}

// TestSignerKeyTypes: a signer line with each of its three key types,
// and which of them a person's token may be signed with.
func TestSignerKeyTypes(t *testing.T) {
	in := linesFile(t, "\t\tsigner carol sk-ssh-ed25519@openssh.com @SKED@\n\t\tsigner dave ssh-rsa @RSA@\n\t\tperson erin ssh-ed25519 @ED3@\n\t\tgrant erin viewer box")
	shape, err := Walk(in)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, k := range shape.Access.Keys {
		if k.Signer {
			got[k.Type+" "+k.Name] = k.Person()
		}
	}
	want := map[string]bool{
		"ssh-ed25519 alice@example.com":    true,
		"ssh-ed25519 bob":                  true,
		"sk-ssh-ed25519@openssh.com carol": true,
		"ssh-rsa dave":                     false,
	}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: a person's key %v, want %v", k, got[k], v)
		}
	}
	// The signers are as before: the ssh-rsa one still signs commits.
	if len(shape.Signers) != 4 || !shape.Signers.Has("dave") {
		t.Errorf("signers %+v", shape.Signers)
	}
}

// TestDescribePeople is describe on the design's sample, as a golden.
func TestDescribePeople(t *testing.T) {
	dir := filepath.Join("testdata", "describe-people")
	var out strings.Builder
	if err := describe(filepath.Join(dir, "Caddyfile"), &out); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(dir, "describe.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("describe:\n%s\nwant:\n%s", out.String(), want)
	}

	// Without the monitor block, its warning follows the grants.
	in, err := os.ReadFile(filepath.Join(dir, "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(in), "\tmonitor {")
	end := strings.Index(string(in), "\tliveswap {")
	path := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(path, append(in[:start:start], in[end:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := describe(path, &out); err != nil {
		t.Fatal(err)
	}
	wantWarn := string(want) + "warning people are declared and no monitor channel is, so what they do on this box will be told to no one; add a channel line to the monitor block: channel <name> <kind> {secret.NOTIFY_<NAME>}\n"
	if out.String() != wantWarn {
		t.Errorf("describe:\n%s\nwant:\n%s", out.String(), wantWarn)
	}
}

// TestLinesNearPlaceholdersAndImports: a `{$` or an `import` beside the
// new lines is refused as everywhere in the box block.
func TestLinesNearPlaceholdersAndImports(t *testing.T) {
	for name, c := range map[string]struct{ lines, want string }{
		"a person's name":    {"\t\tperson {$WHO} ssh-ed25519 @ED3@", "has a placeholder where a directive name, a site address or a box line goes ({$WHO})"},
		"a person's key":     {"\t\tperson carol ssh-ed25519 {$KEY:@ED3@}", "has a placeholder"},
		"a grant's app":      {"\t\tgrant bob viewer app {$APP:blog}", "has a placeholder"},
		"a grant's address":  {"\t\tgrant bob viewer box from {$OFFICE}", "has a placeholder"},
		"a placeholder line": {"\t\tgrant bob viewer box\n\t\t{$MORE}", "has a placeholder"},
		"an import between":  {"\t\tperson carol ssh-ed25519 @ED3@\n\t\timport people\n\t\tgrant carol viewer box", "imports people; inline the snippet"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Walk(linesFile(t, c.lines))
			var r *Refusal
			if !errors.As(err, &r) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// TestNameGrammarIsTheSigners: a name on a person line and a principal
// on a signer line are held to one grammar and one bound, so a name is
// one person whichever line declares it.
func TestNameGrammarIsTheSigners(t *testing.T) {
	ed := lineKeys["ED3"]
	for _, name := range []string{"alice@example.com", "a.b_c+d-e", "A9", "", "a b", "a*", "a,b", "a!b", "a?b", "é", "{env.X}", `a"b`, strings.Repeat("n", proof.MaxPrincipal), strings.Repeat("n", proof.MaxPrincipal+1)} {
		_, signerErr := proof.ParseSigner(name, "ssh-ed25519", ed)
		_, personErr := access.ParsePerson([]string{name, "ssh-ed25519", ed})
		if (signerErr == nil) != (personErr == nil) {
			t.Errorf("%q: signer %v, person %v", name, signerErr, personErr)
		}
	}
	if access.MaxName != proof.MaxPrincipal {
		t.Errorf("MaxName %d, MaxPrincipal %d", access.MaxName, proof.MaxPrincipal)
	}
}

// TestSameReading is the comparison of the walk's two readings: a
// reading that differs from the other only in a person, or only in a
// grant's from list, is not the same. A real file can make them differ:
// one that hides a placeholder's start in a comment and its end before
// a grant line, which the raw reading takes as an unknown word and the
// expanded one as a grant.
func TestSameReading(t *testing.T) {
	unsetForTest(t, "MOTEL_BOX_TEST_UNSET_H")
	hidden := linesFile(t, "\t\t# {$MOTEL_BOX_TEST_UNSET_H:\n\t\t}grant bob viewer box")
	if _, err := Walk(hidden); err == nil || err.Error() != "the Caddyfile reads differently once its placeholders are expanded" {
		t.Errorf("a grant hidden from the raw reading: %v", err)
	}

	read := func() *Shape {
		t.Helper()
		s, err := Walk(linesFile(t, "\t\tperson carol ssh-ed25519 @ED3@\n\t\tgrant carol viewer app blog from 10.0.0.0/8"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if !sameReading(read(), read()) {
		t.Fatal("one file read twice reads differently")
	}
	person := read()
	k, err := access.ParsePerson([]string{"dave", "ssh-ed25519", lineKeys["ED4"]})
	if err != nil {
		t.Fatal(err)
	}
	person.Access.Keys = append(person.Access.Keys, k)
	from := read()
	from.Access.Grants[0].From = nil
	for name, other := range map[string]*Shape{"a person": person, "a grant's from": from} {
		if sameReading(read(), other) || sameReading(other, read()) {
			t.Errorf("readings differing in %s are the same", name)
		}
	}
}

// TestWalkMonitorWithoutChannel: a monitor block with no channel line
// (a metrics line alone) is no channel, and people beside it are
// warned of; a channel line ends the warning.
func TestWalkMonitorWithoutChannel(t *testing.T) {
	for monitor, warned := range map[string]bool{
		"\tmonitor {\n\t\tmetrics\n\t}\n":                                          true,
		"\tmonitor {\n\t\tchannel ops json https://hooks.example.com/motel\n\t}\n": false,
	} {
		in := strings.Replace(string(linesFile(t, "\t\tgrant bob viewer box")), "\tliveswap {", monitor+"\n\tliveswap {", 1)
		s, err := Walk([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		got := slices.ContainsFunc(s.AccessWarnings, func(w string) bool { return strings.HasPrefix(w, noChannel) })
		if got != warned {
			t.Errorf("%q: warned %v, want %v (%q)", monitor, got, warned, s.AccessWarnings)
		}
	}
}

// TestWalkAppThroughPlaceholder: an app the liveswap block names
// through a placeholder cannot be granted, since the walk reads the
// raw file, and the refusal says so rather than that no such app runs.
func TestWalkAppThroughPlaceholder(t *testing.T) {
	unsetForTest(t, "MOTEL_BOX_TEST_UNSET_APP")
	for _, app := range []string{"{$MOTEL_BOX_TEST_UNSET_APP:shop}", "shop{$MOTEL_BOX_TEST_UNSET_APP}"} {
		in := strings.Replace(string(linesFile(t, "\t\tgrant bob viewer app shop")), "\t\tapp blog {", "\t\tapp "+app+" {", 1)
		_, err := Walk([]byte(in))
		want := "the Caddyfile has a bad grant bob viewer app shop: no app line in the liveswap block names shop as written; an app named through a placeholder cannot be granted"
		if err == nil || err.Error() != want {
			t.Errorf("app %s: got %v, want %q", app, err, want)
		}
	}
}

// TestLinesModuleJSONScope: a grant posted as JSON whose app disagrees
// with its scope is refused at load, not checked as one and kept as
// the other.
func TestLinesModuleJSONScope(t *testing.T) {
	tc := localTrust(t)
	signer := SignerConfig{Principal: "bob", Type: "ssh-ed25519", Key: lineKeys["ED1"]}
	for _, g := range []GrantConfig{
		{Name: "bob", Role: "viewer", Scope: "box", App: "example"},
		{Name: "bob", Role: "viewer", Scope: "app"},
	} {
		a := App{DeployTrust: []deploytrust.TrustConfig{tc}, Signers: []SignerConfig{signer}, Grants: []GrantConfig{g}}
		err := a.provision(zap.NewNop())
		if err == nil || !strings.Contains(err.Error(), `box: grant "bob": a grant on the box names no app, and a grant on an app names one`) {
			t.Errorf("%+v: %v", g, err)
		}
	}
	a := App{DeployTrust: []deploytrust.TrustConfig{tc}, Signers: []SignerConfig{signer}, Grants: []GrantConfig{{Name: "bob", Role: "viewer", Scope: "app", App: "example"}, {Name: "bob", Role: "viewer", Scope: "box"}}}
	if err := a.provision(zap.NewNop()); err != nil {
		t.Errorf("good grants: %v", err)
	}
}

// TestLinesModuleJSONRoundTrip: the people and the grants go through the
// app's JSON (the adapter's output, marshalled and loaded again, as a
// config is) and come out as the walk reads them.
func TestLinesModuleJSONRoundTrip(t *testing.T) {
	in := linesFile(t, `
		person carol ssh-ed25519 @ED3@
		signer dave sk-ssh-ed25519@openssh.com @SKED@
		person carol sk-ssh-ed25519@openssh.com @SKED4@
		grant carol viewer app blog from 198.51.100.7 2001:db8:5::/48
		grant carol viewer box from 10.0.0.0/8
		grant bob viewer app example
		grant dave viewer box`)
	cfg, _, err := adaptRaw(in)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Apps struct {
			Box json.RawMessage `json:"box"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	var first, loaded App
	if err := json.Unmarshal(c.Apps.Box, &first); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &loaded); err != nil {
		t.Fatal(err)
	}
	signers := proof.Signers{}
	for _, s := range loaded.Signers {
		sg, err := proof.ParseSigner(s.Principal, s.Type, s.Key)
		if err != nil {
			t.Fatal(err)
		}
		signers = append(signers, sg)
	}
	got, err := loaded.lines(signers)
	if err != nil {
		t.Fatal(err)
	}
	shape, err := Walk(in)
	if err != nil {
		t.Fatal(err)
	}
	// The module holds the signers apart from the people: its keys are
	// the signers', then the people's, each in file order.
	want := access.Lines{Grants: shape.Access.Grants}
	for _, signer := range []bool{true, false} {
		for _, k := range shape.Access.Keys {
			if k.Signer == signer {
				want.Keys = append(want.Keys, k)
			}
		}
	}
	if !got.Equal(want) {
		t.Errorf("the module's JSON reads\n%+v\nthe walk\n%+v", got, want)
	}
}
