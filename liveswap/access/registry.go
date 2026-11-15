package access

import (
	"net/netip"
	"slices"

	"golang.org/x/crypto/ssh"
)

// Registry is who a box admits over HTTPS with a token their own key
// signs: each key a person may sign with, by its fingerprint, the name
// it belongs to, that name's grants, and the box's identity, which every
// token's aud and the request's Host must equal. The box app builds one
// at each load from the walk of the installed file (box/app.go), never
// from the running configuration, and both webhooks read it. A nil
// Registry admits no one.
type Registry struct {
	identity string
	keys     map[string]listedKey
	types    map[string]bool
	grants   map[string][]Grant
}

// listedKey is one key a token may be signed with, parsed once.
type listedKey struct {
	name string
	pub  ssh.PublicKey
}

// Source is what a Caddy app that builds the registry offers the
// handlers that read it: liveswap's webhook reaches the box app's
// through ctx.AppIfConfigured("box") and this interface, so liveswap
// imports nothing of box. A nil Registry admits no one.
type Source interface {
	Access() *Registry
}

// NewRegistry is the registry of lines the walk read and CheckLines
// took, for the box whose identity is identity (its box_webhook host,
// a lowercase bare hostname). Only the keys a person may sign with are
// listed (Key.Person): an ssh-rsa signer key, or an Ed25519 key of
// small order, lists nothing. A grant is kept only for a name with a
// listed key. Lines without a person or a grant line declare no one
// (Lines.Declared), so a block of signers alone lists no key.
func NewRegistry(identity string, l Lines) *Registry {
	r := &Registry{identity: identity, keys: map[string]listedKey{}, types: map[string]bool{}, grants: map[string][]Grant{}}
	if !l.Declared() {
		return r
	}
	for _, k := range l.Keys {
		if !k.Person() {
			continue
		}
		pub, err := ssh.ParsePublicKey(k.Wire)
		if err != nil || pub == nil {
			continue // parsed when the line was read; never here
		}
		r.keys[k.Fingerprint] = listedKey{name: k.Name, pub: pub}
		r.types[pub.Type()] = true
	}
	names := map[string]bool{}
	for _, k := range r.keys {
		names[k.name] = true
	}
	for _, g := range l.Grants {
		if names[g.Name] {
			r.grants[g.Name] = append(r.grants[g.Name], g)
		}
	}
	return r
}

// Identity is the box's identity, "" for a nil Registry, which no
// token's aud can equal.
func (r *Registry) Identity() string {
	if r == nil {
		return ""
	}
	return r.identity
}

// Lookup is the registry as CheckToken's KeyLookup: the listed key of
// a fingerprint. It never returns a nil key with true.
func (r *Registry) Lookup(fingerprint string) (ssh.PublicKey, bool) {
	if r == nil {
		return nil, false
	}
	k, ok := r.keys[fingerprint]
	if !ok || k.pub == nil {
		return nil, false
	}
	return k.pub, true
}

// ListsType reports whether some listed key is of keyType: CheckToken's
// ListedType. A nil Registry lists none.
func (r *Registry) ListsType(keyType string) bool {
	return r != nil && r.types[keyType]
}

// Name is the person a listed key belongs to.
func (r *Registry) Name(fingerprint string) (string, bool) {
	if r == nil {
		return "", false
	}
	k, ok := r.keys[fingerprint]
	return k.name, ok
}

// Keys is how many keys the registry lists, for the journal at load.
func (r *Registry) Keys() int {
	if r == nil {
		return 0
	}
	return len(r.keys)
}

// Views is what a person may view from where one request came from:
// the box, which covers every app, or the apps named, sorted. It is
// built by Registry.Views, from the grants whose address list holds the
// request's address.
type Views struct {
	Box  bool
	Apps []string
}

// Covers reports whether the views include the app.
func (v *Views) Covers(app string) bool {
	return v != nil && app != "" && (v.Box || slices.Contains(v.Apps, app))
}

// Views is what name may view from addr: every grant of theirs whose
// address list, if it has one, holds addr. An address that is not
// valid (a request whose address could not be read) is held by no
// list, so only the grants without one apply. granted is whether name
// holds any grant at all, from anywhere: a person with grants and none
// left from addr is refused as a stranger is, and a person with none is
// still told who they are.
func (r *Registry) Views(name string, addr netip.Addr) (v *Views, granted bool) {
	v = &Views{}
	if r == nil {
		return v, false
	}
	gs := r.grants[name]
	// The form a from entry is compared in: IPv4 as IPv4, and no zone,
	// which no prefix contains.
	addr = addr.Unmap().WithZone("")
	for _, g := range gs {
		if !g.holds(addr) {
			continue
		}
		if g.App == "" {
			v.Box = true
		} else if !slices.Contains(v.Apps, g.App) {
			v.Apps = append(v.Apps, g.App)
		}
	}
	slices.Sort(v.Apps)
	return v, len(gs) > 0
}

// Empty reports whether the views cover nothing.
func (v *Views) Empty() bool { return v == nil || (!v.Box && len(v.Apps) == 0) }

// holds reports whether the grant applies to a request from addr: a
// grant with no address list applies from anywhere.
func (g Grant) holds(addr netip.Addr) bool {
	if len(g.From) == 0 {
		return true
	}
	if !addr.IsValid() {
		return false
	}
	for _, p := range g.From {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
