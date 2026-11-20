package access

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The keys of these tests are public halves made in the test; no
// private key is kept.

func b64(wire []byte) string { return base64.StdEncoding.EncodeToString(wire) }

func edKey(t testing.TB) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return b64(k.Marshal())
}

func ecdsaKey(t testing.TB, c elliptic.Curve) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return b64(k.Marshal())
}

// skEdKey and skECDSAB64 are the wire forms OpenSSH writes for a
// hardware key's public half (PROTOCOL.u2f): no device is needed to
// make one.
func skEdKey(t testing.TB) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return b64(ssh.Marshal(struct {
		Type, Key, Application string
	}{"sk-ssh-ed25519@openssh.com", string(pub), "ssh:"}))
}

func skECDSAB64(t testing.TB) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	point, err := priv.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b64(ssh.Marshal(struct {
		Type, Curve, Key, Application string
	}{"sk-ecdsa-sha2-nistp256@openssh.com", "nistp256", string(point), "ssh:"}))
}

// edForms is one Ed25519 key written three ways: ssh-ed25519, and
// sk-ssh-ed25519@openssh.com under two application strings.
func edForms(t testing.TB) (plain, sk, skOther string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	skForm := func(app string) string {
		return b64(ssh.Marshal(struct{ Type, Key, Application string }{"sk-ssh-ed25519@openssh.com", string(pub), app}))
	}
	return b64(k.Marshal()), skForm("ssh:"), skForm("ssh:other")
}

// prefixes is n distinct from entries, each after a space.
func prefixes(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, " 10.0.%d.0/24", i)
	}
	return b.String()
}

// smallOrder is an Ed25519 public key of small order, from its hex
// encoding (libsodium's blocklist), in the plain or the sk form.
func smallOrder(t testing.TB, hexKey string, sk bool) string {
	t.Helper()
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	if sk {
		return b64(ssh.Marshal(struct{ Type, Key, Application string }{"sk-ssh-ed25519@openssh.com", string(key), "ssh:"}))
	}
	return b64(ssh.Marshal(struct{ Type, Key string }{"ssh-ed25519", string(key)}))
}

func rsaKey(t testing.TB) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return b64(k.Marshal())
}

