package box

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/motel/box/proof"
)

// `motel box describe`'s reading of the secrets beside a Caddyfile
// (DESIGN-box.md, "`box describe`", "Which checks run where"): the
// working tree's `<dir>/secrets/`, held to the layout table by the code
// that holds the commit's (proof.SecretsLayout, proof.AppLayout), so the
// two refuse alike, and without a key. Each value's length comes from its
// ciphertext alone — age pads nothing — to within the line break step
// 15a strips; the caps past 75% are warnings, so a pull request's check
// run shows a cliff before a push meets it.
//
// The lines it adds to describe's, in this order, for bin/check to read
// (each value held to a grammar, so none can break a line):
//
//	recipients <n>                 the recipients file's recipient lines, when there is one
//	secret <app> <VAR> <bytes>     each secret, apps and variables in git's order
//	warning <text>                 each cap past 75%, and each dot-name, which is not read

// treeSecrets is a working tree's secrets as describe reads them.
type treeSecrets struct {
	shown      string           // the secrets directory, as a message names it
	secrets    *proof.Secrets   // names only: what secretsFit reads
	sizes      map[string]int64 // <app>/<VAR> → the value's length in bytes
	recipients int              // recipient lines, -1 when there is no recipients file
	warnings   []string
	// files are the secrets' files, and recipientsFile the recipients
	// file ("" when there is none): what measure reads.
	files          []treeSecret
	recipientsFile string
}

// treeSecret is one secret's file in the working tree.
type treeSecret struct {
	app, v, path, shown string
}

// Thresholds of the 75% warnings (DESIGN-box.md, Caps).
const (
	warnSecretApps = proof.MaxSecretApps * 3 / 4
	warnSecretVars = proof.MaxSecretVars * 3 / 4
	warnEnvFile    = maxEnvFile * 3 / 4
	warnCiphertext = proof.MaxBundle * 3 / 4
	// age's own bounds on a recipients file (cmd/age, parseRecipientsFile):
	// the file, a recipient's line, and any line, which its scanner reads.
	maxRecipientsFile = 16 << 20
	maxRecipientLine  = 8 << 10
	maxScannedLine    = bufio.MaxScanTokenSize
)

// readTreeSecrets reads the secrets beside the Caddyfile at caddyfile, a
// path in the working tree, as root's proof reads a commit's: each
// entry in git's order through the proof's own layout rules
// (proof.SecretsLayout, proof.AppLayout), app by app, then each
// secret's size — so the first refusal is the one root would name. No
// secret's file is opened here: the values' lengths and the recipients
// are measure's, once secretsFit has passed, as root decrypts only at
// step 15a and never reads the recipients. No `secrets` beside the
// Caddyfile is none.
func readTreeSecrets(caddyfile string) (*treeSecrets, error) {
	parent := filepath.Dir(caddyfile)
	dir := filepath.Join(parent, "secrets")
	ts := &treeSecrets{shown: secretsShown(caddyfile), secrets: &proof.Secrets{}, sizes: map[string]int64{}, recipients: -1}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ts, nil
	case err != nil:
		return nil, err
	}
	mode, err := gitMode(parent, fs.FileInfoToDirEntry(fi))
	if err != nil {
		return nil, err
	}
	layout := proof.NewSecretsLayout(ts.shown, "", secretNames)
	if err := layout.Dir(mode); err != nil {
		return nil, err
	}
	entries, err := ts.gitTree(dir, ts.shown)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := layout.Entry([]byte(e.mode), []byte(e.name), nil); err != nil {
			return nil, err
		}
		if e.name == "recipients" {
			ts.recipientsFile = filepath.Join(dir, e.name)
		}
	}
	if err := layout.Done(); err != nil {
		return nil, err
	}
	for _, a := range layout.Apps {
		app, err := ts.readTreeApp(filepath.Join(dir, a.Name), a)
		if err != nil {
			return nil, err
		}
		ts.secrets.Apps = append(ts.secrets.Apps, app)
	}
	return ts, nil
}

// readTreeApp reads one app's directory through the proof's layout,
// then holds each secret's file to its cap, as the proof holds each
// blob.
func (ts *treeSecrets) readTreeApp(dir string, a proof.LayoutRef) (proof.SecretApp, error) {
	app := proof.SecretApp{Name: a.Name}
	entries, err := ts.gitTree(dir, a.Shown)
	if err != nil {
		return app, err
	}
	layout := proof.NewAppLayout(a.Name, a.Shown, "", secretNames)
	for _, e := range entries {
		if err := layout.Entry([]byte(e.mode), []byte(e.name), nil); err != nil {
			return app, err
		}
	}
	if err := layout.Done(); err != nil {
		return app, err
	}
	for _, v := range layout.Vars {
		p := filepath.Join(dir, v.Name+".age")
		fi, err := os.Lstat(p)
		if err != nil {
			return app, err
		}
		if fi.Size() > proof.MaxSecretFile {
			return app, refused("%s", proof.SecretFileTooLarge(v.Shown, fi.Size()))
		}
		app.Vars = append(app.Vars, proof.SecretVar{Name: v.Name})
		ts.files = append(ts.files, treeSecret{app: a.Name, v: v.Name, path: p, shown: v.Shown})
	}
	return app, nil
}

