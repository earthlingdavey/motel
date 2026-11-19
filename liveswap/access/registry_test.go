package access_test

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
)

const box = "deploy.example.test"

// lines reads person and grant lines as a reader does, failing the test
// on a line either parser or CheckLines refuses.
func lines(t *testing.T, signers []accesstest.Key, people map[string]accesstest.Key, grants ...[]string) access.Lines {
	t.Helper()
	var l access.Lines
	for i, s := range signers {
		k, err := access.ParsePerson([]string{"signer" + string(rune('a'+i)), s.Type(), s.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, access.SignerKey(k.Name, k.Type, k.B64, k.Wire))
	}
	for name, k := range people {
		pk, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, pk)
	}
	for _, args := range grants {
		g, err := access.ParseGrant(args)
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	if _, err := access.CheckLines(l, nil); err != nil {
		t.Fatal(err)
	}
	return l
}

// TestRegistryLooksUpListedKeys: the registry is CheckToken's lookup. A
// token signed by a listed key verifies and names its person; one by a
// key no line lists is refused before any signature check of its own,
// and an ECDSA key's by its type; and a nil registry, a box with no
// people, admits no one.
func TestRegistryLooksUpListedKeys(t *testing.T) {
	alice, stranger, ecdsa := accesstest.Ed25519(), accesstest.SKEd25519(), accesstest.ECDSA()
	reg := access.NewRegistry(box, lines(t, nil, map[string]accesstest.Key{"alice": alice}, []string{"alice", "viewer", "box"}))
	now := time.Unix(1794657600, 0)
	check := func(reg *access.Registry, k accesstest.Key) (access.VerifiedToken, error) {
		return access.CheckToken(access.TokenRequest{Authorization: []string{k.Token(box, now, time.Minute)}, Method: "GET", Host: box}, reg.Identity(), now, reg.Lookup, reg.ListsType)
	}
	v, err := check(reg, alice)
	if err != nil {
		t.Fatalf("alice's token: %v", err)
	}
	if name, ok := reg.Name(v.Fingerprint); !ok || name != "alice" || v.Fingerprint != alice.Fingerprint() {
		t.Errorf("alice's token names %q (%v), key %s", name, ok, v.Fingerprint)
	}
	var ref *access.TokenRefusal
	if _, err := check(reg, stranger); !errors.As(err, &ref) || ref.Reason != "key-not-listed" || ref.Fingerprint != "" {
		t.Errorf("a stranger's token: %v (%+v), want key-not-listed naming no key", err, ref)
	}
	if _, err := check(reg, ecdsa); !errors.As(err, &ref) || ref.Reason != "key-type" || ref.Fingerprint != "" {
		t.Errorf("an ECDSA key's token: %v (%+v), want key-type naming no key", err, ref)
	}
	var none *access.Registry
	if _, err := check(none, alice); !errors.As(err, &ref) || ref.Reason != "key-not-listed" {
		t.Errorf("no registry: %v, want key-not-listed", err)
	}
	if pub, ok := none.Lookup(alice.Fingerprint()); ok || pub != nil {
		t.Error("a nil registry lists a key")
	}
	if none.Identity() != "" || none.Keys() != 0 {
		t.Error("a nil registry has an identity or keys")
	}
}

// TestRegistryListsPersonKeysOnly: a signer line's ed25519 key is a
// person's key once the block declares people, and a block of
// signers alone (no person and no grant line) lists nothing: its signers
// are not logins. A refusal after the key was found names the key.
func TestRegistryListsPersonKeysOnly(t *testing.T) {
	sig, bob := accesstest.Ed25519(), accesstest.SKEd25519()
	signersOnly := lines(t, []accesstest.Key{sig}, nil)
	if reg := access.NewRegistry(box, signersOnly); reg.Keys() != 0 {
		t.Errorf("a block of signers alone lists %d keys", reg.Keys())
	}
	reg := access.NewRegistry(box, lines(t, []accesstest.Key{sig}, map[string]accesstest.Key{"bob": bob}))
	if reg.Keys() != 2 {
		t.Fatalf("lists %d keys, want the signer's and bob's", reg.Keys())
	}
	if name, ok := reg.Name(sig.Fingerprint()); !ok || name != "signera" {
		t.Errorf("the signer's key names %q", name)
	}
	now := time.Unix(1794657600, 0)
	_, err := access.CheckToken(access.TokenRequest{Authorization: []string{bob.Token("other.example.test", now, time.Minute)}, Method: "GET", Host: box}, reg.Identity(), now, reg.Lookup, reg.ListsType)
	var ref *access.TokenRefusal
	if !errors.As(err, &ref) || ref.Reason != "aud" || ref.Fingerprint != bob.Fingerprint() {
		t.Errorf("a token for another box: %v (%+v), want aud naming bob's key", err, ref)
	}
}

// TestRegistryViews: what a person may view from an address is the
// grants whose list holds it, a grant without one from anywhere; a box
// grant covers every app; a person with grants and none from here is
// told apart from one with none at all.
func TestRegistryViews(t *testing.T) {
	alice, bob, carol := accesstest.Ed25519(), accesstest.Ed25519(), accesstest.Ed25519()
	reg := access.NewRegistry(box, lines(t, nil,
		map[string]accesstest.Key{"alice": alice, "bob": bob, "carol": carol},
		[]string{"alice", "viewer", "box", "from", "198.51.100.0/24"},
		[]string{"bob", "viewer", "app", "shop"},
		[]string{"bob", "viewer", "app", "blog", "from", "2001:db8::/32", "192.0.2.7"},
	))
	for _, c := range []struct {
		name, addr string
		box        bool
		apps       []string
		granted    bool
	}{
		{"alice", "198.51.100.9", true, nil, true},
		{"alice", "::ffff:198.51.100.9", true, nil, true},
		{"alice", "198.51.101.9", false, nil, true},
		{"alice", "", false, nil, true},
		{"bob", "192.0.2.7", false, []string{"blog", "shop"}, true},
		{"bob", "2001:db8::1", false, []string{"blog", "shop"}, true},
		{"bob", "fe80::1%eth0", false, []string{"shop"}, true},
		{"bob", "192.0.2.8", false, []string{"shop"}, true},
		{"carol", "192.0.2.7", false, nil, false},
		{"nobody", "192.0.2.7", false, nil, false},
	} {
		var addr netip.Addr
		if c.addr != "" {
			addr = netip.MustParseAddr(c.addr)
		}
		v, granted := reg.Views(c.name, addr)
		if v.Box != c.box || len(v.Apps) != len(c.apps) || granted != c.granted {
			t.Errorf("%s from %q: %+v granted %v, want box %v apps %v granted %v", c.name, c.addr, v, granted, c.box, c.apps, c.granted)
			continue
		}
		for i := range c.apps {
			if v.Apps[i] != c.apps[i] {
				t.Errorf("%s from %q: apps %v, want %v", c.name, c.addr, v.Apps, c.apps)
			}
		}
	}
	v, _ := reg.Views("alice", netip.MustParseAddr("198.51.100.9"))
	if !v.Covers("anything") || v.Covers("") {
		t.Error("a box view covers every app and no empty name")
	}
	var nilViews *access.Views
	if nilViews.Covers("shop") || !nilViews.Empty() {
		t.Error("no views cover something")
	}
}

// TestRegistryListsTypes: the registry says which key types it lists,
// for CheckToken; a nil registry and one listing no key list none.
func TestRegistryListsTypes(t *testing.T) {
	alice := accesstest.Ed25519()
	reg := access.NewRegistry(box, lines(t, nil, map[string]accesstest.Key{"alice": alice}, []string{"alice", "viewer", "box"}))
	if !reg.ListsType("ssh-ed25519") || reg.ListsType("ecdsa-sha2-nistp521") || reg.ListsType("sk-ssh-ed25519@openssh.com") {
		t.Error("an ed25519 registry lists the wrong types")
	}
	var none *access.Registry
	empty := access.NewRegistry(box, access.Lines{})
	for _, typ := range []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp521"} {
		if none.ListsType(typ) || empty.ListsType(typ) {
			t.Errorf("no registry, or an empty one, lists %s", typ)
		}
	}
}

// TestRegistryListsPersonKeyTypesOnly: a key type is listed only for a
// key a person may sign with: a key of a deferred ECDSA type, which no
// reader takes from a line, lists nothing even when a Key is made of
// one by hand.
func TestRegistryListsPersonKeyTypesOnly(t *testing.T) {
	bob, eve := accesstest.SKEd25519(), accesstest.P521()
	pk, err := access.ParsePerson([]string{"bob", bob.Type(), bob.B64()})
	if err != nil {
		t.Fatal(err)
	}
	ek := access.SignerKey("eve", eve.Type(), eve.B64(), eve.Public().Marshal())
	if ek.Person() {
		t.Errorf("a %s key is a person's", eve.Type())
	}
	reg := access.NewRegistry(box, access.Lines{Keys: []access.Key{pk, ek}})
	if !reg.ListsType(bob.Type()) || reg.ListsType(eve.Type()) || reg.Keys() != 1 {
		t.Errorf("lists sk-ed25519 %v, p521 %v, %d keys; want only sk-ed25519", reg.ListsType(bob.Type()), reg.ListsType(eve.Type()), reg.Keys())
	}
}
