package liveswap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

// The networks each person's key was used from, and the two events a
// stolen key should set off (liveswap/access/DESIGN-access.md, "A key
// used from somewhere new"): a key's use from a network it was never
// accepted from before, its first use among them, and a valid signature
// from outside every address list on its person's grants. Both are told
// and never gate: a read goes on as before, whatever this bookkeeping
// says or fails at.
//
// A network is the request's client_ip, as the address lists read it
// (clientAddr: unmapped, no zone), cut to a /24 or a /48; an address
// clientAddr could not read is one network of its own, told as "an
// address the box could not read". Each key keeps its last
// networksPerKey networks, the most recently used first; using a
// seventeenth drops the one used longest ago. Only a request whose
// token verified reaches it, and only a key the newest registry lists
// is kept, so it is config-sized, never traffic-sized. No time is kept:
// the order is the record.
//
// The file. Kept in memory, and in PeopleNetworksFile once moteld's
// main names it (KeepPeopleNetworksIn), so a restart does not tell
// every key again. Nothing a request waits for touches the disk: the
// file is read in the background at the first person's request a
// process admits, and until it has been read (or networksLoadWait has
// passed) uses are recorded and their announcements held, then told
// for each network the file did not hold. A network is told, then
// written: no write holds one whose announcement is not yet made, so a
// crash between the two tells it again rather than never, and a stop
// waits for the announcements being made. A new network, or a key
// dropped, is written at once, in the background, one write at a time;
// a change of order alone at most once a minute. A write that fails is
// said once, kept in memory, and tried again each minute. A file that
// does not read, is not this release's form, or is from a newer one is
// said, and moteld starts afresh and writes over it: each key's next
// use is then told again, as its first.

const (
	// PeopleNetworksFile is where moteld keeps the networks: motel's own,
	// 0600, in /var/lib/motel, the motel user's home, which the package
	// owns and never removes (and the data home moteld.service names).
	PeopleNetworksFile = "/var/lib/motel/people-networks.json"

	// The two events, names in notify's closed set of alerts.
	EventPersonNewNetwork  = "person_new_network"
	EventPersonOutsideList = "person_outside_list"

	networksPerKey = 16
	// networksHeldPerKey is the cut while the file is being read: a
	// held network dropped would never be told, and a burst after a
	// start can bring more than 16 at once, a throttled request
	// counting as a use. At load each key is cut to networksPerKey,
	// and a network told then and cut is told again at its next use.
	networksHeldPerKey = 4 * networksPerKey
	networksVersion    = 1
	// networksMaxBytes bounds the file read: a box lists at most
	// access.MaxKeyLines keys, each of up to 16 networks, about 450
	// bytes a key at the longest, under half a megabyte.
	networksMaxBytes = 1 << 20
	networksPattern  = ".people-networks-*.tmp"
	// networksLoadWait is how long announcements wait for the file to be
	// read: past it, a read that has not come back (a disk that hangs) is
	// said, and moteld goes on as with a file it could not read, and
	// writes over it. Long enough that a disk merely busy at boot reads
	// a file of at most a megabyte well within it.
	networksLoadWait = 30 * time.Second
	// networksUrgentEvery is the least time between two writes of a new
	// network or a key dropped: a thief cycling networks, throttled or
	// not, has the file written once a second at most, not once a
	// request.
	networksUrgentEvery = time.Second
	// networksStopWait bounds how long a stop waits for what is pending
	// to be written (FlushPeopleNetworks).
	networksStopWait = 2 * time.Second
	// networksReorderEvery is how often a change of order alone is
	// written, and a failed write tried again.
	networksReorderEvery = time.Minute
	// outsideListEvery is the least time between two announcements of
	// one key's valid signature from outside every address list.
	outsideListEvery = time.Hour
	// unknownNetwork is how the file spells an address the box could
	// not read.
	unknownNetwork = "unknown"
	// summaryNameRunes cuts a person's name in a summary, and
	// summaryKeyRunes the key's fingerprint, in bytes on a character's
	// edge (deploytrust.CutRunes), so that the longest sentence
	// (an outside-list's, with an IPv6 address) stays under the
	// channels' 200: the journal's line names both whole, and the alert's
	// instance label is the whole fingerprint.
	summaryNameRunes = 48
	summaryKeyRunes  = len("SHA256:") + 12
)