// measure reads each secret's value length from its file's header, then
// the recipients file — what root reads only at step 15a, and what it
// never reads — and adds the warnings of the caps past 75%.
func (ts *treeSecrets) measure() error {
	var ciphertext int64
	rendered := map[string]int64{}
	for _, f := range ts.files {
		size, n, err := secretFileLength(f.path)
		var big *tooLargeError
		var notAge *notAgeError
		switch {
		case errors.As(err, &big):
			return refused("%s", proof.SecretFileTooLarge(f.shown, size))
		case errors.As(err, &notAge):
			return refused("%s does not decrypt: %s", f.shown, notAge.why)
		case err != nil:
			return err
		}
		ts.sizes[f.app+"/"+f.v] = n
		ciphertext += size
		rendered[f.app] += int64(len(f.v)) + n + 4 // KEY="value"\n
	}
	for _, app := range ts.secrets.Apps {
		for _, v := range app.Vars {
			if app.Name != boxSlot && strings.HasPrefix(v.Name, "NOTIFY_") {
				ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's secret %s is withheld from the app: a NOTIFY_ variable in an env file the box writes is notify's (a channel's URL goes in %s/%s/); name it otherwise if the app reads it", app.Name, v.Name, ts.shown, boxSlot))
			}
		}
		if n := len(app.Vars); n >= warnSecretVars {
			ts.warnings = append(ts.warnings, fmt.Sprintf("app %s has %d secrets; the cap is %d", app.Name, n, proof.MaxSecretVars))
		}
		// A warning, not a refusal: the cap is step 15a's, and each
		// value's length counts a line break (up to 2 bytes) that 15a may
		// strip, so the box surely refuses only when the file is over
		// even without them.
		switch r := rendered[app.Name]; {
		case r-2*int64(len(app.Vars)) > maxEnvFile:
			ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's env file would be about %d bytes, over the cap of %d: the box will refuse the push", app.Name, r, maxEnvFile))
		case r > maxEnvFile:
			ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's env file would be about %d bytes, at or just over the cap of %d: the box refuses the push unless values end in the line break it strips", app.Name, r, maxEnvFile))
		case r >= warnEnvFile:
			ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's env file would be about %d bytes; the cap is %d", app.Name, r, maxEnvFile))
		}
	}
	if n := len(ts.secrets.Apps); n >= warnSecretApps {
		ts.warnings = append(ts.warnings, fmt.Sprintf("%d apps have secrets; the cap is %d", n, proof.MaxSecretApps))
	}
	if ciphertext >= warnCiphertext {
		ts.warnings = append(ts.warnings, fmt.Sprintf("%s holds %d bytes of ciphertext; a push's bundle is capped at %d", ts.shown, ciphertext, proof.MaxBundle))
	}
	if ts.recipientsFile != "" {
		n, err := countRecipients(ts.recipientsFile, ts.shown+"/recipients", ts)
		if err != nil {
			return err
		}
		ts.recipients = n
	}
	return nil
}

// treeEntry is a working tree's entry as a commit of it would hold it:
// its name, and its mode as git writes it.
type treeEntry struct{ name, mode string }

// gitTree lists dir as a commit of it would (gitTree, the function),
// and warns of each dot-name it leaves out, which describe does not
// read: the commit may not have it, but the box refuses it if it does.
func (ts *treeSecrets) gitTree(dir, shown string) ([]treeEntry, error) {
	entries, dots, err := gitTree(dir)
	if err != nil {
		return nil, err
	}
	for _, d := range dots {
		ts.warnings = append(ts.warnings, notCommitted(shown+"/"+d))
	}
	return entries, nil
}

// gitTree lists dir as a commit of it would: each entry with the mode
// git gives it (gitMode), in a git tree's order — by name, a tree's name
// read as if it ended in `/`, so `api-v2` comes before `api` — the
// order root walks the commit's. It leaves out every dot-name
// (`.DS_Store`, an editor's swap file), which git usually ignores, and
// every directory git would not commit, one that holds nothing but
// those; it returns the dot-names it left out, each as its path from
// dir, every part bounded.
func gitTree(dir string) ([]treeEntry, []string, error) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var entries []treeEntry
	var dots []string
	for _, e := range list {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			dots = append(dots, proof.Bound(name))
			continue
		}
		mode, err := gitMode(dir, e)
		if err != nil {
			return nil, nil, err
		}
		if mode == proof.ModeDir {
			inner, innerDots, err := gitTree(filepath.Join(dir, name))
			if err != nil {
				return nil, nil, err
			}
			if len(inner) == 0 { // git commits no empty tree
				for _, d := range innerDots {
					dots = append(dots, proof.Bound(name)+"/"+d)
				}
				continue
			}
		}
		entries = append(entries, treeEntry{name, mode})
	}
	key := func(e treeEntry) string {
		if e.mode == proof.ModeDir {
			return e.name + "/"
		}
		return e.name
	}
	slices.SortFunc(entries, func(a, b treeEntry) int { return strings.Compare(key(a), key(b)) })
	return entries, dots, nil
}

