package access

// The `person` and `grant` lines of the box block, and the rules that
// hold them together with its `signer` lines. Two readers take them
// through these functions: the box module's Caddyfile parser, so that a
// bad line fails `motel validate` and the load, and the box's walk of
// the installed file's raw tokens. One parser for both is what keeps
// the two from disagreeing about who is declared.
//
// In this release the lines are read and checked, and nothing admits a
// person: no handler reads them.

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"filippo.io/edwards25519"
	"golang.org/x/crypto/ssh"
)

// The bounds of "Limits": a box, not a directory. Key lines are the
// `signer` and `person` lines together.
const (
	MaxKeyLines   = 1024
	MaxGrantLines = 4096
	MaxFrom       = 16
	// MaxName is the signer principal's bound (box/proof's
	// MaxPrincipal): a name on a signer line and on a person line is
	// the same person, so both are held to one grammar.
	MaxName = 256
)

// lineNameRE is the signer principal's grammar (box/proof's principalRE),
// for the same reason as MaxName.
var lineNameRE = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)

// lineAppNameRE is liveswap's app-name grammar (liveswap/names.go). This
// package imports nothing of liveswap's, so it is written again here.
var lineAppNameRE = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// personKeyTypes are the key types a person may hold: no ssh-rsa. A
// person's token is verified by key type before anything else, and
// with no RSA there is no size floor to keep and no rsa-sha2 rule.
var personKeyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"sk-ssh-ed25519@openssh.com":         true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// Role is what a grant gives.
type Role string

// Viewer reads an app's status and deploys, and on the box also the
// box's status. The one role a grant may name in this release.
const Viewer Role = "viewer"

// Key is one key line: a `person` line, or a `signer` line, which also
// declares its name as a person.
type Key struct {
	Name string
	Type string
	B64  string // the base64 as written: its one canonical line
	Wire []byte // the wire-format public key the base64 decoded to
	// Fingerprint is SHA-256 of Wire as ssh-keygen -l prints it,
	// "SHA256:<unpadded base64>": what a token's key is looked up by.
	Fingerprint string
	// Signer is whether the line is a signer line.
	Signer bool

	// material is the public key itself, whatever form the line wrote
	// it in: an Ed25519 key is the same key as ssh-ed25519 and as
	// sk-ssh-ed25519@openssh.com under any application string, and an
	// ECDSA key the same as ecdsa-sha2-* and sk-ecdsa-sha2-*.
	material string
	// smallOrder is an Ed25519 key, in either form, that is no point or
	// a point of small order (unusableEd25519).
	smallOrder bool
}

// Person reports whether a person's token may be signed with the key.
// Every person line's key may; a signer line's unless it is ssh-rsa,
// which signs commits only, or an Ed25519 key of small order, which
// anyone can sign for.
func (k Key) Person() bool { return personKeyTypes[k.Type] && !k.smallOrder }

// identity is the key the one-key rule compares: its material, or its
// wire bytes for a Key not made by ParsePerson or SignerKey.
func (k Key) identity() string {
	if k.material != "" {
		return k.material
	}
	return "wire " + string(k.Wire)
}

// kind is the line's keyword, for messages.
func (k Key) kind() string {
	if k.Signer {
		return "signer"
	}
	return "person"
}

// Args are the line's arguments, as ParsePerson takes them.
func (k Key) Args() []string { return []string{k.Name, k.Type, k.B64} }

// Equal reports whether two keys are the same line.
func (k Key) Equal(o Key) bool {
	return k.Name == o.Name && k.Type == o.Type && k.B64 == o.B64 && k.Signer == o.Signer
}

// keyMaterial is the public key a wire form carries, for the one-key
// rule: the curve point of an Ed25519 or ECDSA key in either form, the
// wire bytes of any other.
func keyMaterial(wire []byte) string {
	pub, err := ssh.ParsePublicKey(wire)
	if err == nil {
		if cpk, ok := pub.(ssh.CryptoPublicKey); ok {
			switch k := cpk.CryptoPublicKey().(type) {
			case ed25519.PublicKey:
				return "ed25519 " + string(k)
			case *ecdsa.PublicKey:
				if point, err := k.Bytes(); err == nil {
					return "ecdsa " + k.Curve.Params().Name + " " + string(point)
				}
			}
		}
	}
	return "wire " + string(wire)
}

