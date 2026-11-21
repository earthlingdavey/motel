package engine

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/backups/dump"
	"github.com/smallhoursorg/motel/backups/envfile"
	"github.com/smallhoursorg/motel/backups/plan"
	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/unit"
	"golang.org/x/sys/unix"
)

// Terminal is the operator at a terminal: what setup asks, with echo
// off for a secret, and what it tells them.
type Terminal interface {
	Ask(ctx context.Context, prompt string, secret bool) (string, error)
	Say(line string)
}

// SetupOptions is what setup is given: the repository, which is not a
// secret and travels as an argument, and the operator.
type SetupOptions struct {
	Repository string
	Terminal   Terminal
	// Existing is the operator's word that a repository is there — a
	// rebuilt box's, the guide's "A rebuilt box" — so setup makes no
	// password and shows none: it asks for the repository's own, and
	// refuses where the storage says there is none. The look still
	// runs, for a key the storage refuses, under setup's longer clock:
	// a storage slow to answer is waited for, and a clock met all the
	// same has the key asked for again; the third ends setup with
	// nothing written.
	Existing bool
}

// SetupReport is what setup did.
type SetupReport struct {
	Account      string // "made" or "present"
	Apps         []string
	New          bool // the repository was made by this setup
	RepositoryID string
	Aside        string   // where the record of the previous repository went, if anywhere
	Swept        []string // files an interrupted setup left, removed
	OldEnvFile   string   // a credential file from before this version, still there
}

// setupClock bounds restic init and the opening of an existing
// repository. restic init answers at once whatever is wrong — a wrong
// key, a host that does not resolve, a port nobody listens on, all
// measured — and after 30 s for a host that swallows packets; opening
// with a wrong password answers at once (exit 12). The clock is for
// what was not measured. setupNote is when a person waiting is told
// what for.
//
// setupProbeClock bounds the look for a repository that comes before
// any password is made: `cat config` with a throwaway password answers
// at once with a right key — exit 10 where there is no repository, 12
// where there is one — and retries for minutes on a bucket not there
// yet, a wrong key, a host that does not resolve [measured]. Those are
// init's to answer, at once, so the look is given up on soon.
var (
	setupClock      = 2 * time.Minute
	setupProbeClock = 10 * time.Second
	setupNote       = 20 * time.Second
)

// What setup checks the box has before it asks anyone for a secret, and
// how it makes the account. Variables so that the flow can be tested
// against a box that lacks them.
var (
	haveProgram = func(path string) bool {
		// Run as motel-backup and as the app's own uid, never as root:
		// the execute bit that counts is the one for everyone.
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular() && st.Mode()&0o001 != 0
	}
	// account is the account as the manager itself resolves it — getent,
	// so NSS, where os/user reads /etc/passwd alone: its uid, gid and
	// comment, and whether it is there at all. getent's argv is
	// constant; its exit 2 is "not found".
	account = func(ctx context.Context, name string) (passwd, error) {
		out, exit, err := lookup(ctx, lookupClock, getent, "passwd", name)
		if err != nil {
			return passwd{}, err
		}
		switch exit {
		case 0:
			return passwdLine(strings.TrimRight(string(out), "\n"), getent+" passwd "+name)
		case 2:
			return passwd{}, nil
		}
		return passwd{}, fmt.Errorf("%s passwd %s: exit status %d", getent, name, exit)
	}
	// makeAccount runs useradd to its end, with no context and no clock:
	// it commits passwd, shadow, group and gshadow one rename at a time
	// [measured], and stopped part way leaves the account without its
	// group. Ctrl-C at the terminal reaches it as it reaches setup, the
	// two sharing a process group; and setup is attended (D4).
	makeAccount = func() error {
		if out, err := exec.Command(useradd[0], useradd[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", useraddArgv(), err, record.Text(string(out)))
		}
		return nil
	}
)

// useradd makes the account restic runs as: no home, no shell, and
// nothing of its own but the cache the manager makes for it. The
// package's postinstall makes it with the same line, before setup ever
// runs (TestPostinstallMakesTheAccountAsSetupDoes).
//
// Its comment is the mark that it is motel's (accountMark): the one
// account every restic unit runs as is one motel made, or none —
// an account of that name on the box for another reason is refused,
// never taken by its name. The mark lives with the account, which
// outlives a purge, so a reinstall finds it still ours.
var useradd = []string{"/usr/sbin/useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", "--comment", accountMark, backupUser}

// localAccount is the account as /etc/passwd holds it — which only
// root writes, and where useradd puts it — asked of the files source
// alone. Exit 2 is "not there".
var localAccount = func(ctx context.Context, name string) (passwd, error) {
	out, exit, err := lookup(ctx, lookupClock, getent, "-s", "files", "passwd", name)
	if err != nil {
		return passwd{}, err
	}
	switch exit {
	case 0:
		return passwdLine(strings.TrimRight(string(out), "\n"), getent+" -s files passwd "+name)
	case 2:
		return passwd{}, nil
	}
	return passwd{}, fmt.Errorf("%s -s files passwd %s: exit status %d", getent, name, exit)
}

// accountMark is the comment motel gives the account it makes.
const accountMark = "made-by-motel"

const getent = "/usr/bin/getent"

// lookupClock bounds one lookup in the account databases. They answer
// at once from a file, and from a directory in the time its own
// timeouts give it; a run holds the lock while it asks, so a directory
// that never answers is given up on, and said.
var lookupClock = 10 * time.Second

// lookup runs one program of the account databases — getent — with
// a constant argv, and returns what it printed and its exit status. The
// command ends with the context, and at the bound: an error is a
// command that could not be run or did not answer, never an exit
// status, which is the caller's to read.
var lookup = func(ctx context.Context, within time.Duration, argv ...string) (out []byte, exit int, err error) {
	bounded, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	cmd := exec.CommandContext(bounded, argv[0], argv[1:]...) //nolint:gosec // a constant program and constant arguments
	// What the command leaves behind holding its pipe is not waited for.
	cmd.WaitDelay = time.Second
	out, err = cmd.Output()
	var ended *exec.ExitError
	switch {
	case err == nil:
		return out, 0, nil
	case ctx.Err() != nil:
		return nil, 0, fmt.Errorf("%s: %w", strings.Join(argv, " "), ctx.Err())
	case bounded.Err() != nil:
		return nil, 0, fmt.Errorf("%s did not answer within %s", strings.Join(argv, " "), within)
	case errors.As(err, &ended) && ended.ExitCode() >= 0:
		return out, ended.ExitCode(), nil
	}
	return nil, 0, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
}

// passwdLine reads one line of the passwd database, as from says it
// was asked for.
func passwdLine(line, from string) (passwd, error) {
	fields := strings.Split(line, ":")
	if len(fields) != 7 {
		return passwd{}, fmt.Errorf("%s: not a passwd line: %s", from, record.Text(line))
	}
	uid, uerr := strconv.Atoi(fields[2])
	gid, gerr := strconv.Atoi(fields[3])
	if uerr != nil || gerr != nil {
		return passwd{}, fmt.Errorf("%s: not a passwd line: %s", from, record.Text(line))
	}
	return passwd{line: line, name: fields[0], uid: uid, gid: gid, comment: fields[4], exists: true}, nil
}

// passwd is an account as getent gives it: its uid and gid, its
// comment, and whether it is there at all.
type passwd struct {
	line     string // the whole line, as it was answered
	name     string
	uid, gid int
	comment  string
	exists   bool
}

func useraddArgv() string { return strings.Join(useradd, " ") }

// accountUsable is what setup, and every run, restore and drill, ask
// of an account that is there: that motel made it. Every restic unit
// runs as it, and whoever is it, or can log in as it, reads the
// repository credential from a running restic's environment. So the
// account is motel's own, made with its mark, or it is refused —
// an account of that name on the box for another reason is never
// taken by its name, and never changed (the owner, 2026-09-27). What
// root does to motel's account after is root's: root reads the
// credential file itself.
//
// Ours is local as well as marked: the account in /etc/passwd, and the
// one the system resolves by that name. A directory's account of that
// name carries whatever comment its administrator gives it, the mark
// among them (the owner's review of #155).
func accountUsable(ctx context.Context, acct passwd) error {
	remedy := fmt.Sprintf("as root, remove it (userdel %s) and run motel-backup setup <repository>, which makes it (%s); or, if it is one you made for motel, mark it: usermod --comment %s %s",
		backupUser, useraddArgv(), accountMark, backupUser)
	if acct.comment != accountMark {
		return fmt.Errorf("the %s account on this box was not made by motel (its comment is %q, not %q): every restic unit runs as it, so motel uses only an account it made itself; %s",
			backupUser, record.Text(acct.comment), accountMark, remedy)
	}
	local, err := localAccount(ctx, backupUser)
	if err != nil {
		return err
	}
	switch {
	case !local.exists:
		return fmt.Errorf("the %s account this box resolves is not in /etc/passwd — a directory's, whose comment is its administrator's to write — so it is not one motel made: every restic unit runs as it; %s",
			backupUser, remedy)
	case local.uid != acct.uid || local.comment != acct.comment || local.line != acct.line:
		// The whole line, not the uid and the mark alone: a source asked
		// before files can answer with both, and a shell or a home of
		// its own (Copilot on #155).
		return fmt.Errorf("the %s account this box resolves is not the one in /etc/passwd (%s, where /etc/passwd has %s): another source answers for the name first, so it is not one motel made; %s",
			backupUser, record.Text(acct.line), record.Text(local.line), remedy)
	}
	return nil
}

// AccountReady is accountReady for the `account` command: what the
// package's postinstall asks, so that its warning is setup's own words,
// and what an administrator asks after mending the account.
func AccountReady(ctx context.Context) error { _, err := accountReady(ctx); return err }

// accountReady is what a run, a restore and a drill ask of the account
// restic runs as, before any unit: that it is there, and motel's —
// what setup asks of one that exists, held at every run, since it is
// the run that puts the credential in that account's environment. Not there: a box with a credential file written by
// hand and no account, whose units would end 217/USER.
//
// The account it returns is the one that was looked at: what a fetch
// gives its directory to, with no second lookup for a directory to
// answer otherwise.
func accountReady(ctx context.Context) (passwd, error) {
	acct, err := account(ctx, backupUser)
	if err != nil {
		return passwd{}, err
	}
	if !acct.exists {
		return passwd{}, fmt.Errorf("the %s account is not there: motel-backup setup <repository>, as root, makes it (%s)", backupUser, useraddArgv())
	}
	return acct, accountUsable(ctx, acct)
}

// programs are what the units run, and where. A unit whose command is
// not there ends 203/EXEC, which says nothing of what to install: setup
// and a run each refuse first, in the same words. The restic line says
// what restic is, for an operator told to install it who may not have
// met it; every other line uses the bare name. The gloss sits before
// the colon so that the remedy stays the line's tail, as its siblings'.
func programsInstalled(cfg Config) error {
	for _, p := range []struct{ name, path, fix string }{
		{"restic, the backup program motel-backup runs,", cfg.Restic, ": apt install restic"},
		{"sqlite3", dump.Program(), ": apt install sqlite3"},
		{"moteld", plan.Program(), ""},
		{"motel-backup", cfg.Self, ", where the units run it"},
	} {
		if !haveProgram(p.path) {
			return fmt.Errorf("%s is not installed at %s%s", p.name, p.path, p.fix)
		}
	}
	return nil
}

// errByHand marks a backend the run can use and this command does not
// set up: the file is written by hand.
var errByHand = errors.New("not set up by this command")

// credential is one thing a backend takes from the environment.
type credential struct {
	label, variable string
	secret          bool
}

// backendOf says what the repository's backend needs typed, or why the
// box cannot use it. Only backends that take their credentials from the
// environment are usable at all [handover]; of those, azure: is not in
// Debian's restic [measured], and gs: and swift: take a credentials
// file or a dozen variables, which this command does not ask for.
func backendOf(repo, envFile string) ([]credential, error) {
	scheme, rest, ok := strings.Cut(repo, ":")
	switch {
	case repo == "":
		return nil, errors.New("no repository was given")
	case !ok || strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, ".") || scheme == "local":
		return nil, errors.New("a path on this box is not a repository: a backup must survive losing the box")
	}
	switch scheme {
	case "sftp":
		return nil, errors.New("sftp: is not supported: ssh takes its key and known_hosts from a home directory, which no backup unit has")
	case "rclone":
		return nil, errors.New("rclone: is not supported: rclone takes its remotes from a config file, which no backup unit has")
	case "azure":
		return nil, errors.New("azure: is not a backend Debian's restic has")
	case "gs", "swift", "rest":
		return nil, fmt.Errorf("rest:, gs: and swift: are %w: write %s by hand (see the README)", errByHand, envFile)
	case "s3":
		if rest == "" {
			return nil, fmt.Errorf("%s: names no bucket", scheme)
		}
		// Any @ at all, not only one in the authority: a secret may hold a
		// /, ? or # that ends the authority before its @ (restic then fails
		// to parse the URL, printing it whole), and no bucket name holds
		// an @ — a path that does is refused with them, and said to be.
		if strings.Contains(repo, "@") {
			return nil, errors.New("the repository URL holds an @, which is refused: a key and secret in it are on the command line and in the shell's history, so give the URL without them and type them when asked; no bucket name holds an @, so a path with one is not supported")
		}
		// restic reads what follows s3: as a URL only where it begins
		// "http" in lower case (internal/backend/s3/config.go); in any
		// other case the scheme is a host of that name and there is no
		// bucket, which restic retries without end [measured, restic
		// 0.18.0: s3:HTTPS://… says "Bucket name cannot be empty"].
		if i := strings.Index(rest, "://"); i > 0 && isScheme(rest[:i]) && !strings.HasPrefix(rest, "http") && strings.HasPrefix(strings.ToLower(rest[:i]), "http") {
			return nil, fmt.Errorf("restic reads the scheme %s: only in lower case, and otherwise takes it for a host: give the URL as s3:%s://…", rest[:i], strings.ToLower(rest[:i]))
		}
		// B2's S3-compatible endpoint takes HTTPS alone [Backblaze's
		// documentation]: said here, before any prompt, not by the storage
		// after the key is typed.
		if urlScheme, authority := s3Split(repo); urlScheme == "http" && b2RegionOf(authority) != "" {
			return nil, errors.New("B2's S3-compatible endpoint takes HTTPS alone: give the URL as s3:https://…")
		}
		return []credential{{"Storage key id", "AWS_ACCESS_KEY_ID", false}, {"Storage secret key", "AWS_SECRET_ACCESS_KEY", true}}, nil
	case "b2":
		return nil, b2Refused(repo)
	}
	return nil, fmt.Errorf("%q is not a repository restic knows (setup asks for s3:)", scheme+":")
}

