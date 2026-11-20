package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/smallhoursorg/motel/backups/dump"
	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/unit"
	"github.com/smallhoursorg/motel/liveswap/backupdecl"
)

const (
	snapA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// snapC is the id of a clean-run record: a snapshot of its own.
	snapC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// box is a liveswap root with one deployed app, the engine's
// directories, and a scripted Runner standing in for the manager. What
// the real manager does with a Spec is the unit package's integration
// suite; what is tested here is which units a run starts, with what,
// in what order, and what it makes of how they end.
type box struct {
	t       *testing.T
	cfg     Config
	root    string
	plan    string // what the plan unit prints
	specs   []unit.Spec
	stopped []string
	// how each role ends; the default is a clean run of everything
	outcome map[string]unit.Outcome
	err     map[string]error
	dump    func(paths []string) []dump.Result
	summary string
	ls      func(parent string) string
	// before runs as a unit is about to start: the moment the manager
	// would resolve its bind sources.
	before func(unit.Spec)
	// mount points, and the directory each was made from
	mounted      map[string]string
	unmounted    []string
	leftMounts   []string
	stopErr      error
	failClean    string         // the app whose clean unit fails
	failUnstage  string         // the app whose unstage unit fails
	history      string         // what `restic snapshots` prints for an app
	listing      string         // what `restic snapshots` prints for every app, at the end of a run
	listingErr   string         // what it writes to stderr beside that
	noListingErr bool           // the manager made no stderr file for it
	uploadExit   map[string]int // an app whose upload exits with this
	fetch        string         // what `restic restore --json` prints
	install      string         // what the install unit, or a drill's check unit, prints
	size         string         // what `restic stats --mode restore-size` prints
	free         uint64         // what is free where a fetch lands
	oneDisk      bool           // the fetch and the install land on one filesystem
	// setup's own: what `restic init` and `restic cat config` print,
	// a role whose unit never ends, the manager's version, which
	// programs are installed and whether the account is
	initOut, initErr, probeOut string
	probeErr                   string // what the look says on stderr
	openErr                    string // what the open unit says on stderr
	planErr                    string // what the plan unit says on stderr
	hang                       string
	version                    int
	haveProgram                func(string) bool
	account                    bool     // whether the motel-backup account exists
	comment                    string   // its comment: motel's mark, where motel made it
	localComment               string   // its comment in /etc/passwd
	nssLine, localLine         string   // the whole line the box resolves, and /etc/passwd's, where a row sets them
	notLocal                   bool     // /etc/passwd holds no account of that name
	madePrivate                []string // what was made private, in order
	unmountedAt                []string // every unmount, in order, the probe's among them
	runDirMounted              bool     // something is mounted on the run directory itself
	uid, gid                   int
	unseen                     bool     // the manager does not see this command's mounts
	seesErr                    error    // the manager could not be asked
	lookedFor                  []string // the mounts the manager was asked about
	selfBound, selfUnbound     []string // the probe's own mount, made and taken away
	accountLookups             int      // how often the account was looked up
	owned4                     []passwd // the accounts a fetch's directory was given to
	bareNodes                  bool     // the listing says nothing of which file a node is
	accountsMade               int
	owned, synced              []string // what setup asked to be root's, and put on the disk
	waited                     []string // units a lock holder waited for
	waitErr                    error
	release                    chan struct{} // closed at the test's end: a hung init ends
	// then is how a role's units end one after another, before outcome
	// speaks for the rest: the probe before a check and the one after.
	then map[string][]unit.Outcome
	// What the repository check prints, what the unit that writes a
	// clean-run record prints, and what the listing of those records
	// prints.
	repocheck, vouch, vouches string
	vouchesErr                string // what restic says on stderr beside that listing
}

func (b *box) ManagerVersion(context.Context) (int, error) { return b.version, nil }

// Sees: the manager sees the mounts this command makes, unless a test
// says the command has a mount namespace of its own.
func (b *box) Sees(_ context.Context, mountPoint string) (bool, error) {
	b.lookedFor = append(b.lookedFor, mountPoint)
	return !b.unseen, b.seesErr
}

var roleRe = regexp.MustCompile(`^motel_backup_([a-z]+)[0-9]*_`)

func newBox(t *testing.T) *box {
	t.Helper()
	dir := t.TempDir()
	b := &box{t: t, root: filepath.Join(dir, "liveswap"), outcome: map[string]unit.Outcome{}, err: map[string]error{}, release: make(chan struct{})}
	t.Cleanup(func() { close(b.release) })
	// Every program a command needs is there unless a test says not:
	// this lane runs where none of them is installed.
	b.haveProgram = func(string) bool { return true }
	oldHave := haveProgram
	haveProgram = func(p string) bool { return b.haveProgram(p) }
	t.Cleanup(func() { haveProgram = oldHave })
	// The account restic runs as, as setup makes it, unless a test says
	// otherwise: a run looks at it before any unit, as setup does.
	b.account, b.comment, b.localComment, b.uid, b.gid = true, accountMark, accountMark, 995, 995
	oldAccount, oldMake, oldLocal := account, makeAccount, localAccount
	localAccount = func(context.Context, string) (passwd, error) {
		return passwd{name: backupUser, comment: b.localComment, uid: b.uid, gid: b.gid, line: b.localLine, exists: b.account && !b.notLocal}, nil
	}
	t.Cleanup(func() { localAccount = oldLocal })
	account = func(context.Context, string) (passwd, error) {
		b.accountLookups++
		return passwd{name: backupUser, comment: b.comment, uid: b.uid, gid: b.gid, line: b.nssLine, exists: b.account}, nil
	}
	makeAccount = func() error {
		b.accountsMade++
		b.account, b.comment, b.localComment, b.notLocal, b.uid, b.gid = true, accountMark, accountMark, false, 995, 995
		return nil
	}
	t.Cleanup(func() { account, makeAccount = oldAccount, oldMake })
	b.cfg = Config{
		ConfigDir: "/etc/motel",
		EnvFile:   filepath.Join(dir, "backup.env"), StateDir: filepath.Join(dir, "state"), RunDir: filepath.Join(dir, "run"),
		Self: "/usr/bin/motel-backup", Restic: "/usr/bin/restic",
	}
	must(t, os.WriteFile(b.cfg.EnvFile, []byte("RESTIC_PASSWORD=the-secret\n"), 0o600))
	must(t, os.MkdirAll(filepath.Join(b.root, "blog", "shared", "uploads"), 0o755))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["app.db"],"files":["uploads"]}}}`, b.root)
	b.dump = func(paths []string) []dump.Result {
		out := make([]dump.Result, len(paths))
		for i, p := range paths {
			out[i] = dump.Result{Path: p, Class: dump.OK, Bytes: 4096}
		}
		return out
	}
	b.summary = `{"message_type":"summary","snapshot_id":"` + snapA + `"}`
	b.listing = `[{"id":"` + snapA + `","time":"2026-09-02T00:00:00Z","tags":["app:blog"]}]`
	b.fetch = `{"message_type":"summary","total_files":4,"files_restored":4}`
	b.size, b.free = `{"total_size":4096,"total_file_count":4,"snapshots_count":1}`, 1<<30
	b.install = `{"plan":{"sqlite":["app.db"],"files":["uploads"]},"items":[{"kind":"sqlite","path":"app.db","class":"ok"},{"kind":"files","path":"uploads","class":"ok"}]}`
	b.then = map[string][]unit.Outcome{}
	b.repocheck = `{"message_type":"summary","num_errors":0,"broken_packs":null,"suggest_repair_index":false,"suggest_prune":false}`
	b.vouch = `{"message_type":"summary","snapshot_id":"` + snapC + `"}`
	b.vouches = `[]`
	b.ls = func(parent string) string {
		switch parent {
		case "/backup/blog/sqlite":
			return `{"struct_type":"snapshot","time":"2026-09-02T07:08:09.123456789Z"}` + "\n" + `{"struct_type":"node","path":"/backup/blog/sqlite/app.db","type":"file","size":4096}`
		case "/backup/blog/files":
			return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}`
		}
		return ""
	}
	oldFree, oldSame := freeUnder, sameFilesystem
	freeUnder = func(string) (uint64, error) { return b.free, nil }
	sameFilesystem = func(string, string) (bool, error) { return b.oneDisk, nil }
	t.Cleanup(func() { freeUnder, sameFilesystem = oldFree, oldSame })
	old, oldMount, oldUnmount, oldUnder := dataOwner, bindMount, unmountDetach, mountsUnder
	dataOwner = func(context.Context) (int, int, error) { return os.Getuid(), os.Getgid(), nil }
	// Which program the helpers are started from: the file, where the
	// test made one; this lane has no /usr/bin/motel-backup.
	oldProgramOf := programOf
	programOf = func(path string) (string, error) {
		if _, err := os.Stat(path); err == nil {
			return oldProgramOf(path)
		}
		return "the program under test", nil
	}
	t.Cleanup(func() { programOf = oldProgramOf })
	// The credential file is the account under test's, as root's is on a box.
	oldCredentialOwner := credentialOwner
	credentialOwner = uint32(os.Getuid())
	t.Cleanup(func() { credentialOwner = oldCredentialOwner })
	oldOwnerOf, oldSelfBind := ownerOf, selfBind
	ownerOf = func(acct passwd) (int, int) { b.owned4 = append(b.owned4, acct); return os.Getuid(), os.Getgid() }
	selfBind = func(dir string) error { b.selfBound = append(b.selfBound, dir); return nil }
	t.Cleanup(func() { ownerOf, selfBind = oldOwnerOf, oldSelfBind })
	// mount(2) needs a privilege this lane does not have, and what the
	// kernel does with it is the integration suite's to show. Here it is
	// enough to know what was asked for: which directory, at the moment
	// of asking, onto which mount point.
	b.mounted = map[string]string{}
	bindMount = func(source, target string) error {
		was, err := os.Readlink(source)
		b.mounted[target] = was
		return err
	}
	oldPrivate, oldMountPoint := private, mountPoint
	private = func(target string) error { b.madePrivate = append(b.madePrivate, target); return nil }
	mountPoint = func(string) (bool, error) { return b.runDirMounted, nil }
	t.Cleanup(func() { private, mountPoint = oldPrivate, oldMountPoint })
	unmountDetach = func(target string) error {
		b.unmountedAt = append(b.unmountedAt, target)
		if slices.Contains(b.selfBound, target) {
			b.selfUnbound = append(b.selfUnbound, target)
			return nil
		}
		b.unmounted = append(b.unmounted, target)
		return nil
	}
	mountsUnder = func(string) ([]string, error) { return b.leftMounts, nil }
	t.Cleanup(func() {
		dataOwner, bindMount, unmountDetach, mountsUnder = old, oldMount, oldUnmount, oldUnder
	})
	return b
}