// keyFingerprint is SHA-256 of a key's wire bytes as ssh-keygen -l
// prints it.
func keyFingerprint(wire []byte) string {
	sum := sha256.Sum256(wire)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// SignerKey is a signer line as a key line. The line has already been
// checked by its own parser (box/proof's ParseSigner), which this
// package cannot import.
func SignerKey(name, keyType, b64 string, wire []byte) Key {
	return Key{Name: name, Type: keyType, B64: b64, Wire: wire, Fingerprint: keyFingerprint(wire), Signer: true, material: keyMaterial(wire), smallOrder: unusableEd25519(wire) != ""}
}

// unusableEd25519 is why wire, an Ed25519 key in either form, can be
// no person's key, "" when it can (or is not Ed25519): its bytes are no
// point on the curve, which nothing verifies under, or a point of small
// order (the identity, and the seven others whose multiple by the
// cofactor 8 is the identity), in any encoding, canonical or not: the
// points libsodium's crypto_sign_ed25519 refuses as public keys (its
// blocklist, ge25519_has_small_order). Under such a key a signature
// anyone can make without a private key (R the base point, S = 1, for
// the identity) verifies for every message with crypto/ed25519 and with
// ssh-keygen -Y verify.
func unusableEd25519(wire []byte) string {
	pub, err := ssh.ParsePublicKey(wire)
	if err != nil {
		return ""
	}
	cpk, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return ""
	}
	k, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	p, err := new(edwards25519.Point).SetBytes(k)
	if err != nil {
		return "its bytes are no point on the curve"
	}
	if new(edwards25519.Point).MultByCofactor(p).Equal(edwards25519.NewIdentityPoint()) == 1 {
		return "its point has small order, so anyone can sign for it"
	}
	return ""
}

// ParsePerson checks one person line's arguments, `<name> <key-type>
// <base64>`: the name's grammar and length, the key type against the
// list, and that the base64 is the one canonical line of a public key
// of that type.
func ParsePerson(args []string) (Key, error) {
	if len(args) != 3 {
		return Key{}, errors.New("person line: it takes <name> <key-type> <base64>")
	}
	name, keyType, b64 := args[0], args[1], args[2]
	if err := checkName("person", name); err != nil {
		return Key{}, err
	}
	if keyType == "ssh-rsa" {
		return Key{}, fmt.Errorf("person %s: a person's key is ssh-ed25519, ecdsa or the sk- form of either, not ssh-rsa (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)", name)
	}
	if !personKeyTypes[keyType] {
		return Key{}, fmt.Errorf("person %s: key type %s is not one of ssh-ed25519, sk-ssh-ed25519@openssh.com, ecdsa-sha2-nistp256, ecdsa-sha2-nistp384, ecdsa-sha2-nistp521, sk-ecdsa-sha2-nistp256@openssh.com", name, lineQuote(keyType))
	}
	wire, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return Key{}, fmt.Errorf("person %s: the key is not base64", name)
	}
	// The decoder ignores CR and LF, and a quoted Caddyfile token can
	// span lines: one key, one spelling.
	if base64.StdEncoding.EncodeToString(wire) != b64 {
		return Key{}, fmt.Errorf("person %s: the key is not one line of canonical base64", name)
	}
	pub, err := ssh.ParsePublicKey(wire)
	if err != nil {
		return Key{}, fmt.Errorf("person %s: the key does not parse as an SSH public key", name)
	}
	if pub.Type() != keyType {
		return Key{}, fmt.Errorf("person %s: the key is %s, not %s", name, lineQuote(pub.Type()), keyType)
	}
	// A token carries the key as its holder's software marshals it, and
	// is looked up by the fingerprint of those bytes: a line whose bytes
	// parse to the key but are not its marshalling would never match.
	if string(pub.Marshal()) != string(wire) {
		return Key{}, fmt.Errorf("person %s: the key is not in its one wire form", name)
	}
	if why := unusableEd25519(wire); why != "" {
		return Key{}, fmt.Errorf("person %s: the key is not a usable ed25519 key: %s", name, why)
	}
	return Key{Name: name, Type: keyType, B64: b64, Wire: wire, Fingerprint: keyFingerprint(wire), material: keyMaterial(wire)}, nil
}

