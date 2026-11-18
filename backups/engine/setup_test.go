package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/backups/envfile"
	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/unit"
)

const (
	repoID = "bbd0e899f1b628ed8f158336b70b450ef1c2e86ff7eb636f5e85b5911ca42fe1"
	// What restic 0.18 says [measured, M39/M40]: one line on stdout for
	// a repository it made; on stderr, as --json puts it, for one that
	// was there already; and its config, for a password that opens it.
	initialized = `{"message_type":"initialized","id":"` + repoID + `","repository":"s3:http://e2e-s3:9000/box"}` + "\n"
	alreadyInit = `{"message_type":"exit_error","code":1,"message":"Fatal: create key in repository at s3:http://e2e-s3:9000/box failed: repository master key and config already initialized\n"}` + "\n"
	// The other wording restic has for it, where the backend's Stat of
	// the config answers [measured: a local repository].
	alreadyThere = `{"message_type":"exit_error","code":1,"message":"Fatal: create repository at s3:http://e2e-s3:9000/box failed: Fatal: unable to open repository at s3:http://e2e-s3:9000/box: config file already exists\n"}` + "\n"
	wrongKey     = `{"message_type":"exit_error","code":1,"message":"Fatal: create repository at s3:http://e2e-s3:9000/box failed: Fatal: unable to open repository at s3:http://e2e-s3:9000/box: client.BucketExists: The request signature we calculated does not match the signature you provided. Check your key and signing method.\n"}` + "\n"
	// A bucket not there, and a key that may not make it: restic's S3
	// backend tries to make the bucket [measured, B2's S3 endpoint, RC
	// check 1].
	bucketNotThere = `{"message_type":"exit_error","code":1,"message":"Fatal: create repository at s3:http://e2e-s3:9000/box failed: Fatal: unable to open repository at s3:http://e2e-s3:9000/box: client.MakeBucket: not entitled\n"}` + "\n"
	configJSON     = `{"version": 2, "id": "` + repoID + `", "chunker_polynomial": "27ec33083365e9"}` + "\n"
	// What a look that could not tell adds to the password's showing.
	unsureStore = "The look could not tell whether a repository is there: store this password beside any you already keep for this repository, never in its place; if one is there, setup says so next and asks for its own."
	// What the look's `cat config` (no --json) says on stderr where the
	// storage refuses the key at once: over S3 restic gives up at once
	// only on a 403, which a HEAD's, having no body, reaches it as
	// "Access Denied." [restic 0.18.0's shape, from its source —
	// cmd/restic/global.go's open, internal/backend/s3/s3.go's Stat and
	// IsPermanentError, minio-go 7.0.89's 403 — measured on B2's S3
	// endpoint for a wrong secret, RC check 1 of v0.5.0-rc1, where restic
	// puts "Stat(<config/>) failed: Stat: Access Denied." before it]. The
	// same opening init's first step makes.
	refusedAtOnce = "Fatal: unable to open config file: Stat: Access Denied.\nIs there a repository at the following location?\n" + b2Repo + "\n"
	// A repository at B2's S3-compatible endpoint, as the guide writes one.
	b2Repo = "s3:https://s3.eu-central-003.backblazeb2.com/hs-rc-bucket/rc-check1"
	// What setup says of refusedAtOnce, cleaned: one line.
	refusedSaid = "restic could not open the repository (exit 1): Fatal: unable to open config file: Stat: Access Denied. Is there a repository at the following location? " + b2Repo
	// An init that got past the opening and failed writing: restic's
	// shape for it (cmd/restic/cmd_init.go, "create key in repository
	// at … failed"), with a reason of the storage's [not measured].
	keyWriteFailed = `{"message_type":"exit_error","code":1,"message":"Fatal: create key in repository at s3:http://e2e-s3:9000/box failed: Save(<key/8f2a1c>) returned error, retrying after 15m0s: 503 Service Unavailable\n"}` + "\n"
	// An init that found a key file and no config: restic's own check
	// before it writes (internal/repository/repository.go Init), as
	// --json puts it [the phrase from the source; not measured].
	halfThere = `{"message_type":"exit_error","code":1,"message":"Fatal: create key in repository at s3:http://e2e-s3:9000/box failed: repository already contains keys\n"}` + "\n"
)

// term is the operator: it answers in order, and remembers what it was
// asked and told. An answer that is not there is nobody answering.
type term struct {
	mu      sync.Mutex // Say is called from the clock's goroutine too
	answers []string
	asked   []string // the prompts, in order; "secret:" marks one asked with echo off
	said    []string
	// at runs at every prompt, before the answer: the moment to look at
	// what exists on disk.
	at func(prompt string)
}

func (m *term) Ask(ctx context.Context, prompt string, secret bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if secret {
		prompt = "secret:" + prompt
	}
	m.asked = append(m.asked, prompt)
	if m.at != nil {
		m.at(prompt)
	}
	// An interrupt at the prompt is no answer, as at a real terminal.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(m.answers) == 0 {
		return "", io.EOF
	}
	a := m.answers[0]
	m.answers = m.answers[1:]
	return a, nil
}

func (m *term) Say(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.said = append(m.said, line)
}

func (m *term) saidAll() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.said, "\n")
}

func (m *term) askedAll() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.asked, "\n")
}

// setupBox is a box with no credential file, whose init unit makes a
// repository, and whose account is there.
func setupBox(t *testing.T) (*box, *term) {
	t.Helper()
	b := newBox(t)
	must(t, os.Remove(b.cfg.EnvFile))
	b.cfg.EnvFile = filepath.Join(filepath.Dir(b.cfg.EnvFile), "etc", "motel-backup", "repository.env")
	b.cfg.OldEnvFile = filepath.Join(filepath.Dir(b.cfg.EnvFile), "..", "motel", "backup.env")
	b.initOut, b.probeOut = initialized, configJSON
	// A fresh box: the look for a repository finds none.
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
	b.version = 257
	// The account hooks are newBox's: a run looks at the account too.
	oldClock, oldProbe, oldNote, oldOwn, oldSync, oldID := setupClock, setupProbeClock, setupNote, ownByRoot, syncDir, writeID
	// chown to root is root's to do; here what is asked for is recorded.
	ownByRoot = func(path string) error { b.owned = append(b.owned, path); return nil }
	syncDir = func(path string) error { b.synced = append(b.synced, path); return nil }
	setupClock, setupProbeClock, setupNote = 200*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		setupClock, setupProbeClock, setupNote, ownByRoot, syncDir, writeID = oldClock, oldProbe, oldNote, oldOwn, oldSync, oldID
	})
	return b, &term{answers: []string{"AKIDX", "the-secret", "stored"}}
}

func (b *box) setup(t *testing.T, m *term, repo string) (*SetupReport, error) {
	t.Helper()
	return Setup(context.Background(), b.cfg, b, SetupOptions{Repository: repo, Terminal: m})
}

func envOf(t *testing.T, path string) envfile.Values {
	t.Helper()
	raw, err := os.ReadFile(path)
	must(t, err)
	v, findings := envfile.Parse(raw)
	if len(findings) != 0 {
		t.Fatalf("the file setup wrote is not read as written: %q", findings)
	}
	return v
}

// leftForInit: the working file untouched, and what the running init
// reads and writes left in place for it — the staged file it takes its
// environment from, and the run directory its output goes to — since
// the manager opens both in the unit's own first moments, which an
// interrupt may come before. The next lock holder waits for init, then
// sweeps both.
func leftForInit(t *testing.T, b *box, m *term) {
	t.Helper()
	if _, err := os.Lstat(b.cfg.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the credential file exists: %v", err)
	}
	if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 1 || left[0] != envfile.Staged(b.cfg.EnvFile) {
		t.Fatalf("beside the working file: %v, want the staged file alone", left)
	}
	entries, _ := os.ReadDir(b.cfg.RunDir)
	dirs := 0
	for _, e := range entries {
		if e.IsDir() {
			dirs++
		}
	}
	if dirs != 1 {
		t.Fatalf("the run directory init writes to is gone: %v", entries)
	}
	if len(m.asked) != 0 {
		t.Fatalf("prompts were asked: %q", m.asked)
	}
}

func nothingWritten(t *testing.T, b *box, m *term) {
	t.Helper()
	if _, err := os.Lstat(b.cfg.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the credential file exists: %v", err)
	}
	if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 0 {
		t.Fatalf("left beside it: %v", left)
	}
	if len(m.asked) != 0 {
		t.Fatalf("prompts were asked: %q", m.asked)
	}
}

var passwordRe = regexp.MustCompile(`\b([a-z2-7]{52})\b`)

func shownPassword(t *testing.T, m *term) string {
	t.Helper()
	pw := passwordRe.FindStringSubmatch(m.saidAll())
	if pw == nil {
		t.Fatalf("no password was shown:\n%s", m.saidAll())
	}
	return pw[1]
}

func TestAFreshSetupShowsThePasswordBeforeAnythingIsWritten(t *testing.T) {
	b, m := setupBox(t)
	var unitsBeforeStored, tmpAtStored, finalAtStored string
	m.at = func(prompt string) {
		if strings.Contains(prompt, "stored") {
			unitsBeforeStored = b.roles()
			for _, f := range func() []string { l, _ := filepath.Glob(b.cfg.EnvFile + ".*"); return l }() {
				raw, _ := os.ReadFile(f)
				tmpAtStored += string(raw)
			}
			if _, err := os.Lstat(b.cfg.EnvFile); err == nil {
				finalAtStored = "exists"
			}
		}
	}
	var finalAtInit bool
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			_, err := os.Lstat(b.cfg.EnvFile)
			finalAtInit = err == nil
		}
	}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if unitsBeforeStored != "plan probe" {
		t.Fatalf("before the password was confirmed stored, units had run: %s", unitsBeforeStored)
	}
	if finalAtInit || finalAtStored != "" {
		t.Fatal("the credential file was in place before the repository had answered")
	}
	pw := shownPassword(t, m)
	// The file the probe read holds a throwaway, never the password
	// that was shown: nothing shown has taken effect before "stored".
	if !strings.Contains(tmpAtStored, "AWS_SECRET_ACCESS_KEY=the-secret") || strings.Contains(tmpAtStored, pw) {
		t.Fatalf("at the stored prompt the temp file held %q", tmpAtStored)
	}
	if !strings.Contains(m.saidAll(), "looking for a repository at s3:http://e2e-s3:9000/box") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	// The look said none: no word of a repository that may be there.
	if strings.Contains(m.saidAll(), unsureStore) {
		t.Fatalf("a look that found none was said unsure:\n%s", m.saidAll())
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nType stored to go on: "; got != want {
		t.Fatalf("asked:\n%s\nwant:\n%s", got, want)
	}
	v := envOf(t, b.cfg.EnvFile)
	want := envfile.Values{"RESTIC_REPOSITORY": "s3:http://e2e-s3:9000/box", "RESTIC_PASSWORD": pw, "AWS_ACCESS_KEY_ID": "AKIDX", "AWS_SECRET_ACCESS_KEY": "the-secret"}
	if len(v) != len(want) {
		t.Fatalf("the file holds %v", v)
	}
	for k, w := range want {
		if v[k] != w {
			t.Fatalf("%s = %q, want %q", k, v[k], w)
		}
	}
	st, err := os.Stat(b.cfg.EnvFile)
	must(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("the file is %o", st.Mode().Perm())
	}
	if d, err := os.Stat(filepath.Dir(b.cfg.EnvFile)); err != nil || d.Mode().Perm() != 0o755 {
		t.Fatalf("the directory: %v %v", d, err)
	}
	// The directory is made root's whoever made it before, and before
	// anything is written into it.
	if len(b.owned) != 1 || b.owned[0] != filepath.Dir(b.cfg.EnvFile) {
		t.Fatalf("owned by root: %v", b.owned)
	}
	if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 0 {
		t.Fatalf("left beside it: %v", left)
	}
	// The init unit: the backup account, the credential through the
	// manager, network, no capability, and the temp file — the working
	// file is not touched until the repository has answered.
	s := b.spec("init")
	if s.User != backupUser || !s.Network || len(s.Capabilities) != 0 || s.SameUIDNamespaces || s.CacheDirectory != "motel-backup" {
		t.Fatalf("the init unit: %+v", s)
	}
	if p := b.spec("probe"); strings.Join(p.Argv, " ") != "/usr/bin/restic cat config --no-lock" || p.User != backupUser || !p.Network || len(p.Capabilities) != 0 || !strings.HasPrefix(p.EnvironmentFile, b.cfg.EnvFile+".") {
		t.Fatalf("the probe unit: %+v", p)
	}
	if s.EnvironmentFile == b.cfg.EnvFile || !strings.HasPrefix(s.EnvironmentFile, b.cfg.EnvFile+".") {
		t.Fatalf("the init unit reads %q", s.EnvironmentFile)
	}
	if got, want := strings.Join(s.Argv, " "), "/usr/bin/restic init --json"; got != want {
		t.Fatalf("argv %q", got)
	}
	if s.StdoutFile == "" || s.StderrFile == "" {
		t.Fatalf("restic's words are not kept where the run can read them: %+v", s)
	}
	for _, s := range b.specs {
		all := strings.Join(s.Argv, " ") + strings.Join(s.Environment, " ") + s.Description
		if strings.Contains(all, pw) || strings.Contains(all, "the-secret") || strings.Contains(all, "AKIDX") {
			t.Fatalf("a secret reached a unit's argv or environment: %+v", s)
		}
	}
	if !rep.New || rep.RepositoryID != repoID || rep.Account != "present" {
		t.Fatalf("report %+v", rep)
	}
	if !strings.Contains(m.saidAll(), "repository ready: s3:http://e2e-s3:9000/box (new, id bbd0e899)") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	if !strings.Contains(m.saidAll(), "no app declares a backup yet") && !strings.Contains(m.saidAll(), "blog") {
		t.Fatalf("the plan was not said:\n%s", m.saidAll())
	}
}

