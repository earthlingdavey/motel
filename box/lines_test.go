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
	skEd := base64.StdEncoding.EncodeToString(ssh.Marshal(struct{ Type, Key, Application string }{"sk-ssh-ed25519@openssh.com", string(ed()), "ssh:"}))
	point, err := ec(elliptic.P256()).Bytes()
	if err != nil {
		panic(err)
	}
	skEC := base64.StdEncoding.EncodeToString(ssh.Marshal(struct{ Type, Curve, Key, Application string }{"sk-ecdsa-sha2-nistp256@openssh.com", "nistp256", string(point), "ssh:"}))
	return map[string]string{
		"ED1": marshal(ed()), "ED2": marshal(ed()), "ED3": marshal(ed()), "ED4": marshal(ed()),
		"SKED": skEd, "SKEC": skEC,
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

// readModule is `motel validate`'s reading of the box block: the
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

// adaptRaw is adapt without its K1/K2 template, which a real key's
// base64 may hold.
func adaptRaw(in []byte) ([]byte, []caddyconfig.Warning, error) {
	return caddyconfig.GetAdapter("caddyfile").Adapt(in, map[string]any{"filename": "Caddyfile"})
}

// noChannel is the walk's warning for this file, which has no monitor
// block: the module cannot see that block, so it is left out where the
// two readers' warnings are compared.
const noChannel = "people are declared but no monitor channel is"

// TestLinesBothReaders feeds one table of person and grant lines to
// both readers, the module's (adapter and Provision: `motel validate`
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
		person carol ecdsa-sha2-nistp256 @P256@
		person carol ecdsa-sha2-nistp384 @P384@
		person carol ecdsa-sha2-nistp521 @P521@
		person carol sk-ecdsa-sha2-nistp256@openssh.com @SKEC@
		grant carol viewer box`},
		"the design's lines": {lines: `
		person alice@example.com sk-ssh-ed25519@openssh.com @SKED@
		person bob@example.com   ecdsa-sha2-nistp256 @P256@

		grant bob@example.com   viewer app example
		grant bob@example.com   viewer app blog from 198.51.100.0/24 2001:db8:5::/48
		grant alice@example.com viewer box`},
		"a signer granted, no person line": {lines: "\t\tgrant bob viewer app example"},
		"a lone address":                   {lines: "\t\tgrant bob viewer box from 198.51.100.7 2001:db8::1"},
		"sixteen prefixes":                 {lines: "\t\tgrant bob viewer box from" + strings.Repeat(" 10.0.0.0/8", 16)},
		"a name of 256 bytes":              {lines: "\t\tperson " + strings.Repeat("n", 256) + " ssh-ed25519 @ED3@\n\t\tgrant " + strings.Repeat("n", 256) + " viewer box"},
		"a quoted argument":                {lines: "\t\tgrant \"bob\" viewer \"box\""},
		"a person with no grant": {lines: "\t\tperson carol ssh-ed25519 @ED3@",
			warn: "person carol has no grant: their key reaches only /_motel/whoami"},
		"a box grant and an app grant": {lines: "\t\tgrant bob viewer box\n\t\tgrant bob viewer app blog",
			warn: "grant bob viewer app blog: the box grant already covers it"},
		"an ssh-rsa signer": {lines: "\t\tsigner carol ssh-rsa @RSA@\n\t\tgrant bob viewer box",
			warn: "signer carol: an ssh-rsa key signs commits but cannot sign a person's token"},

		// Refused, one line at a time.
		"a person's ssh-rsa key": {lines: "\t\tperson carol ssh-rsa @RSA@",
			want: "person carol: a person's key is ssh-ed25519, ecdsa or the sk- form of either, not ssh-rsa (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"},
		"an unknown type":   {lines: "\t\tperson carol ssh-dss @ED3@", want: `person carol: key type "ssh-dss" is not one of`},
		"the wrong type":    {lines: "\t\tperson carol ecdsa-sha2-nistp256 @ED3@", want: `person carol: the key is "ssh-ed25519", not ecdsa-sha2-nistp256`},
		"not base64":        {lines: "\t\tperson carol ssh-ed25519 !!!!", want: "person carol: the key is not base64"},
		"not a key":         {lines: "\t\tperson carol ssh-ed25519 AAAA", want: "person carol: the key does not parse as an SSH public key"},
		"a person, 2 args":  {lines: "\t\tperson carol ssh-ed25519", want: "person line: it takes <name> <key-type> <base64>"},
		"a person, 4 args":  {lines: "\t\tperson carol ssh-ed25519 @ED3@ x", want: "person line: it takes <name> <key-type> <base64>"},
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
		"grant unknown role":    {lines: "\t\tgrant bob admin box", want: `grant bob "admin": unknown role; the one role a grant gives is viewer`},
		"grant no scope":        {lines: "\t\tgrant bob viewer", want: "grant line: it takes <name> <role> box|app <app> [from <prefix>...]"},
		"grant bad scope":       {lines: "\t\tgrant bob viewer all", want: `grant bob viewer "all": the scope is box or app <app>`},
		"grant app no name":     {lines: "\t\tgrant bob viewer app", want: "grant bob viewer app: app takes the app's name"},
		"grant bad app":         {lines: "\t\tgrant bob viewer app Blog", want: `grant bob viewer app "Blog": not an app name`},
		"grant a word after":    {lines: "\t\tgrant bob viewer box please", want: `grant bob viewer box: "please" after the scope`},
		"grant from nothing":    {lines: "\t\tgrant bob viewer box from", want: "grant bob viewer box: from takes 1 to 16 addresses or prefixes"},
		"seventeen prefixes": {lines: "\t\tgrant bob viewer box from" + strings.Repeat(" 10.0.0.0/8", 17),
			want: "grant bob viewer box: from lists 17 addresses or prefixes; a grant takes at most 16"},
		"host bits":        {lines: "\t\tgrant bob viewer box from 198.51.100.7/24", want: "grant bob viewer box: 198.51.100.7/24 has host bits set; write 198.51.100.0/24"},
		"slash zero":       {lines: "\t\tgrant bob viewer box from 0.0.0.0/0", want: "grant bob viewer box: 0.0.0.0/0 would admit every address; leave from out instead"},
		"a zone":           {lines: "\t\tgrant bob viewer box from fe80::1%eth0", want: `grant bob viewer box: "fe80::1%eth0" has a zone; an address on a grant has none`},
		"a name for host":  {lines: "\t\tgrant bob viewer box from office.example.com", want: `grant bob viewer box: "office.example.com" is not an address or a prefix`},
		"a grant, a block": {lines: "\t\tgrant bob viewer box {\n\t\t\tx\n\t\t}", want: "grant line: it takes no block"},
		"a grant, an empty block": {lines: "\t\tgrant bob viewer box {\n\t\t}\n\t\tgrant alice@example.com viewer box",
			want: "grant line: it takes no block"},

		// Refused across lines.
		"a person with a signer's key": {lines: "\t\tperson carol ssh-ed25519 @ED1@",
			want: "signer alice@example.com and person carol are the same key"},
		"a signer after a person, one key": {lines: "\t\tperson carol ssh-ed25519 @ED3@\n\t\tsigner dave ssh-ed25519 @ED3@",
			want: "signer dave and person carol are the same key"},
		"two people, one key": {lines: "\t\tperson carol ssh-ed25519 @ED3@\n\t\tperson dave ssh-ed25519 @ED3@",
			want: "person carol and person dave are the same key"},
		"a grant for no one": {lines: "\t\tgrant carol viewer box", want: "grant carol viewer box: no person or signer line declares carol"},
		"a grant for an ssh-rsa signer": {lines: "\t\tsigner carol ssh-rsa @RSA@\n\t\tgrant carol viewer box",
			want: "grant carol viewer box: carol has no key a person's token can use (an ssh-rsa signer key signs commits only); add a person line with an ed25519 or ecdsa key"},
		"two grants on one scope": {lines: "\t\tgrant bob viewer app blog\n\t\tgrant bob viewer app blog from 10.0.0.0/8",
			want: "grant bob viewer app blog: bob already has a grant on app blog; one per scope"},

		// The module's parser sees the box block alone.
		"a grant on an app the file does not run": {lines: "\t\tgrant bob viewer app shop",
			walkOnly: "grant bob viewer app shop: the liveswap block runs no app shop"},
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
	if shape.Access.Declared() || len(shape.AccessWarnings) != 0 {
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
	wantWarn := string(want) + "warning people are declared but no monitor channel is: what they do here will be told to no one (from step 2, a key's first use from a new network; from step 3, their acts)\n"
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