// s3Authority is an s3: repository URL's authority — the host, with
// any userinfo and port as written — where restic finds its host
// (internal/backend/s3/config.go): restic hands anything after "s3:"
// that begins "http" to url.Parse, which reads a scheme in any case
// (httpS: is https:), so there the authority follows the scheme's
// "://", up to where url.Parse ends one (a /, ? or #); after s3:// or
// s3: otherwise, up to the first slash. Never at a "://" further on, in
// the path. Not url.Parse's Host, which drops a "user:pass@" that is
// still on the command line and in the shell's history.
func s3Authority(repo string) string {
	_, authority := s3Split(repo)
	return authority
}

// s3Split is s3Authority's reading, with the URL's scheme in lower case
// as url.Parse gives it ("" for the forms with none).
func s3Split(repo string) (scheme, authority string) {
	rest := strings.TrimPrefix(repo, "s3:")
	end := "/"
	switch {
	case strings.HasPrefix(rest, "http"):
		if i := strings.Index(rest, ":"); i >= 0 && isScheme(rest[:i]) && strings.HasPrefix(rest[i:], "://") {
			scheme = strings.ToLower(rest[:i])
			rest, end = rest[i+len("://"):], "/?#"
		}
	case strings.HasPrefix(rest, "//"):
		rest = rest[len("//"):]
	}
	if i := strings.IndexAny(rest, end); i >= 0 {
		rest = rest[:i]
	}
	return scheme, rest
}

// isScheme is whether s is a URL scheme as url.Parse reads one: a
// letter, then letters, digits, +, - or . (RFC 3986, 3.1).
func isScheme(s string) bool {
	for i, c := range s {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// b2Why is why a b2: repository is refused, at setup and at every run:
// restic's own B2 backend — the blazer library, 0.6.1 in Debian 13's
// restic — retries an upload whose connection fails on the same upload
// URL, without end and without a word (#177: a run's lock file, 27 h).
// B2 is reached through its S3-compatible endpoint instead (#354),
// where minio-go's retries of a request are counted and restic's own
// end after a quarter of an hour (restic 0.18.0, minio-go 7.0.89).
const b2Why = "restic's own B2 backend retries a failed upload without end"

// b2Refused is setup's word on a b2: repository, which status as root
// says of a file that names one: the URL of the same repository at B2's
// S3-compatible endpoint, with the region left for the operator — it is
// in the bucket's endpoint, which a b2: URL does not hold.
func b2Refused(repo string) error {
	return fmt.Errorf("b2: is not supported: B2 is reached at its S3-compatible endpoint, %s, with an application key, not the master key; %s", b2AsS3(repo), b2Why)
}

// b2RunRefused is a run's, a restore's and a drill's word on a file that
// names a b2: repository: what to run, first, so that the record's cut
// (record.Text, 300 runes) takes the reason before the remedy — short,
// so that the cut takes neither for a bucket and path of up to 58
// characters as escaped (b2AsS3), the slash between them counted. The URL is in single quotes, so that the command pasted as
// it is reaches setup whole: the placeholder's < and >, and a $, & or ;
// the path may hold, are the shell's otherwise; b2AsS3 leaves no ' in it
// to end the quotes.
func b2RunRefused(repo string) error {
	return fmt.Errorf("the repository is b2:, which is not supported: as root, motel-backup setup --existing '%s' moves it to B2's S3 endpoint, with an application key; %s", b2AsS3(repo), b2Why)
}

// b2AsS3 is a b2: repository's URL at B2's S3-compatible endpoint: the
// bucket and the path as restic's b2 backend reads them
// (internal/backend/b2/config.go: the bucket up to the first colon,
// the path cleaned and its leading slash dropped), under which both of
// restic's backends lay the repository out alike, so the one URL is the
// other's objects. What is not there is a placeholder; the region always
// is. A bucket is one B2 can name or a placeholder, and each path segment
// is escaped, a ' among what is (b2RunRefused quotes the URL in '…').
// Cleaned, since it is said whole.
func b2AsS3(repo string) string {
	bucket, prefix, _ := strings.Cut(strings.TrimPrefix(repo, "b2:"), ":")
	if !b2BucketName.MatchString(bucket) {
		// None, or one no B2 bucket is named: escaped, a / in it would be
		// read back as the bucket's end, so it is named no URL.
		bucket = "<bucket>"
	}
	u := "s3:https://s3.<region>.backblazeb2.com/" + bucket
	if prefix != "" {
		prefix = strings.TrimPrefix(path.Clean(prefix), "/")
		switch {
		case prefix == ".." || strings.HasPrefix(prefix, "../"):
			// A path that climbs, which path.Clean leaves: B2 takes it as
			// written, but a server or a proxy on the way may resolve the
			// dot segments of an S3 request's path, so no URL is named that
			// may reach other objects.
			u += "/<path>"
		case prefix != "" && prefix != ".":
			// Each segment escaped: B2 reads the path as written, and restic
			// reads an s3:https URL's path as url.Parse decodes it
			// (internal/backend/s3/config.go), where a # or a ? would end the
			// path and a %2F would be a slash — another repository's objects.
			segments := strings.Split(prefix, "/")
			for i, s := range segments {
				segments[i] = s3Escape(s)
			}
			u += "/" + strings.Join(segments, "/")
		}
	}
	return record.Clean(u)
}

// b2BucketName is a B2 bucket name its S3-compatible endpoint takes:
// letters, digits and hyphens [Backblaze's documentation,
// b2_create_bucket], in lower case, as an S3 bucket is named — one with
// a capital B2's native API may hold, but the S3 endpoint may not reach
// it by that name, and whether it matches it in another case is not
// measured, so it is named no URL.
var b2BucketName = regexp.MustCompile(`^[a-z0-9-]+$`)

// s3Escape is one segment of an S3 URL's path, escaped so that restic's
// url.Parse decodes it back (url.PathEscape), with its @ escaped too,
// which setup refuses anywhere in an s3: URL.
func s3Escape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "@", "%40")
}