// Grant is one grant line: a role for a name on the box or on one app,
// from anywhere or from the listed addresses.
type Grant struct {
	Name string
	Role Role
	// App is the app the grant is on, "" for the box.
	App string
	// From are the addresses the grant holds from, nil for any. A lone
	// address is its /32 or /128.
	From []netip.Prefix
}

// Scope is the grant's scope as the line writes it: "box" or "app
// <app>".
func (g Grant) Scope() string {
	if g.App == "" {
		return "box"
	}
	return "app " + g.App
}

// Args are the line's arguments, as ParseGrant takes them: a lone
// address printed as an address, a prefix as a prefix.
func (g Grant) Args() []string {
	args := []string{g.Name, string(g.Role)}
	args = append(args, strings.Fields(g.Scope())...)
	if len(g.From) > 0 {
		args = append(args, "from")
		for _, p := range g.From {
			args = append(args, printPrefix(p))
		}
	}
	return args
}

// printPrefix is a from entry in its one spelling: a lone address as
// an address, a prefix as a prefix.
func printPrefix(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// Equal reports whether two grants say the same.
func (g Grant) Equal(o Grant) bool {
	return g.Name == o.Name && g.Role == o.Role && g.App == o.App && slices.Equal(g.From, o.From)
}

// line is the grant as a refusal names it: everything but the address
// list.
func (g Grant) line() string { return "grant " + g.Name + " " + string(g.Role) + " " + g.Scope() }

// grantUsage is the grant line's shape.
const grantUsage = "grant line: it takes <name> <role> box|app <app> [from <prefix>...]"

// ParseGrant checks one grant line's arguments, `<name> <role> box
// [from <prefix>...]` or `<name> <role> app <app> [from <prefix>...]`.
// The only role is viewer; deployer, signer and root are refused by
// name, saying who holds each today.
func ParseGrant(args []string) (Grant, error) {
	if len(args) < 3 {
		return Grant{}, errors.New(grantUsage)
	}
	name, role := args[0], args[1]
	if err := checkName("grant", name); err != nil {
		return Grant{}, err
	}
	switch Role(role) {
	case Viewer:
	case "deployer":
		return Grant{}, fmt.Errorf("grant %s deployer: deployer is held by a deploy_trust block, not granted", name)
	case "signer":
		// The word is reserved: a later release may take `grant <name>
		// signer box` as a second spelling of a signer line, so this
		// says where the role is held, not that it never can be granted.
		return Grant{}, fmt.Errorf("grant %s signer: signer is held by a signer line (signer %s <key-type> <base64>); this release takes it from no grant", name, name)
	case "root":
		return Grant{}, fmt.Errorf("grant %s root: root is held by root's key on the box, not granted", name)
	default:
		return Grant{}, fmt.Errorf("grant %s %s: unknown role; the one role a grant gives is viewer", name, lineQuote(role))
	}
	g := Grant{Name: name, Role: Viewer}
	rest := args[3:]
	switch args[2] {
	case "box":
	case "app":
		if len(rest) == 0 {
			return Grant{}, fmt.Errorf("grant %s viewer app: app takes the app's name", name)
		}
		if !lineAppNameRE.MatchString(rest[0]) {
			return Grant{}, fmt.Errorf("grant %s viewer app %s: not an app name (lowercase letters, digits and -, at most 63)", name, lineQuote(rest[0]))
		}
		g.App, rest = rest[0], rest[1:]
	default:
		return Grant{}, fmt.Errorf("grant %s viewer %s: the scope is box or app <app>", name, lineQuote(args[2]))
	}
	if len(rest) == 0 {
		return g, nil
	}
	if rest[0] != "from" {
		return Grant{}, fmt.Errorf("%s: %s after the scope; only from <prefix>... may follow", g.line(), lineQuote(rest[0]))
	}
	list := rest[1:]
	switch {
	case len(list) == 0:
		return Grant{}, fmt.Errorf("%s: from takes 1 to %d addresses or prefixes", g.line(), MaxFrom)
	case len(list) > MaxFrom:
		return Grant{}, fmt.Errorf("%s: from lists %d addresses or prefixes; a grant takes at most %d", g.line(), len(list), MaxFrom)
	}
	for _, s := range list {
		p, err := parseFrom(s)
		if err != nil {
			return Grant{}, fmt.Errorf("%s: %w", g.line(), err)
		}
		// One spelling per line: an address and its /32, or a prefix
		// written twice, would be one entry read twice.
		if slices.Contains(g.From, p) {
			return Grant{}, fmt.Errorf("%s: from lists %s twice", g.line(), printPrefix(p))
		}
		g.From = append(g.From, p)
	}
	return g, nil
}

// parseFrom reads one entry of a from list: a prefix or an address,
// each with one meaning per spelling.
func parseFrom(s string) (netip.Prefix, error) {
	var p netip.Prefix
	if strings.Contains(s, "/") {
		var err error
		if p, err = netip.ParsePrefix(s); err != nil {
			return netip.Prefix{}, fmt.Errorf("%s is not an address or a prefix", lineQuote(s))
		}
		if p.Bits() == 0 {
			// It would read as a limit and be none.
			return netip.Prefix{}, fmt.Errorf("%s would admit every address; leave from out instead", s)
		}
		if p != p.Masked() {
			return netip.Prefix{}, fmt.Errorf("%s has host bits set; write %s", s, p.Masked())
		}
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%s is not an address or a prefix", lineQuote(s))
		}
		if a.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("%s has a zone; an address on a grant has none", lineQuote(s))
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	// The address a request comes from is compared unmapped, so an IPv4
	// network written in IPv6 would hold no request.
	if p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("%s is an IPv4 address written as IPv6; write it as IPv4", s)
	}
	return p, nil
}