// gitMode is the mode git commits a working tree's entry with. A
// directory holding `.git` is another repository, which git commits as
// a link to it, a submodule's mode; what git cannot commit — a socket,
// a device, a pipe — is "", which the layout refuses as neither a file
// nor a directory.
func gitMode(dir string, e fs.DirEntry) (string, error) {
	switch t := e.Type(); {
	case t&fs.ModeSymlink != 0:
		return proof.ModeSymlink, nil
	case t.IsRegular():
		return proof.ModeFile, nil
	case t.IsDir():
		_, err := os.Lstat(filepath.Join(dir, e.Name(), ".git"))
		switch {
		case err == nil:
			return proof.ModeSubmodule, nil
		case errors.Is(err, fs.ErrNotExist):
			return proof.ModeDir, nil
		}
		return "", err
	}
	return "", nil
}

// notCommitted is the warning for a dot-name describe left out.
func notCommitted(path string) string {
	return path + " is not a secret, and is not read here; the box refuses the push if it is committed"
}

// notAgeError is why a secret's file is not an age file the box reads,
// in the words of root's refusal for it.
type notAgeError struct{ why string }

func (e *notAgeError) Error() string { return e.why }

// secretFileLength is a secret's file's size and its value's length,
// read as far as the header: the file is never read whole. A file over
// the cap is a *tooLargeError with its size.
func secretFileLength(path string) (size, n int64, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) //nolint:gosec // the operator's own working tree
	if err != nil {
		return 0, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, 0, fmt.Errorf("%s: %w", path, errNotRegular)
	}
	if size = fi.Size(); size > proof.MaxSecretFile {
		return size, 0, &tooLargeError{name: path, limit: proof.MaxSecretFile}
	}
	n, err = valueLengthOf(f, size)
	return size, n, err
}

// valueLength is valueLengthOf over bytes in memory.
func valueLength(data []byte) (int64, error) {
	return valueLengthOf(bytes.NewReader(data), int64(len(data)))
}

// valueLengthOf is a binary age file's plaintext length, from its header
// and its size alone: the payload after the header is a 16-byte nonce,
// then the value in 64 KiB chunks, each with a 16-byte tag, and at
// least one chunk. Why it is not one the box reads is a *notAgeError.
func valueLengthOf(r io.ReaderAt, size int64) (int64, error) {
	notAge := func(why string) error { return &notAgeError{why: why} }
	head := make([]byte, len(ageArmor))
	got, _ := r.ReadAt(head, 0)
	head = head[:got]
	if bytes.HasPrefix(head, []byte(ageArmor)) {
		return 0, notAge("it is ASCII-armored (age -a); the box reads age's binary form")
	}
	if !bytes.HasPrefix(head, []byte(ageIntro)) {
		return 0, notAge("it is not an age file")
	}
	header, err := age.ExtractHeader(io.NewSectionReader(r, 0, size))
	if err != nil {
		return 0, notAge("its header is not one age reads")
	}
	// Every chunk but the last is full; the last is never empty unless
	// it is the only one (age's STREAM). So after the full frames comes
	// either nothing, or a frame with a tag and at least one byte: a
	// remainder of 1 to 16 bytes is a frame cut short.
	const nonce, chunk, tag = 16, 64 << 10, 16
	const frame = chunk + tag
	q := size - int64(len(header)) - nonce
	full, rest := q/frame, q%frame
	switch {
	case q == tag: // the empty value: one empty chunk
		return 0, nil
	case q < tag, rest > 0 && rest <= tag:
		return 0, notAge("its payload is cut short")
	case rest == 0:
		return full * chunk, nil
	}
	return full*chunk + rest - tag, nil
}