// Announcement is one of the two events about a person's key.
type Announcement struct {
	// Name is EventPersonNewNetwork or EventPersonOutsideList.
	Name string
	// Person and Key are the person's name and the key's fingerprint:
	// the operator's own lines, going to the operator's own channels.
	Person, Key string
	// From is the network (a new network) or the address (outside the
	// lists), the request's; "" when the box could not read it.
	From string
	// First says a new network is the key's first use the box knows of.
	First bool
}

// Summary is the announcement's one sentence, as the operator's
// channels and the journal give it.
func (a Announcement) Summary() string {
	who := a.shownPerson() + " (key " + a.shownKey() + ")"
	from := "from " + a.From
	if a.From == "" {
		from = "from an address the box could not read"
	}
	switch {
	case a.Name == EventPersonOutsideList:
		return "a valid signature by " + who + " " + from + ", outside every address list on their grants; refused"
	case a.First:
		return who + " used this box for the first time, " + from
	default:
		return who + " used this box " + from + " for the first time"
	}
}

// shownPerson and shownKey are the name and the fingerprint as Summary
// writes them: the fingerprint's start, as the person's line in `moteld
// box describe` begins.
func (a Announcement) shownPerson() string { return deploytrust.CutRunes(a.Person, summaryNameRunes) }

func (a Announcement) shownKey() string {
	if len(a.Key) <= summaryKeyRunes {
		return a.Key
	}
	return deploytrust.CutRunes(a.Key, summaryKeyRunes) + "…"
}

// Safe are the strings of the summary notify's filter must leave as
// written: the name and the fingerprint as it shows them (a
// fingerprint looks generated).
func (a Announcement) Safe() []string { return []string{a.shownPerson(), a.shownKey()} }

// Announcer is told each announcement: notify's, which raises it as a
// one-shot to the operator's channels, installed by its Provision
// (SetAnnouncer). It is called on a request's goroutine, or on the
// file's load, and must not block. With none, or with a notify that
// runs no config, each announcement is its journal line alone.
type Announcer interface {
	Announce(Announcement)
}

// SetAnnouncer installs the process's Announcer. The record is the
// process's, across loads, so the announcer is too.
func SetAnnouncer(a Announcer) {
	sharedNetworks.mu.Lock()
	defer sharedNetworks.mu.Unlock()
	sharedNetworks.announcer = a
}

// KeepPeopleNetworksIn names the file the process's record is kept in,
// before anything is served; never called, the record is in memory
// alone (a test's handlers, which provision with the process's record).
func KeepPeopleNetworksIn(path string) {
	sharedNetworks.mu.Lock()
	defer sharedNetworks.mu.Unlock()
	sharedNetworks.file, sharedNetworks.state = path, networksUnread
}

// SharedPeopleNetworks is the process's record, which both webhooks'
// person gates take. Tests make their own (newPeopleNetworks).
func SharedPeopleNetworks() *PeopleNetworks { return sharedNetworks }

var sharedNetworks = newPeopleNetworks("", time.Now)

type networksState int

const (
	networksUnread networksState = iota // a file, not yet asked for
	networksReading
	networksRead // or no file: in memory alone
)

// PeopleNetworks is the record.
type PeopleNetworks struct {
	now func() time.Time
	// read and write are the file's, seams for a test.
	read  func(path string) ([]byte, error)
	write func(path string, data []byte) error
	every time.Duration // networksReorderEvery, but in a test
	wait  time.Duration // networksLoadWait, but in a test

	mu        sync.Mutex
	file      string
	logger    *zap.Logger // the last request's: for what the load and the writes say
	announcer Announcer
	keys      map[string]*keyNetworks
	outside   map[string]time.Time // when each key's outside-list was last told
	// generation is the newest registry's (access.Registry.Generation),
	// and listed that registry: keys it does not list are dropped.
	generation uint64
	listed     *access.Registry

	state     networksState
	loadTimer *time.Timer
	// dirty: memory holds what the file does not; urgent: a network
	// added or a key dropped, written at once rather than within the
	// minute.
	dirty, urgent bool
	timer         *time.Timer // the next write
	writing       bool
	lastWrite     time.Time
	failing       bool
	// telling counts the new networks' announcements being made outside
	// the lock (a request's, or the load's, the load's journal line among
	// them): a stop waits for them, as for a write.
	telling int
}

type keyNetworks struct {
	person string // for an announcement held until the file is read
	nets   []seenNetwork
}