func checkName(kind, name string) error {
	if !lineNameRE.MatchString(name) {
		return fmt.Errorf("%s name %s may only contain letters, digits and . _ @ + -", kind, lineQuote(name))
	}
	if len(name) > MaxName {
		return fmt.Errorf("%s name %s is longer than %d bytes", kind, lineQuote(name), MaxName)
	}
	return nil
}

// Lines are the box block's key and grant lines as one reader read
// them, each kind in file order.
type Lines struct {
	Keys   []Key // signer and person lines
	Grants []Grant
}

// Declared reports whether the block has a person or a grant line. A
// block without one is read, checked and described exactly as before
// these lines existed: its signer lines alone add no person to what
// any reader says.
func (l Lines) Declared() bool {
	return len(l.Grants) > 0 || slices.ContainsFunc(l.Keys, func(k Key) bool { return !k.Signer })
}

// Equal reports whether two readings are the same lines in the same
// order.
func (l Lines) Equal(o Lines) bool {
	return slices.EqualFunc(l.Keys, o.Keys, Key.Equal) && slices.EqualFunc(l.Grants, o.Grants, Grant.Equal)
}

// Around is what a reader knows of the file outside the box block. The
// box module's parser sees only the block and has none; the walk reads
// the whole file.
type Around struct {
	// Apps are the apps the liveswap block runs.
	Apps []string
	// Channels is whether the monitor block has a channel line.
	Channels bool
	// AppPlaceholder is whether an app line in the liveswap block names
	// its app through a placeholder, which the walk reads as written: no
	// grant can name that app.
	AppPlaceholder bool
}