// agePluginLine is a plugin's age recipient, age1<name>1<data>
// (age-plugin-yubikey, age-plugin-se, age1pq…): a second `1` after the
// name, then bech32 data, which has no `1` of its own. Its checksum is
// the plugin's to check; an X25519 recipient (one `1`) that does not
// parse is a mistyped key, never a plugin's.
var agePluginLine = regexp.MustCompile(`^age1[a-z0-9]+1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]+$`)

// countRecipients reads a recipients file as age does — one recipient a
// line, read as it is (no space trimmed), a line that is empty or starts
// with `#` skipped, within age's bounds on the file and its lines — and
// counts the recipients. There must be one, and the first must be the
// box's key, an X25519 recipient ("The box's key"); every other an SSH
// key age encrypts to, or an age recipient.
func countRecipients(path, shown string, ts *treeSecrets) (int, error) {
	data, err := readFile(path, maxRecipientsFile, false)
	var big *tooLargeError
	switch {
	case errors.As(err, &big):
		return 0, refused("%s is over %d bytes, more than age reads of a recipients file", shown, maxRecipientsFile)
	case err != nil:
		return 0, err
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4<<10), maxScannedLine)
	line := 0
	for sc.Scan() {
		line++
		s := sc.Text()
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		switch {
		case !utf8.ValidString(s):
			return 0, refused("%s:%d: not UTF-8, which age refuses in a recipients file", shown, line)
		case len(s) > maxRecipientLine:
			return 0, refused("%s:%d: over %d bytes, longer than age reads a recipient's line", shown, line, maxRecipientLine)
		case strings.HasPrefix(s, "AGE-"):
			return 0, refused("%s:%d: an age identity, a private key, not a recipient: remove it, and make a new key for whoever it was", shown, line)
		}
		// A line's CR is the scanner's to drop, as age's own scanner does;
		// a space is the recipient's, which age then cannot parse.
		if s != strings.TrimSpace(s) {
			return 0, refused("%s:%d: a space or tab at the line's start or end, which age reads as part of the recipient", shown, line)
		}
		if n == 0 {
			if _, err := age.ParseX25519Recipient(s); err != nil {
				return 0, refused("%s:%d: the first recipient is not the box's key, an age1… X25519 recipient (motel box key prints it)", shown, line)
			}
			n++
			continue
		}
		var ok bool
		switch {
		case strings.HasPrefix(s, "age1"):
			_, err := age.ParseX25519Recipient(s)
			ok = err == nil || agePluginLine.MatchString(s)
		default:
			_, err := agessh.ParseRecipient(s)
			ok = err == nil
			if !ok && unsupportedSSHKey(s) {
				// As age does: skipped, with a warning, never an error.
				ts.warnings = append(ts.warnings, fmt.Sprintf("%s:%d: age skips this %s key (it encrypts only to ssh-ed25519 and ssh-rsa): its holder cannot read the secrets; give them an age1… recipient instead", shown, line, sshKeyType(s)))
				continue
			}
		}
		if !ok {
			return 0, refused("%s:%d: not a recipient age encrypts to (an ssh-ed25519 or ssh-rsa key, or an age1… recipient)", shown, line)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return 0, refused("%s:%d: over %d bytes, longer than age reads a line", shown, line+1, maxScannedLine)
		}
		return 0, err
	}
	if n == 0 {
		return 0, refused("%s has no recipient: its first is the box's key (motel box key prints it)", shown)
	}
	return n, nil
}

// unsupportedSSHKey reports whether s is an SSH public key that age reads
// but does not encrypt to — an sk- key; an ECDSA one, which a
// recipients file written before 2026-11-06 may still carry — which age
// skips in a recipients file with a warning.
func unsupportedSSHKey(s string) bool {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
	return err == nil && k.Type() != ssh.KeyAlgoED25519 && k.Type() != ssh.KeyAlgoRSA
}

// sshKeyType is s's key type, the line's first field.
func sshKeyType(s string) string {
	t, _, _ := strings.Cut(s, " ")
	return proof.Bound(t)
}

// lines are describe's lines for the secrets.
func (ts *treeSecrets) lines() []string {
	var out []string
	if ts.recipients >= 0 {
		out = append(out, fmt.Sprintf("recipients %d", ts.recipients))
	}
	for _, a := range ts.secrets.Apps {
		for _, v := range a.Vars {
			out = append(out, fmt.Sprintf("secret %s %s %d", a.Name, v.Name, ts.sizes[a.Name+"/"+v.Name]))
		}
	}
	for _, w := range ts.warnings {
		out = append(out, "warning "+w)
	}
	return out
}