type seenNetwork struct {
	net netip.Prefix // the zero Prefix: an address the box could not read
	// untold: its announcement not yet made, held until the file is read
	// or being made now. A write leaves it out (tell, then remember),
	// and memory keeping it is what tells it once: a request from it
	// meanwhile finds it known.
	untold bool
}

func newPeopleNetworks(file string, now func() time.Time) *PeopleNetworks {
	k := &PeopleNetworks{
		now: now,
		read: func(path string) ([]byte, error) {
			b, _, err := readOwnFile(path, networksMaxBytes, "the record of people's networks")
			return b, err
		},
		write:   func(path string, data []byte) error { return writeOwnFile(path, networksPattern, data) },
		every:   networksReorderEvery,
		wait:    networksLoadWait,
		file:    file,
		logger:  zap.NewNop(),
		keys:    map[string]*keyNetworks{},
		outside: map[string]time.Time{},
		state:   networksRead,
	}
	if file != "" {
		k.state = networksUnread
	}
	return k
}

// networkOf is the network of an address: a /24 for IPv4, a /48 for
// IPv6, IPv4 written in IPv6 taken as IPv4 and a zone dropped, as the
// address lists compare them; the zero Prefix for an address that is
// not valid.
func networkOf(addr netip.Addr) netip.Prefix {
	addr = addr.Unmap().WithZone("")
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

func networkText(p netip.Prefix) string {
	if !p.IsValid() {
		return ""
	}
	return p.String()
}

// seen records that a verified token of key fp, person's, was accepted
// from addr under reg, and announces the network when it is new. A nil
// record keeps nothing.
func (k *PeopleNetworks) seen(reg *access.Registry, fp, person string, addr netip.Addr, logger *zap.Logger) {
	if k == nil {
		return
	}
	net := networkOf(addr)
	k.mu.Lock()
	k.logger = logger
	k.dropUnlistedLocked(reg)
	if k.droppedLocked(reg, fp) {
		k.mu.Unlock()
		return
	}
	kn := k.keys[fp]
	if kn == nil {
		kn = &keyNetworks{}
		k.keys[fp] = kn
	}
	kn.person = person
	i := slices.IndexFunc(kn.nets, func(s seenNetwork) bool { return s.net == net })
	if i == 0 {
		k.mu.Unlock()
		return
	}
	if i > 0 {
		s := kn.nets[i]
		copy(kn.nets[1:i+1], kn.nets[:i])
		kn.nets[0] = s
		k.changedLocked(false)
		k.mu.Unlock()
		return
	}
	first := len(kn.nets) == 0
	kn.nets = slices.Insert(kn.nets, 0, seenNetwork{net: net, untold: true})
	// Cut to the networks a key keeps once told (toldLocked): an
	// announcement cut short then forgets no network told before it.
	// Until then, the cut of the held: announcements that block (on
	// notify's engine) leave the key no longer than that.
	if len(kn.nets) > networksHeldPerKey {
		kn.nets = kn.nets[:networksHeldPerKey]
	}
	if k.state != networksRead {
		if k.state == networksUnread {
			k.startLoadLocked()
		}
		k.mu.Unlock()
		return
	}
	// Told outside the lock (the journal, notify's engine), then
	// written: a crash between the two tells it again, never not at all.
	// Deferred, so an announcer that panics (the http server recovers
	// the request) leaves no stop waiting, and the network untold.
	k.telling++
	a := k.announcer
	k.mu.Unlock()
	told := false
	defer func() {
		k.mu.Lock()
		k.toldLocked(fp, net, told)
		k.telling--
		k.mu.Unlock()
	}()
	k.announce(logger, a, Announcement{Name: EventPersonNewNetwork, Person: person, Key: fp, From: networkText(net), First: first})
	told = true
}

// toldLocked marks key fp's network net told, cuts the key to the
// networks it keeps (the one used longest ago dropped), and has it
// written; or, its announcement cut short (told false), forgets it, so
// its next use tells it. A network dropped meanwhile (its key removed,
// or cut by the networks a key keeps) is not written.
func (k *PeopleNetworks) toldLocked(fp string, net netip.Prefix, told bool) {
	kn := k.keys[fp]
	if kn == nil {
		return
	}
	i := slices.IndexFunc(kn.nets, func(s seenNetwork) bool { return s.net == net && s.untold })
	switch {
	case i < 0:
	case told:
		kn.nets[i].untold = false
		if len(kn.nets) > networksPerKey {
			kn.nets = kn.nets[:networksPerKey]
		}
		k.changedLocked(true)
	default:
		kn.nets = slices.Delete(kn.nets, i, i+1)
	}
}

// outsideList announces a valid signature of key fp, person's, from
// addr, outside every address list on their grants: at most once per
// key in outsideListEvery, kept in memory alone (a restart may tell one
// more).
func (k *PeopleNetworks) outsideList(reg *access.Registry, fp, person string, addr netip.Addr, logger *zap.Logger) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.logger = logger
	k.dropUnlistedLocked(reg)
	if k.droppedLocked(reg, fp) {
		k.mu.Unlock()
		return
	}
	now := k.now()
	if last, ok := k.outside[fp]; ok && now.Sub(last) < outsideListEvery {
		k.mu.Unlock()
		return
	}
	k.outside[fp] = now
	a := k.announcer
	k.mu.Unlock()
	from := ""
	if addr.IsValid() {
		from = addr.Unmap().WithZone("").String()
	}
	k.announce(logger, a, Announcement{Name: EventPersonOutsideList, Person: person, Key: fp, From: from})
}