func TestNothingIsAskedBeforeThePreflightPasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(b *box)
		want   string
	}{
		{"restic missing", func(b *box) { b.haveProgram = func(p string) bool { return p != b.cfg.Restic } }, "restic, the backup program motel-backup runs, is not installed at /usr/bin/restic: apt install restic"},
		{"sqlite3 missing", func(b *box) { b.haveProgram = func(p string) bool { return p != "/usr/bin/sqlite3" } }, "sqlite3 is not installed at /usr/bin/sqlite3: apt install sqlite3"},
		{"moteld missing", func(b *box) { b.haveProgram = func(p string) bool { return p != "/usr/bin/moteld" } }, "moteld is not installed at /usr/bin/moteld"},
		{"this command elsewhere", func(b *box) { b.haveProgram = func(p string) bool { return p != b.cfg.Self } }, "motel-backup is not installed at /usr/bin/motel-backup, where the units run it"},
		{"old systemd", func(b *box) { b.version = 255 }, "systemd 257 or later is needed (Debian 13's); this box has 255"},
		{"the motel account missing", func(b *box) {
			dataOwner = func(context.Context) (int, int, error) { return 0, 0, errors.New("the motel account is not there") }
		}, "the motel account is not there"},
		{"the plan fails", func(b *box) {
			b.outcome["plan"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.planErr = "root depends on {$LIVESWAP_ROOT}\x1b[0m: give it a literal value\n"
		}, "could not be turned into a plan (exit 1): root depends on {$LIVESWAP_ROOT} [0m: give it a literal value"},
		{"the plan fails with nothing said", func(b *box) { b.outcome["plan"] = unit.Outcome{Result: "exit-code", ExitStatus: 1} }, "could not be turned into a plan (exit 1); `journalctl -u motel_backup_plan_"},
		{"a run holds the lock", func(b *box) {
			unlock, err := lock(filepath.Join(b.cfg.RunDir, "lock"))
			must(b.t, err)
			b.t.Cleanup(unlock)
		}, "another backup run is in progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
			tc.break_(b)
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			nothingWritten(t, b, m)
			if strings.Contains(b.roles(), "init") {
				t.Fatalf("a repository unit ran: %s", b.roles())
			}
		})
	}
}

// What restic reads as a URL, or as a host and bucket, is not refused
// for its scheme's case: only a scheme that is not "http…" in lower case
// at its start, which restic takes for a host.
func TestASchemeResticReadsIsNotRefusedForItsCase(t *testing.T) {
	for _, repo := range []string{"s3:https://s3.example.com/b", "s3:httpS://s3.example.com/b", "s3:http://e2e-s3:9000/box", "s3:s3.example.com/b", "s3://s3.example.com/b"} {
		if _, err := backendOf(repo, "/etc/motel-backup/repository.env"); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
	}
}

func TestARepositoryTheBoxCannotUseIsRefusedBeforeAnyPrompt(t *testing.T) {
	for _, tc := range []struct{ repo, want string }{
		{"sftp:user@host:/srv/backups", "sftp: is not supported"},
		{"/srv/backups", "a path on this box is not a repository"},
		{"local:/srv/backups", "a path on this box is not a repository"},
		{"rclone:remote:bucket", "rclone: is not supported"},
		{"azure:container:path", "azure: is not a backend Debian's restic has"},
		{"gs:bucket:path", "gs: and swift: are not set up by this command"},
		{"swift:container:/path", "gs: and swift: are not set up by this command"},
		{"s3:http://user:pass@e2e-s3:9000/box", "the repository URL holds an @, which is refused"},
		{"rest:https://user:pass@host/", "rest:, gs: and swift: are not set up by this command"},
		{"ftp://host/x", `"ftp:" is not a repository restic knows`},
		{"", "no repository was given"},
		{"s3:", "s3: names no bucket"},
		{"s3:http://e2e-s3:9000/box ", "the repository URL has whitespace at an end"},
		{"s3:AKID:secret@s3.example.com/bucket", "the repository URL holds an @, which is refused"},
		{"s3://AKID:secret@s3.example.com/bucket", "the repository URL holds an @, which is refused"},
		// A "://" in the path is not where the authority starts.
		{"s3:AKID:secret@s3.example.com/bucket/x://y", "the repository URL holds an @, which is refused"},
		{"s3://AKID:secret@s3.example.com/bucket/x://y", "the repository URL holds an @, which is refused"},
		{"s3:https://AKID:secret@s3.example.com/bucket/x://y", "the repository URL holds an @, which is refused"},
		// restic url.Parses whatever begins s3:http, whose scheme is read
		// in any case; an @ is refused before the scheme's case is.
		{"s3:httpS://AKID:secret@s3.example.com/bucket", "the repository URL holds an @, which is refused"},
		{"s3:hTTp://AKID:secret@s3.example.com/bucket", "the repository URL holds an @, which is refused"},
		{"s3:https+x://AKID:secret@s3.example.com/bucket", "the repository URL holds an @, which is refused"},
		// A secret with a character that ends an authority before its @.
		{"s3:https://AKID:se#cret@s3.example.com/bucket", "the repository URL holds an @, which is refused"},
		{"s3:https://AKID:se?cret@s3.example.com/bucket", "the repository URL holds an @, which is refused"},
		{"s3:https://AKIAEXAMPLE:wJalrXUtn/K7MDENG@s3.amazonaws.com/bucket", "the repository URL holds an @, which is refused"},
		{"s3:AKID:wJal/K7@s3.example.com/bucket", "the repository URL holds an @, which is refused"},
		// restic reads a scheme only in lower case: another is a host.
		{"s3:HTTPS://s3.eu-central-003.backblazeb2.com/bucket", "restic reads the scheme HTTPS: only in lower case, and otherwise takes it for a host: give the URL as s3:https://…"},
		{"s3:Http://e2e-s3:9000/box", "give the URL as s3:http://…"},
		{"s3:hTTp://e2e-s3:9000/box", "give the URL as s3:http://…"},
		// No bucket name holds an @; a path that does is refused with them.
		{"s3:https://s3.example.com/bucket/a@b", "no bucket name holds an @, so a path with one is not supported"},
		// B2's S3-compatible endpoint takes HTTPS alone.
		{"s3:http://s3.eu-central-003.backblazeb2.com/bucket", "B2's S3-compatible endpoint takes HTTPS alone"},
		{"s3:http://S3.EU-CENTRAL-003.BACKBLAZEB2.COM.:443/bucket", "B2's S3-compatible endpoint takes HTTPS alone"},
		{"s3:http://s3.us-west-004.backblazeb2.com:80/bucket", "B2's S3-compatible endpoint takes HTTPS alone"},
		{"s3:http://s3.us-west-004.backblazeb2.com:/bucket", "B2's S3-compatible endpoint takes HTTPS alone"},
		{"rest:user:pass@host:8000/", "rest:, gs: and swift: are not set up by this command"},
		{"s3:http://e2e-s3:9000/b\x01x", "the repository URL cannot go in the file: it holds a control character"},
		{"ftp://host/x", "(setup asks for s3:)"},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			b, m := setupBox(t)
			_, err := b.setup(t, m, tc.repo)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			nothingWritten(t, b, m)
			if b.roles() != "" {
				t.Fatalf("units ran: %s", b.roles())
			}
		})
	}
}

// restic's own B2 backend retries a failed upload without end (#177),
// so b2: is refused before any prompt, in every form restic reads, and
// the refusal names the same repository at B2's S3-compatible
// endpoint: the bucket and path as restic's b2 backend reads them, the
// region left for the operator. Nothing is made, the account included.
func TestAB2RepositoryIsRefusedBeforeAnyPromptNamingItsS3URL(t *testing.T) {
	const tail = ", with an application key, not the master key; restic's own B2 backend retries a failed upload without end"
	for _, tc := range []struct{ repo, url string }{
		{"b2:hs-backups:dev", "s3:https://s3.<region>.backblazeb2.com/hs-backups/dev"},
		{"b2:hs-backups:", "s3:https://s3.<region>.backblazeb2.com/hs-backups"},
		{"b2:hs-backups", "s3:https://s3.<region>.backblazeb2.com/hs-backups"},
		{"b2:", "s3:https://s3.<region>.backblazeb2.com/<bucket>"},
		{"b2::dev", "s3:https://s3.<region>.backblazeb2.com/<bucket>/dev"},
		// The path as restic cleans it: the objects are the same.
		{"b2:hs-backups:/srv//dev/", "s3:https://s3.<region>.backblazeb2.com/hs-backups/srv/dev"},
		{"b2:hs-backups:/", "s3:https://s3.<region>.backblazeb2.com/hs-backups"},
		{"b2:hs-backups:a:b", "s3:https://s3.<region>.backblazeb2.com/hs-backups/a:b"},
		// A path that climbs is named no URL: a server on the way may
		// resolve its dot segments.
		{"b2:hs-backups:../x", "s3:https://s3.<region>.backblazeb2.com/hs-backups/<path>"},
		{"b2:hs-backups:..", "s3:https://s3.<region>.backblazeb2.com/hs-backups/<path>"},
		{"b2:hs-backups:a/../../x", "s3:https://s3.<region>.backblazeb2.com/hs-backups/<path>"},
		{"b2:hs-backups:a/../x", "s3:https://s3.<region>.backblazeb2.com/hs-backups/x"},
		// A bucket B2 could not name is named no URL: escaped, a / in it
		// would be read back as the bucket's end.
		{"b2:a/b:dev", "s3:https://s3.<region>.backblazeb2.com/<bucket>/dev"},
		{"b2:it's:x", "s3:https://s3.<region>.backblazeb2.com/<bucket>/x"},
		{"b2:MyBackups:dev", "s3:https://s3.<region>.backblazeb2.com/<bucket>/dev"},
		// Escaped, so that restic's url.Parse reads back the path B2 holds;
		// what is not printable is escaped too, so never echoed.
		{"b2:hs-backups:dev#daily", "s3:https://s3.<region>.backblazeb2.com/hs-backups/dev%23daily"},
		{"b2:hs-backups:dev?x/y", "s3:https://s3.<region>.backblazeb2.com/hs-backups/dev%3Fx/y"},
		{"b2:hs-backups:dev%2Fdaily", "s3:https://s3.<region>.backblazeb2.com/hs-backups/dev%252Fdaily"},
		{"b2:hs-backups:d\x01v", "s3:https://s3.<region>.backblazeb2.com/hs-backups/d%01v"},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			b, m := setupBox(t)
			b.account, b.comment, b.localComment = false, "", ""
			_, err := b.setup(t, m, tc.repo)
			if want := "b2: is not supported: B2 is reached at its S3-compatible endpoint, " + tc.url + tail; err == nil || err.Error() != want {
				t.Fatalf("err = %v\nwant  %s", err, want)
			}
			nothingWritten(t, b, m)
			if b.roles() != "" || b.accountsMade != 0 {
				t.Fatalf("units ran: %q; accounts made: %d", b.roles(), b.accountsMade)
			}
		})
	}
}