func TestParsePerson(t *testing.T) {
	ed := edKey(t)
	for _, c := range []struct{ typ, key string }{
		{"ssh-ed25519", ed},
		{"sk-ssh-ed25519@openssh.com", skEdKey(t)},
	} {
		k, err := ParsePerson([]string{"bob@example.com", c.typ, c.key})
		if err != nil {
			t.Errorf("%s: %v", c.typ, err)
			continue
		}
		wire, _ := base64.StdEncoding.DecodeString(c.key)
		pub, _ := ssh.ParsePublicKey(wire)
		if k.Name != "bob@example.com" || k.Type != c.typ || k.B64 != c.key || k.Signer || !k.Person() || k.Fingerprint != ssh.FingerprintSHA256(pub) {
			t.Errorf("%s: %+v", c.typ, k)
		}
	}

	wire, _ := base64.StdEncoding.DecodeString(ed)
	skEd := skEdKey(t) // 74 bytes: its base64 ends in =
	for line, want := range map[string]Role{"bob viewer box": Viewer, "bob logs box": Logs, "bob logs app blog from 10.0.0.0/8": Logs} {
		if g, err := ParseGrant(strings.Fields(line)); err != nil || g.Role != want {
			t.Errorf("%s: %+v %v, want role %s", line, g, err, want)
		}
	}
	long := strings.Repeat("a", MaxName)
	if _, err := ParsePerson([]string{long, "ssh-ed25519", ed}); err != nil {
		t.Errorf("a name of %d bytes: %v", MaxName, err)
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"two args":         {[]string{"bob", "ssh-ed25519"}, "person line: it takes <name> <key-type> <base64>"},
		"four args":        {[]string{"bob", "ssh-ed25519", ed, "x"}, "person line: it takes <name> <key-type> <base64>"},
		"none":             {nil, "person line: it takes <name> <key-type> <base64>"},
		"bad name":         {[]string{"a*b", "ssh-ed25519", ed}, `person name "a*b" may only contain letters, digits and . _ @ + -`},
		"empty name":       {[]string{"", "ssh-ed25519", ed}, `person name "" may only contain`},
		"long name":        {[]string{long + "a", "ssh-ed25519", ed}, "is longer than 256 bytes"},
		"placeholder name": {[]string{"{env.WHO}", "ssh-ed25519", ed}, `person name "{env.WHO}" may only contain`},
		"rsa": {[]string{"carol@example.com", "ssh-rsa", rsaKey(t)},
			"person carol@example.com: a person's key is ssh-ed25519 or its sk- form, not ssh-rsa (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"},
		// ECDSA is deferred: each type refused by name, a good key of it
		// as much as any other.
		"ecdsa p256": {[]string{"bob@example.com", "ecdsa-sha2-nistp256", ecdsaKey(t, elliptic.P256())},
			"person bob@example.com: a person's key is an Ed25519 key for now, and this release does not take ecdsa-sha2-nistp256 yet (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"},
		"ecdsa p384": {[]string{"bob", "ecdsa-sha2-nistp384", ecdsaKey(t, elliptic.P384())},
			"person bob: a person's key is an Ed25519 key for now, and this release does not take ecdsa-sha2-nistp384 yet (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"},
		"ecdsa p521": {[]string{"bob", "ecdsa-sha2-nistp521", ecdsaKey(t, elliptic.P521())},
			"person bob: a person's key is an Ed25519 key for now, and this release does not take ecdsa-sha2-nistp521 yet (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"},
		"sk ecdsa p256": {[]string{"bob", "sk-ecdsa-sha2-nistp256@openssh.com", skECDSAB64(t)},
			"person bob: a person's key is an Ed25519 key for now, and this release does not take sk-ecdsa-sha2-nistp256@openssh.com yet (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"},
		"ecdsa, before its base64": {[]string{"bob", "ecdsa-sha2-nistp256", "!!!!"},
			"this release does not take ecdsa-sha2-nistp256 yet"},
		"dss":           {[]string{"bob", "ssh-dss", ed}, `person bob: key type "ssh-dss" is not one of ssh-ed25519, sk-ssh-ed25519@openssh.com`},
		"a certificate": {[]string{"bob", "ssh-ed25519-cert-v01@openssh.com", ed}, `key type "ssh-ed25519-cert-v01@openssh.com" is not one of`},
		"not base64":    {[]string{"bob", "ssh-ed25519", "!!!!"}, "person bob: the key is not base64"},
		"padding off":   {[]string{"bob", "sk-ssh-ed25519@openssh.com", strings.TrimRight(skEd, "=")}, "person bob: the key is not base64"},
		"two lines":     {[]string{"bob", "ssh-ed25519", ed[:20] + "\n" + ed[20:]}, "person bob: the key is not one line of canonical base64"},
		"not a key":     {[]string{"bob", "ssh-ed25519", "AAAA"}, "person bob: the key does not parse as an SSH public key"},
		"trailing":      {[]string{"bob", "ssh-ed25519", b64(append(slices.Clone(wire), 0))}, "person bob: the key does not parse as an SSH public key"},
		"other type":    {[]string{"bob", "sk-ssh-ed25519@openssh.com", ed}, `person bob: the key is "ssh-ed25519", not sk-ssh-ed25519@openssh.com`},
		"the identity point": {[]string{"bob", "ssh-ed25519", smallOrder(t, "0100000000000000000000000000000000000000000000000000000000000000", false)},
			"person bob: the key is not a usable ed25519 key: its point has small order, so anyone can sign for it"},
		"the point of order 2":                  {[]string{"bob", "ssh-ed25519", smallOrder(t, "ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", false)}, "not a usable ed25519 key"},
		"a point of order 8":                    {[]string{"bob", "ssh-ed25519", smallOrder(t, "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05", false)}, "not a usable ed25519 key"},
		"the identity, written non-canonically": {[]string{"bob", "ssh-ed25519", smallOrder(t, "eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", false)}, "not a usable ed25519 key"},
		"no point":                              {[]string{"bob", "ssh-ed25519", smallOrder(t, "0200000000000000000000000000000000000000000000000000000000000000", false)}, "person bob: the key is not a usable ed25519 key: its bytes are no point on the curve"},
		"the identity, as an sk key":            {[]string{"bob", "sk-ssh-ed25519@openssh.com", smallOrder(t, "0100000000000000000000000000000000000000000000000000000000000000", true)}, "not a usable ed25519 key"},
	} {
		_, err := ParsePerson(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestParseGrant(t *testing.T) {
	for line, want := range map[string]string{
		"bob viewer box":                                           "bob viewer box",
		"bob viewer app example":                                   "bob viewer app example",
		"bob viewer app box":                                       "bob viewer app box",
		"bob viewer box from 198.51.100.0/24":                      "bob viewer box from 198.51.100.0/24",
		"bob viewer app blog from 198.51.100.0/24 2001:db8:5::/48": "bob viewer app blog from 198.51.100.0/24 2001:db8:5::/48",
		// One value, printed in its one spelling: a lone address as an
		// address, IPv6 in lower case and shortest form.
		"bob viewer box from 198.51.100.7":           "bob viewer box from 198.51.100.7",
		"bob viewer box from 198.51.100.7/32":        "bob viewer box from 198.51.100.7",
		"bob viewer box from 2001:DB8:0:0::/48":      "bob viewer box from 2001:db8::/48",
		"bob viewer box from 2001:db8::1 10.0.0.0/8": "bob viewer box from 2001:db8::1 10.0.0.0/8",
		// A logs line keeps its role: it is no viewer line.
		"bob logs box":                                    "bob logs box",
		"bob logs app blog":                               "bob logs app blog",
		"bob logs app blog from 203.0.113.0/24":           "bob logs app blog from 203.0.113.0/24",
		"bob logs box from 198.51.100.7/32 2001:db8::/32": "bob logs box from 198.51.100.7 2001:db8::/32",
	} {
		g, err := ParseGrant(strings.Fields(line))
		if err != nil {
			t.Errorf("%s: %v", line, err)
			continue
		}
		if got := strings.Join(g.Args(), " "); got != want {
			t.Errorf("%s: prints %q, want %q", line, got, want)
		}
	}
	from16 := "bob viewer box from" + prefixes(MaxFrom)
	if _, err := ParseGrant(strings.Fields(from16)); err != nil {
		t.Errorf("%d prefixes: %v", MaxFrom, err)
	}
	for line, want := range map[string]string{
		"":                                                     grantUsage,
		"bob viewer":                                           grantUsage,
		"b*b viewer box":                                       `grant name "b*b" may only contain letters, digits and . _ @ + -`,
		"bob deployer box":                                     "grant bob deployer: deployer is held by a deploy_trust block, not granted",
		"bob signer box":                                       "grant bob signer: signer is held by a signer line (signer bob <key-type> <base64>); this release takes it from no grant",
		"bob root box":                                         "grant bob root: root is held by root's key on the box, not granted",
		"bob admin box":                                        `grant bob "admin": unknown role; a grant gives viewer or logs`,
		"bob Viewer box":                                       `grant bob "Viewer": unknown role`,
		"bob Logs box":                                         `grant bob "Logs": unknown role`,
		"bob log box":                                          `grant bob "log": unknown role`,
		"bob logs everything":                                  `grant bob logs "everything": the scope is box or app <app>`,
		"bob logs app":                                         "grant bob logs app: app takes the app's name",
		"bob logs app Blog":                                    `grant bob logs app "Blog": not an app name`,
		"bob logs box please":                                  `grant bob logs box: "please" after the scope`,
		"bob logs app blog from 198.51.100.7/24":               "grant bob logs app blog: 198.51.100.7/24 has host bits set",
		"bob viewer everything":                                `grant bob viewer "everything": the scope is box or app <app>`,
		"bob viewer app":                                       "grant bob viewer app: app takes the app's name",
		"bob viewer app Blog":                                  `grant bob viewer app "Blog": not an app name (lowercase letters, digits and -, at most 63)`,
		"bob viewer app {env.APP}":                             `grant bob viewer app "{env.APP}": not an app name`,
		"bob viewer box example":                               `grant bob viewer box: "example" after the scope; only from <prefix>... may follow`,
		"bob viewer app blog extra":                            `grant bob viewer app blog: "extra" after the scope`,
		"bob viewer box from":                                  "grant bob viewer box: from takes 1 to 16 addresses or prefixes",
		from16 + " 10.1.0.0/16":                                "grant bob viewer box: from lists 17 addresses or prefixes; a grant takes at most 16",
		"bob viewer box from 198.51.100.7/24":                  "grant bob viewer box: 198.51.100.7/24 has host bits set; write 198.51.100.0/24",
		"bob viewer box from 0.0.0.0/0":                        "grant bob viewer box: 0.0.0.0/0 would admit every address; leave from out instead",
		"bob viewer box from ::/0":                             "grant bob viewer box: ::/0 would admit every address",
		"bob viewer box from example.com":                      `grant bob viewer box: "example.com" is not an address or a prefix`,
		"bob viewer box from 10.0.0.0/33":                      `grant bob viewer box: "10.0.0.0/33" is not an address or a prefix`,
		"bob viewer box from 010.0.0.1":                        `grant bob viewer box: "010.0.0.1" is not an address or a prefix`,
		"bob viewer box from fe80::1%eth0":                     `grant bob viewer box: "fe80::1%eth0" has a zone; an address on a grant has none`,
		"bob viewer box from ::ffff:10.0.0.1":                  "grant bob viewer box: ::ffff:10.0.0.1 is an IPv4 address written as IPv6; write it as IPv4",
		"bob viewer box from ::ffff:10.0.0.0/104":              "is an IPv4 address written as IPv6",
		"bob viewer box from 10.0.0.0/8 nope":                  `grant bob viewer box: "nope" is not an address or a prefix`,
		"bob viewer box from 10.0.0.0/8 10.0.0.0/8":            "grant bob viewer box: from lists 10.0.0.0/8 twice",
		"bob viewer box from 198.51.100.7 198.51.100.7/32":     "grant bob viewer box: from lists 198.51.100.7 twice",
		"bob viewer app blog from 2001:db8::/32 2001:DB8::/32": "grant bob viewer app blog: from lists 2001:db8::/32 twice",
	} {
		_, err := ParseGrant(strings.Fields(line))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", line, err, want)
		}
	}
	for line, want := range map[string]Role{"bob viewer box": Viewer, "bob logs box": Logs, "bob logs app blog from 10.0.0.0/8": Logs} {
		if g, err := ParseGrant(strings.Fields(line)); err != nil || g.Role != want {
			t.Errorf("%s: %+v %v, want role %s", line, g, err, want)
		}
	}
	long := strings.Repeat("a", MaxName)
	if _, err := ParseGrant([]string{long, "viewer", "box"}); err != nil {
		t.Errorf("a name of %d bytes: %v", MaxName, err)
	}
	if _, err := ParseGrant([]string{long + "a", "viewer", "box"}); err == nil || !strings.Contains(err.Error(), "is longer than 256 bytes") {
		t.Errorf("a name of %d bytes: %v", MaxName+1, err)
	}
}

// fixture builds Lines from short lines: "signer <name> <type> <key>",
// "person <name> <type> <key>" and "grant <args>...".
func fixture(t *testing.T, lines ...string) Lines {
	t.Helper()
	var l Lines
	for _, line := range lines {
		f := strings.Fields(line)
		switch f[0] {
		case "signer":
			wire, err := base64.StdEncoding.DecodeString(f[3])
			if err != nil {
				t.Fatal(err)
			}
			l.Keys = append(l.Keys, SignerKey(f[1], f[2], f[3], wire))
		case "person":
			k, err := ParsePerson(f[1:])
			if err != nil {
				t.Fatal(err)
			}
			l.Keys = append(l.Keys, k)
		case "grant":
			g, err := ParseGrant(f[1:])
			if err != nil {
				t.Fatal(err)
			}
			l.Grants = append(l.Grants, g)
		}
	}
	return l
}

// TestCheckLines is each rule across lines, taken and refused.
func TestCheckLines(t *testing.T) {
	k1, k2, k3, k4, rsa1, rsa2 := edKey(t), skEdKey(t), skEdKey(t), edKey(t), rsaKey(t), rsaKey(t)
	ed, edSK, edSKOther := edForms(t)
	apps := &Around{Apps: []string{"example", "blog"}, Channels: true}
	noWarning := []string(nil)
	for name, c := range map[string]struct {
		lines    []string
		around   *Around
		want     string   // the refusal, "" for taken
		warnings []string // when taken
	}{
		"signers alone are not checked": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "signer alice ssh-ed25519 " + k1}, around: apps, warnings: noWarning,
		},
		"a person who is not a signer, granted": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "person bob sk-ssh-ed25519@openssh.com " + k2, "grant bob viewer app example"}, around: apps, warnings: noWarning,
		},
		"a second key for a signer's name": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "person alice sk-ssh-ed25519@openssh.com " + k3, "grant alice viewer box"}, around: apps, warnings: noWarning,
		},
		"a signer granted with no person line": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "grant alice viewer box"}, around: apps, warnings: noWarning,
		},
		"a signer with no grant is not warned": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "person bob sk-ssh-ed25519@openssh.com " + k2, "grant bob viewer box"}, around: apps, warnings: noWarning,
		},

		"the same key, a signer and a person": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "person bob ssh-ed25519 " + k1}, want: "signer alice and person bob are the same key",
		},
		"the same key, the person first": {
			lines: []string{"person bob ssh-ed25519 " + k1, "signer alice ssh-ed25519 " + k1}, want: "signer alice and person bob are the same key",
		},
		"the same key, two people": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "person bob sk-ssh-ed25519@openssh.com " + k2, "person carol sk-ssh-ed25519@openssh.com " + k2}, want: "person bob and person carol are the same key",
		},
		"the same key, one name twice": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "person alice ssh-ed25519 " + k1}, want: "signer alice and person alice are the same key",
		},

		"one Ed25519 key, plain and sk": {
			lines: []string{"signer alice ssh-ed25519 " + ed, "person bob sk-ssh-ed25519@openssh.com " + edSK},
			want:  "signer alice and person bob are the same key, written as ssh-ed25519 and as sk-ssh-ed25519@openssh.com",
		},
		"one Ed25519 key, two sk applications": {
			lines: []string{"person bob sk-ssh-ed25519@openssh.com " + edSK, "person carol sk-ssh-ed25519@openssh.com " + edSKOther},
			want:  "person bob and person carol are the same key",
		},
		"one Ed25519 key, sk and plain, two people": {
			lines: []string{"person bob sk-ssh-ed25519@openssh.com " + edSK, "person carol ssh-ed25519 " + ed},
			want:  "person bob and person carol are the same key, written as sk-ssh-ed25519@openssh.com and as ssh-ed25519",
		},
		"one signer key in two forms, beside a person": {
			lines: []string{"signer alice ssh-ed25519 " + ed, "signer bob sk-ssh-ed25519@openssh.com " + edSK, "person carol ssh-ed25519 " + k4, "grant carol viewer box"},
			want:  "signer alice and signer bob are the same key, written as ssh-ed25519 and as sk-ssh-ed25519@openssh.com",
		},

		"a grant for no one": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "grant carol viewer box"}, want: "grant carol viewer box: no person or signer line declares carol",
		},
		"a grant for an ssh-rsa signer alone": {
			lines: []string{"signer carol ssh-rsa " + rsa1, "grant carol viewer box"},
			want:  "grant carol viewer box: carol has no key a person's token can use (an ssh-rsa signer key signs commits only); add a person line with an ed25519 key",
		},
		"an ssh-rsa signer with a person key": {
			lines: []string{"signer carol ssh-rsa " + rsa1, "person carol ssh-ed25519 " + k1, "grant carol viewer box"}, around: apps, warnings: noWarning,
		},

		"a grant on an app the file runs": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app blog"}, around: apps, warnings: noWarning,
		},
		"a grant on an app the file does not run": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app shop"}, around: apps,
			want: "grant bob viewer app shop: the liveswap block runs no app shop",
		},
		"a grant on an app named through a placeholder": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app shop"}, around: &Around{Apps: []string{"example"}, AppPlaceholder: true},
			want: "grant bob viewer app shop: no app line in the liveswap block names shop as written; an app named through a placeholder cannot be granted",
		},
		"a grant on an app the file runs, beside a placeholder": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example"}, around: &Around{Apps: []string{"example"}, Channels: true, AppPlaceholder: true}, warnings: noWarning,
		},
		"a grant on an app, the apps unknown": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app shop"}, around: nil, warnings: noWarning,
		},

		"two grants on one app": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob viewer app example from 10.0.0.0/8"}, around: apps,
			want: "grant bob viewer app example: bob already has a viewer grant on app example; one per role and scope",
		},
		"two grants on the box": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer box", "grant bob viewer box"},
			want:  "grant bob viewer box: bob already has a viewer grant on the box; one per role and scope",
		},
		"two logs grants on one app": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob logs app example from 10.0.0.0/8", "grant bob logs app example"}, around: apps,
			want: "grant bob logs app example: bob already has a logs grant on app example; one per role and scope",
		},
		"two logs grants on the box": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob logs box", "grant bob logs box from 10.0.0.0/8"},
			want:  "grant bob logs box: bob already has a logs grant on the box; one per role and scope",
		},
		"a logs grant on an app the file does not run": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob logs app shop"}, around: apps,
			want: "grant bob logs app shop: the liveswap block runs no app shop",
		},
		"a logs grant for no one": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "grant carol logs box"}, want: "grant carol logs box: no person or signer line declares carol",
		},

		// viewer and logs on one scope: the office case, a from list on
		// the logs line alone, is taken with no word.
		"viewer from anywhere, logs from a list, on one app": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob logs app example from 203.0.113.0/24"}, around: apps, warnings: noWarning,
		},
		"viewer from anywhere, logs from a list, on the box": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob logs box from 203.0.113.0/24", "grant bob viewer box"}, around: apps, warnings: noWarning,
		},
		"viewer and logs on one app, both from lists": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example from 198.51.100.0/24", "grant bob logs app example from 203.0.113.0/24"}, around: apps, warnings: noWarning,
		},
		"a logs grant alone": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob logs app example"}, around: apps, warnings: noWarning,
		},
		"logs from anywhere covers viewer on one app": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example from 10.0.0.0/8", "grant bob logs app example"}, around: apps,
			warnings: []string{"grant bob viewer app example: the logs grant on app example already covers it"},
		},
		"logs on the box from anywhere covers viewer on the box": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer box", "grant bob logs box"}, around: apps,
			warnings: []string{"grant bob viewer box: the logs grant on the box already covers it"},
		},
		"logs on the box from anywhere covers viewer on an app": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob logs box"}, around: apps,
			warnings: []string{"grant bob viewer app example: the logs grant on the box already covers it"},
		},
		"logs on the box from anywhere covers logs on an app": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob logs app example from 10.0.0.0/8", "grant bob logs box"}, around: apps,
			warnings: []string{"grant bob logs app example: the logs grant on the box already covers it"},
		},
		"viewer on the box does not cover logs on an app": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob logs app example", "grant bob viewer box"}, around: apps, warnings: noWarning,
		},
		"viewer on an app does not cover logs on it": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob logs app example from 10.0.0.0/8", "grant bob viewer app example"}, around: apps, warnings: noWarning,
		},
		"logs on another app covers nothing": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob logs app blog"}, around: apps, warnings: noWarning,
		},
		"logs on the box from a list covers nothing": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob viewer box from 10.0.0.0/8", "grant bob logs box from 10.0.0.0/8", "grant bob logs app blog"}, around: apps, warnings: noWarning,
		},
		"viewer and logs on the box, both covering": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob viewer box", "grant bob logs box"}, around: apps,
			warnings: []string{"grant bob viewer app example: the box grant already covers it", "grant bob viewer box: the logs grant on the box already covers it"},
		},
		"two names on one app": {
			lines: []string{"person bob ssh-ed25519 " + k1, "person carol ssh-ed25519 " + k4, "grant bob viewer app example", "grant carol viewer app example"}, around: apps, warnings: noWarning,
		},
		"one name on two apps": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob viewer app blog"}, around: apps, warnings: noWarning,
		},

		"a signer's second key, no grant": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "person alice sk-ssh-ed25519@openssh.com " + k3}, around: apps, warnings: noWarning,
		},
		"a box grant and an app grant": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob viewer box"}, around: apps,
			warnings: []string{"grant bob viewer app example: the box grant already covers it"},
		},
		// Outside the box grant's list the app grant is all bob holds, so
		// removing it would take a read away: not told it adds nothing.
		"a box grant from a list and an app grant": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer app example", "grant bob viewer box from 198.51.100.0/24"}, around: apps, warnings: noWarning,
		},
		"a person with no grant": {
			lines: []string{"signer alice ssh-ed25519 " + k1, "person carol sk-ssh-ed25519@openssh.com " + k2, "person carol sk-ssh-ed25519@openssh.com " + k3}, around: apps,
			warnings: []string{"person carol has no grant: their key is told only who they are here (/_motel/whoami)"},
		},
		"two ssh-rsa signer keys for one name, warned once": {
			lines: []string{"signer carol ssh-rsa " + rsa1, "signer carol ssh-rsa " + rsa2, "person bob ssh-ed25519 " + k1, "grant bob viewer box"}, around: apps,
			warnings: []string{"signer carol: an ssh-rsa key signs commits but cannot sign a person's token; to reach this box over HTTPS, add a person line for carol with an ed25519 key (ssh-keygen -t ed25519)"},
		},
		"an ssh-rsa signer beside people": {
			lines: []string{"signer carol ssh-rsa " + rsa1, "person bob ssh-ed25519 " + k1, "grant bob viewer box"}, around: apps,
			warnings: []string{"signer carol: an ssh-rsa key signs commits but cannot sign a person's token; to reach this box over HTTPS, add a person line for carol with an ed25519 key (ssh-keygen -t ed25519)"},
		},
		"people and no monitor channel": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer box"}, around: &Around{},
			warnings: []string{"people are declared and no monitor channel is, so what they do on this box will be told to no one; add a channel line to the monitor block: channel <name> <kind> {secret.NOTIFY_<NAME>}"},
		},
		"people and no monitor known": {
			lines: []string{"person bob ssh-ed25519 " + k1, "grant bob viewer box"}, around: nil, warnings: noWarning,
		},
	} {
		t.Run(name, func(t *testing.T) {
			warnings, err := CheckLines(fixture(t, c.lines...), c.around)
			switch {
			case c.want != "":
				if err == nil || err.Error() != c.want {
					t.Fatalf("got %v, want %q", err, c.want)
				}
			case err != nil:
				t.Fatalf("refused: %v", err)
			case !slices.Equal(warnings, c.warnings):
				t.Fatalf("warnings %q, want %q", warnings, c.warnings)
			}
		})
	}
}