// b2Host is the host of Backblaze B2's S3-compatible endpoint,
// s3.<region>.backblazeb2.com, its second label the region [Backblaze's
// documentation], matched in lower case (b2RegionOf lowers an ASCII
// host first: DNS and SigV4's signed host read no case), after any port
// and trailing dot are taken off.
var b2Host = regexp.MustCompile(`^s3\.([a-z]{2}-[a-z]+-[0-9]{3})\.backblazeb2\.com$`)

// b2RegionOf is the region in an authority that is B2's S3-compatible
// endpoint, or "", read past a port (an empty one too) and a trailing
// dot.
func b2RegionOf(authority string) string {
	for i := 0; i < len(authority); i++ {
		if authority[i] >= utf8.RuneSelf {
			return "" // no B2 host; and a fold of ſ or K into [a-z] is never read as one
		}
	}
	host := strings.ToLower(authority)
	// An empty port too, which url.Parse takes for the scheme's own.
	if i := strings.LastIndexByte(host, ':'); i >= 0 && (i == len(host)-1 || isDigits(host[i+1:])) {
		host = host[:i]
	}
	host = strings.TrimSuffix(host, ".")
	if m := b2Host.FindStringSubmatch(host); m != nil {
		return m[1]
	}
	return ""
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// RegionAdvice is what status as root says of a credential file whose
// repository is at B2's S3-compatible endpoint and which sets no
// AWS_DEFAULT_REGION, or another than the endpoint's name holds — one
// written by hand — or "": what setup would have written (b2Region).
// Nothing of a URL setup refuses, which status has said instead.
func RegionAdvice(repo, region string) string {
	if RepositoryUsable(repo, "") != nil {
		return ""
	}
	r := b2Region(repo)
	switch {
	case r == "":
		return ""
	case region == "":
		return "the repository is at B2's S3-compatible endpoint and AWS_DEFAULT_REGION is not set, or set to nothing: add AWS_DEFAULT_REGION=" + r + ", as setup writes it"
	case region != r:
		return "the repository is at B2's S3-compatible endpoint in " + r + ", and AWS_DEFAULT_REGION is " + record.Text(region) + ": set AWS_DEFAULT_REGION=" + r + ", as setup writes it"
	}
	return ""
}

// b2Region is the region of a repository at B2's S3-compatible
// endpoint, read from the host as restic reads the host from the URL
// (internal/backend/s3/config.go), or "" for any other repository.
// Setup writes it into the file as AWS_DEFAULT_REGION. Without it,
// minio-go first asks the storage where the bucket is, a request B2
// grants only to a key holding readBuckets, which the guide's keys do
// not, and on its refusal signs every request for us-east-1
// (minio-go 7.0.89, bucket-cache.go); and that one request is not
// retried, so a 500 on it puts restic's retry lines on stderr beside a
// listing, which is then not believed (besides).
func b2Region(repo string) string {
	if !strings.HasPrefix(repo, "s3:") {
		return ""
	}
	// Userinfo matches no B2 host, and setup refuses it before this. The
	// region is the name's, read past a port or a trailing dot: whether B2
	// takes the host so written is another question, not measured.
	return b2RegionOf(s3Authority(repo))
}

// ReadRegular reads a file that is a regular one, following a link as
// the manager does, and opened O_NONBLOCK so that a FIFO is refused, not
// waited on: how a run reads the credential file, and status as root.
// What it returns beside the bytes is the open file's own stat, so that
// a caller asks the file it read, not the name again.
func ReadRegular(name string) ([]byte, fs.FileInfo, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // root's own file, a constant of the installation
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("it is not a regular file (%s)", fi.Mode().Type())
	}
	raw, err := io.ReadAll(f)
	return raw, fi, err
}

// credentialOwner is the uid the credential file must belong to: root,
// or the account under test.
var credentialOwner = uint32(0)

// CredentialRootsAlone says why the credential file at name, as fi
// (its open descriptor's stat) describes it, is not root's alone, or
// nil: that it belongs to root and nobody else may read, write or run
// it — what ssh asks of a private key — and that its directory is
// root's and writable by nobody else, whose writer could put another
// file in its place. The file holds the repository's password and the
// storage key, and the motel uid must never read or replace them. Said
// with the command that puts it right, as root; setup makes both so.
func CredentialRootsAlone(name string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the credential file %s: whose it is could not be told", name)
	}
	switch {
	case st.Uid != credentialOwner:
		return fmt.Errorf("the credential file %s belongs to uid %d, not root, and it holds the repository's password and the storage key: chown root:root %s && chmod 0600 %s, as root", name, st.Uid, name, name)
	case st.Mode&0o077 != 0:
		return fmt.Errorf("the credential file %s is open to others than root (mode %04o), and it holds the repository's password and the storage key: chmod 0600 %s, as root", name, st.Mode&0o7777, name)
	}
	dir := filepath.Dir(name)
	var d unix.Stat_t
	if err := unix.Stat(dir, &d); err != nil {
		return fmt.Errorf("the credential file's directory %s could not be looked at: %w", dir, err)
	}
	if d.Uid != credentialOwner || d.Mode&0o022 != 0 {
		return fmt.Errorf("the credential file's directory %s is uid %d's, mode %04o: it has to be root's and writable by nobody else, or someone else could put another file in the credential file's place: chown root:root %s && chmod 0755 %s, as root", dir, d.Uid, d.Mode&0o7777, dir, dir)
	}
	return nil
}

// repositoryInUse is what a run, a restore and a drill ask of the
// credential file and the repository it names, before any unit: that
// the file is root's alone, and that the repository is not a b2: one.
// The value is read as the manager reads it (envfile: the last
// assignment, quotes and whitespace as systemd takes them), and as
// restic tells a backend by its URL: the scheme, as written. A file the
// manager could not read either is said here, in place of every unit
// failing to start. Opened without blocking and read only if it is a
// regular file: a FIFO in its place would otherwise hold the run, and
// its lock, for ever.
func repositoryInUse(envFile string) error {
	raw, fi, err := ReadRegular(envFile)
	if err != nil {
		return fmt.Errorf("the credential file %s could not be read: %w", envFile, err)
	}
	if err := CredentialRootsAlone(envFile, fi); err != nil {
		return err
	}
	v, findings := envfile.Parse(raw)
	if why := envfile.RefusedWhole(findings); why != "" {
		// The manager refuses the whole file, and every unit with it.
		return fmt.Errorf("the credential file %s: %s", envFile, why)
	}
	// restic also takes the repository from a file named by
	// RESTIC_REPOSITORY_FILE, which this does not read: a repository
	// named there is one no refusal here could see.
	if v["RESTIC_REPOSITORY_FILE"] != "" {
		return fmt.Errorf("the credential file %s names the repository in RESTIC_REPOSITORY_FILE, which is not supported: name it in RESTIC_REPOSITORY", envFile)
	}
	if repo := v["RESTIC_REPOSITORY"]; strings.HasPrefix(repo, "b2:") {
		return b2RunRefused(repo)
	}
	return nil
}

// RepositoryUsable says why the box could not use the repository the
// credential file names, or nil: what status says of a hand-edited
// file. A backend setup does not ask for, the run still uses.
func RepositoryUsable(repo, envFile string) error {
	if err := repositoryValue(repo); err != nil {
		return err
	}
	_, err := backendOf(repo, envFile)
	if errors.Is(err, errByHand) {
		return nil
	}
	return err
}

// repositoryValue is what setup refuses of the URL as a value, before
// it looks at it as a URL: what a prompt would refuse of any value,
// and whitespace at an end, which the manager trims of a plain value
// and keeps of a quoted one.
func repositoryValue(repo string) error {
	if strings.TrimSpace(repo) != repo {
		return errors.New("the repository URL has whitespace at an end")
	}
	if err := envfile.Refuse(repo); err != nil {
		return fmt.Errorf("the repository URL cannot go in the file: %w", err)
	}
	return nil
}

// OldEnvFileNote is what a run and status add when the credential file
// is missing and one from an earlier version of this branch is there:
// it is not read, and setup is what to run. Empty otherwise.
func OldEnvFileNote(cfg Config) string {
	if _, err := os.Lstat(cfg.OldEnvFile); cfg.OldEnvFile == "" || err != nil {
		return ""
	}
	return fmt.Sprintf(" (%s is from before this version and is not read: run `motel-backup setup --existing <its RESTIC_REPOSITORY>` as root, which asks for its RESTIC_PASSWORD; then remove it)", cfg.OldEnvFile)
}

// setup is one setup under way: the run whose lock it holds, the
// operator, the repository, the file being written beside the working
// one, and what has become of the password shown, where one was.
type setup struct {
	*run
	term Terminal
	repo string
	// existing is SetupOptions.Existing: a repository is there, on the
	// operator's word, and no password is made.
	existing bool
	// staged is the file beside the working one, which the units here
	// read, and which takes the working one's place at the end.
	staged string
	// password is the one made for a new repository: shown once, and
	// kept through a key typed wrong — it has taken effect nowhere
	// until init has made the repository with it. Empty until shown.
	password string
	// initRan is whether a restic init started with password may have
	// written something: from then on no failure proves it unused, and
	// it is never said to be. An init that restic says never opened the
	// storage, or that found files there and wrote nothing, leaves it as
	// it was. made is more: the repository has that password for
	// certain — init made it, or it opened one an earlier init made.
	initRan, made bool
	// writing is set as the opening starts — it takes the repository's
	// lock, so an interrupt from then on may leave restic's lock file,
	// and says so — and cleared where the opening was not opened and
	// wrote nothing (notOpened: exit 12, or 10 after init).
	writing bool
	// initLeft is whether init was left running — by its clock or an
	// interrupt — and so still has the staged file and the run
	// directory to read: the manager opens both in the unit's own first
	// moments, which the end of this command may come before. Both are
	// then left for it, and swept by the next lock holder after its wait.
	initLeft bool
}