// The S3 URL setup names for a b2: repository reaches the same objects:
// read back as restic 0.18.0 reads an s3:https URL
// (internal/backend/s3/config.go: url.Parse, the decoded path, the
// bucket up to the first slash, the rest cleaned), it gives the bucket
// and the path restic's b2 backend reads from the b2: URL
// (internal/backend/b2/config.go: the bucket up to the first colon, the
// path cleaned, its leading slash dropped).
func TestTheS3URLNamedForAB2RepositoryIsTheSameRepository(t *testing.T) {
	for _, repo := range []string{
		"b2:hs-backups:dev", "b2:hs-backups:dev#daily", "b2:hs-backups:dev?x=1",
		"b2:hs-backups:dev%2Fdaily", "b2:hs-backups:100%", "b2:hs-backups:a b/c;d,e",
		"b2:hs-backups:/srv//dev/", "b2:hs-backups:a:b/c@d", "b2:hs-backups:a+b&c=d",
		"b2:hs-backups:dev$DAILY", "b2:hs-backups:it's",
	} {
		t.Run(repo, func(t *testing.T) {
			wantBucket, wantPath, _ := strings.Cut(strings.TrimPrefix(repo, "b2:"), ":")
			wantPath = strings.TrimPrefix(path.Clean(wantPath), "/")
			u, err := url.Parse(strings.TrimPrefix(b2AsS3(repo), "s3:"))
			if err != nil {
				t.Fatalf("%s: %v", b2AsS3(repo), err)
			}
			if u.RawQuery != "" || u.Fragment != "" {
				t.Fatalf("%s: query %q, fragment %q", b2AsS3(repo), u.RawQuery, u.Fragment)
			}
			// setup takes it: no @, which it refuses anywhere.
			if _, err := backendOf(b2AsS3(repo), "/env"); err != nil {
				t.Fatalf("setup refuses %s: %v", b2AsS3(repo), err)
			}
			bucket, p, _ := strings.Cut(u.Path[1:], "/")
			if p != "" {
				p = path.Clean(p)
			}
			if bucket != wantBucket || p != wantPath {
				t.Fatalf("%s reads as bucket %q path %q; b2: holds %q %q", b2AsS3(repo), bucket, p, wantBucket, wantPath)
			}
		})
	}
}

// The run's remedy is a command to paste: its URL in single quotes, which
// a POSIX shell passes on literally, so nothing in a bucket or a path —
// a $, an &, a ;, the placeholder's < and > — is the shell's, and b2AsS3
// leaves no ' in the URL to end the quotes.
func TestTheRunsRemedyPastesAsOneLiteralArgument(t *testing.T) {
	for _, repo := range []string{
		"b2:hs-backups:dev", "b2:hs-backups:dev$DAILY", "b2:hs-backups:a&b;c", "b2:hs-backups:it's",
		"b2:it's:x", "b2:", "b2:hs-backups:`x`|y>z", "b2:hs-backups:a\\b",
	} {
		t.Run(repo, func(t *testing.T) {
			u := b2AsS3(repo)
			if strings.Contains(u, "'") {
				t.Fatalf("%s holds a quote", u)
			}
			msg := b2RunRefused(repo).Error()
			if want := "as root, motel-backup setup --existing '" + u + "' moves it"; !strings.Contains(msg, want) {
				t.Fatalf("%s\nwant it to hold %s", msg, want)
			}
		})
	}
}

// The record keeps the run's b2: refusal whole for a bucket and path of
// up to 58 characters, the slash between them counted, and the command
// to paste whole, its closing quote included, up to 173 (the README's
// accepted residual; 184 before #389's `--existing`); one more cuts each.
func TestTheRunsB2RefusalFitsTheRecord(t *testing.T) {
	for _, c := range []struct {
		n              int
		whole, command bool
	}{{58, true, true}, {59, false, true}, {173, false, true}, {174, false, false}} {
		repo := "b2:" + strings.Repeat("b", 20) + ":" + strings.Repeat("p", c.n-21)
		msg := b2RunRefused(repo).Error()
		kept := record.Text(msg)
		if got := kept == msg; got != c.whole {
			t.Errorf("%d characters: kept whole = %v, want %v (%d runes)", c.n, got, c.whole, len([]rune(msg)))
		}
		if got := strings.Contains(kept, "setup --existing '"+b2AsS3(repo)+"'"); got != c.command {
			t.Errorf("%d characters: command kept = %v, want %v", c.n, got, c.command)
		}
	}
}

// B2's region is in the name of its S3-compatible endpoint, and is read
// from that host's name, as restic reads the host out of the URL, in
// any ASCII case and past a port or a trailing dot (the region is the
// name's, whatever B2 makes of the rest) — but not from a longer name
// ending in it, a host named only in a path or behind userinfo, or one
// whose letters are not ASCII.
func TestB2sRegionIsReadFromItsEndpointsHostAlone(t *testing.T) {
	longS := string(rune(0x17f))   // ſ, which (?i) would fold into s
	kelvin := string(rune(0x212a)) // K, which strings.ToLower makes k
	for repo, want := range map[string]string{
		"s3:https://s3.u" + longS + "-west-004.backblazeb2.com/b":  "",
		"s3:https://s3.us-west-004.back" + kelvin + "lazeb2.com/b": "",
		"s3:https://s3.eu-central-003.backblazeb2.com/bucket/path": "eu-central-003",
		"s3:https://s3.us-west-004.backblazeb2.com/bucket":         "us-west-004",
		"s3:http://s3.us-east-005.backblazeb2.com/bucket":          "us-east-005",
		"s3:s3.ca-east-006.backblazeb2.com/bucket/path":            "ca-east-006",
		"s3://s3.eu-central-003.backblazeb2.com/bucket":            "eu-central-003",
		"s3:s3.eu-central-003.backblazeb2.com":                     "eu-central-003",

		"s3:https://s3.eu-central-003.backblazeb2.com:443/bucket":          "eu-central-003",
		"s3:https://s3.eu-central-003.backblazeb2.com./bucket":             "eu-central-003",
		"s3:s3.eu-central-003.backblazeb2.com.:443/bucket":                 "eu-central-003",
		"s3:https://s3.eu-central-003.backblazeb2.com:/bucket":             "eu-central-003",
		"s3:https://s3.eu-central-003.backblazeb2.com.evil.example/bucket": "",
		"s3:https://evil.example/s3.eu-central-003.backblazeb2.com/bucket": "",
		"s3:https://x.s3.eu-central-003.backblazeb2.com/bucket":            "",
		"s3:https://bucket.s3.eu-central-003.backblazeb2.com/path":         "",
		"s3:https://S3.EU-CENTRAL-003.BACKBLAZEB2.COM/bucket":              "eu-central-003",
		"s3:https://s3.eu-central-003.backblazeb2.com:443x/bucket":         "",
		"s3:https://s3.eu-central-003.backblazeb2.com:9000/bucket":         "eu-central-003", // the name's region, whatever the port
		"s3:https://s3.eu-central-003.backblazeb2.com..:443/bucket":        "",
		"s3:https://s3.eu-central-003.backblazeb2.com@evil.example/bucket": "",
		"s3:https://u:p@s3.eu-central-003.backblazeb2.com/bucket":          "", // refused by setup before this
		"s3:httpS://s3.eu-central-003.backblazeb2.com/bucket":              "eu-central-003",
		"s3:HTTPS://s3.eu-central-003.backblazeb2.com/bucket":              "", // not s3:http: restic reads "HTTPS:" as the host
		// The host as url.Parse ends it, at the ?, which restic reads too
		// (and then refuses, the bucket being in the query).
		"s3:https://s3.eu-central-003.backblazeb2.com?x/bucket":                          "eu-central-003",
		"s3:s3.eu-central-003.backblazeb2.com/bucket/x://s3.us-west-004.backblazeb2.com": "eu-central-003",
		"s3:s3.eu-central-003.backblazeb2.com@evil.example/bucket":                       "",
		"s3:https://s3.eu-central-3.backblazeb2.com/bucket":                              "",
		"s3:https://s3.eu-central-003.backblazeb2.com%2Fevil.example/bucket":             "",
		"s3:https://s3.amazonaws.com/bucket":                                             "",
		"s3:http://e2e-s3:9000/box":                                                      "",
		"b2:bucket:path":                                                                 "",
		"rest:https://s3.eu-central-003.backblazeb2.com/":                                "",
		"s3:https://s3.eu-central-003.backblazeb2.com\\@evil.example/bucket":             "",
		"s3:https://[s3.eu-central-003.backblazeb2.com]/bucket":                          "",
		"s3:https://s3.eu-central-003.backblazeb2.com\x00.evil.example/bucket":           "",
	} {
		if got := b2Region(repo); got != want {
			t.Errorf("%q: region %q, want %q", repo, got, want)
		}
	}
}

// status as root says what setup would have written of a file written
// by hand for B2's S3-compatible endpoint with no region, and nothing of
// one that has it or of another host.
func TestStatusAdvisesB2sRegionWhereAFileHasNone(t *testing.T) {
	for _, tc := range []struct{ repo, region, want string }{
		{"s3:https://s3.eu-central-003.backblazeb2.com/b", "", "the repository is at B2's S3-compatible endpoint and AWS_DEFAULT_REGION is not set, or set to nothing: add AWS_DEFAULT_REGION=eu-central-003, as setup writes it"},
		{"s3:https://s3.eu-central-003.backblazeb2.com/b", "eu-central-003", ""},
		{"s3:https://s3.eu-central-003.backblazeb2.com/b", "us-east-1", "the repository is at B2's S3-compatible endpoint in eu-central-003, and AWS_DEFAULT_REGION is us-east-1: set AWS_DEFAULT_REGION=eu-central-003, as setup writes it"},
		{"s3:https://s3.example.com/b", "", ""},
		{"b2:b:p", "", ""},
		// A URL setup refuses: status has said so, and nothing more.
		{"s3:http://s3.eu-central-003.backblazeb2.com/b", "", ""},
	} {
		if got := RegionAdvice(tc.repo, tc.region); got != tc.want {
			t.Errorf("%s, %q: %q, want %q", tc.repo, tc.region, got, tc.want)
		}
	}
}

// A repository at B2's S3-compatible endpoint gets its region in the
// file, beside the key setup asks for, and so do setup's own units,
// which read the file being written: the look, init and the opening
// with the lock alike. Any other S3 host gets none.
func TestSetupWritesB2sRegionForEveryUnitToRead(t *testing.T) {
	for _, tc := range []struct{ repo, region string }{
		{b2Repo, "eu-central-003"},
		{"s3:http://e2e-s3:9000/box", ""},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			b, m := setupBox(t)
			read := regionsRead(t, b)
			if _, err := b.setup(t, m, tc.repo); err != nil {
				t.Fatal(err)
			}
			if got, want := strings.Join(*read, " "), "probe="+tc.region+" init="+tc.region+" open="+tc.region; got != want {
				t.Fatalf("the units read %s, want %s", got, want)
			}
			v := envOf(t, b.cfg.EnvFile)
			if got, ok := v["AWS_DEFAULT_REGION"]; got != tc.region || ok != (tc.region != "") {
				t.Fatalf("the file holds %v", v)
			}
			said := strings.Contains(m.saidAll(), "B2's region, eu-central-003, from the endpoint's name, goes in the file as AWS_DEFAULT_REGION")
			if said != (tc.region != "") || m.askedAll() != "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nType stored to go on: " {
				t.Fatalf("asked:\n%s\nsaid:\n%s", m.askedAll(), m.saidAll())
			}
			// What to keep off the box names the region on B2, and is
			// as it was elsewhere.
			keep := "Store it off the box now, with the repository URL, " + tc.repo + ", and the storage key: with those three, "
			if tc.region != "" {
				keep = "Store it off the box now, with the repository URL, " + tc.repo + ", the storage key, and B2's region, " + tc.region + ", set as AWS_DEFAULT_REGION wherever restic runs: with those four, "
			}
			if !strings.Contains(m.saidAll(), keep) {
				t.Fatalf("said:\n%s\nwant it to hold %s", m.saidAll(), keep)
			}
		})
	}
	// The opening of a repository that was there reads it too.
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	read := regionsRead(t, b)
	if _, err := b.setup(t, m, b2Repo); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(*read, " "), "probe=eu-central-003 open=eu-central-003"; got != want {
		t.Fatalf("the units read %s, want %s", got, want)
	}
}

// regionsRead records, for every unit that reads a credential file, its
// role and the AWS_DEFAULT_REGION the manager would give it, as the
// unit starts.
func regionsRead(t *testing.T, b *box) *[]string {
	t.Helper()
	read := &[]string{}
	b.before = func(s unit.Spec) {
		if s.EnvironmentFile == "" {
			return
		}
		raw, err := os.ReadFile(s.EnvironmentFile)
		must(t, err)
		v, _ := envfile.Parse(raw)
		*read = append(*read, roleRe.FindStringSubmatch(s.Name)[1]+"="+v["AWS_DEFAULT_REGION"])
	}
	return read
}