// whichFile is restic saying which file each node is: the inode and
// the owner of what the upload unit was shown at that path — what was
// bound there, as the kernel shows a bind mount [M63]. A node a test
// wrote with an inode of its own is left as written.
func (b *box) whichFile(listing string) string {
	if b.bareNodes {
		return listing
	}
	shown := map[string]string{} // a path in the unit's view -> what was bound there
	for _, s := range b.specs {
		if m := roleRe.FindStringSubmatch(s.Name); m != nil && m[1] == "upload" {
			for _, bind := range s.Binds {
				if was, ok := b.mounted[bind.Source]; ok {
					shown[bind.Dest] = was
				}
			}
		}
	}
	lines := strings.Split(listing, "\n")
	for i, line := range lines {
		var n map[string]any
		if json.Unmarshal([]byte(line), &n) != nil || n["struct_type"] != "node" {
			continue
		}
		if _, said := n["inode"]; said {
			continue
		}
		was, ok := shown[fmt.Sprint(n["path"])]
		if !ok {
			continue
		}
		var st syscall.Stat_t
		if err := syscall.Stat(was, &st); err != nil {
			continue
		}
		n["inode"], n["uid"], n["gid"] = st.Ino, st.Uid, st.Gid
		raw, _ := json.Marshal(n)
		lines[i] = string(raw)
	}
	return strings.Join(lines, "\n")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (b *box) Stop(name string) error { b.stopped = append(b.stopped, name); return b.stopErr }

// Wait is what a lock holder does about a unit an earlier setup left
// to finish: recorded, and the unit taken as gone.
func (b *box) Wait(ctx context.Context, name string) error {
	b.waited = append(b.waited, name)
	if ctx.Err() != nil {
		return fmt.Errorf("%s: still activating: %w", name, ctx.Err())
	}
	return b.waitErr
}

func (b *box) Run(ctx context.Context, s unit.Spec) (unit.Outcome, error) {
	b.specs = append(b.specs, s)
	if b.before != nil {
		b.before(s)
	}
	m := roleRe.FindStringSubmatch(s.Name)
	if m == nil {
		b.t.Fatalf("unit name %q", s.Name)
	}
	role := m[1]
	if err := b.err[role]; err != nil {
		return unit.Outcome{}, err
	}
	if role == b.hang {
		// As the real runner: the unit is stopped by name once the
		// context ends, and what could not be confirmed gone wraps the
		// cause in ErrNotConfirmedGone. Init runs on a context nothing
		// cancels; its hang ends with the test. An opening that hangs is
		// restic retrying its lock file's write, and its retry lines are
		// on stderr before the clock ends it: written first where the
		// test gives them.
		if role == "open" && s.StderrFile != "" && b.openErr != "" {
			must(b.t, os.WriteFile(s.StderrFile, []byte(b.openErr), 0o600))
		}
		// A look that hangs is restic retrying too, its retry lines on
		// stderr before the clock ends it.
		if role == "probe" && s.StderrFile != "" && b.probeErr != "" {
			must(b.t, os.WriteFile(s.StderrFile, []byte(b.probeErr), 0o600))
		}
		select {
		case <-ctx.Done():
		case <-b.release:
			return unit.Outcome{Result: "success"}, nil
		}
		b.stopped = append(b.stopped, s.Name)
		if b.stopErr != nil {
			return unit.Outcome{}, fmt.Errorf("%w: %s: %w", b.stopErr, s.Name, ctx.Err())
		}
		return unit.Outcome{}, ctx.Err()
	}
	write := func(body string) {
		if s.StdoutFile != "" {
			must(b.t, os.WriteFile(s.StdoutFile, []byte(body), 0o600))
		}
	}
	switch role {
	case "plan":
		write(b.plan)
		if s.StderrFile != "" {
			must(b.t, os.WriteFile(s.StderrFile, []byte(b.planErr), 0o600))
		}
	case "dump":
		var decl struct {
			SQLite []string `json:"sqlite"`
		}
		for _, bind := range s.Binds {
			if bind.Dest == "/plan.json" {
				raw, err := os.ReadFile(bind.Source)
				must(b.t, err)
				must(b.t, json.Unmarshal(raw, &decl))
			}
		}
		raw, _ := json.Marshal(b.dump(decl.SQLite))
		write(string(raw))
	case "upload":
		write(b.summary)
	case "history":
		write(b.history)
	case "listing":
		write(b.listing)
		if s.StderrFile == "" {
			b.t.Errorf("the listing unit keeps nothing of what restic says beside its answer: %+v", s)
		} else if !b.noListingErr {
			must(b.t, os.WriteFile(s.StderrFile, []byte(b.listingErr), 0o600))
		}
	case "size":
		write(b.size)
	case "fetch":
		write(b.fetch)
	case "install", "extract", "check":
		write(b.install)
	case "repocheck":
		write(b.repocheck)
	case "vouch":
		write(b.vouch)
	case "vouches":
		write(b.vouches)
		if s.StderrFile == "" {
			b.t.Errorf("the records' listing keeps nothing of what restic says beside its answer: %+v", s)
		} else {
			must(b.t, os.WriteFile(s.StderrFile, []byte(b.vouchesErr), 0o600))
		}
	case "mkshared":
		must(b.t, os.MkdirAll(filepath.Join(b.root, "blog", "shared"), 0o755))
	case "unmake":
		_ = os.Remove(filepath.Join(b.root, "blog", "shared"))
		_ = os.Remove(filepath.Join(b.root, "blog"))
	case "init":
		write(b.initOut)
		if s.StderrFile != "" {
			must(b.t, os.WriteFile(s.StderrFile, []byte(b.initErr), 0o600))
		}
	case "probe", "open", "reopen":
		write(b.probeOut)
		if s.StderrFile != "" {
			stderr := b.probeErr
			if role == "open" {
				stderr = b.openErr
			}
			must(b.t, os.WriteFile(s.StderrFile, []byte(stderr), 0o600))
		}
	case "verify":
		// Everything after "--" and the snapshot id is a parent to list.
		var out []string
		for i, arg := range s.Argv {
			if arg == "--" {
				for _, parent := range s.Argv[i+2:] {
					out = append(out, b.ls(parent))
				}
			}
		}
		write(b.whichFile(strings.Join(out, "\n")))
	}
	for app, exit := range b.uploadExit {
		if role == "upload" && strings.Contains(s.Name, "_upload_"+app+"_") {
			return unit.Outcome{Result: "exit-code", ExitStatus: exit}, nil
		}
	}
	if role == "clean" && b.failClean != "" && strings.Contains(s.Name, "_clean_"+b.failClean+"_") {
		return unit.Outcome{Result: "exit-code", ExitStatus: 1}, nil
	}
	if role == "unstage" && b.failUnstage != "" && strings.Contains(s.Name, "_unstage_"+b.failUnstage+"_") {
		return unit.Outcome{Result: "exit-code", ExitStatus: 1}, nil
	}
	if next := b.then[role]; len(next) > 0 {
		b.then[role] = next[1:]
		return next[0], nil
	}
	if o, ok := b.outcome[role]; ok {
		return o, nil
	}
	return unit.Outcome{Result: "success"}, nil
}

func (b *box) roles() string {
	var r []string
	for _, s := range b.specs {
		r = append(r, roleRe.FindStringSubmatch(s.Name)[1])
	}
	return strings.Join(r, " ")
}

func (b *box) spec(role string) unit.Spec {
	for _, s := range b.specs {
		if roleRe.FindStringSubmatch(s.Name)[1] == role {
			return s
		}
	}
	b.t.Fatalf("no %s unit was started (started: %s)", role, b.roles())
	return unit.Spec{}
}

func TestACleanRun(t *testing.T) {
	b := newBox(t)
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	// The backup, and — nothing of this app's having been proven yet — a
	// drill of the snapshot it made.
	// The size first: a run says how large its own drill is, and leaves
	// a large one to the drill. The record that the run ended ok on the
	// app, once its copies are gone. Last, one listing of the repository.
	if got, want := b.roles(), "plan clean dump upload verify clean vouch size unstage fetch handover check unstage listing"; got != want {
		t.Fatalf("units, in order: %s\nwant:            %s", got, want)
	}
	app := st.Apps["blog"]
	if app.Class != record.OK || app.Snapshot == nil || app.Snapshot.ID != snapA || app.LastOK == nil {
		t.Fatalf("%+v", app)
	}
	if app.Looked != filepath.Join(b.root, "blog", "shared") {
		t.Errorf("the record does not say where it looked: %q", app.Looked)
	}
	again, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	if err != nil || again.Apps["blog"].Class != record.OK {
		t.Fatalf("the record on disk: %+v, %v", again, err)
	}
	if left, _ := os.ReadDir(b.cfg.RunDir); len(left) != 2 { // the lock and the unit list
		t.Errorf("left in the run dir: %v", left)
	}
}

// Who holds what. The unit that parses the app's bytes has no network
// and no credential; the unit with the credential is another account
// and sees one app, read-only, at paths that do not depend on the root.
func TestWhatEachUnitIsGiven(t *testing.T) {
	b := newBox(t)
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"plan", "dump", "clean"} {
		s := b.spec(role)
		if s.Network || s.EnvironmentFile != "" || len(s.Capabilities) != 0 || !s.SameUIDNamespaces || s.AsRoot {
			t.Errorf("%s: network=%v envfile=%q caps=%v namespaces=%v root=%v", role, s.Network, s.EnvironmentFile, s.Capabilities, s.SameUIDNamespaces, s.AsRoot)
		}
	}
	if s := b.spec("plan"); s.User != backupUser {
		t.Errorf("the plan unit runs as %q: the config dir holds env files the motel group can read", s.User)
	}
	if s := b.spec("dump"); s.User != dataUser {
		t.Errorf("the dump unit runs as %q: only the data user may open a live database", s.User)
	}
	up := b.spec("upload")
	if up.User != backupUser || !up.Network || up.EnvironmentFile != b.cfg.EnvFile || up.SameUIDNamespaces || up.AsRoot ||
		len(up.Capabilities) != 1 || up.Capabilities[0] != unit.CapDACReadSearch {
		t.Errorf("upload: %+v", up)
	}
	shared := filepath.Join(b.root, "blog", "shared")
	var dests []string
	for _, bind := range up.Binds {
		if bind.Writable {
			t.Errorf("the upload unit can write %s", bind.Source)
		}
		if bind.Source == shared {
			t.Errorf("the upload unit is given the whole shared dir, though only uploads is declared")
		}
		dests = append(dests, bind.Dest)
	}
	if got := strings.Join(dests, " "); got != "/backup/blog/sqlite /backup/blog/plan.json /backup/blog/files/uploads /backup-exclude" {
		t.Errorf("the upload unit's view: %s", got)
	}
	for _, s := range b.specs {
		if s.Environment != nil && strings.Contains(strings.Join(s.Environment, " "), "the-secret") {
			t.Errorf("%s: the engine read the credential file", s.Name)
		}
	}
}