// Setup makes the box ready to back up into one repository: the
// account, the directories, and the credential file — written whole and
// put in place only once the repository has answered. Everything that
// can be known to fail is checked before anyone is asked for a secret;
// the password of a new repository is shown, and confirmed stored,
// before it takes effect anywhere.
func Setup(ctx context.Context, cfg Config, r Runner, o SetupOptions) (*SetupReport, error) {
	term := o.Terminal
	creds, err := backendOf(o.Repository, cfg.EnvFile)
	if err != nil {
		return nil, err
	}
	// The one value not typed at a prompt: refused here for what a
	// prompt would refuse, before anyone is asked for anything.
	if err := repositoryValue(o.Repository); err != nil {
		return nil, err
	}
	if err := programsInstalled(cfg); err != nil {
		return nil, err
	}
	if v, err := r.ManagerVersion(ctx); err != nil {
		return nil, err
	} else if v < 257 {
		return nil, fmt.Errorf("systemd 257 or later is needed (Debian 13's); this box has %d", v)
	}
	rep := &SetupReport{Account: "present"}
	acct, err := account(ctx, backupUser)
	if err != nil {
		return nil, err
	}
	if acct.exists {
		if err := accountUsable(ctx, acct); err != nil {
			return nil, err
		}
	} else {
		if err := makeAccount(); err != nil {
			return nil, fmt.Errorf("making the %s account: %w", backupUser, err)
		}
		rep.Account = "made"
	}
	term.Say("account " + backupUser + ": " + rep.Account)

	x, end, err := open(ctx, cfg, r, term.Say)
	if x == nil {
		return nil, err
	}
	defer end()
	s := &setup{run: x, term: term, repo: o.Repository, existing: o.Existing, staged: envfile.Staged(cfg.EnvFile)}
	if err != nil {
		return nil, s.err(err)
	}
	// Knowable before anyone is asked for a secret: every run dumps and
	// cleans as the data user.
	if x.dataErr != nil {
		return nil, s.err(x.dataErr)
	}
	dir := filepath.Dir(cfg.EnvFile)
	// Anyone may see that the file is there and when it was written —
	// that is how status tells a fresh setup from one that never ran —
	// and nobody but root what is in it: root's own directory, whoever
	// made it before (a directory's writer may rename or remove what is
	// in it), and not a link, which would put the file somewhere else.
	if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a link, and the credential file has to be in a directory of root's own", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // holds one root-only file
		return nil, err
	}
	if err := ownByRoot(dir); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // said again past the caller's umask
		return nil, err
	}
	// What a setup that did not live to remove them left was swept by
	// open, after the wait for its init, and said.
	rep.Swept = x.swept
	// Whether there was a file before: with none, no run could have
	// written the record against this repository. What it names is not
	// read — the record's repository is known by its id, below.
	hadOld := false
	if _, err := os.Lstat(cfg.EnvFile); err == nil {
		hadOld = true
	}

	p, err := x.planWith(ctx, s.errFile("plan"))
	if err != nil {
		return nil, s.err(err)
	}
	rep.Apps = p.Names()
	if len(rep.Apps) == 0 {
		term.Say("no app declares a backup yet: a run would back nothing up")
	} else {
		term.Say(fmt.Sprintf("a run would back up %s, under %s", record.Clean(strings.Join(rep.Apps, ", ")), record.Text(p.Root)))
	}

	// The file is written beside the working one, staged under a name
	// the manager reads for the units here, and takes the working one's
	// place only once the repository has answered. Whatever ends this
	// command before that, the working file is as it was and the staged
	// file goes with it — unless init was left running to read it, in
	// which case the next setup removes it. Until a password is made it
	// holds a throwaway one: what the look for a repository needs, and
	// nothing that takes effect anywhere.
	defer func() {
		if !s.initLeft {
			os.Remove(s.staged) //nolint:errcheck,gosec // gone already once it was put in place
		}
	}()
	throwaway, err := newPassword()
	if err != nil {
		return nil, err
	}
	exists := false
	// A repository that is there has a password of its own, which is
	// asked for below; one shown here is not it, and init, where it
	// ran, made nothing with it.
	needsOwn := func() {
		exists, s.initRan = true, false
		msg := "the repository exists; its password is needed"
		if s.password != "" {
			msg += " (the one shown above is not it)"
		}
		term.Say(msg)
	}
	pairs := []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: o.Repository}, {Key: "RESTIC_PASSWORD"}}
	// B2's region is in its endpoint's name, and is not asked: written
	// with the rest, so that setup's own units read it too (b2Region).
	if region := b2Region(o.Repository); region != "" {
		pairs = append(pairs, envfile.Pair{Key: "AWS_DEFAULT_REGION", Value: region})
		term.Say("B2's region, " + region + ", from the endpoint's name, goes in the file as AWS_DEFAULT_REGION")
	}
	fixed := len(pairs)
	for attempt := 1; ; attempt++ {
		// Whether an earlier attempt's init ran with the shown password:
		// a repository found there, by the look or by this attempt's
		// init, may be one that init made.
		ranBefore := s.initRan
		pairs = pairs[:fixed]
		for _, c := range creds {
			v, err := s.ask(ctx, c.label+" ("+c.variable+"): ", c.secret)
			if err != nil {
				return nil, s.err(err)
			}
			pairs = append(pairs, envfile.Pair{Key: c.variable, Value: v})
		}
		pairs[1].Value = throwaway
		if err := writeEnv(s.staged, pairs); err != nil {
			return nil, s.err(err)
		}
		// Before any password is made: is there a repository already?
		// With a right key restic says at once — none (10), or one this
		// password does not open (12) — and a rebuilt box is then asked
		// for the password it has, not shown one it does not need. A
		// storage that refuses at once says why, and the key is asked
		// for again. Where the look cannot tell, init answers, and says
		// why.
		// With --existing the look has setup's longer clock: the
		// operator's word is that a repository is there, so a storage
		// slow to answer [RC check 7, B2: once in four looks] is waited
		// for, and the question is not init's. A clock met all the same
		// — a bucket not there, a wrong key, a wrong host, which restic
		// retries alike, or a storage slow for two minutes — is said,
		// and the key asked for again as for one refused at once; the
		// third ends setup with nothing written: the look only reads,
		// and no password was made or asked for.
		within := setupProbeClock
		if s.existing {
			within = setupClock
		}
		term.Say(fmt.Sprintf("looking for a repository at %s (up to %s)", o.Repository, within))
		found, message, err := s.look(ctx, within)
		if err != nil {
			return nil, s.err(err)
		}
		// Two answers have the key asked for again, up to three times in
		// all, init's refusals counted with these. Refused at once:
		// restic gave the look up and said why — the storage refused the
		// key, or could not be reached — and init, whose first step is
		// the same opening, would say the same; no password is made on a
		// key the storage has not taken. The clock met with --existing
		// (or an exit 0 that says nothing): the three look alike here,
		// and the key is one of them. The third ending says, with the
		// flag, what to check and that nothing was written.
		var retried error
		again := s.again()
		switch {
		case found == lookRefused:
			retried = errors.New("restic could not open the repository (exit 1): " + record.Text(message))
		case found == lookUnsure && s.existing:
			retried, again = s.clockMet(within, message), againAfterClock
		}
		if retried != nil {
			if attempt >= 3 {
				if s.existing {
					retried = s.wayOut(retried)
				}
				return nil, s.err(retried)
			}
			term.Say(retried.Error())
			term.Say(again)
			continue
		}
		unsure := false
		switch {
		case found == lookFound && ranBefore:
			// A repository is there, and an earlier attempt's init ran
			// with the password shown: the throwaway not opening it says
			// nothing of that one. Tried first — a repository it opens is
			// one this setup made.
			pairs[1].Value = s.password
			if err := writeEnv(s.staged, pairs); err != nil {
				return nil, s.err(err)
			}
			if err := s.tryShown(ctx, rep, needsOwn); err != nil {
				return nil, s.err(err)
			}
		case found == lookFound:
			needsOwn()
		case found == lookNone && s.existing:
			// The storage took the key and says there is none: not the
			// repository the operator meant, and nothing to make.
			return nil, s.err(s.noneForExisting())
		case found == lookNone:
			// None: one is made below.
		case s.password != "":
			// The clock ended the look, restic still retrying: init
			// answers at once, and says which.
			term.Say(fmt.Sprintf("no repository answered within %s: "+lookAlike+"; a new repository will be made, and the password shown above still applies%s", within, resticSaid(message)))
		default:
			unsure = true
			term.Say(fmt.Sprintf("no repository answered within %s: "+lookAlike+"; a new repository will be made with the password shown next%s", within, resticSaid(message)))
		}
		if exists || rep.New {
			break
		}
		if s.password == "" {
			if s.password, err = newPassword(); err != nil {
				return nil, err
			}
			term.Say("Repository password (new): " + s.password)
			keep := "the repository URL, " + o.Repository + ", and the storage key: with those three"
			if r := b2Region(o.Repository); r != "" {
				// What restic needs beside them there: the guide's keys may not
				// ask B2 where the bucket is (b2Region).
				keep = "the repository URL, " + o.Repository + ", the storage key, and B2's region, " + r + ", set as AWS_DEFAULT_REGION wherever restic runs: with those four"
			}
			term.Say("Store it off the box now, with " + keep + ", `restic -r <url>` reads every backup from any machine; without the password nothing can.")
			if unsure {
				// A repository may be there after all — a rebuilt box's, on a
				// storage slow to answer [RC check 7, B2: once in four looks]
				// — and its password, in the store this one goes to, is then
				// the only way to read it: an entry for the URL updated with
				// this one would lose it.
				term.Say("The look could not tell whether a repository is there: store this password beside any you already keep for this repository, never in its place; if one is there, setup says so next and asks for its own.")
			}
			if err := s.confirmStored(ctx); err != nil {
				return nil, s.err(err)
			}
		}
		pairs[1].Value = s.password
		if err := writeEnv(s.staged, pairs); err != nil {
			return nil, s.err(err)
		}
		s.initRan = true
		o1, message, err := s.makeRepository(ctx)
		if err != nil {
			return nil, s.err(err)
		}
		if o1.OK() {
			// Exit 0 is a repository made, with the password that was
			// shown: whatever restic did not say, that password is now
			// the repository's.
			s.made = true
			if rep.RepositoryID = initializedID(s.outFile("init")); rep.RepositoryID == "" {
				return nil, s.err(errors.New("restic made the repository and exited 0 but said nothing of its id; run setup again, which opens it"))
			}
			rep.New = true
			// Init proved the key may write, and deleted nothing: the
			// repository just made is opened with its lock once, so that
			// restic's own word on a lock file it could not remove
			// refuses a key that may write and not delete here as on a
			// rebuilt box — the first setup is where such a key is most
			// often first used.
			if _, opened, err := s.openRepository(ctx); err != nil {
				return nil, s.err(err)
			} else if !opened {
				return nil, s.err(errors.New("the repository restic just made does not open yet: the storage may not show all of it for a moment; run setup again, which looks first and asks for the password"))
			}
			break
		}
		if o1.Result == "exit-code" && o1.ExitStatus == 1 && repositoryExists(message) {
			// The look could not tell, and init can: the repository has a
			// password already. This init made nothing with the one
			// shown; an earlier attempt's may have, and is tried first,
			// as above. Otherwise it is unused again, whatever ends the
			// opening that follows.
			if ranBefore {
				if err := s.tryShown(ctx, rep, needsOwn); err != nil {
					return nil, s.err(err)
				}
			} else {
				needsOwn()
			}
			break
		}
		var failure error
		if o1.Result == "exit-code" && o1.ExitStatus == 1 {
			failure = errors.New("restic could not make or open the repository (exit 1): " + record.Text(message))
			if repositoryHalfThere(message) {
				// Files of a repository and no config: an init that ended
				// between its two writes — an earlier attempt's of this
				// setup, maybe — or a rule that removed the config.
				// Nothing opens them, with the password shown or any
				// other, and init will not write over them: there is no
				// way through but the storage itself, so setup ends here,
				// says so, and the password is dead whatever was written
				// with it.
				s.initRan = false
				// Recovery comes before removal: a repository that lost its
				// config may be whole in the storage's kept versions (the
				// guide, "After an attack"), and nothing here can tell
				// one from an init's leftovers.
				return nil, s.err(fmt.Errorf("%w; the storage holds files of a restic repository and no config — a repository whose config was removed, or an init that did not finish — which nothing opens and init will not write over. If a backup was ever made there, recover its earlier state from the storage's kept versions first (the guide, \"After an attack\") and remove nothing; only an unfinished repository that never held one is removed at the storage. Or give setup another path; then run setup again", failure))
			}
			if backendRefused(message) {
				// Refused at the door: init never opened the storage, so
				// nothing was written with the password, and this
				// attempt says nothing of an earlier one's.
				s.initRan = ranBefore
				if noBucket(message, o.Repository) {
					// No bucket by that name, and a key that may not make
					// one — the guide's keys never may: the key asked for
					// again is refused the same, so setup ends here, naming
					// the bucket rather than the key.
					return nil, s.err(fmt.Errorf("%w; the storage has no bucket by that name for this key, and the key may not make one: check the bucket's name in the URL, or make the bucket first (the guide, \"Before you start\"); then run setup again", failure))
				}
			}
			// What the storage says of the key, the host or the bucket is
			// a mistake in what was typed, as often as not: the key is
			// asked for again. The password stands: it has taken effect
			// nowhere.
			if attempt < 3 {
				term.Say(failure.Error())
				term.Say(s.again())
				continue
			}
		} else {
			detail, _ := resticFailure(o1)
			failure = errors.New(detail)
		}
		return nil, s.err(failure)
	}
	if exists {
		for attempt := 1; ; attempt++ {
			own, err := s.ask(ctx, "Repository password: ", true)
			if err != nil {
				return nil, s.err(err)
			}
			pairs[1].Value = own
			if err := writeEnv(s.staged, pairs); err != nil {
				return nil, s.err(err)
			}
			id, opened, err := s.openRepository(ctx)
			if err != nil {
				return nil, s.err(err)
			}
			if opened {
				rep.RepositoryID = id
				break
			}
			if attempt < 3 {
				term.Say("this password cannot open the repository (exit 12): again")
				continue
			}
			return nil, s.err(errors.New("this password cannot open the repository (exit 12)"))
		}
	}
	// The record speaks of the repository it was written against,
	// known by its id: the one setup keeps beside it (`repository-id`)
	// for the file it puts in place. A URL says only where — a bucket
	// emptied and made again is another repository at the same one,
	// and one reached by another URL is the same. With another
	// repository now in use — or none known, or one this setup made —
	// the record is put aside, so that the next run drills what it
	// backs up into the repository it is now using. Aside first, then
	// the file: whatever ends setup between the two leaves no credential
	// file with a record of another repository beside it (a record aside
	// with the old file in place is a fresh start, which the next run
	// mends); a file that could not be put in place gets its record
	// back.
	statusPath := filepath.Join(cfg.StateDir, "status.json")
	idFile := filepath.Join(cfg.StateDir, "repository-id")
	prevID := ""
	if raw, err := os.ReadFile(idFile); err == nil { //nolint:gosec // root's own file under the state directory
		prevID = strings.TrimSpace(string(raw))
	}
	why := ""
	if _, err := os.Lstat(statusPath); err == nil {
		switch {
		case rep.New:
			why = "the repository was made by this setup, so it holds none of the record's snapshots"
		case !hadOld:
			why = "no credential file was there to tie it to this repository"
		case prevID == "":
			why = "no setup recorded which repository it was written against"
		case prevID != rep.RepositoryID:
			why = fmt.Sprintf("it was written against another repository (id %s)", record.Short(prevID))
		}
	}
	// On the disk before the file is — the two live in different
	// directories, and a power cut must not keep the new file and lose
	// the aside — and so is a put-back. Whatever fails, the two files
	// are left agreeing: the record comes back while the old file is
	// still in place, and stays aside once the new one is.
	putBack := func(cause error) error {
		if back := os.Rename(rep.Aside, statusPath); back != nil {
			return fmt.Errorf("%w; and the record, put aside first, could not be put back from %s: %w", cause, rep.Aside, back)
		}
		rep.Aside = ""
		if sync := syncDir(cfg.StateDir); sync != nil {
			return fmt.Errorf("%w; and the record, put back, could not be synced: %w", cause, sync)
		}
		return cause
	}
	if why != "" {
		// Named by the time and this setup's nonce, so that no aside
		// replaces another: two in one second, or a clock set back.
		rep.Aside = statusPath + ".aside-" + time.Now().UTC().Format("20060102T150405Z") + "-" + s.nonce
		if err := os.Rename(statusPath, rep.Aside); err != nil {
			return nil, s.err(err)
		}
		if err := syncDir(cfg.StateDir); err != nil {
			return nil, s.err(putBack(err))
		}
	}
	if err := commit(s.staged, cfg.EnvFile); err != nil {
		if why != "" {
			return nil, s.err(putBack(err))
		}
		return nil, s.err(err)
	}
	// The file on the disk first, then the id: a power cut must never
	// leave the new id beside the old file, or a record the next run
	// writes against the old repository would pass for the new one's
	// — the old id beside the new file, at worst, which puts the record
	// aside once more, a fresh start the next run mends. From here the
	// file is in place for every reader, and stays; what fails after is
	// said with the record's whereabouts, and the password's fate from
	// what setup knows.
	inPlace := "the credential file is in place"
	if rep.Aside != "" {
		inPlace += "; the record was put aside: " + rep.Aside
	}
	if err := syncDir(dir); err != nil {
		return nil, s.err(fmt.Errorf("%s; its directory could not be synced to the disk, and the repository's id was not recorded beside the record, so the next setup puts the record aside: %w", inPlace, err))
	}
	if err := writeID(idFile, rep.RepositoryID); err != nil {
		return nil, s.err(fmt.Errorf("%s; the repository's id could not be kept beside the record, so the next setup puts the record aside: %w", inPlace, err))
	}
	if err := syncDir(cfg.StateDir); err != nil {
		return nil, s.err(fmt.Errorf("%s; the state directory could not be synced to the disk: %w", inPlace, err))
	}
	if why != "" {
		term.Say(fmt.Sprintf("the record was put aside (%s): %s; the next run drills what it backs up", why, rep.Aside))
	}
	kind := "existing"
	if rep.New {
		kind = "new"
	}
	term.Say(fmt.Sprintf("repository ready: %s (%s, id %s)", o.Repository, kind, record.Short(rep.RepositoryID)))
	if s.password != "" && !rep.New {
		term.Say("the password shown above was never used: discard it")
	}
	// The file from before this version, if it is still there, holds a
	// copy of the credential: said, not removed — it is root's, and an
	// operator may want its contents once more.
	if _, err := os.Lstat(cfg.OldEnvFile); cfg.OldEnvFile != "" && err == nil {
		rep.OldEnvFile = cfg.OldEnvFile
		term.Say(fmt.Sprintf("%s is still there, from before this version, and is not read; it holds a copy of the credential: remove it (rm %s)", cfg.OldEnvFile, cfg.OldEnvFile))
	}
	return rep, nil
}