// What setup does not ask for, the run still uses: status must not
// warn of it. What setup refuses of the value itself — whitespace at an
// end, a control character — status warns of too, and b2:, which the
// run refuses as well.
func TestABackendWrittenByHandIsUsable(t *testing.T) {
	for _, repo := range []string{"gs:bucket:path", "swift:container:/path", "s3:http://h/b", "rest:https://h/", b2Repo} {
		if err := RepositoryUsable(repo, "/etc/x"); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
	}
	for _, repo := range []string{"sftp:h:/p", "local:/x", "/x", "azure:c:p", "rclone:r:b", "s3:http://u:p@h/b", "s3:u:p@h/b", "s3:http://h/b ", " s3:http://h/b", "s3:http://h/b\x01", "", "b2:b:p", "b2:"} {
		if err := RepositoryUsable(repo, "/etc/x"); err == nil {
			t.Errorf("%q: usable", repo)
		}
	}
	if err := RepositoryUsable("b2:hs-backups:dev", "/etc/x"); err == nil || err.Error() != "b2: is not supported: B2 is reached at its S3-compatible endpoint, s3:https://s3.<region>.backblazeb2.com/hs-backups/dev, with an application key, not the master key; restic's own B2 backend retries a failed upload without end" {
		t.Errorf("status's word on b2: %v", err)
	}
}

// restic init that exited 0 has made the repository with the password
// that was shown: whatever else went wrong after, that password is the
// repository's, and is never said to be dead.
func TestAPasswordInitUsedIsNeverSaidDead(t *testing.T) {
	b, m := setupBox(t)
	b.initOut = "not json at all\n"
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || strings.Contains(err.Error(), "discard") || !strings.Contains(err.Error(), "restic made the repository and exited 0 but said nothing of its id; run setup again, which opens it; the password shown above is the repository's: keep it") {
		t.Fatalf("err = %v", err)
	}
	m.asked = nil
	nothingWritten(t, b, m)
}

func TestEachBackendIsAskedForItsOwnVariables(t *testing.T) {
	for _, tc := range []struct {
		repo    string
		prompts string
		keys    []string
	}{
		// B2 through its S3-compatible endpoint: the S3 pair, and the
		// region, which is not asked.
		{b2Repo, "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nType stored to go on: ", []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION"}},
		{"s3:http://e2e-s3:9000/box", "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nType stored to go on: ", []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			b, m := setupBox(t)
			if _, err := b.setup(t, m, tc.repo); err != nil {
				t.Fatal(err)
			}
			if m.askedAll() != tc.prompts {
				t.Fatalf("asked:\n%s", m.askedAll())
			}
			v := envOf(t, b.cfg.EnvFile)
			if v[tc.keys[0]] != "AKIDX" || v[tc.keys[1]] != "the-secret" || len(v) != 2+len(tc.keys) {
				t.Fatalf("the file holds %v", v)
			}
		})
	}
}

func TestThePasswordNotConfirmedStoredWritesNothing(t *testing.T) {
	// A word that is not "stored" gets one more asking; " Stored " is
	// stored — the point is that it was, not the typing.
	for _, ok := range []string{" Stored ", "STORED"} {
		b, m := setupBox(t)
		m.answers = []string{"AKIDX", "the-secret", ok}
		if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
	}
	b, m := setupBox(t)
	m.answers = []string{"AKIDX", "the-secret", "y", "stored"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil || strings.Count(m.askedAll(), "stored") != 2 || !strings.Contains(m.saidAll(), "that is not stored: type stored to go on") {
		t.Fatalf("err = %v\nasked:\n%s\nsaid:\n%s", err, m.askedAll(), m.saidAll())
	}
	for _, answer := range []string{"y", ""} {
		t.Run(answer, func(t *testing.T) {
			b, m := setupBox(t)
			m.answers = []string{"AKIDX", "the-secret", answer, answer}
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), "not confirmed stored: nothing was written") {
				t.Fatalf("err = %v", err)
			}
			shownPassword(t, m)
			m.asked = nil
			nothingWritten(t, b, m)
			if b.roles() != "plan probe" {
				t.Fatalf("a repository unit ran: %s", b.roles())
			}
		})
	}
	t.Run("nobody answers", func(t *testing.T) {
		b, m := setupBox(t)
		m.answers = []string{"AKIDX"}
		_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
		if err == nil || !strings.Contains(err.Error(), "nobody answered") {
			t.Fatalf("err = %v", err)
		}
		m.asked = nil
		nothingWritten(t, b, m)
	})
}

func TestAValueTheFileCannotHoldIsAskedAgain(t *testing.T) {
	b, m := setupBox(t)
	m.answers = []string{"AKIDX", "a\x01b", "a\rb", "good", "stored"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if v := envOf(t, b.cfg.EnvFile); v["AWS_SECRET_ACCESS_KEY"] != "good" {
		t.Fatalf("the file holds %q", v["AWS_SECRET_ACCESS_KEY"])
	}
	if strings.Count(m.saidAll(), "it holds a control character; again") != 2 {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	// A value the plain form would not carry is written all the same,
	// quoted: a password made elsewhere is what it is.
	b, m = setupBox(t)
	m.answers = []string{" AKIDX ", `"the-secret`, "stored"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if v := envOf(t, b.cfg.EnvFile); v["AWS_ACCESS_KEY_ID"] != " AKIDX " || v["AWS_SECRET_ACCESS_KEY"] != `"the-secret` {
		t.Fatalf("the file holds %q", v)
	}
	b, m = setupBox(t)
	m.answers = []string{"", "", "", "x", "stored"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), "three times") {
		t.Fatalf("err = %v", err)
	}
	m.asked = nil
	nothingWritten(t, b, m)
}

// A repository that exists is found by the probe, before any password
// is made: the operator is asked for its own, and nothing else.
func TestAnExistingRepositoryIsOpenedWithItsOwnPassword(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nsecret:Repository password: "; got != want {
		t.Fatalf("asked:\n%s", got)
	}
	if !strings.Contains(m.saidAll(), "the repository exists; its password is needed") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	if len(passwordRe.FindAllString(m.saidAll(), -1)) != 0 || strings.Contains(m.saidAll(), "stored") {
		t.Fatalf("a password was made for a repository that exists:\n%s", m.saidAll())
	}
	v := envOf(t, b.cfg.EnvFile)
	if v["RESTIC_PASSWORD"] != "its-own-password" {
		t.Fatalf("the file holds password %q", v["RESTIC_PASSWORD"])
	}
	// The opening takes the repository's lock — cat config without
	// --no-lock — which proves the key may write a lock file to it, and
	// remove it; the look alone reads without one.
	s := b.spec("open")
	if got, want := strings.Join(s.Argv, " "), "/usr/bin/restic cat config"; got != want {
		t.Fatalf("the open unit's argv: %q", got)
	}
	if s.User != backupUser || !s.Network || len(s.Capabilities) != 0 || !strings.HasPrefix(s.EnvironmentFile, b.cfg.EnvFile+".") {
		t.Fatalf("the open unit: %+v", s)
	}
	if got, want := strings.Join(b.spec("probe").Argv, " "), "/usr/bin/restic cat config --no-lock"; got != want {
		t.Fatalf("the look's argv: %q", got)
	}
	if rep.New || rep.RepositoryID != repoID || !strings.Contains(m.saidAll(), "repository ready: s3:http://e2e-s3:9000/box (existing, id bbd0e899)") {
		t.Fatalf("report %+v\nsaid:\n%s", rep, m.saidAll())
	}
}

// A look that failed for a reason of the unit's own — a manager that
// could not set it up, a unit that could not be started — is said as
// such, before any password is made.
func TestALookThatFailedOnItsOwnIsSaidBeforeAnyPassword(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(b *box)
		want string
	}{
		{"the manager could not set the unit up", func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 217} }, "looking for the repository: systemd could not set the unit up (status 217)"},
		{"ended by a signal", func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "signal"} }, "looking for the repository: restic was ended by signal"},
		{"could not be started", func(b *box) { b.err["probe"] = errors.New("starting motel_backup_probe: no such user") }, "no such user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			tc.set(b)
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(passwordRe.FindAllString(m.saidAll(), -1)) != 0 || strings.Contains(b.roles(), "init") {
				t.Fatalf("a password was made, or init ran (%s):\n%s", b.roles(), m.saidAll())
			}
			m.asked = nil
			nothingWritten(t, b, m)
		})
	}
}

// A program is one every account can run: the units run restic as
// motel-backup and sqlite3 as the app's own uid, so an execute bit
// for root alone is not installed either.
func TestAProgramIsOneEveryAccountCanRun(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		mode os.FileMode
		want bool
	}{{"for-all", 0o755, true}, {"root-only", 0o700, false}, {"not-executable", 0o644, false}} {
		p := filepath.Join(dir, tc.name)
		must(t, os.WriteFile(p, []byte("#!/bin/sh\n"), tc.mode))
		must(t, os.Chmod(p, tc.mode))
		if got := haveProgram(p); got != tc.want {
			t.Errorf("%s (%o): %v, want %v", tc.name, tc.mode, got, tc.want)
		}
	}
	if haveProgram(dir) {
		t.Error("a directory passes for a program")
	}
}

// A restic that is there but not executable is refused as not
// installed, before anything is asked.
func TestAResticThatCannotRunIsNotInstalled(t *testing.T) {
	b, m := setupBox(t)
	b.haveProgram = func(p string) bool { return p != b.cfg.Restic }
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), "restic, the backup program motel-backup runs, is not installed") {
		t.Fatalf("err = %v", err)
	}
	if len(m.asked) != 0 {
		t.Fatalf("asked %q", m.asked)
	}
}

// The init unit is the one not recorded for the next lock holder to
// stop: stopped half way it leaves a repository no password opens. It
// is recorded for the next lock holder to wait for instead, so that
// nothing probes or makes the same repository while it still writes.
func TestTheInitUnitIsLeftToFinishAndWaitedFor(t *testing.T) {
	b, m := setupBox(t)
	b.cfg.BindsTo = "hs-setup.service"
	var initUnitDuring string
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			raw, _ := os.ReadFile(filepath.Join(b.cfg.RunDir, "init-unit"))
			initUnitDuring = strings.TrimSpace(string(raw))
		}
	}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(b.cfg.RunDir, "units"))
	must(t, err)
	recorded := string(raw)
	if !strings.Contains(recorded, "_plan_") || !strings.Contains(recorded, "_probe_") {
		t.Fatalf("the plan and the look are not recorded: %q", recorded)
	}
	if strings.Contains(recorded, "_init_") {
		t.Fatalf("the init unit is recorded to be stopped, and the next run would stop it half way: %q", recorded)
	}
	if !strings.Contains(initUnitDuring, "_init_") {
		t.Fatalf("while init ran, nothing recorded it for the next lock holder to wait for: %q", initUnitDuring)
	}
	if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "init-unit")); err == nil {
		t.Fatal("the init unit is still recorded after it ended")
	}
	// (Nor bound to setup's own service: TestEveryUnitOfACommandIsBoundToItsService.)
	// The next lock holder — a setup here, a run the same — waits for
	// the unit a killed setup left, before anything of its own.
	b2, m2 := setupBox(t)
	must(t, os.MkdirAll(b2.cfg.RunDir, 0o700))
	must(t, os.WriteFile(filepath.Join(b2.cfg.RunDir, "init-unit"), []byte("motel_backup_init_0123456789ab.service\n"), 0o600))
	if _, err := b2.setup(t, m2, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if len(b2.waited) != 1 || b2.waited[0] != "motel_backup_init_0123456789ab.service" {
		t.Fatalf("waited for %v", b2.waited)
	}
	if !strings.Contains(m2.saidAll(), "waiting for the restic init a setup that did not finish left running: motel_backup_init_0123456789ab.service") {
		t.Fatalf("said:\n%s", m2.saidAll())
	}
	if _, err := os.Lstat(filepath.Join(b2.cfg.RunDir, "init-unit")); err == nil {
		t.Fatal("the waited-for unit is still recorded")
	}
	// A run waits too, and a unit that cannot be seen to its end stops
	// the lock holder with the runner's words.
	b3, _ := setupBox(t)
	must(t, os.MkdirAll(b3.cfg.RunDir, 0o700))
	must(t, os.WriteFile(filepath.Join(b3.cfg.RunDir, "init-unit"), []byte("motel_backup_init_0123456789ab.service\n"), 0o600))
	must(t, os.MkdirAll(filepath.Dir(b3.cfg.EnvFile), 0o755))
	must(t, os.WriteFile(b3.cfg.EnvFile, []byte("RESTIC_PASSWORD=x\n"), 0o600))
	b3.waitErr = errors.New("still running after 3m")
	if _, err := Run(context.Background(), b3.cfg, b3); err == nil || !strings.Contains(err.Error(), "still running after 3m") || len(b3.waited) != 1 || b3.roles() != "" {
		t.Fatalf("err %v, waited %v, units %s", err, b3.waited, b3.roles())
	}
}