// The units with no clock — the upload, the fetch and the check — make
// one try at each S3 request, so a storage that never answers ends
// them within restic's own retries (#177, M93); every other restic unit
// keeps minio-go's ten, which a listing needs to stay silent over one
// 500 (M89).
func TestOnlyTheUnclockedUnitsMakeOneTry(t *testing.T) {
	b := restoreBox(t)
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Drill(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	o := inPlace()
	o.Confirm = func(RestoreAsk) bool { return true }
	if _, err := Restore(context.Background(), b.cfg, b, o); err != nil {
		t.Fatal(err)
	}
	unclocked := map[string]bool{"upload": true, "fetch": true, "repocheck": true}
	seen := map[string]bool{}
	for _, s := range b.specs {
		if len(s.Argv) == 0 || s.Argv[0] != b.cfg.Restic {
			continue
		}
		role := roleRe.FindStringSubmatch(s.Name)[1]
		argv := " " + strings.Join(s.Argv, " ") + " "
		tries := strings.Count(argv, " -o s3.retries=1 ")
		switch {
		case unclocked[role] && tries != 1:
			t.Errorf("%s has no clock and makes ten tries at a request: %q", role, s.Argv)
		case !unclocked[role] && strings.Contains(argv, "s3.retries"):
			t.Errorf("%s is clocked, or a listing, and was given %q", role, s.Argv)
		}
		seen[role] = true
	}
	for role := range unclocked {
		if !seen[role] {
			t.Errorf("no %s unit was started (started: %s)", role, b.roles())
		}
	}
	if !seen["history"] && !seen["vouch"] {
		t.Errorf("no other restic unit was looked at (started: %s)", b.roles())
	}
}

// Nothing an operator or an app chose is in any command line: systemd
// never expands it there, and it is not there to expand.
func TestDeclaredStringsNeverReachArgv(t *testing.T) {
	b := newBox(t)
	// (Braces are refused long before this: a placeholder. What is left
	// is what systemd itself would expand.)
	b.root = filepath.Join(filepath.Dir(b.root), "live $RESTIC_PASSWORD swap")
	evil := "up $RESTIC_PASSWORD $HOME %h"
	must(t, os.MkdirAll(filepath.Join(b.root, "blog", "shared", evil), 0o755))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["$DB.db"],"files":[%q]}}}`, b.root, evil)
	b.ls = func(string) string { return "" }
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	for _, s := range b.specs {
		for _, arg := range s.Argv {
			if strings.ContainsAny(arg, "$%") || strings.Contains(arg, "swap") {
				t.Errorf("%s: argv holds %q", s.Name, arg)
			}
		}
	}
}

func TestADatabaseInsideAFilesPathIsMaskedThere(t *testing.T) {
	b := newBox(t)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["data/app.db"],"files":["."]}}}`, b.root)
	b.ls = func(string) string { return "" }
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(b.spec("upload").Masked, " ")
	want := "/backup/blog/files/data/app.db /backup/blog/files/data/app.db-wal /backup/blog/files/data/app.db-shm /backup/blog/files/data/app.db-journal"
	if got != want {
		t.Fatalf("masked: %s\nwant:   %s", got, want)
	}
}