// announce writes the announcement's journal line, then tells the
// announcer, outside the record's lock.
func (k *PeopleNetworks) announce(logger *zap.Logger, a Announcer, ann Announcement) {
	from := ann.From
	if from == "" {
		from = unknownNetwork
	}
	switch ann.Name {
	case EventPersonNewNetwork:
		logger.Warn("access: a key used from a new network", zap.String("person", ann.Person), zap.String("key", ann.Key),
			zap.String("network", from), zap.Bool("first_use", ann.First), zap.String("summary", ann.Summary()))
	case EventPersonOutsideList:
		logger.Warn("access: a valid signature from outside every address list (told at most once an hour per key)",
			zap.String("person", ann.Person), zap.String("key", ann.Key), zap.String("address", from), zap.String("summary", ann.Summary()))
	}
	if a != nil {
		a.Announce(ann)
	}
}

// dropUnlistedLocked drops every key a registry newer than the last
// one seen does not list: a person removed is forgotten, and named anew
// as a first use if added back, once a person's request has come under
// the load that removed them (a load that admits no one has no
// registry, and drops nothing until a person is admitted again). A key
// is its record whatever name its line gives it. An older registry (a reload's old
// handlers, serving for a moment beside the new) changes nothing.
func (k *PeopleNetworks) dropUnlistedLocked(reg *access.Registry) {
	if reg == nil || reg.Generation() <= k.generation {
		return
	}
	k.generation, k.listed = reg.Generation(), reg
	dropped := false
	for fp := range k.keys {
		if _, ok := reg.Name(fp); !ok {
			delete(k.keys, fp)
			dropped = true
		}
	}
	for fp := range k.outside {
		if _, ok := reg.Name(fp); !ok {
			delete(k.outside, fp)
		}
	}
	if dropped {
		k.changedLocked(true)
	}
}

// droppedLocked reports whether a request under reg, older than the
// newest registry seen, is of a key the newest no longer lists: one of a
// removed person's, still served by a reload's old handlers. It is
// neither recorded nor told: the person is gone.
func (k *PeopleNetworks) droppedLocked(reg *access.Registry, fp string) bool {
	if reg == nil || reg.Generation() >= k.generation {
		return false
	}
	_, listed := k.listed.Name(fp)
	return !listed
}

// changedLocked says memory holds what the file does not, and has it
// written: at once when urgent (a network added, a key dropped), else
// within networksReorderEvery of the last write. Nothing is written
// before the file was read, nor with no file.
func (k *PeopleNetworks) changedLocked(urgent bool) {
	if k.file == "" || k.state != networksRead {
		return
	}
	k.dirty = true
	k.urgent = k.urgent || urgent
	if k.writing {
		return // the write in flight looks again when it ends
	}
	if k.timer != nil {
		// A write is due already. Brought forward for an urgent change,
		// unless writes are failing, when each waits its minute.
		if !k.urgent || k.failing || !k.timer.Stop() {
			return
		}
		k.timer = nil
	}
	every := networksUrgentEvery
	if !k.urgent || k.failing {
		every = k.every
	}
	delay := max(0, k.lastWrite.Add(every).Sub(k.now()))
	k.timer = time.AfterFunc(delay, k.flush)
}