// An init the runner lost sight of — the request may have reached the
// manager all the same, or its state could not be read — stays
// recorded: the next lock holder waits for it, and a unit that is not
// there has ended.
func TestAnInitTheRunnerLostSightOfStaysRecorded(t *testing.T) {
	b, m := setupBox(t)
	b.err["init"] = errors.New("motel_backup_init: its state could not be read 10 times running")
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "could not be read 10 times") || !strings.Contains(err.Error(), "keep the password shown above") {
		t.Fatalf("err = %v", err)
	}
	// One the runner tried to stop and could not confirm gone says the
	// fate the same: init ran with the password, whatever the runner
	// could see of it.
	b, m = setupBox(t)
	b.err["init"] = fmt.Errorf("%w: motel_backup_init: its state could not be read (stopping it: timeout)", unit.ErrNotConfirmedGone)
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !errors.Is(err, unit.ErrNotConfirmedGone) || !strings.Contains(err.Error(), "keep the password shown above") {
		t.Fatalf("err = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(b.cfg.RunDir, "init-unit"))
	if err != nil || !strings.Contains(string(raw), "_init_") {
		t.Fatalf("the init unit is not recorded for the next lock holder: %q, %v", raw, err)
	}
}

// Ctrl-C while waiting for an earlier setup's init is an interrupt like
// any other, at once — and leaves the marker for the next lock holder,
// since the init it names is still to be waited for.
func TestAnInterruptDuringTheWaitForAnEarlierInitIsHonoured(t *testing.T) {
	b, m := setupBox(t)
	must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
	marker := filepath.Join(b.cfg.RunDir, "init-unit")
	must(t, os.WriteFile(marker, []byte("motel_backup_init_0123456789ab.service\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || len(b.waited) != 1 || b.roles() != "" {
		t.Fatalf("err %v, waited %v, units %s", err, b.waited, b.roles())
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Fatal("the marker was removed though the wait was interrupted")
	}
	// A run, the same.
	must(t, os.MkdirAll(filepath.Dir(b.cfg.EnvFile), 0o755))
	must(t, os.WriteFile(b.cfg.EnvFile, []byte("RESTIC_PASSWORD=x\n"), 0o600))
	if _, err := Run(ctx, b.cfg, b); !errors.Is(err, context.Canceled) || b.roles() != "" {
		t.Fatalf("a run: %v, units %s", err, b.roles())
	}
}

// After an init attempt that failed past the opening, the next
// attempt's look saying "exists" does not prove the shown password is
// not that repository's: the failed init may have made it. The shown
// password is tried first, and a repository it opens is one this setup
// made. (An init refused at the opening wrote nothing, and is not
// tried: TestAnInitRefusedAtTheOpeningIsNotTriedAsTheMaker.)
func TestAfterAFailedInitTheShownPasswordIsTriedBeforeItIsCalledUnused(t *testing.T) {
	b, m := setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", keyWriteFailed
	inits := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			if inits++; inits == 1 {
				// The second attempt's look finds a repository.
				b.hang = ""
				b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			}
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret"}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init probe open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if strings.Contains(m.askedAll(), "Repository password: ") {
		t.Fatalf("the operator was asked for a password the setup had shown:\n%s", m.askedAll())
	}
	pw := shownPassword(t, m)
	if v := envOf(t, b.cfg.EnvFile); v["RESTIC_PASSWORD"] != pw {
		t.Fatalf("the file holds %q, not the shown password", v["RESTIC_PASSWORD"])
	}
	if !rep.New || rep.RepositoryID != repoID || strings.Contains(m.saidAll(), "discard it") || !strings.Contains(m.saidAll(), "the repository exists, and the password shown above opens it: it was made by an earlier attempt of this setup") {
		t.Fatalf("report %+v\nsaid:\n%s", rep, m.saidAll())
	}
	// The shown password does not open it: it is not ours, and the
	// operator is asked, as before.
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", keyWriteFailed
	opens := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			b.hang = ""
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
		}
		if strings.Contains(s.Name, "_open_") {
			if opens++; opens == 2 {
				delete(b.outcome, "open")
			}
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret", "its-own-password"}
	rep, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init probe open open"; got != want || rep.New || !strings.Contains(m.saidAll(), "the password shown above was never used: discard it") {
		t.Fatalf("units %s, report %+v\nsaid:\n%s", got, rep, m.saidAll())
	}
	// Where the second attempt's look cannot tell either, and its init
	// says the repository exists: an earlier init ran with the shown
	// password, so it is tried before it is called unused, the same.
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", keyWriteFailed
	inits = 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			if inits++; inits == 2 {
				b.initErr = alreadyInit
			}
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret"}
	rep, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init probe init open"; got != want || !rep.New || rep.RepositoryID != repoID || strings.Contains(m.saidAll(), "discard it") || strings.Contains(m.askedAll(), "Repository password: ") {
		t.Fatalf("units %s, report %+v\nsaid:\n%s", got, rep, m.saidAll())
	}
	// A first init that says the repository exists ran with no earlier
	// one: nothing to try, the operator is asked, as before.
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", alreadyInit
	m.answers = []string{"AKIDX", "the-secret", "stored", "its-own-password"}
	rep, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init open"; got != want || rep.New || !strings.Contains(m.saidAll(), "the password shown above was never used: discard it") {
		t.Fatalf("units %s, report %+v\nsaid:\n%s", got, rep, m.saidAll())
	}
}

// Ctrl-C while the plan is read is said as an interrupt, like every
// other: nothing has been written.
func TestAnInterruptDuringThePlanIsSaidAsOne(t *testing.T) {
	b, m := setupBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.hang = "plan"
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_plan_") {
			cancel()
		}
	}
	_, err := Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || err.Error() != "interrupted: nothing has been written" {
		t.Fatalf("err = %v", err)
	}
}

// The file from before this version, still there after a setup, is
// where the credential was reachable from: said, with what to do.
func TestTheOldFileStillThereIsSaid(t *testing.T) {
	b, m := setupBox(t)
	must(t, os.MkdirAll(filepath.Dir(b.cfg.OldEnvFile), 0o755))
	must(t, os.WriteFile(b.cfg.OldEnvFile, []byte("RESTIC_PASSWORD=old\n"), 0o600))
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OldEnvFile != b.cfg.OldEnvFile || !strings.Contains(m.saidAll(), b.cfg.OldEnvFile+" is still there, from before this version, and is not read; it holds a copy of the credential: remove it (rm "+b.cfg.OldEnvFile+")") {
		t.Fatalf("report %+v\nsaid:\n%s", rep, m.saidAll())
	}
	if _, err := os.Lstat(b.cfg.OldEnvFile); err != nil {
		t.Fatal("the old file was removed")
	}
	b, m = setupBox(t)
	rep, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil || rep.OldEnvFile != "" || strings.Contains(m.saidAll(), "still there") {
		t.Fatalf("with no old file: %v %+v", err, rep)
	}
}

// Where the probe could not tell — the storage retrying on a wrong key,
// a bucket not there yet — init is what answers; and init may answer
// that the repository exists after all, in either of its wordings, when
// a password has been made and shown: it is said not to be the one.
func TestWhereTheProbeCannotTellInitAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(b *box)
	}{
		{"the probe is given up on at its clock", func(b *box) { b.hang = "probe" }},
		{"the probe exits 0 with nothing", func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "success"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, wording := range []string{alreadyInit, alreadyThere} {
				b, m := setupBox(t)
				tc.set(b)
				b.probeOut = ""
				b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
				// The phrase comes after the URL, which may be long: a message
				// cut for showing must not be the one matched.
				b.initOut, b.initErr = "", strings.ReplaceAll(wording, "s3:http://e2e-s3:9000/box", "s3:https://"+strings.Repeat("a-very-long-host-name.", 12)+"example/bucket")
				b.probeOut = configJSON
				m.answers = []string{"AKIDX", "the-secret", "stored", "its-own-password"}
				rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
				if err != nil {
					t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
				}
				if got, want := b.roles(), "plan probe init open"; got != want {
					t.Fatalf("units: %s, want %s", got, want)
				}
				if !strings.Contains(m.saidAll(), "the repository exists; its password is needed (the one shown above is not it)") {
					t.Fatalf("said:\n%s", m.saidAll())
				}
				// Shown on a look that could not tell, the password is
				// to be stored beside the repository's own, not over it
				// [RC check 7: a rebuilt box, B2 slow to answer].
				if !strings.Contains(m.saidAll(), unsureStore) {
					t.Fatalf("said lacks %q:\n%s", unsureStore, m.saidAll())
				}
				if !strings.Contains(m.saidAll(), "the password shown above was never used: discard it") {
					t.Fatalf("the unused password was not said dead on the way out:\n%s", m.saidAll())
				}
				pw := shownPassword(t, m)
				raw, _ := os.ReadFile(b.cfg.EnvFile)
				if strings.Contains(string(raw), pw) || len(passwordRe.FindAllString(m.saidAll(), -1)) != 1 {
					t.Fatalf("the generated password: in the file %v; shown %d times", strings.Contains(string(raw), pw), len(passwordRe.FindAllString(m.saidAll(), -1)))
				}
				if rep.New || rep.RepositoryID != repoID {
					t.Fatalf("report %+v", rep)
				}
			}
		})
	}
	// And a failure after init has said the repository exists is not
	// told to keep that password: init did not make anything with it.
	b, m := setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", alreadyInit
	b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "stored", "a", "b", "c"}
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.HasSuffix(err.Error(), "the password shown above was never used: discard it") || strings.Contains(err.Error(), "keep the password") {
		t.Fatalf("err = %v", err)
	}
	// A probe that never answers is given up on, and init goes on.
	b, m = setupBox(t)
	b.hang = "probe"
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil || !rep.New {
		t.Fatalf("%v %+v", err, rep)
	}
	if got, want := b.roles(), "plan probe init open"; got != want || len(b.stopped) != 1 || !strings.Contains(b.stopped[0], "_probe_") {
		t.Fatalf("units: %s, stopped %v", got, b.stopped)
	}
}

// A storage that refuses the look at once — a 403, which restic's S3
// backend takes as final — is said in restic's words, and the key is
// asked for again before any password is made: init's first step is
// the same opening, and would be refused the same.
func TestAKeyTheStorageRefusesAtTheLookIsAskedForAgainBeforeAnyPassword(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.probeErr = refusedAtOnce
	looks := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_probe_") {
			if looks++; looks == 2 {
				// The right key this time: no repository there yet.
				b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
				b.probeErr = ""
			}
		}
	}
	m.answers = []string{"K001", "wrong", "K001", "the-secret", "stored"}
	rep, err := b.setup(t, m, b2Repo)
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe probe init open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nStorage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nType stored to go on: "; got != want {
		t.Fatalf("asked:\n%s", got)
	}
	said := m.saidAll()
	for _, want := range []string{
		refusedSaid,
		"the storage refused the key, or could not be reached: the key id and secret again",
	} {
		if !strings.Contains(said, want) {
			t.Fatalf("said lacks %q:\n%s", want, said)
		}
	}
	for _, wrong := range []string{"answered within", "still applies", "a new repository will be made"} {
		if strings.Contains(said, wrong) {
			t.Fatalf("said %q of a look restic refused at once:\n%s", wrong, said)
		}
	}
	// The password came after the refusal, once.
	if i, j := strings.Index(said, "Access Denied"), strings.Index(said, "Repository password (new)"); i < 0 || j < i || len(passwordRe.FindAllString(said, -1)) != 1 {
		t.Fatalf("the password's place:\n%s", said)
	}
	pw := shownPassword(t, m)
	if v := envOf(t, b.cfg.EnvFile); v["RESTIC_PASSWORD"] != pw || v["AWS_SECRET_ACCESS_KEY"] != "the-secret" || !rep.New {
		t.Fatalf("the file holds %v; report %+v", v, rep)
	}
	// Three refusals at the look: the end, in restic's words, with no
	// password ever shown and so nothing said of one.
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.probeErr = refusedAtOnce
	m.answers = []string{"K001", "wrong", "K001", "wrong", "K001", "wrong"}
	_, err = b.setup(t, m, b2Repo)
	if err == nil || err.Error() != refusedSaid {
		t.Fatalf("err = %v", err)
	}
	if got, want := b.roles(), "plan probe probe probe"; got != want || len(passwordRe.FindAllString(m.saidAll(), -1)) != 0 || strings.Count(m.saidAll(), "the key id and secret again") != 2 {
		t.Fatalf("units %s; said:\n%s", got, m.saidAll())
	}
	m.asked = nil
	nothingWritten(t, b, m)
	// A look refused after a password was shown — the first attempt's
	// look ran to its clock, and init was refused — says the password
	// still applies, and the end says it was never used.
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", wrongKey
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			b.hang = ""
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.probeErr = refusedAtOnce
		}
	}
	m.answers = []string{"K001", "wrong", "stored", "K001", "wrong", "K001", "wrong"}
	_, err = b.setup(t, m, b2Repo)
	if err == nil || !strings.HasSuffix(err.Error(), "the password shown above was never used: discard it") || !strings.HasPrefix(err.Error(), refusedSaid) {
		t.Fatalf("err = %v", err)
	}
	if got, want := b.roles(), "plan probe init probe probe"; got != want || !strings.Contains(m.saidAll(), "the key id and secret again (the password shown above still applies)") {
		t.Fatalf("units %s; said:\n%s", got, m.saidAll())
	}
	m.asked = nil
	nothingWritten(t, b, m)
}