// CheckLines holds the lines to the rules across lines, after the
// whole block is read: the bounds, one key on one line, a grant only
// for a declared name with a key a token can use, on an app the file
// runs (when around says which), and one grant per name and scope.
// Every refusal names the lines. What it takes with a doubt is
// returned as warnings, in file order. A block without a person or a
// grant line (Declared) is not checked: its signer lines are held to
// their own rules, as before.
func CheckLines(l Lines, around *Around) (warnings []string, err error) {
	if !l.Declared() {
		return nil, nil
	}
	if len(l.Keys) > MaxKeyLines {
		return nil, fmt.Errorf("set of key lines: there are %d signer and person lines, and a box takes at most %d", len(l.Keys), MaxKeyLines)
	}
	if len(l.Grants) > MaxGrantLines {
		return nil, fmt.Errorf("set of grant lines: there are %d, and a box takes at most %d", len(l.Grants), MaxGrantLines)
	}

	// One key, one line, whatever the kinds and the names: a key names
	// exactly one person, as it names exactly one signer.
	seen := make(map[string]Key, len(l.Keys))
	declared := map[string]bool{}  // a name with any key line
	personKey := map[string]bool{} // a name with a key a token can use
	signerName := map[string]bool{}
	for _, k := range l.Keys {
		if first, dup := seen[k.identity()]; dup {
			// The signer first: the module's reader holds the signer
			// lines apart from the person lines, the walk in file order,
			// and both name the pair alike.
			if k.Signer && !first.Signer {
				first, k = k, first
			}
			if first.Type != k.Type {
				return nil, fmt.Errorf("%s %s and %s %s are the same key, written as %s and as %s", first.kind(), first.Name, k.kind(), k.Name, first.Type, k.Type)
			}
			return nil, fmt.Errorf("%s %s and %s %s are the same key", first.kind(), first.Name, k.kind(), k.Name)
		}
		seen[k.identity()] = k
		declared[k.Name] = true
		if k.Person() {
			personKey[k.Name] = true
		}
		if k.Signer {
			signerName[k.Name] = true
		}
	}

	type scoped struct{ name, app string }
	granted := map[scoped]bool{}
	hasGrant := map[string]bool{}
	for _, g := range l.Grants {
		switch {
		case !declared[g.Name]:
			return nil, fmt.Errorf("%s: no person or signer line declares %s", g.line(), g.Name)
		case !personKey[g.Name]:
			return nil, fmt.Errorf("%s: %s has no key a person's token can use (an ssh-rsa signer key signs commits only); add a person line with an ed25519 or ecdsa key", g.line(), g.Name)
		case g.App != "" && around != nil && !slices.Contains(around.Apps, g.App):
			// A typo would grant nothing, and an app added later under
			// that name would inherit the grant.
			if around.AppPlaceholder {
				return nil, fmt.Errorf("%s: no app line in the liveswap block names %s as written; an app named through a placeholder cannot be granted", g.line(), g.App)
			}
			return nil, fmt.Errorf("%s: the liveswap block runs no app %s", g.line(), g.App)
		case granted[scoped{g.Name, g.App}]:
			where := "the box"
			if g.App != "" {
				where = "app " + g.App
			}
			return nil, fmt.Errorf("%s: %s already has a grant on %s; one per scope", g.line(), g.Name, where)
		}
		granted[scoped{g.Name, g.App}] = true
		hasGrant[g.Name] = true
	}

	// The warnings, in file order: the keys', then the grants', then the
	// box's.
	warned := map[string]bool{}
	for _, k := range l.Keys {
		switch {
		case k.Signer && k.Type == "ssh-rsa" && !personKey[k.Name] && !warned["signer "+k.Name]:
			warned["signer "+k.Name] = true
			warnings = append(warnings, fmt.Sprintf("signer %s: an ssh-rsa key signs commits but cannot be a person's key; for %s to be a person, add a person line with an ed25519 or ecdsa key (ssh-keygen -t ed25519)", k.Name, k.Name))
		case !k.Signer && !hasGrant[k.Name] && !signerName[k.Name] && !warned["person "+k.Name]:
			warned["person "+k.Name] = true
			warnings = append(warnings, fmt.Sprintf("person %s has no grant", k.Name))
		}
	}
	for _, g := range l.Grants {
		// With one people's role an app grant beside a box grant adds
		// nothing; it would matter only once a second role exists.
		if g.App != "" && granted[scoped{g.Name, ""}] {
			warnings = append(warnings, g.line()+": the box grant already covers it")
		}
	}
	if around != nil && !around.Channels && len(personKey) > 0 {
		warnings = append(warnings, "people are declared and no monitor channel is, so what they do on this box will be told to no one; add a channel line to the monitor block: channel <name> <kind> {secret.NOTIFY_<NAME>}")
	}
	return warnings, nil
}

// lineQuote is a value a line wrote, quoted and cut to a length a message
// can carry: it may be anything a token can hold.
func lineQuote(s string) string {
	const limit = 64
	if len(s) > limit {
		return strconv.QuoteToASCII(s[:limit]) + "..."
	}
	return strconv.QuoteToASCII(s)
}