func TestNoDataYetAndDataGone(t *testing.T) {
	b := newBox(t)
	b.history = "[]"
	must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if app := st.Apps["blog"]; app.Class != record.Pending || !strings.Contains(app.Detail, b.root) {
		t.Fatalf("an app never deployed and never backed up: %+v", app)
	}
	// Staging is emptied whatever is found, and — the record holding no
	// snapshot of this app — the repository is asked: every such run,
	// since an answer of "none" that was kept could go stale.
	for i := 0; i < 2; i++ {
		if got := b.roles(); got != "plan clean history" {
			t.Fatalf("run %d: units started for an app with no data: %s", i+1, got)
		}
		b.specs = nil
		if st, err = Run(context.Background(), b.cfg, b); err != nil || st.Apps["blog"].Class != record.Pending {
			t.Fatalf("%+v, %v", st.Apps["blog"], err)
		}
	}
	b.specs = nil

	// Deployed, backed up, and then the data goes.
	must(t, os.MkdirAll(filepath.Join(b.root, "blog", "shared", "uploads"), 0o755))
	if st, err = Run(context.Background(), b.cfg, b); err != nil || st.Apps["blog"].Class != record.OK {
		t.Fatalf("%+v, %v", st.Apps["blog"], err)
	}
	must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
	st, err = Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	app := st.Apps["blog"]
	if app.Class != record.DataMissing || !strings.Contains(app.Detail, snapA[:8]) || app.LastOK == nil {
		t.Fatalf("data that was backed up and is now gone must never read as not-yet-deployed: %+v", app)
	}
}

// Only "no such file" means the data is absent.
func TestAnErrorLookingAtTheDataIsNotItsAbsence(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root is refused nothing")
	}
	b := newBox(t)
	must(t, os.Chmod(filepath.Join(b.root, "blog"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(b.root, "blog"), 0o755) })
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if app := st.Apps["blog"]; app.Class != record.Failed || !strings.Contains(app.Detail, "permission denied") {
		t.Fatalf("%+v", app)
	}
}

func TestHowAnUploadEnds(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome unit.Outcome
		summary string
		class   record.Class
		detail  string
		wide    bool
	}{
		"exit 0 and no snapshot id":   {unit.Outcome{Result: "success"}, `{"message_type":"summary"}`, record.Failed, "names no snapshot", false},
		"exit 0 and a bad id":         {unit.Outcome{Result: "success"}, `{"message_type":"summary","snapshot_id":"latest"}`, record.Failed, "names no snapshot", false},
		"exit 3 with a snapshot":      {unit.Outcome{Result: "exit-code", ExitStatus: 3}, "", record.Incomplete, "exit 3", false},
		"no repository":               {unit.Outcome{Result: "exit-code", ExitStatus: 10}, `{}`, record.Failed, "no repository", true},
		"locked":                      {unit.Outcome{Result: "exit-code", ExitStatus: 11}, `{}`, record.Failed, "stayed locked", true},
		"wrong password":              {unit.Outcome{Result: "exit-code", ExitStatus: 12}, `{}`, record.Failed, "password is wrong", true},
		"exit 1 is not no-repository": {unit.Outcome{Result: "exit-code", ExitStatus: 1}, `{}`, record.Failed, "could not be reached, or refused the key", true},
		"killed":                      {unit.Outcome{Result: "oom-kill", ExitStatus: 9}, `{}`, record.Failed, "ended by oom-kill", false},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			must(t, os.MkdirAll(filepath.Join(b.root, "shop", "shared"), 0o755))
			b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads"]},"shop":{"files":["."]}}}`, b.root)
			b.outcome["upload"] = tc.outcome
			if tc.summary != "" {
				b.summary = tc.summary
			}
			st, err := Run(context.Background(), b.cfg, b)
			if err != nil {
				t.Fatal(err)
			}
			blog, shop := st.Apps["blog"], st.Apps["shop"]
			if blog.Class != tc.class || !strings.Contains(blog.Detail, tc.detail) {
				t.Fatalf("blog: %+v", blog)
			}
			if strings.Contains(blog.Detail, "no repository") && tc.outcome.ExitStatus != 10 {
				t.Fatalf("called a missing repository: %s", blog.Detail)
			}
			if tc.wide != (shop.Class == record.NotAttempted) {
				t.Fatalf("shop after blog's failure: %+v", shop)
			}
			// The listing that ends a run comes after it.
			if !strings.HasSuffix(strings.TrimSuffix(b.roles(), " listing"), "clean") {
				t.Fatalf("staging was not emptied after the failure: %s", b.roles())
			}
		})
	}
}