// leaveInit is init left running, whichever way: the staged file it
// reads and the run directory it writes to stay, since the manager
// opens both in the unit's own first moments, which this end may come
// before. The next lock holder waits for it, then sweeps both.
func (s *setup) leaveInit() { s.initLeft, s.keepDir = true, true }

// tryShown opens the repository with the password shown — the staged
// file holds it — after an earlier attempt's init ran with it: one it
// opens is one this setup made; else the repository's own is needed.
// The key is not the one that made it — an attempt comes round only
// when the key is asked for again — so the opening holds it to
// writing as any key to a repository that was there already. A
// refusal there, once the opening has proven the password, says the
// password is the repository's; a Ctrl-C during the opening says what
// initRan says of it — init ran with it, and may have made the
// repository — since nothing yet has proven it
// (TestAnInterruptDuringTheOpeningNamesTheLockItMayLeave pins it).
func (s *setup) tryShown(ctx context.Context, rep *SetupReport, needsOwn func()) error {
	id, opened, err := s.openRepository(ctx)
	if opened {
		// Made, whatever the opening then refused: the password is the
		// repository's, and a refusal's fate says so.
		s.term.Say("the repository exists, and the password shown above opens it: it was made by an earlier attempt of this setup")
		rep.RepositoryID, rep.New, s.made = id, true, true
		return err
	}
	if err != nil {
		return err
	}
	needsOwn()
	return nil
}