// TestCheckLinesLimits is each bound across lines, at the bound and one
// past it.
func TestCheckLinesLimits(t *testing.T) {
	var l Lines
	for i := range MaxKeyLines {
		k, err := ParsePerson([]string{fmt.Sprintf("p%d", i), "ssh-ed25519", edKey(t)})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, k)
	}
	l.Grants = []Grant{{Name: "p0", Role: Viewer}}
	if _, err := CheckLines(l, nil); err != nil {
		t.Fatalf("%d key lines: %v", MaxKeyLines, err)
	}
	more := l
	more.Keys = append(slices.Clone(l.Keys), SignerKey("alice", "ssh-ed25519", "x", []byte("x")))
	if _, err := CheckLines(more, nil); err == nil || err.Error() != "set of key lines: there are 1025 signer and person lines, and a box takes at most 1024" {
		t.Fatalf("%d key lines: %v", MaxKeyLines+1, err)
	}

	var grants []Grant
	for i := range MaxGrantLines {
		app := fmt.Sprintf("a%d", i/4)
		grants = append(grants, Grant{Name: fmt.Sprintf("p%d", i%4), Role: Viewer, App: app})
	}
	l.Grants = grants
	if _, err := CheckLines(l, nil); err != nil {
		t.Fatalf("%d grant lines: %v", MaxGrantLines, err)
	}
	l.Grants = append(slices.Clone(grants), Grant{Name: "p9", Role: Viewer})
	if _, err := CheckLines(l, nil); err == nil || err.Error() != "set of grant lines: there are 4097, and a box takes at most 4096" {
		t.Fatalf("%d grant lines: %v", MaxGrantLines+1, err)
	}
}