// A snapshot holds the file that was given to the backup: the listing
// says which file each node is, its inode and its owner, and through a
// bind mount those are the bound file's own [M63] — so an item the
// upload unit was shown something else in place of (a bare mount
// point, where the manager could not see the run's mount [M54]) is not
// taken as backed up for being a directory of the right name. An empty
// directory that is the one given is backed up, empty.
func TestASnapshotHoldsTheFileThatWasGiven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files string // the declared path
		node  func(given syscall.Stat_t) string
		bare  bool
		want  string // in the item's detail; empty means ok
	}{
		{"the directory that was given, and it is empty", "uploads", nil, false, ""},
		{"another directory in its place", "uploads", func(given syscall.Stat_t) string {
			return fmt.Sprintf(`{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir","inode":%d,"uid":%d,"gid":%d}`, given.Ino+1, given.Uid, given.Gid)
		}, false, "in the snapshot it is not the directory that was given to the backup"},
		{"the same inode, and root's", "uploads", func(given syscall.Stat_t) string {
			return fmt.Sprintf(`{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir","inode":%d,"uid":%d,"gid":%d}`, given.Ino, given.Uid+1, given.Gid)
		}, false, "in the snapshot it is not the directory that was given to the backup"},
		{"the same inode, another group's", "uploads", func(given syscall.Stat_t) string {
			return fmt.Sprintf(`{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir","inode":%d,"uid":%d,"gid":%d}`, given.Ino, given.Uid, given.Gid+1)
		}, false, "in the snapshot it is not the directory that was given to the backup"},
		{"a listing that does not say which file it is", "uploads", nil, true, "the listing does not say which file it is"},
		{"the file that was given", "uploads/a.png", nil, false, ""},
		{"another file in its place", "uploads/a.png", func(given syscall.Stat_t) string {
			return fmt.Sprintf(`{"struct_type":"node","path":"/backup/blog/files/uploads/a.png","type":"file","size":3,"inode":%d,"uid":%d,"gid":%d}`, given.Ino+1, given.Uid, given.Gid)
		}, false, "in the snapshot it is not the file that was given to the backup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBox(t)
			must(t, os.WriteFile(filepath.Join(b.root, "blog", "shared", "uploads", "a.png"), []byte("pic"), 0o644))
			if tc.files == "uploads" {
				must(t, os.Remove(filepath.Join(b.root, "blog", "shared", "uploads", "a.png")))
			}
			b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":[%q]}}}`, b.root, tc.files)
			var given syscall.Stat_t
			must(t, syscall.Stat(filepath.Join(b.root, "blog", "shared", tc.files), &given))
			kind := "dir"
			if tc.files != "uploads" {
				kind = "file"
			}
			b.bareNodes = tc.bare
			b.ls = func(string) string {
				if tc.node != nil {
					return tc.node(given)
				}
				return fmt.Sprintf(`{"struct_type":"node","path":"/backup/blog/files/%s","type":%q,"size":3}`, tc.files, kind)
			}
			st, err := Run(context.Background(), b.cfg, b)
			if err != nil {
				t.Fatal(err)
			}
			app := st.Apps["blog"]
			if len(app.Items) != 1 {
				t.Fatalf("%+v", app)
			}
			it := app.Items[0]
			if tc.want == "" {
				if !it.OK || app.Class != record.OK {
					t.Fatalf("not ok: %+v, %+v", app, it)
				}
				return
			}
			if it.OK || !strings.Contains(it.Detail, tc.want) || app.Class != record.Incomplete || app.LastOK != nil {
				t.Fatalf("taken as backed up: %+v, %+v\nwant the item to say %q", app, it, tc.want)
			}
			// What it is and what was given, so that whoever reads the
			// record can look: the inodes and the owners.
			if tc.node != nil && !strings.Contains(it.Detail, fmt.Sprintf("inode %d, owner %d:%d", given.Ino, given.Uid, given.Gid)) {
				t.Errorf("the detail does not say what was given: %q", it.Detail)
			}
		})
	}
}

// restic leaves a vanished path out with exit 0, so the listing is what
// finds it.
func TestADeclaredPathMissingFromTheSnapshotIsNotOK(t *testing.T) {
	b := newBox(t)
	b.ls = func(parent string) string {
		if parent == "/backup/blog/sqlite" {
			return `{"struct_type":"node","path":"/backup/blog/sqlite/app.db","type":"file","size":0}`
		}
		return `{"struct_type":"node","path":"/backup/blog/files/other","type":"dir"}`
	}
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	app := st.Apps["blog"]
	if app.Class != record.Incomplete || app.LastOK != nil {
		t.Fatalf("%+v", app)
	}
	for _, it := range app.Items {
		if it.OK {
			t.Errorf("%s %q is reported as in the snapshot", it.Kind, it.Path)
		}
	}
}

func TestOneDatabaseFailingDoesNotStopTheRest(t *testing.T) {
	b := newBox(t)
	b.dump = func(paths []string) []dump.Result {
		return []dump.Result{{Path: "app.db", Class: dump.NeverOpened, Detail: "killed"}}
	}
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	app := st.Apps["blog"]
	if app.Class != record.Incomplete || app.Snapshot == nil || !strings.Contains(app.Detail, "never opened") {
		t.Fatalf("the files are still worth uploading, and the run is not ok: %+v", app)
	}
}

func TestADumpUnitThatLiesIsNotBelieved(t *testing.T) {
	b := newBox(t)
	b.dump = func([]string) []dump.Result { return []dump.Result{{Path: "other.db", Class: dump.OK}} }
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if it := st.Apps["blog"].Items[0]; it.OK || !strings.Contains(it.Detail, "other.db") {
		t.Fatalf("%+v", it)
	}
}

func TestAPlanThatCannotBeMadeKeepsWhatWasKnown(t *testing.T) {
	b := newBox(t)
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	b.specs = nil
	b.outcome["plan"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	st, err := Run(context.Background(), b.cfg, b)
	if err == nil || st.Error == "" {
		t.Fatalf("want the run to fail, got %+v, %v", st, err)
	}
	if b.roles() != "plan" {
		t.Fatalf("units after a failed plan: %s", b.roles())
	}
	// Not as whatever the last run found, under this run's date: as not
	// run, with when it was last ok.
	if app := st.Apps["blog"]; app == nil || app.Class != record.NotRun || app.Snapshot != nil || app.LastOK == nil || app.LastOK.ID != snapA {
		t.Fatalf("an app the run never reached: %+v", st.Apps["blog"])
	}

	// And a plan root would not accept is not acted on, whatever the
	// unit that made it says it checked.
	b.outcome = map[string]unit.Outcome{}
	b.plan = `{"root":"/var/lib/liveswap","apps":{"../etc":{"files":["."]}}}`
	if _, err := Run(context.Background(), b.cfg, b); err == nil || !strings.Contains(err.Error(), "is not one liveswap accepts") {
		t.Fatalf("%v", err)
	}
}

func TestLeftoversAreStoppedByName(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(b.cfg.RunDir, 0o755))
	left := "motel_backup_upload_blog_0123456789ab.service"
	must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "units"), []byte(left+"\n"), 0o600))
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if len(b.stopped) != 1 || b.stopped[0] != left {
		t.Fatalf("stopped: %v", b.stopped)
	}
	raw, _ := os.ReadFile(filepath.Join(b.cfg.RunDir, "units"))
	if strings.Contains(string(raw), left) || len(strings.Fields(string(raw))) != len(b.specs) {
		t.Fatalf("the unit list after the run:\n%s", raw)
	}
}