// confirmStored has the operator type "stored" — a word that means
// something, not "y" — with one more asking for a word that is not
// it; anything then is a no.
func (s *setup) confirmStored(ctx context.Context) error {
	for i := 0; i < 2; i++ {
		said, err := s.term.Ask(ctx, "Type stored to go on: ", false)
		if err != nil {
			return fmt.Errorf("nobody answered: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(said), "stored") {
			return nil
		}
		if i == 0 {
			s.term.Say("that is not stored: type stored to go on, or anything else to stop")
		}
	}
	return errors.New("the password was not confirmed stored: nothing was written")
}

// err is an error ending setup, in the operator's words: an interrupt
// is said as one, and what became of the password shown, where one
// was, is said with it — nothing here returns an error after the
// showing but through this.
func (s *setup) err(err error) error {
	fate := ""
	switch {
	case s.password != "" && s.made:
		fate = "; the password shown above is the repository's: keep it"
	case s.password != "" && s.initRan:
		// Started, init may have written some or all of a repository
		// before it failed or was left running: nothing here proves the
		// password unused, so it is kept.
		fate = "; keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there"
	case s.password != "":
		fate = "; the password shown above was never used: discard it"
	}
	if errors.Is(err, context.Canceled) {
		if s.writing {
			return interrupted("interrupted: " + s.lockLeftLine() + fate)
		}
		return interrupted("interrupted: nothing has been written" + fate)
	}
	if fate != "" {
		return fmt.Errorf("%w%s", err, fate)
	}
	return err
}

// interrupted is context.Canceled in the operator's words: it is that
// error to errors.Is, and says nothing of contexts.
type interrupted string

func (e interrupted) Error() string      { return string(e) }
func (interrupted) Is(target error) bool { return target == context.Canceled }

// ask asks up to three times for a value the file can hold, and says
// each time why it cannot.
func (s *setup) ask(ctx context.Context, prompt string, secret bool) (string, error) {
	for i := 0; i < 3; i++ {
		v, err := s.term.Ask(ctx, prompt, secret)
		if err != nil {
			return "", fmt.Errorf("nobody answered: %w", err)
		}
		why := envfile.Refuse(v)
		if why == nil {
			return v, nil
		}
		s.term.Say(fmt.Sprintf("that cannot go in the file: %v; again", why))
	}
	return "", errors.New("no usable value was given in three times")
}

// commit puts the file in place — the rename alone; its directory is
// synced by the caller, which has to know which of the two failed —
// and writeEnv writes the file beside it. Variables so that a test
// can look at the moment each happens, and make it fail.
var (
	commit   = os.Rename
	writeEnv = envfile.Write
)

// ownByRoot makes a directory root's, and syncDir puts a directory's
// entries on the disk; variables so that the flow can be tested by an
// account that is not root.
var (
	ownByRoot = func(path string) error { return os.Lchown(path, 0, 0) }
	syncDir   = envfile.SyncDir
	// writeID keeps the repository's id beside the record: whole, on
	// the disk before it returns, and for everyone to read whatever the
	// caller's umask, as the record is — nothing a password guards.
	writeID = func(path, id string) error {
		tmp := path + ".new"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // an id, beside status.json
		if err != nil {
			return err
		}
		write := func() error {
			if _, err := f.WriteString(id + "\n"); err != nil {
				return err
			}
			if err := f.Chmod(0o644); err != nil {
				return err
			}
			return f.Sync()
		}
		if err := write(); err != nil {
			f.Close()      //nolint:errcheck,gosec // the write's error is the one
			os.Remove(tmp) //nolint:errcheck,gosec // as above
			return err
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp) //nolint:errcheck,gosec // the close's error is the one
			return err
		}
		return os.Rename(tmp, path)
	}
)

// lookResult is what the look for a repository found.
type lookResult int

const (
	lookUnsure  lookResult = iota // the clock, or exit 0 with nothing: init decides
	lookNone                      // no repository there (exit 10)
	lookFound                     // one there, which the throwaway does not open (exit 12)
	lookRefused                   // restic gave up at once and said why (exit 1)
)

// look asks whether a repository is there, with the throwaway password,
// under the clock given: the short one, or with --existing setup's
// long one. restic's exit 1 is the storage answering
// at once, in its own words on stderr, which are returned with it.
// Over S3, restic gives up at once only on an answer its backend holds
// permanent, a 403 (AccessDenied, what minio-go makes of a 403 with no
// body, a HEAD's) [restic 0.18.0, internal/backend/s3/s3.go
// IsPermanentError]; a wrong secret and a bucket not there are retried,
// and the clock ends the look [measured, on rclone's S3 server]. Only
// the clock, and an exit 0 that says nothing, leave the question to
// init, or with --existing have the key asked for again; any other
// failure is the unit's own, and ends setup before any password is
// made.
func (s *setup) look(ctx context.Context, within time.Duration) (lookResult, string, error) {
	o, message, err := s.repository(ctx, "probe", within, s.cfg.Restic, "cat", "config", "--no-lock")
	if err != nil {
		if errors.Is(err, errDidNotAnswer) {
			// Stopped at its clock, with what restic said of the storage
			// before the stop, for the operator, since nothing else
			// keeps it: the run directory goes with setup [RC check 7:
			// the slow look's words were not kept].
			return lookUnsure, s.lastSaid("probe", true), nil
		}
		return lookUnsure, "", err
	}
	switch {
	case o.Result == "exit-code" && o.ExitStatus == 12:
		return lookFound, "", nil
	case o.Result == "exit-code" && o.ExitStatus == 10:
		return lookNone, "", nil
	case o.Result == "exit-code" && o.ExitStatus == 1:
		if message == "" {
			message, _ = resticFailure(o)
		}
		return lookRefused, message, nil
	case o.OK():
		return lookUnsure, "", nil
	}
	detail, _ := resticFailure(o)
	return lookUnsure, "", errors.New("looking for the repository: " + detail)
}

// again is what the operator is told before the key is asked for once
// more: the password shown, where one was, stands through it.
func (s *setup) again() string {
	line := "the storage refused the key, or could not be reached: the key id and secret again"
	if s.password != "" {
		line += " (the password shown above still applies)"
	}
	return line
}

// lookAlike is what the look's clock cannot tell apart: restic retries
// all three [measured, M47], and only init says which; with --existing
// the look has setup's longer clock, and a clock met there has the key
// asked for again.
const lookAlike = "a bucket not made yet, a wrong key and a wrong host look alike here"

// clockMet is setup --existing's look given up on at setup's clock, or
// restic exiting 0 saying nothing: the three look alike here, and no
// password was made.
func (s *setup) clockMet(within time.Duration, message string) error {
	return fmt.Errorf("no repository answered within %s%s: "+lookAlike+"; with --existing no password is made", within, resticSaid(message))
}

// againAfterClock is what the operator is told before the key is asked
// for again after clockMet: not a refusal, so the key may be right and
// the storage slow, and the same key has the look run again.
const againAfterClock = "the key id and secret again: a wrong key is one of the three; the same key, if the storage was only slow to answer, has the look run again"

// wayOut is the ending of setup --existing's third refused or clocked
// look: what to check, and that nothing was written. The plain path is
// not offered: after a refusal, init's first step is the same opening
// and would be refused the same; after a clock, init on a bucket
// mistyped, with a key that may make one, makes a new repository
// there, with a password shown — the outcome the flag exists to keep a
// rebuilt box from.
func (s *setup) wayOut(err error) error {
	return fmt.Errorf("%w; check the URL and the key against the earlier box's, and run setup again; nothing of setup's has been written", err)
}

// noneForExisting is setup --existing meeting a storage that took the
// key and says there is no repository (exit 10 at the look): the URL
// names a bucket or a path the earlier box's
// repository is not at, or the repository is gone. On the operator's
// word that one is there, setup makes none, and ends here with the
// working file as it was.
func (s *setup) noneForExisting() error {
	return fmt.Errorf("there is no repository at %s (exit 10): setup --existing opens a repository from an earlier box and makes none; check the URL — the bucket and the path — against the earlier box's, or run setup without --existing to make a new repository there", s.repo)
}

// makeRepository runs restic init. This is the one unit here that is
// never stopped: stopped half way it leaves a key file and no config
// (restic 0.18.0 writes the key first, then the config), which nothing
// opens and a later init will not write over, where left to its few
// seconds it makes the repository with the password that was shown. Its
// clock, and an interrupt, end the waiting, not the unit: it is left
// running, recorded for the next lock holder to wait for, and said so.
func (s *setup) makeRepository(ctx context.Context) (unit.Outcome, string, error) {
	return s.repository(ctx, "init", setupClock, s.cfg.Restic, "init", "--json")
}