// An init refused at the opening wrote nothing: a repository the next
// attempt's look finds is not tried with the password shown, which is
// said not to be it, and dead on the way out.
func TestAnInitRefusedAtTheOpeningIsNotTriedAsTheMaker(t *testing.T) {
	b, m := setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", wrongKey
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			b.hang = ""
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret", "its-own-password"}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init probe open"; got != want || rep.New || rep.RepositoryID != repoID {
		t.Fatalf("units %s, report %+v\nsaid:\n%s", got, rep, m.saidAll())
	}
	for _, want := range []string{"the repository exists; its password is needed (the one shown above is not it)", "the password shown above was never used: discard it"} {
		if !strings.Contains(m.saidAll(), want) {
			t.Fatalf("said lacks %q:\n%s", want, m.saidAll())
		}
	}
}

// Files of a repository and no config — an init that ended between its
// two writes, a rule that removed the config — are what init refuses to
// write over: setup ends at once, says what is there and the way out,
// and the password shown was never used.
func TestFilesOfARepositoryWithNoConfigEndSetupAtOnce(t *testing.T) {
	for _, phrase := range []string{"repository already contains keys", "repository already contains snapshots"} {
		b, m := setupBox(t)
		b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
		b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
		b.initOut, b.initErr = "", strings.ReplaceAll(halfThere, "repository already contains keys", phrase)
		m.answers = []string{"AKIDX", "the-secret", "stored"}
		_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
		if err == nil || !strings.Contains(err.Error(), phrase) || !strings.Contains(err.Error(), "the storage holds files of a restic repository and no config") || !strings.Contains(err.Error(), "recover its earlier state from the storage's kept versions first") || !strings.Contains(err.Error(), "only an unfinished repository that never held one is removed at the storage") || !strings.HasSuffix(err.Error(), "the password shown above was never used: discard it") {
			t.Fatalf("err = %v", err)
		}
		if got, want := b.roles(), "plan probe init"; got != want || strings.Contains(m.saidAll(), "the key id and secret again") {
			t.Fatalf("units %s; said:\n%s", got, m.saidAll())
		}
		m.asked = nil
		nothingWritten(t, b, m)
	}
}

// A bucket not there, with a key that may not make it, is said as that:
// setup ends at once, naming the bucket and not the key — asked for
// again, the key is refused the same — and the password shown was never
// used. On B2's S3 endpoint the look says none at once (exit 10), so a
// password has been shown [RC check 1].
func TestABucketNotThereEndsSetupAtOnceNamingIt(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", bucketNotThere
	m.answers = []string{"AKIDX", "the-secret", "stored"}
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "client.MakeBucket: not entitled; the storage has no bucket by that name for this key, and the key may not make one: check the bucket's name in the URL, or make the bucket first (the guide, \"Before you start\"); then run setup again") || !strings.HasSuffix(err.Error(), "the password shown above was never used: discard it") {
		t.Fatalf("err = %v", err)
	}
	if got, want := b.roles(), "plan probe init"; got != want || strings.Contains(m.saidAll(), "the key id and secret again") {
		t.Fatalf("units %s; said:\n%s", got, m.saidAll())
	}
	m.asked = nil
	nothingWritten(t, b, m)
	// Read after the URL's echo, at the opening: restic's words inside
	// the URL are not its answer, and another reason there is the key's.
	for _, tc := range []struct {
		message, repo string
		want          bool
	}{
		{"Fatal: create repository at s3:x failed: Fatal: unable to open repository at s3:x: client.MakeBucket: not entitled", "s3:x", true},
		{"Fatal: create repository at s3:x failed: Fatal: unable to open repository at s3:x: client.BucketExists: Access Denied.", "s3:x", false},
		{"Fatal: create repository at s3:http://h/client.MakeBucket: x failed: Fatal: unable to open repository at s3:http://h/client.MakeBucket: x: client.BucketExists: signature", "s3:http://h/client.MakeBucket: x", false},
		{"Fatal: create key in repository at s3:x failed: Fatal: unable to open repository at s3:x: client.MakeBucket: not entitled", "s3:x", false},
	} {
		if got := noBucket(tc.message, tc.repo); got != tc.want {
			t.Errorf("%q: no bucket %v, want %v", tc.message, got, tc.want)
		}
	}
}

// A storage that refuses the key, or cannot be reached, is a mistake
// in what was typed: the key is asked for again, up to three times,
// and the one password shown stands — shown once, stored once, and
// the password of the repository that is made in the end. Here the
// look runs to its clock, as S3 has it retry, and init is what refuses.
func TestAKeyTheStorageRefusesIsAskedForAgain(t *testing.T) {
	b, m := setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", wrongKey
	tries := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			if tries++; tries == 2 {
				// The right key this time.
				b.hang = ""
				b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
				delete(b.outcome, "init")
				b.initOut = initialized
			}
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret"}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init probe init open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nType stored to go on: \nStorage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): "; got != want {
		t.Fatalf("asked:\n%s", got)
	}
	pw := shownPassword(t, m)
	if len(passwordRe.FindAllString(m.saidAll(), -1)) != 1 {
		t.Fatalf("the password was shown more than once:\n%s", m.saidAll())
	}
	for _, want := range []string{
		"no repository answered within 100ms: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made with the password shown next",
		"restic could not make or open the repository (exit 1): Fatal: create repository at",
		"the storage refused the key, or could not be reached: the key id and secret again (the password shown above still applies)",
	} {
		if !strings.Contains(m.saidAll(), want) {
			t.Fatalf("said lacks %q:\n%s", want, m.saidAll())
		}
	}
	// And promises nothing of the password's fate on the way: that is
	// said once, at the end, from what setup knows by then.
	if strings.Contains(m.saidAll(), "was never used") {
		t.Fatalf("the look promised a fate the ending contradicts:\n%s", m.saidAll())
	}
	if v := envOf(t, b.cfg.EnvFile); v["RESTIC_PASSWORD"] != pw || v["AWS_SECRET_ACCESS_KEY"] != "the-secret" || !rep.New {
		t.Fatalf("the file holds %v; report %+v", v, rep)
	}
	// Three refusals at the opening: the end, and the password shown is
	// said to be dead — init never opened the storage, so it wrote
	// nothing with it (restic's "unable to open repository at").
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", wrongKey
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.HasSuffix(err.Error(), "the password shown above was never used: discard it") || !strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "keep the password") {
		t.Fatalf("err = %v", err)
	}
	if got, want := b.roles(), "plan probe init probe init probe init"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	m.asked = nil
	nothingWritten(t, b, m)
	// Three failures past the opening — restic's "create key in
	// repository at … failed" — may each have written: the password is
	// kept.
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", keyWriteFailed
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.HasSuffix(err.Error(), "keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there") {
		t.Fatalf("err = %v", err)
	}
	m.asked = nil
	nothingWritten(t, b, m)
	// An interrupt after the password was shown says so too.
	b, m = setupBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.hang = "init"
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			cancel()
		}
	}
	_, err = Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || err.Error() != "interrupted: nothing has been written; keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there" {
		t.Fatalf("err = %v", err)
	}
	// Before init has started — at the stored prompt, say — the
	// password has taken effect nowhere and is said to be dead.
	b, m = setupBox(t)
	ctx, cancel = context.WithCancel(context.Background())
	m.answers = []string{"AKIDX", "the-secret"}
	m.at = func(prompt string) {
		if strings.Contains(prompt, "stored") {
			cancel()
		}
	}
	_, err = Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || err.Error() != "interrupted: nothing has been written; the password shown above was never used: discard it" {
		t.Fatalf("err = %v", err)
	}
	// A look that could not be seen gone is not passed over: the
	// runner's own words are the error, and init never runs.
	b, m = setupBox(t)
	b.hang, b.stopErr = "probe", unit.ErrNotConfirmedGone
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if !errors.Is(err, unit.ErrNotConfirmedGone) || strings.Contains(b.roles(), "init") {
		t.Fatalf("err = %v after %s", err, b.roles())
	}
}

// Only restic's two wordings for a repository that exists count as
// one: a storage saying "already exists" of something else is a
// failure of init, and the password stays with it.
func TestOnlyResticsOwnWordsSayTheRepositoryExists(t *testing.T) {
	for _, tc := range []struct {
		message string
		exists  bool
	}{
		{"Fatal: create key in repository at s3:x failed: repository master key and config already initialized", true},
		{"Fatal: create repository at s3:x failed: Fatal: unable to open repository at s3:x: config file already exists", true},
		{"Fatal: create repository at s3:x failed: BucketAlreadyExists: the bucket already exists in another region", false},
		{"Fatal: create repository at s3:x failed: a lock already exists", false},
	} {
		if got := repositoryExists(tc.message); got != tc.exists {
			t.Errorf("%q: exists %v, want %v", tc.message, got, tc.exists)
		}
	}
}

// What init says is read where restic puts it — the head of the
// message for a failure before any write, its tail for files found and
// nothing written — never anywhere in it: the message echoes the
// repository URL, which is the operator's to write, restic's own words
// included.
func TestResticsWordsInsideTheURLAreNotReadAsItsAnswer(t *testing.T) {
	for _, tc := range []struct {
		message            string
		refused, halfThere bool
	}{
		{"Fatal: create repository at s3:x failed: Fatal: unable to open repository at s3:x: client.BucketExists: The request signature we calculated does not match the signature you provided.", true, false},
		{"Fatal: create repository at s3:x failed: Fatal: unable to open repository at s3:x: client.BucketExists: Access Denied.", true, false},
		{"Fatal: create repository at s3:x failed: parsing repository location failed: invalid backend", true, false},
		{"Fatal: create repository at s3:x failed: Fatal: unable to open repository at s3:x: config file already exists", false, false},
		{"Fatal: create key in repository at s3:x failed: repository already contains keys", false, true},
		{"Fatal: create key in repository at s3:x failed: repository already contains snapshots", false, true},
		{"Fatal: create key in repository at s3:x failed: Save(<key/8f2a1c>) returned error, retrying after 15m0s: 503 Service Unavailable", false, false},
		// restic's words inside the URL, echoed by the message.
		{"Fatal: create key in repository at s3:http://h/unable to open repository at x failed: Save(<key/8f2a1c>) returned error: 503 Service Unavailable", false, false},
		{"Fatal: create key in repository at s3:http://h/create repository at x failed: Save(<key/8f2a1c>) returned error: 503 Service Unavailable", false, false},
		{"Fatal: create repository at s3:http://h/repository already contains keys failed: Fatal: unable to open repository at s3:http://h/repository already contains keys: client.BucketExists: signature", true, false},
		{"Fatal: create key in repository at s3:http://h/failed: repository already contains keys failed: Save(<key/8f2a1c>) returned error: 503 Service Unavailable", false, false},
	} {
		if got := backendRefused(tc.message); got != tc.refused {
			t.Errorf("%q: refused at the opening %v, want %v", tc.message, got, tc.refused)
		}
		if got := repositoryHalfThere(tc.message); got != tc.halfThere {
			t.Errorf("%q: files and no config %v, want %v", tc.message, got, tc.halfThere)
		}
	}
	// And through setup: a failure past the opening keeps the password
	// though the URL says "unable to open repository at"; a refusal at
	// the opening asks for the key again, and ends "never used", though
	// the URL says "repository already contains keys".
	url := "s3:http://e2e-s3:9000/unable to open repository at x"
	b, m := setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", strings.ReplaceAll(keyWriteFailed, "s3:http://e2e-s3:9000/box", url)
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
	_, err := b.setup(t, m, url)
	if err == nil || !strings.HasSuffix(err.Error(), "keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there") {
		t.Fatalf("err = %v", err)
	}
	url = "s3:http://e2e-s3:9000/repository already contains keys"
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", strings.ReplaceAll(wrongKey, "s3:http://e2e-s3:9000/box", url)
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
	_, err = b.setup(t, m, url)
	if err == nil || !strings.HasSuffix(err.Error(), "the password shown above was never used: discard it") || strings.Contains(err.Error(), "holds files") {
		t.Fatalf("err = %v", err)
	}
	if got, want := b.roles(), "plan probe init probe init probe init"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
}