// A declared path is the app's own to replace, and the upload unit
// reads any file it is shown. A link is refused wherever it sits, and
// the manager is never given the app's name for anything: only mount
// points of the run's own. (That a mount made from a pin cannot be
// re-aimed is the kernel's doing, and the integration suite's to show.)
func TestALinkIsNeverFollowedAndNothingIsBoundByName(t *testing.T) {
	b := newBox(t)
	sibling := filepath.Join(b.root, "shop", "shared")
	must(t, os.MkdirAll(sibling, 0o755))
	must(t, os.WriteFile(filepath.Join(sibling, "orders.txt"), []byte("the sibling's"), 0o600))
	shared := filepath.Join(b.root, "blog", "shared")
	must(t, os.Symlink(sibling, filepath.Join(shared, "linked")))
	must(t, os.MkdirAll(filepath.Join(shared, "media"), 0o755))
	must(t, os.Symlink(sibling, filepath.Join(shared, "media", "deep")))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads","linked","media/deep/x"]}}}`, b.root)
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }

	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	up := b.spec("upload")
	for _, bind := range up.Binds {
		if strings.Contains(bind.Dest, "linked") || strings.Contains(bind.Dest, "deep") {
			t.Errorf("a path through a link was bound: %+v", bind)
		}
		if strings.HasPrefix(bind.Dest, "/backup/blog/files") {
			// By a mount point of the run's own, never by a name the app
			// can re-aim between the look and the bind.
			if !strings.HasPrefix(bind.Source, b.cfg.RunDir+"/") {
				t.Errorf("a declared path is bound by name: %+v", bind)
			}
			if got, want := b.mounted[bind.Source], filepath.Join(shared, "uploads"); got != want {
				t.Errorf("the mount point was made from %q, want %q", got, want)
			}
		}
	}
	app := st.Apps["blog"]
	if app.Class != record.Incomplete {
		t.Fatalf("%+v", app)
	}
	for _, it := range app.Items[1:] {
		if it.OK || !strings.Contains(it.Detail, "symbolic link") {
			t.Errorf("%q: %+v", it.Path, it)
		}
	}
}

func TestASharedDirReachedThroughALinkIsRefused(t *testing.T) {
	b := newBox(t)
	shared := filepath.Join(b.root, "blog", "shared")
	must(t, os.Rename(shared, shared+".real"))
	must(t, os.Symlink(filepath.Join(b.root, "shop", "shared"), shared))
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if app := st.Apps["blog"]; app.Class != record.Failed || !strings.Contains(app.Detail, "symbolic link") {
		t.Fatalf("%+v", app)
	}
	if b.roles() != "plan clean" {
		t.Fatalf("units were started for it: %s", b.roles())
	}
}

// The liveswap root itself may be an alias; liveswap allows that, and
// it is the operator's to set.
func TestASymlinkedRootIsFollowed(t *testing.T) {
	b := newBox(t)
	alias := filepath.Join(filepath.Dir(b.root), "alias")
	must(t, os.Symlink(b.root, alias))
	b.plan = strings.Replace(b.plan, fmt.Sprintf("%q", b.root), fmt.Sprintf("%q", alias), 1)
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil || st.Apps["blog"].Class != record.OK {
		t.Fatalf("%+v, %v", st.Apps["blog"], err)
	}
}

func TestEveryMountIsTakenAwayAndLeftoversAreSwept(t *testing.T) {
	b := newBox(t)
	left := filepath.Join(b.cfg.RunDir, "deadbeef0000", "mount-1")
	must(t, os.MkdirAll(left, 0o700))
	b.leftMounts = []string{left}
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if len(b.unmounted) == 0 || b.unmounted[0] != left {
		t.Fatalf("the earlier run's mount was not taken away first: %v", b.unmounted)
	}
	for target := range b.mounted {
		found := false
		for _, u := range b.unmounted {
			found = found || u == target
		}
		if !found {
			t.Errorf("%s was mounted and never unmounted", target)
		}
	}
	if _, err := os.Stat(filepath.Dir(left)); err == nil {
		t.Errorf("the earlier run's directory is still there")
	}
}

// An app that has only ever been incomplete has still been backed up.
func TestDataMissingDoesNotNeedAnOKRun(t *testing.T) {
	b := newBox(t)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads","never-there"]}}}`, b.root)
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil || st.Apps["blog"].Class != record.Incomplete || st.Apps["blog"].LastOK != nil || st.Apps["blog"].LastSnapshot == nil {
		t.Fatalf("%+v, %v", st.Apps["blog"], err)
	}
	must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
	for i := 0; i < 2; i++ { // and it stays so on the run after
		st, err = Run(context.Background(), b.cfg, b)
		if err != nil || st.Apps["blog"].Class != record.DataMissing {
			t.Fatalf("run %d after the data went: %+v, %v", i+1, st.Apps["blog"], err)
		}
	}
}

func TestWhatADumpUnitSaysIsCheckedBeforeAnyOfItIsKept(t *testing.T) {
	for name, tc := range map[string]struct {
		results []dump.Result
		want    string
	}{
		"right about the first, wrong about the second": {[]dump.Result{{Path: "a.db", Class: dump.OK}, {Path: "other.db", Class: dump.OK}}, "other.db"},
		"a class that is not one":                       {[]dump.Result{{Path: "a.db", Class: dump.OK}, {Path: "b.db", Class: "ok\x1b[2Jfine"}}, "not an answer"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["a.db","b.db"],"files":["uploads"]}}}`, b.root)
			b.dump = func([]string) []dump.Result { return tc.results }
			b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }
			st, err := Run(context.Background(), b.cfg, b)
			if err != nil {
				t.Fatal(err)
			}
			items := st.Apps["blog"].Items
			if len(items) != 3 {
				t.Fatalf("%d items for three declared paths: %+v", len(items), items)
			}
			for _, it := range items[:2] {
				if it.OK || !strings.Contains(it.Detail, tc.want) || strings.ContainsRune(it.Detail, 0x1b) {
					t.Errorf("%+v", it)
				}
			}
		})
	}
}

// What sqlite3 says of a database holds names the app chose.
func TestAUnitsWordsAreCleanedBeforeTheyAreKept(t *testing.T) {
	b := newBox(t)
	b.dump = func([]string) []dump.Result {
		return []dump.Result{{Path: "app.db", Class: dump.CopyIsDamaged, Detail: "row 1 missing from index \x1b]0;owned\x07\x1b[2J" + strings.Repeat("x", 2000)}}
	}
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(b.cfg.StateDir, "status.json"))
	if d := st.Apps["blog"].Items[0].Detail; strings.ContainsAny(d, "\x1b\x07") || len([]rune(d)) > 320 || strings.Contains(string(raw), `\u001b`) {
		t.Fatalf("kept as: %q", d)
	}
}

func TestADataDirThatIsNotTheDataUsersIsRefused(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to give a directory away")
	}
	b := newBox(t)
	must(t, os.Chown(filepath.Join(b.root, "blog", "shared"), 12345, 12345))
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if app := st.Apps["blog"]; app.Class != record.Failed || !strings.Contains(app.Detail, "does not belong to") {
		t.Fatalf("%+v", app)
	}
	if strings.Contains(b.roles(), "upload") || strings.Contains(b.roles(), "dump") {
		t.Fatalf("units were shown it: %s", b.roles())
	}
}

func TestStagingOfAnAppThatNoLongerDeclaresIsEmptiedAndRemoved(t *testing.T) {
	b := newBox(t)
	gone := filepath.Join(b.cfg.StateDir, "staging", "shop")
	must(t, os.MkdirAll(gone, 0o700))
	must(t, os.MkdirAll(filepath.Join(b.cfg.StateDir, "staging", "Not_An_App"), 0o700))
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	var cleaned []string
	for _, s := range b.specs {
		if roleRe.FindStringSubmatch(s.Name)[1] == "clean" {
			cleaned = append(cleaned, s.Binds[0].Source)
		}
	}
	if len(cleaned) < 3 || cleaned[0] != gone {
		t.Fatalf("cleaned: %v", cleaned)
	}
	if _, err := os.Stat(gone); err == nil {
		t.Errorf("%s is still there", gone)
	}
	for _, c := range cleaned {
		if strings.Contains(c, "Not_An_App") {
			t.Errorf("a unit was named after a directory that is no app's: %s", c)
		}
	}
}

func TestAnUnreadableRecordIsSaidAndDoesNotStopBackups(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, os.WriteFile(filepath.Join(b.cfg.StateDir, "status.json"), nil, 0o644)) // what a power cut leaves
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil || st.Apps["blog"].Class != record.OK || !strings.Contains(st.Warning, "could not be read") {
		t.Fatalf("%+v, %v", st, err)
	}
}

// The one declared string on any command line: the directory part of a
// nested path, to `restic ls`, after "--".
func TestOnlyTheParentOfANestedPathReachesACommandLine(t *testing.T) {
	b := newBox(t)
	nested := "me $DIA %h/up loads"
	must(t, os.MkdirAll(filepath.Join(b.root, "blog", "shared", nested), 0o755))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":[%q]}}}`, b.root, nested)
	b.ls = func(string) string { return "" }
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	for _, s := range b.specs {
		role := roleRe.FindStringSubmatch(s.Name)[1]
		for i, arg := range s.Argv {
			if !strings.ContainsAny(arg, "$%") {
				continue
			}
			if role != "verify" || arg != "/backup/blog/files/me $DIA %h" || i < 2 || s.Argv[i-2] != "--" {
				t.Errorf("%s: argv[%d] = %q", s.Name, i, arg)
			}
		}
	}
}

