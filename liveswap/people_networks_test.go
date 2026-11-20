package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

// heard is an Announcer that keeps what it is told.
type heard struct {
	mu  sync.Mutex
	got []Announcement
}

func (h *heard) Announce(a Announcement) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, a)
}

func (h *heard) take() []Announcement {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.got
	h.got = nil
	return out
}

// testNetworks is a record with an announcer that keeps what it is
// told, in file ("" for memory alone); its timers are stopped and its
// writes waited for when the test ends.
func testNetworks(t testing.TB, file string) (*PeopleNetworks, *heard) {
	t.Helper()
	k := newPeopleNetworks(file, time.Now)
	h := &heard{}
	k.announcer = h
	t.Cleanup(func() { k.settle(t, true) })
	return k, h
}

// settle waits until the record has read its file, has told what it
// was telling, and has no write in flight or, unless stop, due; with
// stop, a write still due is cancelled.
func (k *PeopleNetworks) settle(t testing.TB, stop bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		k.mu.Lock()
		busy := k.state == networksReading || k.telling > 0 || k.writing || (!stop && k.timer != nil)
		if !busy && stop && k.timer != nil {
			// A timer that has fired already has its flush to come,
			// which clears it: waited for, so none runs past the test.
			if busy = !k.timer.Stop(); !busy {
				k.timer = nil
			}
		}
		k.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the record did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

// nets is the record's networks of fp, most recent first.
func (k *PeopleNetworks) nets(fp string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	kn := k.keys[fp]
	if kn == nil {
		return nil
	}
	var out []string
	for _, s := range kn.nets {
		out = append(out, cmpOr(networkText(s.net), unknownNetwork))
	}
	return out
}

func cmpOr(s, or string) string {
	if s == "" {
		return or
	}
	return s
}

func addr(s string) netip.Addr {
	if s == "" {
		return netip.Addr{}
	}
	return netip.MustParseAddr(s)
}

// TestNetworkOf: a /24 for IPv4, a /48 for IPv6, IPv4 written as IPv6
// taken as IPv4, a zone dropped, and an address that is not valid the
// one network of its own.
func TestNetworkOf(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":                    "203.0.113.0/24",
		"203.0.113.255":                  "203.0.113.0/24",
		"::ffff:203.0.113.9":             "203.0.113.0/24",
		"2001:db8:5:ffff::1":             "2001:db8:5::/48",
		"2001:db8:5:1:2:3:4:5":           "2001:db8:5::/48",
		"fe80::1%eth0":                   "fe80::/48",
		"2001:db8::ffff:203.0.113.9%en0": "2001:db8::/48",
		"":                               "",
	} {
		if got := networkText(networkOf(addr(in))); got != want {
			t.Errorf("networkOf(%q) = %q, want %q", in, got, want)
		}
	}
	if networkOf(addr("::ffff:203.0.113.9")) != networkOf(addr("203.0.113.200")) {
		t.Error("an IPv4 address written as IPv6 is another network")
	}
}