// fuzzArgs is one line's tokens from the fuzzer's bytes: NUL separates
// them, so a token may be empty or hold a space, as a quoted Caddyfile
// token may.
func fuzzArgs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

func FuzzParsePerson(f *testing.F) {
	f.Add("bob@example.com\x00ssh-ed25519\x00" + edKey(f))
	f.Add("bob\x00ecdsa-sha2-nistp256\x00" + ecdsaKey(f, elliptic.P256()))
	f.Add("bob\x00sk-ssh-ed25519@openssh.com\x00" + skEdKey(f))
	f.Add("bob\x00sk-ecdsa-sha2-nistp256@openssh.com\x00" + skECDSAB64(f))
	f.Add("bob\x00ssh-rsa\x00AAAA")
	f.Add("bob\x00ssh-ed25519")
	f.Fuzz(func(t *testing.T, s string) {
		k, err := ParsePerson(fuzzArgs(s))
		if err != nil {
			return
		}
		again, err := ParsePerson(k.Args())
		if err != nil {
			t.Fatalf("%q taken, its re-print %q refused: %v", s, k.Args(), err)
		}
		if !again.Equal(k) || string(again.Wire) != string(k.Wire) || again.Fingerprint != k.Fingerprint || !k.Person() {
			t.Fatalf("%+v re-reads as %+v", k, again)
		}
	})
}