func TestALeftoverThatCannotBeStoppedIsRecorded(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
	must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "units"), []byte("motel_backup_upload_blog_0123456789ab.service\n"), 0o600))
	b.stopErr = errors.New("it would not stop")
	st, err := Run(context.Background(), b.cfg, b)
	if err == nil || st == nil || !strings.Contains(st.Error, "could not be stopped") {
		t.Fatalf("%+v, %v", st, err)
	}
	again, rerr := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	if rerr != nil || again.Error == "" {
		t.Fatalf("the record of it: %+v, %v", again, rerr)
	}
}

func TestInterruptedAfterTheDumpUploadsNothing(t *testing.T) {
	b := newBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.before = func(s unit.Spec) {
		if roleRe.FindStringSubmatch(s.Name)[1] == "dump" {
			cancel()
		}
	}
	st, _ := Run(ctx, b.cfg, b)
	if strings.Contains(b.roles(), "upload") {
		t.Fatalf("an upload was started after the interrupt: %s", b.roles())
	}
	if !strings.HasSuffix(b.roles(), "clean") {
		t.Fatalf("staging was not emptied: %s", b.roles())
	}
	if app := st.Apps["blog"]; app == nil || !strings.Contains(app.Detail, "interrupted") {
		t.Fatalf("%+v", st.Apps["blog"])
	}
}

// The exclude file stands behind the masks. restic reads a line as a
// pattern and expands $VAR in it, so a declared name is escaped until
// it means itself and nothing else.
func TestExcludesNameExactlyTheDeclaredDatabases(t *testing.T) {
	decl := &backupdecl.Config{
		SQLite: []string{`da[t]a/app*.db`, `q?$HOME\x.db`, "elsewhere/other.db"},
		Files:  []string{"da[t]a", "."},
	}
	got := excludes("blog", decl)
	for _, want := range []string{
		`/backup/blog/files/da\[t]a/app\*.db` + "\n",
		`/backup/blog/files/da\[t]a/app\*.db-wal` + "\n",
		`/backup/blog/files/q\?$$HOME\\x.db-journal` + "\n",
		"/backup/blog/files/elsewhere/other.db-shm\n", // inside "."
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if none := excludes("blog", &backupdecl.Config{SQLite: []string{"app.db"}, Files: []string{"uploads"}}); none != "" {
		t.Errorf("a database outside every files path is excluded from nothing, got %q", none)
	}

	b := newBox(t)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["data/app.db"],"files":["."]}}}`, b.root)
	b.ls = func(string) string { return "" }
	var file string
	b.before = func(s unit.Spec) {
		if roleRe.FindStringSubmatch(s.Name)[1] != "upload" {
			return
		}
		for _, bind := range s.Binds {
			if bind.Dest == excludePath {
				raw, _ := os.ReadFile(bind.Source)
				file = string(raw)
			}
		}
		if !strings.Contains(strings.Join(s.Argv, " "), "--exclude-file "+excludePath+" ") {
			t.Errorf("restic is not told of it: %v", s.Argv)
		}
	}
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(file, "/backup/blog/files/data/app.db-wal\n") {
		t.Fatalf("the upload unit's exclude file: %q", file)
	}
}

// A mount that would not go is an app's data, bound into the run's
// directory. Root never walks into it deleting.
func TestRemovingARunDirNeverDescends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	stillMounted := filepath.Join(dir, "mount-1")
	must(t, os.MkdirAll(stillMounted, 0o700))
	must(t, os.MkdirAll(filepath.Join(dir, "mount-2"), 0o700)) // unmounted: empty
	must(t, os.WriteFile(filepath.Join(stillMounted, "the-apps-database"), []byte("precious"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "plan.json"), []byte("{}"), 0o600))
	removeRunDir(dir)
	if raw, err := os.ReadFile(filepath.Join(stillMounted, "the-apps-database")); err != nil || string(raw) != "precious" {
		t.Fatalf("what was under a mount point that is not empty was touched: %q, %v", raw, err)
	}
	for _, gone := range []string{"mount-2", "plan.json"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
}

func TestPlaintextLeftByADepartedAppIsSaid(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(filepath.Join(b.cfg.StateDir, "staging", "shop"), 0o700))
	b.failClean = "shop"
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil || st.Apps["blog"].Class != record.OK {
		t.Fatalf("the run goes on: %+v, %v", st.Apps["blog"], err)
	}
	if !strings.Contains(st.Warning, "shop") || !strings.Contains(st.Warning, "may still be in") {
		t.Fatalf("warning: %q", st.Warning)
	}
}

// A rebuilt box: no record, and the data is not there. The repository
// remembers what the box does not, and "not deployed yet" — which a run
// exits 0 on — is never said of an app it holds snapshots of.
func TestWithNoRecordTheRepositoryIsAskedWhetherTheAppWasEverBackedUp(t *testing.T) {
	older, newer := "1111111111111111111111111111111111111111111111111111111111111111", "2222222222222222222222222222222222222222222222222222222222222222"
	for name, tc := range map[string]struct {
		history string
		outcome *unit.Outcome
		class   record.Class
		want    string
	}{
		// The newest stored in another zone than UTC, as a restic run in
		// the box's own zone stores it: said as every command says a time.
		"it holds snapshots of the app": {`[{"id":"` + newer + `","time":"2026-09-18T12:00:00+02:00"},{"id":"` + older + `","time":"2026-09-01T10:00:00Z"}]`, nil, record.DataMissing, "last backed up on 2026-09-18 10:00 UTC (snapshot " + newer[:8] + ")"},
		"it holds none":                 {`[]`, nil, record.Pending, "not been deployed"},
		"it cannot be asked":            {``, &unit.Outcome{Result: "exit-code", ExitStatus: 1}, record.Failed, "could not be asked"},
		"it answers nonsense":           {`{"id":"latest"}`, nil, record.Failed, "could not be asked"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
			b.history = tc.history
			if tc.outcome != nil {
				b.outcome["history"] = *tc.outcome
			}
			st, err := Run(context.Background(), b.cfg, b)
			if err != nil {
				t.Fatal(err)
			}
			app := st.Apps["blog"]
			if app.Class != tc.class || !strings.Contains(app.Detail, tc.want) {
				t.Fatalf("%+v", app)
			}
			h := b.spec("history")
			if h.User != backupUser || !h.Network || h.EnvironmentFile == "" || len(h.Capabilities) != 0 || len(h.Binds) != 0 {
				t.Errorf("the unit that asks: %+v", h)
			}
			if got := strings.Join(h.Argv[1:], " "); got != "snapshots --json --no-lock --host motel --tag app:blog" {
				t.Errorf("asks with: %s", got)
			}
		})
	}
}

// What a backup keeps is files and directories. A FIFO, a socket or a
// device at a declared path is not something a restore can put back.
func TestADeclaredFilesPathThatIsNotAFileOrADirectoryIsRefused(t *testing.T) {
	b := newBox(t)
	must(t, syscall.Mkfifo(filepath.Join(b.root, "blog", "shared", "pipe"), 0o644))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads","pipe"]}}}`, b.root)
	// And what the snapshot says a path is counts too.
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"fifo"}` }
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, bind := range b.spec("upload").Binds {
		if strings.Contains(bind.Dest, "pipe") {
			t.Errorf("the FIFO was bound into the upload unit: %+v", bind)
		}
	}
	items := st.Apps["blog"].Items
	if items[1].OK || !strings.Contains(items[1].Detail, "fifo") {
		t.Errorf("the FIFO: %+v", items[1])
	}
	if items[0].OK || !strings.Contains(items[0].Detail, "in the snapshot it is a fifo") {
		t.Errorf("a path the snapshot holds as a FIFO: %+v", items[0])
	}
}

// One app's failure is not its neighbours' every run: what failed last
// time goes last this time.
func TestAnAppThatFailedLastRunGoesLast(t *testing.T) {
	b := newBox(t)
	for _, app := range []string{"aaa", "zzz"} {
		must(t, os.MkdirAll(filepath.Join(b.root, app, "shared", "uploads"), 0o755))
	}
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"aaa":{"files":["uploads"]},"blog":{"files":["uploads"]},"zzz":{"files":["uploads"]}}}`, b.root)
	b.ls = func(parent string) string {
		return `{"struct_type":"node","path":"` + parent + `/uploads","type":"dir"}`
	}
	b.uploadExit = map[string]int{"aaa": 1}
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if st.Apps["aaa"].Class != record.Failed || st.Apps["blog"].Class != record.NotAttempted || st.Apps["zzz"].Class != record.NotAttempted {
		t.Fatalf("the first run: %v %v %v", st.Apps["aaa"].Class, st.Apps["blog"].Class, st.Apps["zzz"].Class)
	}
	if st, err = Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if st.Apps["blog"].Class != record.OK || st.Apps["zzz"].Class != record.OK || st.Apps["aaa"].Class != record.Failed {
		t.Fatalf("the second run, with aaa still failing: blog %v, zzz %v, aaa %v", st.Apps["blog"].Class, st.Apps["zzz"].Class, st.Apps["aaa"].Class)
	}
}