// openRepository opens the repository with the password the file holds:
// `restic cat config`, which takes the repository's lock — writes a lock
// file and removes it, 0.7 s where the key may write [measured] — so
// that one unit proves the password, the id, and that the key may write
// and delete a lock file. That is locks/ alone: a policy that allows
// locks/ and denies data/, index/ or snapshots/ passes, and the first
// run fails on it, named (accepted, a row). The opening once only read, and a key that may
// read and not write passed it: every run then failed a quarter of an
// hour in [RC check 6, on B2's native API].
//
// What restic said is read, and nothing is inferred beyond it:
//   - exit 0, and restic's own "error while unlocking:" line — it wrote
//     its lock file and could not remove it [M4 cases A, three times out
//     of three, and B, after its retries]: refused, quoting the
//     "Remove(<lock/…>) failed:" line where restic wrote one, which
//     names the lock, else that line. A key that may write and not
//     delete would leave one more lock with every run after, which
//     restic never passes by itself [M75].
//   - exit 0 and no such line: opened.
//   - exit 1: one neutral line with what restic said, without its retry
//     lines, cut at 300 runes — the write refused, the storage gone, a
//     read failed; the clock: the same, with the last line restic said
//     and what a stop may leave. No cause is named, since a key
//     replaced for a storage that throttled reads is the wrong one.
//   - exit 11: another restic holds the repository. restic looks for
//     other locks before it writes its own [M4 case G: an exclusive
//     lock held elsewhere, the opening exited 11 and left nothing], so
//     the opening is read again without the lock (reopen), that is
//     said with the holder, and setup goes on.
//   - exit 12, or 10 where init just made the repository: not opened,
//     nothing written (notOpened).
//
// The id and true where the password opens the repository, with an
// error where restic could not remove its lock file — opened, so that
// the caller knows the password is the repository's.
func (s *setup) openRepository(ctx context.Context) (id string, opened bool, err error) {
	s.writing = true
	o, _, err := s.repository(ctx, "open", setupClock, s.cfg.Restic, "cat", "config")
	switch {
	case errors.Is(err, errDidNotAnswer):
		// Stopped at the clock, restic may have written its lock file
		// and not yet removed it: said, with where restic unlock clears
		// it, as the interrupt says.
		return "", false, fmt.Errorf("restic had not finished opening the repository with its lock within %s%s; %s, with a key that may delete: %s", setupClock, resticSaid(s.lastSaid("open", true)), s.lockLeftLine(), unlockWhere)
	case err != nil:
		return "", false, err
	case o.OK():
		// A lock file restic could not remove is said first, whatever
		// else restic left out: it is in the repository either way.
		id = configID(s.outFile("open"))
		if l := s.unlockFailed(); l != "" {
			return id, true, errors.New("restic opened the repository and could not remove the lock file it wrote" + resticSaid(l) + ": a key that may not delete, or a storage that refuses or does not answer deletes, ends here; the lock stays until restic unlock removes it, with a key that may delete — not this one: " + unlockWhere)
		}
		if id == "" {
			return "", false, errors.New("restic exited 0 but said nothing of the repository's id")
		}
		return id, true, nil
	case o.Result == "exit-code" && s.notOpened(o.ExitStatus):
		// Not opened, nothing written: the key search fails before the
		// lock (12), or no repository where init just made one — a
		// storage that does not show new objects for a moment (10). The
		// password asked for again begins clean; the caller says what
		// to do.
		s.writing = false
		return "", false, nil
	case o.Result == "exit-code" && o.ExitStatus == 11:
		// The holder as restic named it, without its retry lines, which
		// could otherwise fill the cut before the holder's host.
		return s.reopen(ctx, s.withoutRetries("open"))
	case o.Result == "exit-code" && o.ExitStatus == 1:
		// The whole of what restic said, cleaned, as the reopen quotes
		// it: restic prints hint lines last [M4 case G], and the cause
		// is before them. Its retry lines go first, before the cut:
		// a minute of them would fill the 300 runes and leave the cause
		// past it.
		said := record.Text(s.withoutRetries("open"))
		line := "restic could not open the repository with its lock (exit 1): " + said
		if said == "" {
			line = "restic could not open the repository with its lock (exit 1), and said nothing"
		}
		// restic may have written its lock, then failed, then failed to
		// remove it [restic 0.18, lock.go: the deferred unlock runs on
		// every exit]: its own line says so, and where the lock goes.
		if l := s.unlockFailed(); l != "" {
			line += "; and it could not remove the lock file it wrote" + resticSaid(l) + ": the lock stays until restic unlock removes it, with a key that may delete: " + unlockWhere
		}
		return "", false, errors.New(line)
	}
	detail, _ := resticFailure(o)
	return "", false, errors.New(detail)
}

// unlockFailed is restic's own word that it could not remove the lock
// file the opening wrote — its "error while unlocking:" line, on
// stderr [M4 cases A and B] — or "" where it said none. Given back with
// what names the storage's answer: restic's "Remove(<lock/…>) failed:"
// line where it wrote one, which also names the lock; where the DELETE
// failed and was retried rather than refused, restic writes no failed:
// line [M4 case B], and the last "Remove(<lock/…>) returned error,
// retrying after …" line goes with the "error while unlocking:" one,
// since it alone holds the storage's answer (the 500).
func (s *setup) unlockFailed() string {
	raw, _ := os.ReadFile(s.errFile("open"))
	var remove, retried, unlocking string
	for _, l := range strings.Split(string(raw), "\n") {
		switch {
		case strings.Contains(l, "error while unlocking:"):
			unlocking = l
		case strings.Contains(l, "Remove(<lock/") && strings.Contains(l, ") failed:"):
			remove = l
		case strings.Contains(l, "Remove(<lock/") && strings.Contains(l, "returned error, retrying after"):
			retried = l
		}
	}
	switch {
	case unlocking == "":
		return ""
	case remove != "":
		return remove
	case retried != "":
		return retried + "\n" + unlocking
	}
	return unlocking
}