// FlushPeopleNetworks writes what the process's record holds that its
// file does not, and waits for it, or for a write already under way, at
// most networksStopWait: liveswap's Stop, so that a restart (an upgrade,
// a reload's old config) tells nothing again for want of a write that
// was due. It waits for the new networks' announcements being made too
// (a request's, or a read's held uses), each written once told, so none
// is cut off by the exit. (An outside-list's is a request's alone, which
// the http app's graceful stop waits for, and is never written.) A
// write that hangs is left to itself past the wait; while writes fail,
// the minute's retry is left as it is and a stop waits for no write.
func FlushPeopleNetworks() { sharedNetworks.flushWithin(networksStopWait) }

func (k *PeopleNetworks) flushWithin(wait time.Duration) {
	deadline := time.Now().Add(wait)
	for {
		k.mu.Lock()
		// A read of the file under way holds uses not yet told nor
		// written, and an announcement being made is not yet written:
		// waited for too, so they are told and written now rather than
		// at their next use after the restart.
		idle := k.file == "" || k.state == networksUnread || k.state == networksRead && k.telling == 0 && (k.failing || !k.dirty && !k.writing)
		if !idle && k.state == networksRead && k.dirty && !k.failing && !k.writing && (k.timer == nil || k.timer.Stop()) {
			k.timer = time.AfterFunc(0, k.flush)
		}
		k.mu.Unlock()
		if idle || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// flush writes the record, on the timer's goroutine.
func (k *PeopleNetworks) flush() {
	k.mu.Lock()
	k.timer = nil
	if !k.dirty || k.writing {
		k.mu.Unlock()
		return
	}
	snapshot := make(map[string][]netip.Prefix, len(k.keys))
	for fp, kn := range k.keys {
		nets := make([]netip.Prefix, 0, len(kn.nets))
		for _, s := range kn.nets {
			if !s.untold { // written once told (toldLocked)
				nets = append(nets, s.net)
			}
		}
		if len(nets) > 0 {
			snapshot[fp] = nets
		}
	}
	k.dirty, k.urgent, k.writing = false, false, true
	file, write := k.file, k.write
	k.mu.Unlock()

	// A temp file a write cut short left (a crash before its rename),
	// by its pattern alone: here, where no other write of this process
	// can be under way.
	removeOwnLeftovers(filepath.Dir(file), networksPattern)
	err := write(file, encodeNetworks(snapshot))

	k.mu.Lock()
	k.writing = false
	k.lastWrite = k.now()
	logger, was := k.logger, k.failing
	k.failing = err != nil
	if err != nil {
		k.dirty = true
	}
	if k.dirty {
		k.timer = nil
		k.changedLocked(k.urgent)
	}
	k.mu.Unlock()
	switch {
	case err != nil && !was:
		logger.Warn("access: the record of the networks people's keys were used from could not be written; it is kept in memory and tried again each minute, and a restart before it is written tells again the networks used since",
			zap.String("file", file), zap.Error(err))
	case err == nil && was:
		logger.Info("access: the record of the networks people's keys were used from is written again", zap.String("file", file))
	}
}

// startLoadLocked reads the file in the background, and bounds how long
// announcements wait for it.
func (k *PeopleNetworks) startLoadLocked() {
	k.state = networksReading
	file, read := k.file, k.read
	k.loadTimer = time.AfterFunc(k.wait, func() {
		k.loaded(nil, fmt.Errorf("it was not read within %s", k.wait))
	})
	go func() {
		b, err := read(file)
		k.loaded(b, err)
	}()
}

// loaded takes what the file held (or why it held nothing), once: the
// read that comes back after the wait is dropped. Each network seen
// before it that the file did not hold is announced, then written; a
// network the file held is not announced.
func (k *PeopleNetworks) loaded(b []byte, readErr error) {
	k.mu.Lock()
	if k.state == networksRead {
		k.mu.Unlock()
		return
	}
	k.state = networksRead
	k.loadTimer.Stop()
	var fromFile map[string][]netip.Prefix
	var why error
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
	case readErr != nil:
		why = readErr
	default:
		fromFile, why = decodeNetworks(b)
	}
	type heldUse struct {
		ann Announcement
		net netip.Prefix
	}
	var anns []heldUse
	changed, reordered := why != nil, false
	for fp, kn := range k.keys {
		old := fromFile[fp]
		var merged []seenNetwork
		var held []seenNetwork
		for _, s := range kn.nets {
			untold := s.untold && !slices.Contains(old, s.net)
			if untold {
				held = append(held, s)
				changed = true
			}
			merged = append(merged, seenNetwork{net: s.net, untold: untold})
		}
		for _, n := range old {
			if !slices.ContainsFunc(merged, func(s seenNetwork) bool { return s.net == n }) {
				merged = append(merged, seenNetwork{net: n})
			}
		}
		if len(merged) > networksPerKey {
			merged = merged[:networksPerKey]
		}
		if !slices.EqualFunc(merged, old, func(s seenNetwork, n netip.Prefix) bool { return s.net == n }) {
			reordered = true
		}
		kn.nets = merged
		// The oldest held is the key's first use when the file held none.
		for i := len(held) - 1; i >= 0; i-- {
			anns = append(anns, heldUse{Announcement{Name: EventPersonNewNetwork, Person: kn.person, Key: fp, From: networkText(held[i].net),
				First: len(old) == 0 && i == len(held)-1}, held[i].net})
		}
	}
	for fp, nets := range fromFile {
		if _, ok := k.keys[fp]; ok {
			continue
		}
		if _, listed := k.listed.Name(fp); k.listed != nil && !listed {
			changed = true // a key the newest registry does not list: dropped
			continue
		}
		kn := &keyNetworks{}
		for _, n := range nets {
			kn.nets = append(kn.nets, seenNetwork{net: n})
		}
		k.keys[fp] = kn
	}
	// Told outside the lock, then written, as a request's (seen): the
	// write is had once they are told. A request meanwhile writes
	// without them.
	k.telling++
	logger, a := k.logger, k.announcer
	file := k.file
	k.mu.Unlock()
	switch {
	case why != nil:
		logger.Warn("access: the record of the networks people's keys were used from could not be used; starting afresh, so each key's next use is told as from a new network",
			zap.String("file", file), zap.Error(why))
	case fromFile == nil:
		logger.Info("access: no record yet of the networks people's keys were used from; each key's first use from here on is told", zap.String("file", file))
	}
	for _, u := range anns {
		k.announce(logger, a, u.ann)
	}
	k.mu.Lock()
	for _, u := range anns {
		k.toldLocked(u.ann.Key, u.net, true)
	}
	if changed || reordered {
		k.changedLocked(changed)
	}
	k.telling--
	k.mu.Unlock()
}

// networksFile is the file's form: each key's networks by fingerprint,
// the most recently used first; an address the box could not read is
// "unknown".
type networksFile struct {
	Version int                 `json:"version"`
	Keys    map[string][]string `json:"keys"`
}

func encodeNetworks(keys map[string][]netip.Prefix) []byte {
	f := networksFile{Version: networksVersion, Keys: make(map[string][]string, len(keys))}
	for fp, nets := range keys {
		out := make([]string, 0, len(nets))
		for _, n := range nets {
			if s := networkText(n); s != "" {
				out = append(out, s)
			} else {
				out = append(out, unknownNetwork)
			}
		}
		f.Keys[fp] = out
	}
	b, _ := json.Marshal(f) // strings alone: it does not fail
	return append(b, '\n')
}

// decodeNetworks reads the file's bytes. A file that is not this
// release's form is an error, and nothing of it is taken; within one, a
// network that is not one this release writes is left out, and each
// key keeps its first networksPerKey.
func decodeNetworks(b []byte) (map[string][]netip.Prefix, error) {
	var f networksFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("it is not JSON this release reads: %w", err)
	}
	switch {
	case f.Version > networksVersion:
		return nil, fmt.Errorf("it is from a newer moteld (version %d; this one reads %d)", f.Version, networksVersion)
	case f.Version != networksVersion:
		return nil, fmt.Errorf("its version is %d, not %d", f.Version, networksVersion)
	}
	out := make(map[string][]netip.Prefix, len(f.Keys))
	for fp, nets := range f.Keys {
		var kept []netip.Prefix
		for _, s := range nets {
			n, ok := parseNetwork(s)
			if !ok || slices.Contains(kept, n) {
				continue
			}
			if kept = append(kept, n); len(kept) == networksPerKey {
				break
			}
		}
		if len(kept) > 0 {
			out[fp] = kept
		}
	}
	return out, nil
}

// parseNetwork is a network as encodeNetworks writes it: "unknown", or
// a /24 of IPv4 or a /48 of IPv6, masked, as networkOf makes them.
func parseNetwork(s string) (netip.Prefix, bool) {
	if s == unknownNetwork {
		return netip.Prefix{}, true
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || p != p.Masked() || networkOf(p.Addr()) != p {
		return netip.Prefix{}, false
	}
	return p, true
}