// A run reads the repository the credential file names as the manager
// will give it to every unit — the last assignment, quotes, escapes and
// whitespace as systemd takes them, a comment as none — and refuses a
// b2: one before any unit, recorded; anything the units would not be
// given as b2: runs.
func TestARunReadsTheRepositoryAsTheUnitsAreGivenIt(t *testing.T) {
	const s3 = "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\n"
	for _, tc := range []struct {
		name, file string
		refused    bool
	}{
		{"plain", "RESTIC_REPOSITORY=b2:hs-backups:dev\n", true},
		{"double quotes", "RESTIC_REPOSITORY=\"b2:hs-backups:dev\"\n", true},
		{"single quotes", "RESTIC_REPOSITORY='b2:hs-backups:dev'\n", true},
		{"whitespace the manager trims", "  RESTIC_REPOSITORY =  b2:hs-backups:dev  \n", true},
		{"an escaped colon", "RESTIC_REPOSITORY=b2\\:hs-backups:dev\n", true},
		{"a line joined by a backslash", "RESTIC_REPOSITORY=b2\\\n:hs-backups:dev\n", true},
		{"b2: last of two", s3 + "RESTIC_REPOSITORY=b2:hs-backups:dev\n", true},
		{"b2: first of two: the last wins", "RESTIC_REPOSITORY=b2:hs-backups:dev\n" + s3, false},
		{"b2: in a # comment", "#RESTIC_REPOSITORY=b2:hs-backups:dev\n" + s3, false},
		{"b2: in a ; comment", ";RESTIC_REPOSITORY=b2:hs-backups:dev\n" + s3, false},
		{"b2: in another key", "RESTIC_REPOSITORY_OLD=b2:hs-backups:dev\n" + s3, false},
		{"b2: inside the value", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/b2:x\n", false},
		{"no repository", "", false},
		{"the repository named in a file restic reads", "RESTIC_REPOSITORY_FILE=/etc/motel-backup/repo\n", true},
		{"RESTIC_REPOSITORY_FILE beside RESTIC_REPOSITORY", s3 + "RESTIC_REPOSITORY_FILE=/etc/motel-backup/repo\n", true},
		{"RESTIC_REPOSITORY_FILE set empty", "RESTIC_REPOSITORY_FILE=\n" + s3, false},
		{"a byte that is not UTF-8 in a value", s3 + "NOTE=caf\xe9\n", true},
		{"a byte that is not UTF-8 in a comment, which the manager takes", s3 + "# caf\xe9\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBox(t)
			must(t, os.WriteFile(b.cfg.EnvFile, []byte(tc.file+"RESTIC_PASSWORD=the-secret\n"), 0o600))
			st, err := Run(context.Background(), b.cfg, b)
			want := "the repository is b2:, which is not supported: "
			if strings.Contains(tc.file, "RESTIC_REPOSITORY_FILE=/") {
				want = "the credential file " + b.cfg.EnvFile + " names the repository in RESTIC_REPOSITORY_FILE, which is not supported: name it in RESTIC_REPOSITORY"
			}
			if strings.Contains(tc.file, "=caf\xe9") {
				want = "the credential file " + b.cfg.EnvFile + ": line 2: the value of NOTE holds a byte that is not UTF-8: the manager refuses the whole file"
			}
			refused := err != nil && strings.HasPrefix(err.Error(), want)
			if refused != tc.refused {
				t.Fatalf("refused %v, want %v: %v", refused, tc.refused, err)
			}
			if !tc.refused {
				if err != nil || b.roles() == "" {
					t.Fatalf("the run: %v; units %q", err, b.roles())
				}
				return
			}
			if b.roles() != "" || st == nil || !strings.HasPrefix(st.Error, want[:30]) {
				t.Fatalf("units %q; record %+v", b.roles(), st)
			}
		})
	}
}