// withoutRetries is what a unit's restic wrote to stderr, by role, less
// its retry lines ("… returned error, retrying after …") wherever any
// other line remains: they name the storage's answer, which the line
// after them repeats, and a minute of them would fill record.Text's
// cut before the cause. Where the retries are all restic said, the last
// of them is kept.
func (s *setup) withoutRetries(role string) string {
	raw, _ := os.ReadFile(s.errFile(role))
	var kept []string
	lastRetry := ""
	for _, l := range strings.Split(string(raw), "\n") {
		switch {
		case record.Clean(l) == "":
		case strings.Contains(l, "returned error, retrying after"):
			lastRetry = l
		default:
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 && lastRetry != "" {
		kept = []string{lastRetry}
	}
	return strings.Join(kept, "\n")
}

// resticSaid is restic's words, appended, where it had any.
func resticSaid(m string) string {
	if t := record.Text(m); t != "" {
		return " (restic said: " + t + ")"
	}
	return ""
}

// reopen is the opening read again without the lock, where another
// restic holds the repository (exit 11): the password and the id are
// proven, the key's writing and deleting are not, and that is said with
// the holder as restic named it. A plain exit 11 leaves nothing of the
// opening's own: restic looked before it wrote [M4 case G]. What the
// saying names is the race — an exclusive lock that appears between
// restic's look and its write, where restic writes its lock, finds the
// other on its second look and removes its own, which a key that may
// not delete cannot do (accepted, a row).
func (s *setup) reopen(ctx context.Context, held string) (id string, opened bool, err error) {
	o, _, err := s.repository(ctx, "reopen", setupClock, s.cfg.Restic, "cat", "config", "--no-lock")
	switch {
	case err != nil:
		return "", false, err
	case o.OK():
		if id = configID(s.outFile("reopen")); id == "" {
			return "", false, errors.New("restic exited 0 but said nothing of the repository's id")
		}
		// The holder, as restic names it ("repository is already locked
		// exclusively by PID … on <host> by <user> … lock was created at
		// … (… ago)" [M4 case G]), and the one fact that matters where
		// the holder is gone — a check killed hard leaves its exclusive
		// lock [M2], and restic exits 11 for it as for a live one: every
		// run and drill after would exit 11 until someone ran restic
		// unlock [M75]. The holder's lock is restic unlock's to clear at
		// once from the holder's own host, not this box's: the holder
		// may run elsewhere [M4 case G's ran on another host].
		s.term.Say("the repository is locked by another restic (exit 11) — a prune or a check from elsewhere: whether this key can write to it, or remove a lock file, was not checked; restic never passes a lock nothing holds by itself: if the holder is gone, restic unlock clears it from the holder's own host at once, from elsewhere once it is half an hour old, or with --remove-all when nothing else holds the repository" + resticSaid(held))
		return id, true, nil
	case o.Result == "exit-code" && o.ExitStatus == 12:
		// The lock-taking opening has run (exit 11): `writing` stands
		// until the next opening, which clears it on its own exit 12 —
		// though the race at exit 11 may have left a lock (accepted, a
		// row).
		return "", false, nil
	case o.Result == "exit-code" && o.ExitStatus == 1:
		// Without restic's retry lines, which could fill the cut before
		// the cause, as the opening's exit 1.
		said := record.Text(s.withoutRetries("reopen"))
		if said == "" {
			return "", false, errors.New("restic could not open the repository (exit 1), and said nothing")
		}
		return "", false, errors.New("restic could not open the repository (exit 1): " + said)
	}
	detail, _ := resticFailure(o)
	return "", false, errors.New(detail)
}

// notOpened is restic's exit status for a repository not opened and
// nothing written: the key search failing before the lock (12), or no
// repository where init just made one — a storage that does not show
// new objects for a moment (10).
func (s *setup) notOpened(status int) bool {
	return status == 12 || status == 10 && s.made
}

// errFile and outFile are where a unit's stderr and stdout go, by its
// role: the one spelling of each.
func (s *setup) errFile(role string) string { return filepath.Join(s.dir, role+".err") }
func (s *setup) outFile(role string) string { return filepath.Join(s.dir, role+".out") }

// unitDescriptions is what the journal calls each of setup's units.
var unitDescriptions = map[string]string{
	"probe":  "look for the repository",
	"open":   "open the repository, taking its lock",
	"reopen": "open the repository without its lock",
	"init":   "init the repository",
}

// lastSaid is the last line a unit's restic wrote to stderr, by role,
// or "" where it wrote nothing; the unit runs without --json, so the
// lines are plain. Where setup stopped the unit, restic's last words
// are the stop's own — "unable to create lock in backend: context
// canceled", "signal terminated received, cleaning up" [measured on
// restic 0.18.0: the e2e's clock case; M1] — and say nothing of the
// storage: the line before them is the one. Nor do the two lines
// restic puts after every opening that failed, "Is there a repository
// at the following location?" and the URL on the line after it, in
// restic's own spelling, written in one call with the Fatal line
// before them [restic 0.18.0, cmd/restic/global.go; measured: the
// e2e's wrong key at the look's clock, where the URL was the last
// line]: nothing from the hint on is read, and the line before is the
// one, which in that case is restic's last retry line, "Load(<config/>,
// 0, 0) returned error, retrying after …: …", carrying the storage's
// answer. That is read from the words, and a restic that rewords its
// stop would be quoted again; the e2e's clock cases pin it. The
// cleaned message (resticMessage) is the whole of stderr joined and
// cut to its head, where a minute of a read's retries would hide the
// one line that names the key.
func (s *setup) lastSaid(role string, stopped bool) string {
	raw, _ := os.ReadFile(s.errFile(role))
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	end := len(lines)
	for i, l := range lines {
		if strings.Contains(l, isThereARepository) {
			end = i
			break
		}
	}
	for i := end - 1; i >= 0; i-- {
		l := lines[i]
		if record.Text(l) == "" || stopped && (strings.Contains(l, "context canceled") || strings.Contains(l, "signal terminated received")) {
			continue
		}
		return l
	}
	return ""
}

// isThereARepository is the line restic puts after every opening that
// failed, with the URL on the line after it [restic 0.18.0,
// cmd/restic/global.go].
const isThereARepository = "Is there a repository at the following location?"

// lockLeftLine is what a person waiting on the opening is told, and
// what an interrupt from it on says: the lock file is restic's,
// written and removed by it, and one left by a stop between the two
// stays — restic never passes a stale lock by itself [M75] — and holds
// off exclusive operations until `restic unlock`. Nothing of setup's
// is in the repository, but for one init just made with the password
// shown: said so, where it was.
func (s *setup) lockLeftLine() string {
	if s.made {
		return lockLeftMade
	}
	return lockLeft
}

// unlockWhere is where restic unlock clears a lock file the opening
// left, and with what: a key that may delete, which the key setup
// was given may not be — from this box at once once setup has ended,
// since restic takes a lock of this host whose pid is gone for stale
// [M4 cases C, D], but only where no other machine of this hostname
// runs restic against the repository, since it takes that machine's
// live lock for stale as well [M4 case E]; otherwise from elsewhere
// once it is half an hour old.
const unlockWhere = "from this box at once, once setup has ended, given that key's credentials, if no other machine with this hostname runs restic against the repository; otherwise from another machine that holds it once the lock is half an hour old; or with restic unlock --remove-all sooner, when nothing else holds the repository"

// lockLeft is the opening's word on what a stop leaves; lockLeftMade
// the same after init made the repository.
const madeNothingMore = "the repository was made with the password shown, and nothing more of setup's has been written"

const lockLeftMade = madeNothingMore + "; a lock file restic was writing may stay, and hold off a prune, or a check from elsewhere, until restic unlock removes it"

const lockLeft = "nothing of setup's has been written; a lock file restic was writing may stay, and hold off a prune, or a check from elsewhere, until restic unlock removes it"

// errDidNotAnswer is a unit given up on at its clock.
var errDidNotAnswer = errors.New("the repository did not answer")

// repository runs one restic command against the repository as the
// backup account, with the credential file the manager reads, under a
// clock; a person waiting is told what for. It returns what restic
// said on stderr, cleaned — the message of its exit_error line where it
// wrote one — for the caller to match and show.
//
// The look only reads, the opening leaves nothing but a lock file it
// removes, and both are stopped at their clock and on an interrupt.
// Init is not (see makeRepository): the clock and
// an interrupt end the waiting, the unit runs on, and the error names
// it.
func (s *setup) repository(ctx context.Context, role string, within time.Duration, argv ...string) (unit.Outcome, string, error) {
	clock, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	// A person waiting is told what for, and what Ctrl-C would do:
	// nothing, for the look, which only reads; for init,
	// leave a unit running that may be making the repository with the
	// password shown. Said under the terminal's own lock, and waited
	// for if it is mid-sentence when the unit ends.
	noteDone := make(chan struct{})
	note := time.AfterFunc(setupNote, func() {
		defer close(noteDone)
		if role == "init" {
			s.term.Say("still waiting for " + s.repo + " (Ctrl-C leaves restic init running, and the next setup waits for it; it may be making the repository with the password shown: keep it)")
			return
		}
		if s.writing {
			// The opening, and whatever runs after it, may have left
			// restic's lock file.
			s.term.Say("still waiting for " + s.repo + " (Ctrl-C is safe: " + s.lockLeftLine() + ")")
			return
		}
		s.term.Say("still waiting for " + s.repo + " (Ctrl-C is safe: nothing has been written)")
	})
	defer func() {
		if !note.Stop() {
			<-noteDone
		}
	}()
	errFile := s.errFile(role)
	// What an earlier unit of the role said goes before this one
	// starts: the manager truncates the files only when it starts the
	// unit, and one it never started would be read with the last
	// attempt's words.
	for _, f := range []string{errFile, s.outFile(role)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return unit.Outcome{}, "", err
		}
	}
	what, known := unitDescriptions[role]
	if !known {
		what = role + " the repository"
	}
	spec := resticUnit(s.name(role, ""), "motel backup: "+what, s.staged, argv, s.outFile(role))
	spec.StderrFile = errFile
	if role != "init" {
		o, err := s.startWithin(ctx, within, spec)
		if err != nil {
			return o, "", err
		}
		return o, resticMessage(errFile), nil
	}
	// Init: recorded for the next lock holder to wait for, not to stop
	// (the record goes once it was seen to end), and run on a context
	// nothing here cancels. The clock and an interrupt end this wait alone.
	marker := filepath.Join(s.cfg.RunDir, "init-unit")
	if err := os.WriteFile(marker, []byte(spec.Name+"\n"), 0o600); err != nil {
		return unit.Outcome{}, "", err
	}
	// Not bound to setup's own service, where it has one: BindsTo=
	// would have the manager stop it as setup ends.
	type ended struct {
		o   unit.Outcome
		err error
	}
	done := make(chan ended, 1)
	go func() {
		o, err := s.r.Run(context.WithoutCancel(ctx), spec)
		if err == nil {
			// Seen to its end. A runner error is a unit lost sight
			// of — the request may have reached the manager all the
			// same — and the record stays for the next lock holder,
			// whose wait treats a unit that is not there as ended.
			_ = os.Remove(marker)
		}
		done <- ended{o, err}
	}()
	select {
	case e := <-done:
		if e.err != nil {
			// Lost sight of, and recorded still: left what it needs.
			s.leaveInit()
			return e.o, "", e.err
		}
		return e.o, resticMessage(errFile), nil
	case <-clock.Done():
		s.leaveInit()
		if ctx.Err() != nil {
			return unit.Outcome{}, "", ctx.Err()
		}
		return unit.Outcome{}, "", fmt.Errorf("%w within %s; restic init is left running as %s, and the next run or setup waits for it (if it must be ended: systemctl stop %s)", errDidNotAnswer, within, spec.Name, spec.Name)
	}
}

// repositoryExists reads restic's word for a repository that is there
// already, out of an init that exited 1: two wordings, one condition
// [measured] — "config file already exists" where the backend's Stat
// of the config answers (a local path, S3 proper), "repository master
// key and config already initialized" where it does not (the rclone S3
// fixture) and init goes on to find the config as it makes the key.
// Exactly those, so that a storage's own "already exists" about some
// other thing is not taken for it.
func repositoryExists(message string) bool {
	return strings.Contains(message, "config file already exists") || strings.Contains(message, "repository master key and config already initialized")
}

// backendRefused reads restic's word for an init that failed before
// anything of a repository was written: its message opens with "create
// repository at <url> failed:", the wrapper init puts on its first step
// alone — the backend's Create, and what comes before it: the URL that
// does not parse, the host that does not resolve, the key refused
// (S3's signature mismatch [measured, on rclone's S3 server]), the
// bucket out of reach [restic 0.18.0, cmd/restic/cmd_init.go:96, with
// "unable to open repository at" nested for the backend's own failure].
// An init that got past the opening opens with "create key in
// repository at" instead, and may have written. Read at the message's
// head, so that restic's words inside the URL, which the message
// echoes, are not taken for its answer; and the wording that says a
// repository exists shares the head, and is read first.
func backendRefused(message string) bool {
	return strings.HasPrefix(strings.TrimPrefix(message, "Fatal: "), "create repository at ") && !repositoryExists(message)
}

// noBucket reads restic's word for a bucket not there and a key that
// may not make it, out of an init refused at the opening: restic's S3
// backend makes a bucket it does not find, and the storage's refusal
// follows the backend's own echo of the URL as "client.MakeBucket: …"
// [measured, B2's S3 endpoint, RC check 1: "client.MakeBucket: not
// entitled"; there the look says exit 10 at once, so the password has
// been shown]. Read after that echo, so that the words inside the URL
// are not taken for restic's.
func noBucket(message, repository string) bool {
	_, reason, ok := strings.Cut(message, "unable to open repository at "+repository+": ")
	return backendRefused(message) && ok && strings.HasPrefix(reason, "client.MakeBucket: ")
}

// repositoryHalfThere reads restic's word for files of a repository
// with no config, out of an init that exited 1: init's own checks,
// made before it writes anything [restic 0.18.0,
// internal/repository/repository.go Init] — a key file, left by an init
// that ended between its two writes (the key first, then the config),
// or snapshots, left where a lifecycle rule removed the config before
// the data. Nothing opens them, and init will not write over them. The
// reason ends the message, after the echoed URL, and is read there.
func repositoryHalfThere(message string) bool {
	return strings.HasSuffix(message, "failed: repository already contains keys") || strings.HasSuffix(message, "failed: repository already contains snapshots")
}

// resticMessage is what restic said on stderr: the message of its
// exit_error line when --json gave it one, else the text as it is —
// cleaned, but whole: the phrase that says a repository exists comes
// after its URL, however long that is. What is shown is cut by Text.
func resticMessage(file string) string {
	var m struct {
		Message string `json:"message"`
	}
	if jsonLine(file, "exit_error", &m) {
		return record.Clean(m.Message)
	}
	raw, _ := os.ReadFile(file) //nolint:gosec // written by the manager into root's own run dir
	return record.Clean(string(raw))
}

// initializedID is the id of the repository `restic init --json` made,
// from its one line [measured].
func initializedID(file string) string {
	var m struct {
		ID string `json:"id"`
	}
	if jsonLine(file, "initialized", &m) && snapshotRe.MatchString(m.ID) {
		return m.ID
	}
	return ""
}

// configID is the repository's id from `restic cat config` [measured].
func configID(file string) string {
	raw, err := os.ReadFile(file) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return ""
	}
	var c struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &c) != nil || !snapshotRe.MatchString(c.ID) {
		return ""
	}
	return c.ID
}

// newPassword is 32 random bytes, in an alphabet a person can store and
// type back: base32, lower case, no padding.
func newPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}
