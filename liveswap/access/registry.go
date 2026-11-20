package access

import (
	"net/netip"
	"slices"
	"sync/atomic"

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
	// generation orders registries by when they were built: a later
	// load's is greater (Generation).
	generation uint64
	identity   string
	keys       map[string]listedKey
	types      map[string]bool
	grants     map[string][]Grant
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
// listed (Key.Person): an ssh-rsa signer key lists nothing. A grant
// is kept only for a name with a listed key. Lines without a person or
// a grant line declare no one (Lines.Declared), so a block of signers
// alone lists no key.
func NewRegistry(identity string, l Lines) *Registry {
	r := &Registry{generation: built.Add(1), identity: identity, keys: map[string]listedKey{}, types: map[string]bool{}, grants: map[string][]Grant{}}
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

// built counts the registries the process has built.
var built atomic.Uint64

// Generation is greater for a registry built later, 0 for a nil one: a
// reload's handlers and the old config's may serve side by side for a
// moment, and what keeps state by the newest load's keys (liveswap's
// record of the networks each key was used from) moves forward only.
func (r *Registry) Generation() uint64 {
	if r == nil {
		return 0
	}
	return r.generation
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

// Keys is how many keys the registry lists: for the journal at load,
// and for both webhooks, which open a person's path only when it is
// more than none.
func (r *Registry) Keys() int {
	if r == nil {
		return 0
	}
	return len(r.keys)
}

// Scopes is where one role is held: the box, which covers every app,
// or the apps named, sorted.
type Scopes struct {
	Box  bool
	Apps []string
}

// On reports whether the scopes include the app: the box, or the app
// itself. No scope includes the empty name.
func (s Scopes) On(app string) bool {
	return app != "" && (s.Box || slices.Contains(s.Apps, app))
}

func (s Scopes) empty() bool { return !s.Box && len(s.Apps) == 0 }

// Views is what a person may do from where one request came from, role
// by role: built by Registry.Views, from the grants whose address list
// holds the request's address. No role implies another here; what each
// action needs is decidePerson's.
type Views struct {
	Viewer Scopes
	Logs   Scopes
}

// Covers reports whether the views give a viewer's reads on the app: a
// viewer or a logs grant on the app or on the box.
func (v *Views) Covers(app string) bool {
	return v != nil && (v.Viewer.On(app) || v.Logs.On(app))
}

// CoversBox reports whether the views give a viewer's reads on the box:
// a viewer or a logs grant on the box.
func (v *Views) CoversBox() bool {
	return v != nil && (v.Viewer.Box || v.Logs.Box)
}

// ReadsLogs reports whether the views give the app's logs: a logs grant
// on the app or on the box. A viewer grant never does.
func (v *Views) ReadsLogs(app string) bool {
	return v != nil && v.Logs.On(app)
}

// Views is what name may do from addr: every grant of theirs whose
// address list, if it has one, holds addr, each in its own role. An
// address that is not valid (a request whose address could not be
// read) is held by no list, so only the grants without one apply.
// granted is whether name holds any grant at all, from anywhere: a
// person with grants and none left from addr is refused as a stranger
// is, and a person with none is still told who they are.
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
		var s *Scopes
		switch g.Role {
		case Viewer:
			s = &v.Viewer
		case Logs:
			s = &v.Logs
		default:
			continue // ParseGrant takes no other role
		}
		if g.App == "" {
			s.Box = true
		} else if !slices.Contains(s.Apps, g.App) {
			s.Apps = append(s.Apps, g.App)
		}
	}
	slices.Sort(v.Viewer.Apps)
	slices.Sort(v.Logs.Apps)
	return v, len(gs) > 0
}

// Empty reports whether the views give nothing.
func (v *Views) Empty() bool { return v == nil || (v.Viewer.empty() && v.Logs.empty()) }

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