// A password shown on a first attempt and not used — the second
// attempt's look found the repository — is said not to be the one, and
// dead, on the way out.
func TestAPasswordShownAndNotUsedIsSaidDeadOnSuccessToo(t *testing.T) {
	b, m := setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", keyWriteFailed
	opens := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			// The second attempt's look finds a repository the shown
			// password does not open: not ours.
			b.hang = ""
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
		}
		if strings.Contains(s.Name, "_open_") {
			if opens++; opens == 2 {
				delete(b.outcome, "open")
			}
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret", "its-own-password"}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil || rep.New || b.roles() != "plan probe init probe open open" {
		t.Fatalf("%v %+v %s", err, rep, b.roles())
	}
	for _, want := range []string{"the repository exists; its password is needed (the one shown above is not it)", "the password shown above was never used: discard it"} {
		if !strings.Contains(m.saidAll(), want) {
			t.Fatalf("said lacks %q:\n%s", want, m.saidAll())
		}
	}
}

// A wrong password for a repository that exists is asked for again, up
// to three times.
func TestAWrongRepositoryPasswordIsAskedForAgain(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	opens := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_open_") {
			if opens++; opens == 2 {
				delete(b.outcome, "open")
			}
		}
	}
	m.answers = []string{"AKIDX", "the-secret", "not-the-password", "its-own-password"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe open open"; got != want || !strings.Contains(m.saidAll(), "this password cannot open the repository (exit 12): again") {
		t.Fatalf("units: %s\nsaid:\n%s", got, m.saidAll())
	}
	if v := envOf(t, b.cfg.EnvFile); v["RESTIC_PASSWORD"] != "its-own-password" {
		t.Fatalf("the file holds %v", v)
	}
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "a", "b", "c"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), "this password cannot open the repository (exit 12)") || b.roles() != "plan probe open open open" {
		t.Fatalf("err = %v, units %s", err, b.roles())
	}
}

// A mistake over a working setup leaves the working file byte for byte,
// with no copy of a credential beside it and no unit running.
func TestAMistakeLeavesAWorkingSetupAsItWas(t *testing.T) {
	const working = "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\nRESTIC_PASSWORD=old\nAWS_ACCESS_KEY_ID=A\nAWS_SECRET_ACCESS_KEY=B\n"
	for _, tc := range []struct {
		name   string
		break_ func(b *box, m *term)
		want   string
	}{
		{"wrong key", func(b *box, m *term) {
			b.hang = "probe"
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", wrongKey
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
		}, "restic could not make or open the repository (exit 1): Fatal: create repository at"},
		{"wrong key, refused at the look", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.probeErr = refusedAtOnce
			m.answers = []string{"AKIDX", "wrong", "AKIDX", "wrong", "AKIDX", "wrong"}
		}, "restic could not open the repository (exit 1): Fatal: unable to open config file: Stat: Access Denied."},
		{"files of a repository and no config", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", halfThere
		}, "the storage holds files of a restic repository and no config"},
		{"wrong password for an existing repository", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.probeOut = ""
			m.answers = []string{"AKIDX", "the-secret", "a", "b", "c"}
		}, "this password cannot open the repository (exit 12)"},
		{"the repository gone between the probe and the opening", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
			b.probeOut = ""
			m.answers = []string{"AKIDX", "the-secret", "pw"}
		}, "there is no repository at the configured location (exit 10)"},
		{"the storage refusing the opening, in restic's words", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.probeOut, b.openErr = "", "Fatal: unable to open repository: AccessDenied: keys/ is not yours to read\n"
			m.answers = []string{"AKIDX", "the-secret", "pw"}
		}, "restic could not open the repository with its lock (exit 1): Fatal: unable to open repository: AccessDenied: keys/ is not yours to read"},
		{"init exits 0 and names no repository", func(b *box, m *term) { b.initOut = "" }, "restic made the repository and exited 0 but said nothing of its id; run setup again, which opens it; the password shown above is the repository's: keep it"},
		{"init ended by a signal", func(b *box, m *term) {
			b.outcome["init"] = unit.Outcome{Result: "signal"}
		}, "restic was ended by signal"},
		{"the manager could not set the unit up", func(b *box, m *term) {
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 217}
		}, "systemd could not set the unit up (status 217)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			must(t, os.MkdirAll(filepath.Dir(b.cfg.EnvFile), 0o755))
			must(t, os.WriteFile(b.cfg.EnvFile, []byte(working), 0o600))
			tc.break_(b, m)
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q\nsaid:\n%s", err, tc.want, m.saidAll())
			}
			raw, _ := os.ReadFile(b.cfg.EnvFile)
			if string(raw) != working {
				t.Fatalf("the working file was changed:\n%s", raw)
			}
			if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 0 {
				t.Fatalf("a copy of a credential was left: %v", left)
			}
			if entries, _ := os.ReadDir(filepath.Dir(b.cfg.EnvFile)); len(entries) != 1 {
				t.Fatalf("left in the directory: %v", entries)
			}
			if strings.Contains(err.Error(), "no repository") && tc.name == "wrong key" {
				t.Fatalf("an exit 1 was called no repository: %v", err)
			}
		})
	}
}

// A repository that does not answer init is given up on — the waiting,
// not the unit: init is never stopped half way (a key and no config is
// a repository nothing opens), so it is left running, recorded for
// the next lock holder to wait for, and the operator is told how to end
// it if it must be.
func TestARepositoryThatDoesNotAnswerIsGivenUpOnAndInitLeftRunning(t *testing.T) {
	b, m := setupBox(t)
	b.hang = "init"
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "the repository did not answer within") || !strings.Contains(err.Error(), "restic init is left running as motel_backup_init_") || !strings.Contains(err.Error(), "systemctl stop motel_backup_init_") || !strings.HasSuffix(err.Error(), "keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there") {
		t.Fatalf("err = %v", err)
	}
	if len(b.stopped) != 0 {
		t.Fatalf("init was stopped: %v", b.stopped)
	}
	if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "init-unit")); err != nil {
		t.Fatal("the running init is not recorded for the next lock holder")
	}
	m.asked = nil
	leftForInit(t, b, m)
	// What a person waiting on init is told: Ctrl-C leaves a unit
	// running that may have made the repository with the shown password
	// — never "nothing has been written", which is the look's and the
	// opening's.
	if !strings.Contains(m.saidAll(), "still waiting for s3:http://e2e-s3:9000/box (Ctrl-C leaves restic init running, and the next setup waits for it; it may be making the repository with the password shown: keep it)") || strings.Contains(m.saidAll(), "nothing has been written") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.hang = "open"
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(m.saidAll(), "still waiting for s3:http://e2e-s3:9000/box (Ctrl-C is safe: nothing of setup's has been written; a lock file restic was writing may stay") {
		t.Fatalf("err %v\nsaid:\n%s", err, m.saidAll())
	}
	// A unit the runner could not see gone is not said to have been
	// stopped: the runner's own words are the error.
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.hang, b.stopErr = "open", unit.ErrNotConfirmedGone
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || strings.Contains(err.Error(), "the unit was stopped") || !errors.Is(err, unit.ErrNotConfirmedGone) {
		t.Fatalf("err = %v", err)
	}
}

// An interrupt during init leaves it running — recorded for the next
// lock holder — and writes nothing; the password shown is to be kept.
func TestAnInterruptDuringInitLeavesItRunningAndWritesNothing(t *testing.T) {
	b, m := setupBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.hang = "init"
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			cancel()
		}
	}
	_, err := Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "keep the password shown above") {
		t.Fatalf("err = %v", err)
	}
	if len(b.stopped) != 0 {
		t.Fatalf("init was stopped: %v", b.stopped)
	}
	if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "init-unit")); err != nil {
		t.Fatal("the running init is not recorded for the next lock holder")
	}
	m.asked = nil
	leftForInit(t, b, m)
}

var asideRe = regexp.MustCompile(`^status\.json\.aside-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}$`)

func TestTheRecordIsPutAsideWithAChangeOfRepository(t *testing.T) {
	const other = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		old  string // the file before, or none
		id   string // the repository id setup keeps beside the record, or none
		why  string // why the record goes aside; kept when empty
		to   string // the URL setup is given, where not the fixture's
	}{
		{"another repository", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/other\nRESTIC_PASSWORD=x\n", other, "it was written against another repository (id 01234567)", ""},
		{"the same URL, another repository", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\nRESTIC_PASSWORD=x\n", other, "it was written against another repository (id 01234567)", ""},
		{"the same repository", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\nRESTIC_PASSWORD=x\n", repoID, "", ""},
		{"the same repository, by another URL", "RESTIC_REPOSITORY=s3:http://e2e-s3.internal:9000/box\nRESTIC_PASSWORD=x\n", repoID, "", ""},
		// A box set up on b2: before setup refused it, moved to B2's
		// S3-compatible endpoint: the same objects, so the same id, and
		// the record is kept; the file is written whole, B2's own key
		// lines gone.
		{"the same repository, from b2: to B2's S3 endpoint", "RESTIC_REPOSITORY=b2:hs-rc-bucket:rc-check1\nRESTIC_PASSWORD=x\nB2_ACCOUNT_ID=K001\nB2_ACCOUNT_KEY=k\n", repoID, "", b2Repo},
		{"no id recorded", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\nRESTIC_PASSWORD=x\n", "", "no setup recorded which repository it was written against", ""},
		{"no file before", "", repoID, "no credential file was there to tie it to this repository", ""},
		{"the same URL, re-made", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\nRESTIC_PASSWORD=x\n", repoID, "the repository was made by this setup, so it holds none of the record's snapshots", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			if tc.name != "the same URL, re-made" {
				// The repository is there: the look finds it, and it is
				// opened with its own password.
				b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
				m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
			}
			if tc.old != "" {
				must(t, os.MkdirAll(filepath.Dir(b.cfg.EnvFile), 0o755))
				must(t, os.WriteFile(b.cfg.EnvFile, []byte(tc.old), 0o600))
			}
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			idFile := filepath.Join(b.cfg.StateDir, "repository-id")
			if tc.id != "" {
				must(t, os.WriteFile(idFile, []byte(tc.id+"\n"), 0o644))
			}
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
			to := "s3:http://e2e-s3:9000/box"
			if tc.to != "" {
				to = tc.to
			}
			rep, err := b.setup(t, m, to)
			if err != nil {
				t.Fatal(err)
			}
			if v := envOf(t, b.cfg.EnvFile); v["RESTIC_REPOSITORY"] != to || v["B2_ACCOUNT_ID"] != "" || v["B2_ACCOUNT_KEY"] != "" || (to == b2Repo) != (v["AWS_DEFAULT_REGION"] == "eu-central-003") {
				t.Fatalf("the file holds %v", v)
			}
			// The id of the repository the file now names is kept beside
			// the record, for everyone to read, as the record is.
			if raw, err := os.ReadFile(idFile); err != nil || string(raw) != repoID+"\n" {
				t.Fatalf("the repository id beside the record: %q, %v", raw, err)
			}
			if st, err := os.Stat(idFile); err != nil || st.Mode().Perm() != 0o644 {
				t.Fatalf("the id file: %v %v", st, err)
			}
			st, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			must(t, err)
			asides, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, "status.json.aside-*"))
			if tc.why != "" {
				if len(st.Apps) != 0 || len(asides) != 1 || rep.Aside != asides[0] || !strings.Contains(m.saidAll(), "the record was put aside ("+tc.why+"): "+asides[0]+"; the next run drills what it backs up") {
					t.Fatalf("the record was kept: %v %v %+v\n%s", st.Apps, asides, rep, m.saidAll())
				}
				if st, err := os.Stat(asides[0]); err != nil || st.Mode().Perm() != 0o644 {
					t.Fatalf("the aside: %v %v", st, err)
				}
				// Named by the time and this setup's nonce: two asides in
				// one second, or a clock set back, do not replace one
				// another.
				if !asideRe.MatchString(filepath.Base(asides[0])) {
					t.Fatalf("the aside's name: %s", asides[0])
				}
			} else if len(st.Apps) != 1 || len(asides) != 0 || rep.Aside != "" || strings.Contains(m.saidAll(), "put aside") {
				t.Fatalf("the record was put aside: %v %v\n%s", st.Apps, asides, m.saidAll())
			}
		})
	}
	t.Run("no record", func(t *testing.T) {
		b, m := setupBox(t)
		if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
			t.Fatal(err)
		}
		if asides, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, "status.json*")); len(asides) != 0 {
			t.Fatalf("a record appeared: %v", asides)
		}
		if raw, err := os.ReadFile(filepath.Join(b.cfg.StateDir, "repository-id")); err != nil || string(raw) != repoID+"\n" {
			t.Fatalf("the repository id beside the record: %q, %v", raw, err)
		}
	})
}