// TestAnnouncementSummary: the sentences, as the channels and the
// journal give them, the fingerprint's start in them; however long the
// name and the address, every sentence stays within the channels' 200
// runes, its end whole.
func TestAnnouncementSummary(t *testing.T) {
	fp := "SHA256:VKOVexampleexampleexampleexampleexampleexa" // gitleaks:allow
	short := "SHA256:VKOVexamplee…"
	for _, c := range []struct {
		a    Announcement
		want string
	}{
		{Announcement{Name: EventPersonNewNetwork, Person: "alice@example.com", Key: fp, From: "203.0.113.0/24"},
			"alice@example.com (key " + short + ") used this box from 203.0.113.0/24 for the first time"},
		{Announcement{Name: EventPersonNewNetwork, Person: "alice@example.com", Key: fp, From: "2001:db8:5::/48", First: true},
			"alice@example.com (key " + short + ") used this box for the first time, from 2001:db8:5::/48"},
		{Announcement{Name: EventPersonNewNetwork, Person: "alice@example.com", Key: fp},
			"alice@example.com (key " + short + ") used this box from an address the box could not read for the first time"},
		{Announcement{Name: EventPersonOutsideList, Person: "alice@example.com", Key: fp, From: "203.0.113.9"},
			"a valid signature by alice@example.com (key " + short + ") from 203.0.113.9, outside every address list on their grants; refused"},
		{Announcement{Name: EventPersonOutsideList, Person: "alice@example.com", Key: fp},
			"a valid signature by alice@example.com (key " + short + ") from an address the box could not read, outside every address list on their grants; refused"},
	} {
		if got := c.a.Summary(); got != c.want {
			t.Errorf("Summary() =\n  %q\nwant\n  %q", got, c.want)
		}
		if safe := c.a.Safe(); !slices.Equal(safe, []string{"alice@example.com", short}) {
			t.Errorf("Safe() = %q", safe)
		}
	}
	name := strings.Repeat("a", access.MaxName)
	for _, c := range []struct {
		a   Announcement
		end string
	}{
		{Announcement{Name: EventPersonNewNetwork, Person: name, Key: fp, From: "ffff:ffff:ffff::/48"}, "ffff:ffff:ffff::/48 for the first time"},
		{Announcement{Name: EventPersonNewNetwork, Person: name, Key: fp, From: "ffff:ffff:ffff::/48", First: true}, "for the first time, from ffff:ffff:ffff::/48"},
		{Announcement{Name: EventPersonNewNetwork, Person: name, Key: fp}, "from an address the box could not read for the first time"},
		{Announcement{Name: EventPersonOutsideList, Person: name, Key: fp, From: "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}, "from ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff, outside every address list on their grants; refused"},
		{Announcement{Name: EventPersonOutsideList, Person: name, Key: fp}, "from an address the box could not read, outside every address list on their grants; refused"},
	} {
		got := c.a.Summary()
		if n := len([]rune(got)); n > 200 || !strings.HasSuffix(got, c.end) || !strings.Contains(got, strings.Repeat("a", 48)) || strings.Contains(got, strings.Repeat("a", 49)) {
			t.Errorf("a long name: %d runes, %q", n, got)
		}
		if safe := c.a.Safe(); !strings.Contains(got, safe[0]) || !strings.Contains(got, safe[1]) {
			t.Errorf("Safe() %q is not what the summary %q shows", safe, got)
		}
	}
}

// TestNewNetworkInMemory: a key's first use is told as its first, a
// network already seen is not told again, another network is; the
// sixteen most recently used are kept, and the seventeenth drops the one
// used longest ago, which is then told again.
func TestNewNetworkInMemory(t *testing.T) {
	people := newTestPeople(t)
	k, heard := testNetworks(t, "")
	core, logs := observer.New(zap.InfoLevel)
	lg := zap.New(core)
	fp := people.alice.Fingerprint()
	k.seen(people.reg, fp, "alice", addr("203.0.113.9"), lg)
	k.seen(people.reg, fp, "alice", addr("203.0.113.10"), lg)
	k.seen(people.reg, fp, "alice", addr("::ffff:203.0.113.11"), lg)
	got := heard.take()
	if len(got) != 1 || got[0] != (Announcement{Name: EventPersonNewNetwork, Person: "alice", Key: fp, From: "203.0.113.0/24", First: true}) {
		t.Fatalf("the first use: %+v", got)
	}
	if l := logs.FilterMessage("access: a key used from a new network").All(); len(l) != 1 || l[0].ContextMap()["first_use"] != true ||
		l[0].ContextMap()["network"] != "203.0.113.0/24" || l[0].ContextMap()["person"] != "alice" || l[0].ContextMap()["key"] != fp {
		t.Errorf("journal %v", logs.All())
	}
	k.seen(people.reg, fp, "alice", addr("2001:db8:5::1"), lg)
	if got := heard.take(); len(got) != 1 || got[0].From != "2001:db8:5::/48" || got[0].First {
		t.Fatalf("a second network: %+v", got)
	}
	// 2001:db8:5::/48 and 203.0.113.0/24 so far; fifteen more, then
	// 203.0.113.0/24 is the one used longest ago.
	k.seen(people.reg, fp, "alice", addr("2001:db8:5::2"), lg) // already first: nothing moves
	for i := range 15 {
		k.seen(people.reg, fp, "alice", addr(fmt.Sprintf("198.51.%d.1", i)), lg)
	}
	if n := k.nets(fp); len(n) != networksPerKey || n[len(n)-1] != "2001:db8:5::/48" || slices.Contains(n, "203.0.113.0/24") {
		t.Fatalf("kept %v", n)
	}
	heard.take()
	k.seen(people.reg, fp, "alice", addr("203.0.113.1"), lg)
	if got := heard.take(); len(got) != 1 || got[0].From != "203.0.113.0/24" || got[0].First {
		t.Errorf("a network dropped, used again: %+v", got)
	}
	// Used again, a kept network moves to the front and is not told.
	k.seen(people.reg, fp, "alice", addr("198.51.3.9"), lg)
	if n := k.nets(fp); n[0] != "198.51.3.0/24" || len(heard.take()) != 0 {
		t.Errorf("a kept network used again: %v", n)
	}
	// Another key of the box: its own record, and its own first use.
	k.seen(people.reg, people.carol.Fingerprint(), "carol", addr("203.0.113.9"), lg)
	if got := heard.take(); len(got) != 1 || !got[0].First || got[0].Person != "carol" {
		t.Errorf("carol's first use: %+v", got)
	}
}

// TestUnreadableAddressIsOneNetwork: every address the box could not
// read is one network, told once, as such.
func TestUnreadableAddressIsOneNetwork(t *testing.T) {
	people := newTestPeople(t)
	k, heard := testNetworks(t, "")
	fp := people.alice.Fingerprint()
	k.seen(people.reg, fp, "alice", netip.Addr{}, zap.NewNop())
	k.seen(people.reg, fp, "alice", netip.Addr{}, zap.NewNop())
	got := heard.take()
	if len(got) != 1 || got[0].From != "" || !strings.Contains(got[0].Summary(), "from an address the box could not read") {
		t.Fatalf("%+v", got)
	}
	if n := k.nets(fp); !slices.Equal(n, []string{unknownNetwork}) {
		t.Errorf("kept %v", n)
	}
}

// TestOutsideListOncePerHour: a valid signature from outside every list
// is told at most once per key an hour, with the address, and is no use
// of a network.
func TestOutsideListOncePerHour(t *testing.T) {
	people := newTestPeople(t)
	k, heard := testNetworks(t, "")
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	k.now = func() time.Time { return now }
	fp := people.alice.Fingerprint()
	core, logs := observer.New(zap.InfoLevel)
	k.outsideList(people.reg, fp, "alice", addr("203.0.113.9"), zap.New(core))
	now = now.Add(59 * time.Minute)
	k.outsideList(people.reg, fp, "alice", addr("198.51.100.1"), zap.New(core))
	k.outsideList(people.reg, people.carol.Fingerprint(), "carol", netip.Addr{}, zap.New(core))
	now = now.Add(time.Minute)
	k.outsideList(people.reg, fp, "alice", addr("::ffff:198.51.100.2"), zap.New(core))
	got := heard.take()
	want := []Announcement{
		{Name: EventPersonOutsideList, Person: "alice", Key: fp, From: "203.0.113.9"},
		{Name: EventPersonOutsideList, Person: "carol", Key: people.carol.Fingerprint()},
		{Name: EventPersonOutsideList, Person: "alice", Key: fp, From: "198.51.100.2"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("told %+v\nwant %+v", got, want)
	}
	if l := logs.FilterMessage("access: a valid signature from outside every address list (told at most once an hour per key)").All(); len(l) != 3 ||
		l[1].ContextMap()["address"] != unknownNetwork {
		t.Errorf("journal %v", logs.All())
	}
	if k.nets(fp) != nil {
		t.Errorf("an outside-list refusal is a use: %v", k.nets(fp))
	}
}

// TestNetworksRemovedKeyDropped: a newer registry that does not list a
// key drops it; a request a reload's old handlers still serve under the
// older registry, of that removed key, is neither recorded nor told;
// the key added back is told as a first use. An older registry drops
// nothing of what the newest lists.
func TestNetworksRemovedKeyDropped(t *testing.T) {
	people := newTestPeople(t)
	k, heard := testNetworks(t, "")
	fp, carol := people.alice.Fingerprint(), people.carol.Fingerprint()
	k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
	carolOnly := registryOf(t, map[string]accesstest.Key{"carol": people.carol})
	k.seen(carolOnly, carol, "carol", addr("203.0.113.9"), zap.NewNop())
	if k.nets(fp) != nil {
		t.Fatalf("alice, no longer listed, kept: %v", k.nets(fp))
	}
	if got := heard.take(); len(got) != 2 || got[1].Person != "carol" {
		t.Fatalf("told %+v", got)
	}
	// The old registry, after the new: alice's request is nothing, and
	// carol's, whom the newest lists, counts.
	k.seen(people.reg, fp, "alice", addr("198.51.100.9"), zap.NewNop())
	k.outsideList(people.reg, fp, "alice", addr("198.51.100.9"), zap.NewNop())
	k.seen(people.reg, carol, "carol", addr("198.51.100.9"), zap.NewNop())
	if k.nets(fp) != nil || k.nets(carol) == nil || len(k.nets(carol)) != 2 {
		t.Errorf("under the old registry: alice %v, carol %v", k.nets(fp), k.nets(carol))
	}
	if got := heard.take(); len(got) != 1 || got[0].Person != "carol" {
		t.Errorf("under the old registry, told %+v", got)
	}
	again := registryOf(t, map[string]accesstest.Key{"alice": people.alice, "carol": people.carol})
	k.seen(again, fp, "alice", addr("203.0.113.9"), zap.NewNop())
	if got := heard.take(); len(got) != 1 || !got[0].First || got[0].Person != "alice" {
		t.Errorf("alice added back: told %+v", got)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.outside) != 0 || len(k.keys) != 2 {
		t.Errorf("keys %v, outside %v", k.keys, k.outside)
	}
}

func registryOf(t testing.TB, keys map[string]accesstest.Key) *access.Registry {
	t.Helper()
	var l access.Lines
	for name, k := range keys {
		key, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	return access.NewRegistry(personBox, l)
}

// TestNetworksFile: a new network is written at once, 0600, in the
// file's form; a process that reads it tells only what the file did not
// hold, and a key's first use only when the file held none of its.
func TestNetworksFile(t *testing.T) {
	people := newTestPeople(t)
	file := filepath.Join(t.TempDir(), "people-networks.json")
	leftover := filepath.Join(filepath.Dir(file), ".people-networks-1234.tmp")
	if err := os.WriteFile(leftover, []byte("cut short"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, heard := testNetworks(t, file)
	fp, carol := people.alice.Fingerprint(), people.carol.Fingerprint()
	k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
	k.settle(t, false)
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a leftover temp file is still there: %v", err)
	}
	k.seen(people.reg, fp, "alice", addr("2001:db8:5::1"), zap.NewNop())
	k.seen(people.reg, fp, "alice", netip.Addr{}, zap.NewNop())
	k.settle(t, false)
	if got := heard.take(); len(got) != 3 || !got[0].First || got[1].First || got[2].First {
		t.Fatalf("told %+v", got)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"version":1,"keys":{"` + fp + `":["unknown","2001:db8:5::/48","203.0.113.0/24"]}}` + "\n"; string(b) != want {
		t.Fatalf("the file:\n%s\nwant\n%s", b, want)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v", fi.Mode(), err)
	}

	// A restart: what the file held is not told again; what it did not
	// is, alice's as a new network and carol's as her first use.
	k2, heard2 := testNetworks(t, file)
	core, logs := observer.New(zap.InfoLevel)
	k2.seen(people.reg, fp, "alice", addr("203.0.113.200"), zap.New(core))
	k2.seen(people.reg, fp, "alice", addr("198.51.100.1"), zap.New(core))
	k2.seen(people.reg, carol, "carol", addr("198.51.100.1"), zap.New(core))
	k2.settle(t, false)
	got := heard2.take()
	slices.SortFunc(got, func(a, b Announcement) int { return strings.Compare(a.Person, b.Person) })
	want := []Announcement{
		{Name: EventPersonNewNetwork, Person: "alice", Key: fp, From: "198.51.100.0/24"},
		{Name: EventPersonNewNetwork, Person: "carol", Key: carol, From: "198.51.100.0/24", First: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("after the restart, told %+v\nwant %+v", got, want)
	}
	if n := k2.nets(fp); !slices.Equal(n, []string{"198.51.100.0/24", "203.0.113.0/24", "unknown", "2001:db8:5::/48"}) {
		t.Errorf("merged %v", n)
	}
	if logs.FilterMessage("access: the record of the networks people's keys were used from could not be used; starting afresh, so each key's next use is told as from a new network").Len() != 0 {
		t.Errorf("journal %v", logs.All())
	}
	b, _ = os.ReadFile(file)
	var f networksFile
	if err := json.Unmarshal(b, &f); err != nil || len(f.Keys) != 2 || len(f.Keys[fp]) != 4 {
		t.Errorf("rewritten %s", b)
	}
}

// TestNetworksFileMissing: the first start with people, or the first
// after the upgrade: every key's next use is told as its first, and the
// journal says why once.
func TestNetworksFileMissing(t *testing.T) {
	people := newTestPeople(t)
	file := filepath.Join(t.TempDir(), "people-networks.json")
	k, heard := testNetworks(t, file)
	core, logs := observer.New(zap.InfoLevel)
	k.seen(people.reg, people.alice.Fingerprint(), "alice", addr("203.0.113.9"), zap.New(core))
	k.settle(t, false)
	if got := heard.take(); len(got) != 1 || !got[0].First {
		t.Errorf("told %+v", got)
	}
	if logs.FilterMessage("access: no record yet of the networks people's keys were used from; each key's first use from here on is told").Len() != 1 {
		t.Errorf("journal %v", logs.All())
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("not written: %v", err)
	}
}

// TestNetworksFileUnusable: a file that does not parse, of a newer
// version, of no version, too large, a link, or that cannot be read, is
// said once and replaced; the uses held for it are told; nothing
// crashes, and a request never waits on it.
func TestNetworksFileUnusable(t *testing.T) {
	people := newTestPeople(t)
	fp := people.alice.Fingerprint()
	for name, c := range map[string]struct {
		content string
		link    bool
		readErr error
		why     string
	}{
		"empty (a power loss after the rename)": {content: "", why: "not JSON"},
		"not JSON":                              {content: "{", why: "not JSON"},
		"a newer moteld's":                      {content: `{"version":2,"keys":{}}`, why: "from a newer moteld (version 2; this one reads 1)"},
		"no version":                            {content: `{"keys":{}}`, why: "its version is 0, not 1"},
		"too large":                             {content: `{"version":1,"keys":{"x":["` + strings.Repeat("a", networksMaxBytes) + `"]}}`, why: "larger than"},
		"a link":                                {link: true, why: "is a link"},
		"cannot be read (EIO)":                  {readErr: errors.New("input/output error"), why: "input/output error"},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "people-networks.json")
			switch {
			case c.link:
				target := filepath.Join(t.TempDir(), "elsewhere.json")
				if err := os.WriteFile(target, []byte(`{"version":1,"keys":{"`+fp+`":["203.0.113.0/24"]}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, file); err != nil {
					t.Fatal(err)
				}
			case c.readErr == nil:
				if err := os.WriteFile(file, []byte(c.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			k, heard := testNetworks(t, file)
			if c.readErr != nil {
				k.read = func(string) ([]byte, error) { return nil, c.readErr }
			}
			core, logs := observer.New(zap.InfoLevel)
			k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.New(core))
			k.settle(t, false)
			lines := logs.FilterMessage("access: the record of the networks people's keys were used from could not be used; starting afresh, so each key's next use is told as from a new network").All()
			if len(lines) != 1 || !strings.Contains(lines[0].ContextMap()["error"].(string), c.why) {
				t.Errorf("journal %v", logs.All())
			}
			if got := heard.take(); len(got) != 1 || !got[0].First || got[0].From != "203.0.113.0/24" {
				t.Errorf("told %+v", got)
			}
			if c.readErr != nil {
				return
			}
			b, err := os.ReadFile(file)
			if fi, lerr := os.Lstat(file); err != nil || lerr != nil || !fi.Mode().IsRegular() ||
				string(b) != `{"version":1,"keys":{"`+fp+`":["203.0.113.0/24"]}}`+"\n" {
				t.Errorf("replaced with %q (%v, %v)", b, err, lerr)
			}
		})
	}
}

// TestNetworksFileHangs: a read that does not come back holds no
// request, and past the wait the held uses are told and the journal
// says why; the read that comes back after is dropped.
func TestNetworksFileHangs(t *testing.T) {
	people := newTestPeople(t)
	file := filepath.Join(t.TempDir(), "people-networks.json")
	k, heard := testNetworks(t, file)
	k.wait = time.Second
	release := make(chan struct{})
	k.read = func(string) ([]byte, error) {
		<-release
		return []byte(`{"version":1,"keys":{"` + people.alice.Fingerprint() + `":["198.51.100.0/24"],"` + people.carol.Fingerprint() + `":["198.51.100.0/24"]}}`), nil
	}
	core, logs := observer.New(zap.InfoLevel)
	began := time.Now()
	for i := range 50 {
		k.seen(people.reg, people.alice.Fingerprint(), "alice", addr(fmt.Sprintf("10.0.%d.1", i)), zap.New(core))
	}
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Errorf("fifty requests took %s while the file was being read", took)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(logs.FilterMessage("access: a key used from a new network").All()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("nothing told past the wait: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if l := logs.FilterMessageSnippet("could not be used").All(); len(l) != 1 || !strings.Contains(l[0].ContextMap()["error"].(string), "not read within 1s") {
		t.Errorf("journal %v", logs.All())
	}
	// The read that comes back after the wait is dropped: neither
	// network it held is kept.
	close(release)
	time.Sleep(300 * time.Millisecond) // the late read's own goroutine, done
	k.settle(t, false)
	if got := heard.take(); len(got) != 50 || !got[0].First || got[0].From != "10.0.0.0/24" || got[49].From != "10.0.49.0/24" {
		t.Errorf("told %d: %+v", len(got), got)
	}
	if n := k.nets(people.alice.Fingerprint()); len(n) != networksPerKey || slices.Contains(n, "198.51.100.0/24") || k.nets(people.carol.Fingerprint()) != nil {
		t.Errorf("kept %v, carol %v", n, k.nets(people.carol.Fingerprint()))
	}
}

// TestNetworksHeldUntilRead: uses before the file has been read are told
// once it has, each network the file did not hold, oldest first.
func TestNetworksHeldUntilRead(t *testing.T) {
	people := newTestPeople(t)
	fp := people.alice.Fingerprint()
	file := filepath.Join(t.TempDir(), "people-networks.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"keys":{"`+fp+`":["198.51.100.0/24","not a network","203.0.113.0/24"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	k, heard := testNetworks(t, file)
	release := make(chan struct{})
	read := k.read
	k.read = func(p string) ([]byte, error) { <-release; return read(p) }
	for _, a := range []string{"203.0.113.9", "2001:db8:5::1", "2001:db8:6::1", "198.51.100.9"} {
		k.seen(people.reg, fp, "alice", addr(a), zap.NewNop())
	}
	// carol, in a burst from more networks than a key keeps: none is
	// dropped untold while the file is read.
	for i := range 20 {
		k.seen(people.reg, people.carol.Fingerprint(), "carol", addr(fmt.Sprintf("198.51.%d.1", i)), zap.NewNop())
	}
	if got := heard.take(); len(got) != 0 {
		t.Fatalf("told before the read: %+v", got)
	}
	close(release)
	k.settle(t, false)
	var got, carols []Announcement
	for _, a := range heard.take() {
		if a.Person == "carol" {
			carols = append(carols, a)
		} else {
			got = append(got, a)
		}
	}
	if len(got) != 2 || got[0].From != "2001:db8:5::/48" || got[1].From != "2001:db8:6::/48" || got[0].First || got[1].First {
		t.Fatalf("told %+v", got)
	}
	if len(carols) != 20 || !carols[0].First || carols[0].From != "198.51.0.0/24" || carols[19].From != "198.51.19.0/24" || carols[1].First {
		t.Errorf("carol's burst: told %+v", carols)
	}
	if n := k.nets(people.carol.Fingerprint()); len(n) != networksPerKey || n[0] != "198.51.19.0/24" {
		t.Errorf("carol kept %v", n)
	}
	if n := k.nets(fp); !slices.Equal(n, []string{"198.51.100.0/24", "2001:db8:6::/48", "2001:db8:5::/48", "203.0.113.0/24"}) {
		t.Errorf("kept %v", n)
	}
	b, _ := os.ReadFile(file)
	if strings.Contains(string(b), "not a network") {
		t.Errorf("not rewritten: %s", b)
	}
}

// TestNetworksFileDropsUnlisted: a key the newest registry does not list
// is left out of what is kept and written.
func TestNetworksFileDropsUnlisted(t *testing.T) {
	people := newTestPeople(t)
	file := filepath.Join(t.TempDir(), "people-networks.json")
	gone := accesstest.Ed25519().Fingerprint()
	if err := os.WriteFile(file, []byte(`{"version":1,"keys":{"`+gone+`":["203.0.113.0/24"],"`+people.carol.Fingerprint()+`":["203.0.113.0/24"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	k, _ := testNetworks(t, file)
	k.seen(people.reg, people.alice.Fingerprint(), "alice", addr("203.0.113.9"), zap.NewNop())
	k.settle(t, false)
	b, _ := os.ReadFile(file)
	if strings.Contains(string(b), gone) || !strings.Contains(string(b), people.carol.Fingerprint()) {
		t.Errorf("written %s", b)
	}
}

// TestNetworksWriteFails: a write that fails (a full disk, EIO at the
// rename) is said once, the record kept in memory and right, tried
// again each minute (no sooner for a new network), and said again when
// it is written.
func TestNetworksWriteFails(t *testing.T) {
	people := newTestPeople(t)
	file := filepath.Join(t.TempDir(), "people-networks.json")
	k, heard := testNetworks(t, file)
	k.every = time.Hour // the retry comes when the test asks for it
	var mu sync.Mutex
	fail, writes := true, 0
	k.write = func(path string, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		writes++
		if fail {
			return errors.New("no space left on device")
		}
		return os.WriteFile(path, data, 0o600)
	}
	core, logs := observer.New(zap.InfoLevel)
	fp := people.alice.Fingerprint()
	k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.New(core))
	deadline := time.Now().Add(5 * time.Second)
	for logs.FilterMessageSnippet("could not be written").Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for i := range 20 {
		k.seen(people.reg, fp, "alice", addr(fmt.Sprintf("198.51.%d.1", i)), zap.New(core))
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if writes != 1 {
		t.Errorf("%d writes within the minute after a failure", writes)
	}
	fail = false
	mu.Unlock()
	// The minute's retry, now.
	k.mu.Lock()
	if k.timer != nil && k.timer.Stop() {
		k.timer = nil
	}
	k.mu.Unlock()
	k.flush()
	k.settle(t, false)
	if l := logs.FilterMessageSnippet("could not be written").All(); len(l) != 1 || !strings.Contains(l[0].ContextMap()["error"].(string), "no space left") {
		t.Errorf("journal %v", logs.All())
	}
	if logs.FilterMessage("access: the record of the networks people's keys were used from is written again").Len() != 1 {
		t.Errorf("journal %v", logs.All())
	}
	if len(heard.take()) != 21 {
		t.Error("a failed write changed what is told")
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), "198.51.19.0/24") {
		t.Errorf("written %s", b)
	}
}

// TestNetworksReorderWithinTheMinute: a change of order alone waits its
// minute after the last write; a new network does not.
func TestNetworksReorderWithinTheMinute(t *testing.T) {
	people := newTestPeople(t)
	file := filepath.Join(t.TempDir(), "people-networks.json")
	k, _ := testNetworks(t, file)
	k.every = 3 * time.Second
	var mu sync.Mutex
	var at []time.Time
	k.write = func(path string, data []byte) error {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return writeOwnFile(path, networksPattern, data)
	}
	fp := people.alice.Fingerprint()
	k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
	k.settle(t, false)
	k.seen(people.reg, fp, "alice", addr("198.51.100.9"), zap.NewNop())
	k.settle(t, false)
	k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop()) // a reorder alone
	k.settle(t, false)
	mu.Lock()
	defer mu.Unlock()
	// The new network is written within networksUrgentEvery of the
	// last write, not the 3 s of a reorder; the reorder waits its 3 s.
	if len(at) != 3 || at[1].Sub(at[0]) > 2500*time.Millisecond || at[1].Sub(at[0]) < 900*time.Millisecond || at[2].Sub(at[1]) < 2900*time.Millisecond {
		t.Errorf("writes at %v", at)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), `["203.0.113.0/24","198.51.100.0/24"]`) {
		t.Errorf("written %s", b)
	}
}

// TestNetworksFlushAtStop: a stop writes what is due at once, a change
// of order that would wait its minute among it, and waits for it.
func TestNetworksFlushAtStop(t *testing.T) {
	people := newTestPeople(t)
	file := filepath.Join(t.TempDir(), "people-networks.json")
	k, _ := testNetworks(t, file)
	k.every = time.Hour
	fp := people.alice.Fingerprint()
	k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
	k.settle(t, false)
	k.seen(people.reg, fp, "alice", addr("198.51.100.9"), zap.NewNop()) // due within the second
	k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())  // a reorder: due within the hour
	began := time.Now()
	k.flushWithin(5 * time.Second)
	if took := time.Since(began); took > 900*time.Millisecond {
		t.Errorf("the flush took %s", took)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), `["203.0.113.0/24","198.51.100.0/24"]`) {
		t.Errorf("written %s", b)
	}
	// With nothing due, and with no file, it returns at once; and while
	// writes fail, it tries no write and leaves the minute's retry.
	began = time.Now()
	k.flushWithin(5 * time.Second)
	newPeopleNetworks("", time.Now).flushWithin(5 * time.Second)
	var mu sync.Mutex
	writes := 0
	k.write = func(string, []byte) error {
		mu.Lock()
		writes++
		mu.Unlock()
		return errors.New("no space left on device")
	}
	k.seen(people.reg, fp, "alice", addr("192.0.2.9"), zap.NewNop())
	k.flushWithin(5 * time.Second) // the write that fails
	k.flushWithin(5 * time.Second) // nothing more while they fail
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("flushes of nothing, or while writes fail, took %s", took)
	}
	mu.Lock()
	defer mu.Unlock()
	if writes != 1 {
		t.Errorf("%d writes tried while they fail", writes)
	}
}

// TestNetworksFlushWaitsForTheRead: a stop while the file is being read
// waits for the read (within its bound), so the held uses are told and
// written then, not at their next use after the restart.
func TestNetworksFlushWaitsForTheRead(t *testing.T) {
	people := newTestPeople(t)
	file := filepath.Join(t.TempDir(), "people-networks.json")
	k, heard := testNetworks(t, file)
	release := make(chan struct{})
	read := k.read
	k.read = func(p string) ([]byte, error) { <-release; return read(p) }
	k.seen(people.reg, people.alice.Fingerprint(), "alice", addr("203.0.113.9"), zap.NewNop())
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	k.flushWithin(5 * time.Second)
	if got := heard.take(); len(got) != 1 || !got[0].First {
		t.Errorf("told %+v", got)
	}
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "203.0.113.0/24") {
		t.Errorf("written %q, %v", b, err)
	}
	// A read that does not come back holds a stop no longer than its
	// bound.
	k2, _ := testNetworks(t, filepath.Join(t.TempDir(), "people-networks.json"))
	k2.wait = 500 * time.Millisecond
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	k2.read = func(string) ([]byte, error) { <-hang; return nil, errors.New("hung") }
	k2.seen(people.reg, people.alice.Fingerprint(), "alice", addr("203.0.113.9"), zap.NewNop())
	began := time.Now()
	k2.flushWithin(200 * time.Millisecond)
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("a stop waited %s on a read that hangs", took)
	}
}

// steppingClock is a clock an hour later at each reading: no write
// waits for networksUrgentEvery after the last.
func steppingClock() func() time.Time {
	var mu sync.Mutex
	now := time.Now()
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Hour)
		return now
	}
}

// gatedNetwork is the network whose announcements a gated announcer
// holds.
const gatedNetwork = "203.0.113.0/24"

// gated is an Announcer that keeps what it is told and holds the
// announcements of gatedNetwork until opened: a journal or a notify
// slow at the moment a stop or a write comes.
type gated struct {
	heard
	all     bool // every network's announcements held, not gatedNetwork's alone
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// gate installs a gated announcer on k, opened when the test ends,
// before the record settles.
func gate(t testing.TB, k *PeopleNetworks) *gated {
	g := &gated{entered: make(chan struct{}, 1024), release: make(chan struct{})}
	k.mu.Lock()
	k.announcer = g
	k.mu.Unlock()
	t.Cleanup(g.open)
	return g
}

func (g *gated) Announce(a Announcement) {
	g.heard.Announce(a)
	if g.all || a.From == gatedNetwork {
		g.entered <- struct{}{}
		<-g.release
	}
}

func (g *gated) open() { g.once.Do(func() { close(g.release) }) }

// held waits until the gated network's announcement is being made.
func (g *gated) held(t testing.TB) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the announcement was never made")
	}
}

// TestNetworksStopWaitsForTheTelling: a stop while an announcement is
// being made (the file's load telling a held use, or a request telling
// its new network) waits for it, then for its write, so the exit cuts
// off neither; an announcement that does not end holds a stop no
// longer than its bound.
func TestNetworksStopWaitsForTheTelling(t *testing.T) {
	people := newTestPeople(t)
	fp := people.alice.Fingerprint()
	for _, c := range []struct {
		name string
		load bool
	}{{"the load's", true}, {"a request's", false}} {
		t.Run(c.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "people-networks.json")
			k, _ := testNetworks(t, file)
			k.now = steppingClock()
			g := gate(t, k)
			if !c.load {
				k.seen(people.reg, fp, "alice", addr("198.51.100.9"), zap.NewNop())
				k.settle(t, false)
				g.take()
			}
			go k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
			g.held(t)
			stopped := make(chan struct{})
			go func() { k.flushWithin(5 * time.Second); close(stopped) }()
			select {
			case <-stopped:
				t.Fatalf("the stop returned while the announcement was being made: told %+v", g.take())
			case <-time.After(200 * time.Millisecond):
			}
			g.open()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("the stop did not return once told")
			}
			if got := g.take(); len(got) != 1 || got[0].From != "203.0.113.0/24" || got[0].First != c.load {
				t.Errorf("told %+v", got)
			}
			if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "203.0.113.0/24") {
				t.Errorf("written %q, %v", b, err)
			}
		})
	}
	t.Run("one that does not end", func(t *testing.T) {
		k, _ := testNetworks(t, filepath.Join(t.TempDir(), "people-networks.json"))
		g := gate(t, k)
		go k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
		g.held(t)
		began := time.Now()
		k.flushWithin(200 * time.Millisecond)
		if took := time.Since(began); took > 2*time.Second {
			t.Errorf("a stop waited %s on an announcement that does not end", took)
		}
	})
	// A request's announcer that panics (the http server recovers the
	// request) leaves no stop waiting.
	t.Run("an announcer that panics", func(t *testing.T) {
		k, _ := testNetworks(t, filepath.Join(t.TempDir(), "people-networks.json"))
		k.now = steppingClock()
		for i := range networksPerKey {
			k.seen(people.reg, fp, "alice", addr(fmt.Sprintf("198.51.%d.1", i)), zap.NewNop())
		}
		k.settle(t, false)
		k.announcer = panics{}
		func() {
			defer func() { _ = recover() }()
			k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
		}()
		began := time.Now()
		k.flushWithin(5 * time.Second)
		if took := time.Since(began); took > time.Second {
			t.Errorf("a stop waited %s after an announcer panicked", took)
		}
		// Not told, so neither kept nor written: its next use tells it.
		// And the key's sixteen told before it are all kept, the one used
		// longest ago among them.
		if n := k.nets(fp); slices.Contains(n, gatedNetwork) || len(n) != networksPerKey || n[len(n)-1] != "198.51.0.0/24" {
			t.Errorf("kept %v", n)
		}
		k.mu.Lock()
		k.dirty = true
		k.mu.Unlock()
		k.flushWithin(5 * time.Second)
		if b, err := os.ReadFile(k.file); err != nil || strings.Contains(string(b), gatedNetwork) || !strings.Contains(string(b), "198.51.0.0/24") {
			t.Errorf("written %q, %v", b, err)
		}
		h := &heard{}
		k.mu.Lock()
		k.announcer = h
		k.mu.Unlock()
		k.seen(people.reg, fp, "alice", addr("203.0.113.10"), zap.NewNop())
		if got := h.take(); len(got) != 1 || got[0].From != gatedNetwork {
			t.Errorf("its next use: told %+v", got)
		}
	})
}

type panics struct{}

func (panics) Announce(Announcement) { panic("announcer") }

// TestNetworksToldBeforeWritten: tell, then remember. While a new
// network's announcement is being made (a held use at the load, or a
// request's), a write (another key's new network) leaves it out, so a
// crash then tells it again at the next start rather than never; once
// told, it is written.
func TestNetworksToldBeforeWritten(t *testing.T) {
	people := newTestPeople(t)
	fp, carol := people.alice.Fingerprint(), people.carol.Fingerprint()
	for _, c := range []struct {
		name string
		load bool
	}{{"the load's", true}, {"a request's", false}} {
		t.Run(c.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "people-networks.json")
			k, _ := testNetworks(t, file)
			k.now = steppingClock()
			writes := make(chan string, 64)
			k.write = func(path string, data []byte) error {
				writes <- string(data)
				return writeOwnFile(path, networksPattern, data)
			}
			g := gate(t, k)
			if !c.load {
				k.seen(people.reg, fp, "alice", addr("198.51.100.9"), zap.NewNop())
				k.settle(t, false)
				for len(writes) > 0 {
					<-writes
				}
				g.take()
			}
			go k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
			g.held(t)
			k.seen(people.reg, carol, "carol", addr("192.0.2.9"), zap.NewNop())
			select {
			case w := <-writes:
				if strings.Contains(w, "203.0.113.0/24") {
					t.Fatalf("written before it was told: %s", w)
				}
				if !strings.Contains(w, "192.0.2.0/24") {
					t.Errorf("carol's network, told, not written: %s", w)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("carol's network was never written")
			}
			g.open()
			k.settle(t, false)
			if got := g.take(); len(got) != 2 {
				t.Errorf("told %+v", got)
			}
			if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "203.0.113.0/24") || !strings.Contains(string(b), "192.0.2.0/24") {
				t.Errorf("once told, written %q, %v", b, err)
			}
		})
	}
}

// TestNetworksUntoldBounded: a key's requests from more new networks
// than a key holds, at once, while every announcement blocks, keep at
// most networksHeldPerKey of them in memory; once told, each is told
// and the key keeps its sixteen.
func TestNetworksUntoldBounded(t *testing.T) {
	people := newTestPeople(t)
	fp := people.alice.Fingerprint()
	k, _ := testNetworks(t, filepath.Join(t.TempDir(), "people-networks.json"))
	k.now = steppingClock()
	k.seen(people.reg, fp, "alice", addr("198.51.100.9"), zap.NewNop())
	k.settle(t, false)
	g := gate(t, k)
	g.all = true
	const burst = 2 * networksHeldPerKey
	var wg sync.WaitGroup
	for i := range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k.seen(people.reg, fp, "alice", addr(fmt.Sprintf("10.0.%d.1", i)), zap.NewNop())
		}()
	}
	for range burst {
		g.held(t)
	}
	if n := len(k.nets(fp)); n > networksHeldPerKey {
		t.Errorf("%d networks kept while their announcements block", n)
	}
	g.open()
	wg.Wait()
	k.settle(t, false)
	if got := g.take(); len(got) != burst {
		t.Errorf("told %d of %d", len(got), burst)
	}
	if n := len(k.nets(fp)); n != networksPerKey {
		t.Errorf("kept %d", n)
	}
}

// TestNetworksToldOnceAtOnce: requests from one new network at once,
// one of them telling it while the others come, tell it once: memory
// holds it from the first, before its announcement is made.
func TestNetworksToldOnceAtOnce(t *testing.T) {
	people := newTestPeople(t)
	fp := people.alice.Fingerprint()
	k, _ := testNetworks(t, filepath.Join(t.TempDir(), "people-networks.json"))
	k.now = steppingClock()
	g := gate(t, k)
	k.seen(people.reg, fp, "alice", addr("198.51.100.9"), zap.NewNop())
	k.settle(t, false)
	g.take()
	go k.seen(people.reg, fp, "alice", addr("203.0.113.9"), zap.NewNop())
	g.held(t)
	second := make(chan struct{})
	go func() { k.seen(people.reg, fp, "alice", addr("203.0.113.10"), zap.NewNop()); close(second) }()
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("the second request waited on the first's announcement")
	}
	g.open()
	k.settle(t, false)
	if got := g.take(); len(got) != 1 {
		t.Fatalf("told %+v", got)
	}
	// A burst, all released at once.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			k.seen(people.reg, fp, "alice", addr(fmt.Sprintf("192.0.2.%d", i)), zap.NewNop())
		}()
	}
	close(start)
	wg.Wait()
	k.settle(t, false)
	if got := g.take(); len(got) != 1 || got[0].From != "192.0.2.0/24" {
		t.Errorf("a burst from one new network: told %+v", got)
	}
}

// TestNetworksConcurrent: both webhooks' requests at once, and the
// file's writes beside them: under -race, nothing lost, each network
// told once.
func TestNetworksConcurrent(t *testing.T) {
	people := newTestPeople(t)
	k, heard := testNetworks(t, filepath.Join(t.TempDir(), "people-networks.json"))
	k.every = 10 * time.Millisecond
	var wg sync.WaitGroup
	keys := map[string]accesstest.Key{"alice": people.alice, "carol": people.carol, "dave": people.dave}
	for name, key := range keys {
		for g := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 200 {
					k.seen(people.reg, key.Fingerprint(), name, addr(fmt.Sprintf("203.0.%d.%d", i%8, g)), zap.NewNop())
					if i%50 == 0 {
						k.outsideList(people.reg, key.Fingerprint(), name, addr("198.51.100.1"), zap.NewNop())
					}
				}
			}()
		}
	}
	wg.Wait()
	k.settle(t, false)
	got := heard.take()
	seen := map[string]bool{}
	for _, a := range got {
		id := a.Name + a.Key + a.From
		if seen[id] {
			t.Errorf("told twice: %+v", a)
		}
		seen[id] = true
	}
	if len(got) != 3*8+3 {
		t.Errorf("told %d, want %d", len(got), 3*8+3)
	}
}

// FuzzNetworksFile: whatever the file holds, the read never panics and
// keeps only networks this release writes, at most networksPerKey a
// key, each once; and what it keeps is written back as read.
func FuzzNetworksFile(f *testing.F) {
	for _, s := range []string{
		`{"version":1,"keys":{"SHA256:x":["203.0.113.0/24","2001:db8::/48","unknown"]}}`,
		`{"version":1,"keys":{"k":["203.0.113.9/24","203.0.113.0/25","::ffff:203.0.113.0/120","fe80::/48%eth0","unknown","unknown"]}}`,
		`{"version":2}`, `{"version":1,"keys":null}`, `{"version":1,"keys":{"k":null}}`, `[]`, `null`, ``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		keys, err := decodeNetworks(b)
		if err != nil {
			return
		}
		for fp, nets := range keys {
			if len(nets) == 0 || len(nets) > networksPerKey {
				t.Fatalf("%s: %d networks", fp, len(nets))
			}
			for i, n := range nets {
				if n.IsValid() && networkOf(n.Addr()) != n || slices.Contains(nets[:i], n) {
					t.Fatalf("%s: kept %v", fp, nets)
				}
			}
		}
		again, err := decodeNetworks(encodeNetworks(keys))
		if err != nil || len(again) != len(keys) {
			t.Fatalf("written back: %v, %d keys of %d", err, len(again), len(keys))
		}
		for fp, nets := range keys {
			if !slices.Equal(again[fp], nets) {
				t.Fatalf("%s: %v read back as %v", fp, nets, again[fp])
			}
		}
	})
}

// TestOnlyAVerifiedTokenCounts: through the webhook, no refused token,
// from any address, records a network or tells anything, whatever the
// reason; a verified one does, throttled or not; an outside-list
// refusal is told and records nothing.
func TestOnlyAVerifiedTokenCounts(t *testing.T) {
	people := newTestPeople(t)
	now := time.Now()
	good := token(people.alice)
	// One character of the signature changed, mid-way, so the encoding
	// stays canonical and only the signature check refuses it.
	i := len(good) - 20
	tampered := good[:i] + map[bool]string{true: "B", false: "A"}[good[i] == 'A'] + good[i+1:]
	if tampered == good {
		t.Fatal("the tampered token is the token")
	}
	refused := [][]string{
		{token(people.stranger)},
		{people.alice.Token("other.example.test", now, time.Minute)},
		{people.alice.Token(personBox, now.Add(-time.Hour), time.Minute)},
		{people.alice.Token(personBox, now, time.Hour)},
		{good, "Bearer x"},
		{"Motel"},
		{tampered},
		{people.alice.Sign(`{"aud":"` + personBox + `","iat":1,"exp":2,"act":{}}`)},
	}
	h, _ := newTestHandler(t)
	h.people = people.reg
	k, heard := testNetworks(t, "")
	h.networks = k
	from := func(i int, auth ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/demo", nil)
		req.RemoteAddr = fmt.Sprintf("198.51.%d.1:1234", i)
		for _, a := range auth {
			req.Header.Add("Authorization", a)
		}
		w := httptest.NewRecorder()
		if err := h.ServeHTTP(w, req, nil); err != nil {
			t.Fatal(err)
		}
		return w
	}
	for i, auth := range refused {
		for j := range 20 {
			if w := from(i*20+j%250, auth...); w.Code != http.StatusUnauthorized {
				t.Fatalf("refused token %d: %d", i, w.Code)
			}
		}
	}
	// A POST of a good token is refused at the method.
	req := httptest.NewRequest(http.MethodPost, "http://"+personBox+"/demo", nil)
	req.Header.Set("Authorization", good)
	if err := h.ServeHTTP(httptest.NewRecorder(), req, nil); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	n, o := len(k.keys), len(k.outside)
	k.mu.Unlock()
	if n != 0 || o != 0 || len(heard.take()) != 0 {
		t.Fatalf("a refused token was counted: %d keys, %d outside", n, o)
	}
	// Verified, and throttled past the burst: each still a use.
	h.rate = access.NewRate(func() time.Time { return now })
	for i := range access.RateBurst + 3 {
		from(0, token(people.alice))
		_ = i
	}
	if w := from(1, token(people.alice)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("past the burst: %d", w.Code)
	}
	if got := heard.take(); len(got) != 2 || got[1].From != "198.51.1.0/24" {
		t.Errorf("told %+v", got)
	}
}

// TestOutsideListThroughTheGate: a valid signature from outside every
// list is the flat 401, told once an hour, and is no use.
func TestOutsideListThroughTheGate(t *testing.T) {
	alice := accesstest.Ed25519()
	key, err := access.ParsePerson([]string{"alice", alice.Type(), alice.B64()})
	if err != nil {
		t.Fatal(err)
	}
	g, err := access.ParseGrant([]string{"alice", "viewer", "app", "demo", "from", "198.51.100.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := newTestHandler(t)
	h.people = access.NewRegistry(personBox, access.Lines{Keys: []access.Key{key}, Grants: []access.Grant{g}})
	k, heard := testNetworks(t, "")
	h.networks = k
	for _, a := range []string{"203.0.113.9:1", "203.0.113.10:1", "[::ffff:203.0.113.11]:1"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/demo", nil)
		req.RemoteAddr = a
		req.Header.Set("Authorization", token(alice))
		w := httptest.NewRecorder()
		if err := h.ServeHTTP(w, req, nil); err != nil || w.Code != http.StatusUnauthorized {
			t.Fatalf("from %s: %d %v", a, w.Code, err)
		}
	}
	if got := heard.take(); len(got) != 1 || got[0].Name != EventPersonOutsideList || got[0].From != "203.0.113.9" {
		t.Errorf("told %+v", got)
	}
	if k.nets(alice.Fingerprint()) != nil {
		t.Error("a refusal counted as a use")
	}
}

// TestForwardedNotStrictIsUnknown: a request a trusted proxy forwarded,
// on a server without trusted_proxies_strict, is from an address the box
// could not read, whatever client_ip says: the one network of its own.
func TestForwardedNotStrictIsUnknown(t *testing.T) {
	people := newTestPeople(t)
	h, _ := newTestHandler(t)
	h.people = people.reg
	k, heard := testNetworks(t, "")
	h.networks = k
	for _, ip := range []string{"198.51.100.7", "203.0.113.7"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/demo", nil)
		req.RemoteAddr = "192.0.2.1:4321"
		ctx := context.WithValue(req.Context(), caddyhttp.VarsCtxKey, map[string]any{caddyhttp.ClientIPVarKey: ip, caddyhttp.TrustedProxyVarKey: true})
		req = req.WithContext(context.WithValue(ctx, caddyhttp.ServerCtxKey, &caddyhttp.Server{}))
		req.Header.Set("Authorization", token(people.alice))
		if err := h.ServeHTTP(httptest.NewRecorder(), req, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := heard.take(); len(got) != 1 || got[0].From != "" {
		t.Errorf("told %+v", got)
	}
}

// BenchmarkPersonGate is what the record adds to a person's admitted
// request: AdmitPerson, the whole of a person's way in before Decide
// (the token's check, the address list, the failure budgets, the rate),
// with no record, with the record in memory and the network already
// known (every request but a key's first from a network), and with a
// new network each time (the record, its journal line and an announcer
// that keeps nothing).
func BenchmarkPersonGate(b *testing.B) {
	people := newTestPeople(b)
	for _, c := range []struct {
		name  string
		k     func() *PeopleNetworks
		fresh bool
	}{
		{"no record", func() *PeopleNetworks { return nil }, false},
		{"known network", func() *PeopleNetworks { return newPeopleNetworks("", time.Now) }, false},
		{"new network", func() *PeopleNetworks { k := newPeopleNetworks("", time.Now); k.announcer = discard{}; return k }, true},
	} {
		b.Run(c.name, func(b *testing.B) {
			g := PersonGate{Registry: people.reg, Limiter: deploytrust.NewLimiter(wallClock{}), Rate: access.NewRate(time.Now), Networks: c.k(), Logger: zap.NewNop()}
			tok := token(people.alice)
			write := func(int, any) error { b.Fatal("refused"); return nil }
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/demo", nil)
				if c.fresh {
					i++
					req.RemoteAddr = fmt.Sprintf("10.%d.%d.1:1", i>>8&255, i&255)
				}
				req.Header.Set("Authorization", tok)
				g.Rate = access.NewRate(time.Now) // never throttled
				if _, ok, err := AdmitPerson(httptest.NewRecorder(), req, g, "app", "demo", write); !ok || err != nil {
					b.Fatal(ok, err)
				}
			}
		})
	}
}

// BenchmarkNetworksSeen is the record alone, for a known network.
func BenchmarkNetworksSeen(b *testing.B) {
	people := newTestPeople(b)
	k := newPeopleNetworks("", time.Now)
	fp, a, lg := people.alice.Fingerprint(), addr("203.0.113.9"), zap.NewNop()
	k.seen(people.reg, fp, "alice", a, lg)
	b.ReportAllocs()
	for b.Loop() {
		k.seen(people.reg, fp, "alice", a, lg)
	}
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type discard struct{}

func (discard) Announce(Announcement) {}