func FuzzParseGrant(f *testing.F) {
	f.Add("bob\x00viewer\x00box")
	f.Add("bob\x00viewer\x00app\x00example")
	f.Add("bob\x00viewer\x00app\x00blog\x00from\x00198.51.100.0/24\x002001:db8:5::/48")
	f.Add("bob\x00viewer\x00box\x00from\x00198.51.100.7/32\x00::1\x002001:DB8::/32")
	f.Add("bob\x00signer\x00box")
	f.Add("bob\x00viewer\x00box\x00from\x00::ffff:1.2.3.4")
	f.Add("bob\x00logs\x00app\x00blog\x00from\x00203.0.113.0/24")
	f.Add("bob\x00logs\x00box")
	f.Fuzz(func(t *testing.T, s string) {
		g, err := ParseGrant(fuzzArgs(s))
		if err != nil {
			return
		}
		again, err := ParseGrant(g.Args())
		if err != nil {
			t.Fatalf("%q taken, its re-print %q refused: %v", s, g.Args(), err)
		}
		if !again.Equal(g) || !slices.Equal(again.Args(), g.Args()) || (g.Role != Viewer && g.Role != Logs) {
			t.Fatalf("%+v re-reads as %+v", g, again)
		}
	})
}

// TestEqual changes one field at a time: each makes two keys, two
// grants, two readings unequal.
func TestEqual(t *testing.T) {
	k, err := ParsePerson([]string{"bob", "ssh-ed25519", edKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	if same := k; !k.Equal(same) {
		t.Fatal("a key is not equal to a copy of itself")
	}
	for name, change := range map[string]func(*Key){
		"name":   func(k *Key) { k.Name = "carol" },
		"type":   func(k *Key) { k.Type = "sk-ssh-ed25519@openssh.com" },
		"base64": func(k *Key) { k.B64 = edKey(t) },
		"signer": func(k *Key) { k.Signer = true },
	} {
		o := k
		change(&o)
		if k.Equal(o) || o.Equal(k) {
			t.Errorf("keys differing in %s are equal", name)
		}
	}

	g, err := ParseGrant(strings.Fields("bob viewer app blog from 10.0.0.0/8 2001:db8::/32"))
	if err != nil {
		t.Fatal(err)
	}
	if same := g; !g.Equal(same) {
		t.Fatal("a grant is not equal to a copy of itself")
	}
	for name, change := range map[string]func(*Grant){
		"name":        func(g *Grant) { g.Name = "carol" },
		"role":        func(g *Grant) { g.Role = "other" },
		"scope":       func(g *Grant) { g.App = "" },
		"app":         func(g *Grant) { g.App = "example" },
		"an address":  func(g *Grant) { g.From = []netip.Prefix{g.From[0], netip.MustParsePrefix("2001:db8::/48")} },
		"one fewer":   func(g *Grant) { g.From = g.From[:1] },
		"no from":     func(g *Grant) { g.From = nil },
		"one more":    func(g *Grant) { g.From = append(slices.Clone(g.From), netip.MustParsePrefix("192.0.2.0/24")) },
		"their order": func(g *Grant) { g.From = []netip.Prefix{g.From[1], g.From[0]} },
	} {
		o := g
		change(&o)
		if g.Equal(o) || o.Equal(g) {
			t.Errorf("grants differing in %s are equal", name)
		}
	}

	l := Lines{Keys: []Key{k}, Grants: []Grant{g}}
	if !l.Equal(Lines{Keys: []Key{k}, Grants: []Grant{g}}) {
		t.Fatal("a reading is not equal to itself")
	}
	other := k
	other.Name = "carol"
	otherGrant := g
	otherGrant.From = nil
	for name, o := range map[string]Lines{
		"a key":        {Keys: []Key{other}, Grants: []Grant{g}},
		"a grant":      {Keys: []Key{k}, Grants: []Grant{otherGrant}},
		"a key more":   {Keys: []Key{k, other}, Grants: []Grant{g}},
		"a grant more": {Keys: []Key{k}, Grants: []Grant{g, otherGrant}},
		"no grant":     {Keys: []Key{k}},
		"nothing":      {},
	} {
		if l.Equal(o) || o.Equal(l) {
			t.Errorf("readings differing in %s are equal", name)
		}
	}
}