// A file that could not be put in place keeps the record it goes
// with: the box is still on the old repository.
func TestARecordIsPutAsideOnlyOnceTheFileIsInPlace(t *testing.T) {
	b, m := setupBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	idFile := filepath.Join(b.cfg.StateDir, "repository-id")
	must(t, os.WriteFile(idFile, []byte("other\n"), 0o644))
	// A directory where the file has to go: the rename over it fails.
	must(t, os.MkdirAll(b.cfg.EnvFile, 0o755))
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || b.roles() != "plan probe init open" {
		t.Fatalf("setup with a directory in the file's place: %v after %s", err, b.roles())
	}
	st, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if asides, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, "status.json.aside-*")); len(st.Apps) != 1 || len(asides) != 0 {
		t.Fatalf("the record was put aside: %v %v", st.Apps, asides)
	}
	// And the id beside it is the old repository's again.
	if raw, err := os.ReadFile(idFile); err != nil || string(raw) != "other\n" {
		t.Fatalf("the repository id: %q, %v", raw, err)
	}
	// Where there was none, none is left.
	b, m = setupBox(t)
	must(t, os.MkdirAll(b.cfg.EnvFile, 0o755))
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil {
		t.Fatal("setup succeeded with a directory in the file's place")
	}
	if _, err := os.Lstat(filepath.Join(b.cfg.StateDir, "repository-id")); err == nil {
		t.Fatal("a repository id was left beside no credential file")
	}
}

// A credential directory that is a link leads the root-only file
// somewhere else: refused, before anything is written.
func TestACredentialDirectoryThatIsALinkIsRefused(t *testing.T) {
	b, m := setupBox(t)
	dir := filepath.Dir(b.cfg.EnvFile)
	must(t, os.MkdirAll(filepath.Dir(dir), 0o755))
	elsewhere := t.TempDir()
	must(t, os.Symlink(elsewhere, dir))
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), dir+" is a link, and the credential file has to be in a directory of root's own") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 || len(m.asked) != 0 {
		t.Fatalf("written through the link: %v; asked %v", entries, m.asked)
	}
}

// The record is put aside before the file takes its place, and put
// back if the file cannot: whatever ends setup between the two leaves
// no credential file with a record of another repository beside it.
// The aside is on the disk before the file is, and so is a put-back.
func TestTheRecordIsAsideBeforeTheFileIsInPlace(t *testing.T) {
	b, m := setupBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	statusPath := filepath.Join(b.cfg.StateDir, "status.json")
	must(t, record.Write(statusPath, &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	var recordAtCommit string
	oldCommit := commit
	commit = func(from, to string) error {
		if _, err := os.Lstat(statusPath); err == nil {
			recordAtCommit = "in place"
		} else {
			recordAtCommit = "aside"
		}
		return oldCommit(from, to)
	}
	t.Cleanup(func() { commit = oldCommit })
	var syncedAtCommit int
	inner := commit
	commit = func(from, to string) error {
		syncedAtCommit = len(b.synced)
		return inner(from, to)
	}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if recordAtCommit != "aside" {
		t.Fatalf("at the moment the file took its place the record was %s", recordAtCommit)
	}
	if syncedAtCommit != 1 || b.synced[0] != b.cfg.StateDir {
		t.Fatalf("the state directory was not put on the disk before the file was committed: synced %v", b.synced)
	}
	// And a put-back is synced too.
	b, m = setupBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	must(t, os.MkdirAll(b.cfg.EnvFile, 0o755))
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || len(b.synced) != 2 {
		t.Fatalf("err %v, synced %v", err, b.synced)
	}
}

// What each failure between the aside and the commit leaves: the two
// files agree, whichever step failed.
func TestAFailureBetweenTheAsideAndTheCommitLeavesTheTwoFilesAgreeing(t *testing.T) {
	seed := func(t *testing.T) (*box, *term, string) {
		b, m := setupBox(t)
		must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
		statusPath := filepath.Join(b.cfg.StateDir, "status.json")
		must(t, record.Write(statusPath, &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
		must(t, os.WriteFile(filepath.Join(b.cfg.StateDir, "repository-id"), []byte("other\n"), 0o644))
		return b, m, statusPath
	}
	recordInPlace := func(t *testing.T, statusPath string) {
		t.Helper()
		st, err := record.Read(statusPath)
		must(t, err)
		if asides, _ := filepath.Glob(statusPath + ".aside-*"); len(st.Apps) != 1 || len(asides) != 0 {
			t.Fatalf("the record: %v, asides %v", st.Apps, asides)
		}
		if raw, err := os.ReadFile(filepath.Join(filepath.Dir(statusPath), "repository-id")); err != nil || string(raw) != "other\n" {
			t.Fatalf("the repository id beside the record: %q, %v", raw, err)
		}
	}
	// The sync after the aside fails: the record is put back, and that
	// put-back is synced, before the error is returned.
	b, m, statusPath := seed(t)
	calls := 0
	syncDir = func(path string) error {
		calls++
		b.synced = append(b.synced, path)
		if calls == 1 {
			return errors.New("EIO on the state directory")
		}
		return nil
	}
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "EIO on the state directory") {
		t.Fatalf("err = %v", err)
	}
	recordInPlace(t, statusPath)
	if _, err := os.Lstat(b.cfg.EnvFile); err == nil {
		t.Fatal("the credential file was put in place after the aside could not be synced")
	}
	if len(b.synced) != 2 {
		t.Fatalf("the put-back was not synced: %v", b.synced)
	}
	// The credential's rename fails: the record is put back.
	b, m, statusPath = seed(t)
	must(t, os.MkdirAll(b.cfg.EnvFile, 0o755))
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil {
		t.Fatal("setup succeeded with a directory in the file's place")
	}
	recordInPlace(t, statusPath)
	// The credential directory's sync fails after the rename: the file
	// is in place, so the record stays aside — the two agree — and the
	// error says what state was left.
	b, m, statusPath = seed(t)
	calls = 0
	syncDir = func(path string) error {
		calls++
		b.synced = append(b.synced, path)
		if path == filepath.Dir(b.cfg.EnvFile) {
			return errors.New("EIO on the credential directory")
		}
		return nil
	}
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	asides, _ := filepath.Glob(statusPath + ".aside-*")
	if err == nil || !strings.Contains(err.Error(), "EIO on the credential directory") || !strings.Contains(err.Error(), "the credential file is in place") || len(asides) != 1 || !strings.Contains(err.Error(), "the record was put aside: "+asides[0]) {
		t.Fatalf("err = %v (asides %v)", err, asides)
	}
	if _, err := os.Lstat(b.cfg.EnvFile); err != nil {
		t.Fatal("the credential file is not in place")
	}
	st, err := record.Read(statusPath)
	must(t, err)
	if asides, _ := filepath.Glob(statusPath + ".aside-*"); len(st.Apps) != 0 || len(asides) != 1 {
		t.Fatalf("the record was put back under a file already in place: %v, asides %v", st.Apps, asides)
	}
	// The file's directory could not be synced: the id stays the old
	// one, since the rename may not be on the disk, and the next setup
	// puts the record aside — never the new id beside the old file.
	if raw, err := os.ReadFile(filepath.Join(b.cfg.StateDir, "repository-id")); err != nil || string(raw) != "other\n" {
		t.Fatalf("the repository id after a failed directory sync: %q, %v", raw, err)
	}
	// The id is written once the file is in place, never before: a
	// power cut between the two must not leave the new id beside the
	// old file, or a record the next run writes against the old
	// repository would be kept by a setup onto the new one. An id that
	// could not be written after the commit is said, with the file in
	// place; the old id stays, and the next setup puts the record aside.
	b, m, _ = seed(t)
	idAtCommit := ""
	inner := commit
	commit = func(from, to string) error {
		raw, _ := os.ReadFile(filepath.Join(b.cfg.StateDir, "repository-id"))
		idAtCommit = string(raw)
		return inner(from, to)
	}
	t.Cleanup(func() { commit = inner })
	writeID = func(string, string) error { return errors.New("EIO on the id") }
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if idAtCommit != "other\n" {
		t.Fatalf("at the moment the file took its place the id was %q", idAtCommit)
	}
	if err == nil || !strings.Contains(err.Error(), "EIO on the id") || !strings.Contains(err.Error(), "the credential file is in place") || !strings.Contains(err.Error(), "keep it") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(b.cfg.StateDir, "repository-id")); string(raw) != "other\n" {
		t.Fatalf("the id after a failed write: %q", raw)
	}
	if _, err := os.Lstat(b.cfg.EnvFile); err != nil {
		t.Fatal("the credential file is not in place")
	}
}

func TestTheAccountIsMadeWhenMissingAndLeftAloneWhenNot(t *testing.T) {
	b, m := setupBox(t)
	b.account = false
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatal(err)
	}
	if b.accountsMade != 1 || rep.Account != "made" || !strings.Contains(m.saidAll(), "account motel-backup: made") {
		t.Fatalf("made %d, report %+v\n%s", b.accountsMade, rep, m.saidAll())
	}
	b, m = setupBox(t)
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if b.accountsMade != 0 || !strings.Contains(m.saidAll(), "account motel-backup: present") {
		t.Fatalf("made %d\n%s", b.accountsMade, m.saidAll())
	}
	if !strings.Contains(useraddArgv(), "useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment made-by-motel motel-backup") {
		t.Fatalf("useradd: %s", useraddArgv())
	}
}

func TestALeftoverTempFileIsRemovedFirstAndSaid(t *testing.T) {
	b, m := setupBox(t)
	dir := filepath.Dir(b.cfg.EnvFile)
	must(t, os.MkdirAll(dir, 0o755))
	// The two shapes a setup that did not live to the end leaves: the
	// staged file the units read, and the one envfile.Write makes on
	// the way to it. What an operator keeps beside the file is theirs.
	stale := envfile.Staged(b.cfg.EnvFile)
	dotted := filepath.Join(dir, ".repository.env.staged-4207310592")
	kept := []string{b.cfg.EnvFile + ".bak", b.cfg.EnvFile + ".old", filepath.Join(dir, "repository.env.gs"), filepath.Join(dir, "notes.txt"), b.cfg.EnvFile + ".0123456789ab"}
	for _, f := range append([]string{stale, dotted}, kept...) {
		must(t, os.WriteFile(f, []byte("RESTIC_PASSWORD=leaked\n"), 0o600))
	}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{stale, dotted} {
		if _, err := os.Lstat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the leftover %s is still there", f)
		}
	}
	for _, f := range kept {
		if _, err := os.Lstat(f); err != nil {
			t.Errorf("the operator's %s was removed", f)
		}
	}
	if len(rep.Swept) != 2 || !strings.Contains(m.saidAll(), "removed a file an interrupted setup left: "+stale) || !strings.Contains(m.saidAll(), "removed a file an interrupted setup left: "+dotted) {
		t.Fatalf("report %+v\n%s", rep, m.saidAll())
	}
}

func TestThePlanIsSaidAndAPlanWithNoAppGoesOn(t *testing.T) {
	b, m := setupBox(t)
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.saidAll(), "a run would back up blog, under "+b.root) {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	b, m = setupBox(t)
	b.plan = `{"root":"/var/lib/liveswap","apps":{}}`
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Apps) != 0 || !strings.Contains(m.saidAll(), "no app declares a backup yet: a run would back nothing up") {
		t.Fatalf("report %+v\n%s", rep, m.saidAll())
	}
}
