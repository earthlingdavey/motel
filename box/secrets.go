package box

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"filippo.io/age"

	"github.com/smallhoursorg/motel/box/proof"
	"github.com/smallhoursorg/motel/liveswap"
)

// The box's side of the repository's secrets (DESIGN-box.md, "Secrets"):
// its key, the opening of each value, the env file each app's values
// become, and the checks that an app's block reads them. The proof that
// the bytes are the commit's is box/proof's ProveSecrets ("Proving the
// secrets"); what root does with the files is the install transaction.

// secretsDir holds one env file per app with secrets, written by root
// and read by motel at each launch (DESIGN-box.md, "Paths, owners,
// and who may touch what"). The box's key is /etc/motel/age/identity
// ("The box's key"): root's alone, 0600 in a 0700 directory, so that no
// other account — the motel uid, which every app also runs as,
// included — can read it whatever its mode.
const secretsDir = "/etc/motel/secrets" //nolint:gosec // a directory's path, not a credential

// Caps (DESIGN-box.md, "Caps").
const (
	// maxIdentity caps the key file as read: age-keygen's format is
	// two comment lines and the key, under 200 bytes.
	maxIdentity = 4 << 10
	// maxEnvFile caps one app's env file as rendered.
	maxEnvFile = 64 << 10
)

// managedEnvFile is the env file the box writes for app: the path an
// app's block names with env_file to read its secrets.
func managedEnvFile(app string) string { return secretsDir + "/" + app + ".env" }

// boxSlot is the one secrets directory that names no app: the box's
// own (DESIGN-box.md, "The box's own slot"), whose values notify reads
// as its channel URLs and no app is given. Outside the app grammar, so
// no app can be named for it; its env file is /etc/motel/secrets/_box.env
// (managedEnvFile), notify's default secrets_file.
const boxSlot = "_box"

// SecretsDir and BoxSecretsFile are secretsDir and the box's own env
// file, for cmd/motel's test to hold equal to what liveswap withholds
// from (BoxSecretsDir) and notify reads (DefaultSecretsFile): the three
// modules restate them and import none of the others for it.
const (
	SecretsDir     = secretsDir
	BoxSecretsFile = secretsDir + "/" + boxSlot + ".env"
)

// secretSlot is a secrets directory's name: an app's, in liveswap's
// grammar, or the box's own.
func secretSlot(name string) bool { return name == boxSlot || appName.MatchString(name) }

// secretNames are the grammars the secrets' names are held to: a
// directory is named for a liveswap app (appName, as the token walk
// reads app lines) or is the box's own slot, a value's file for a
// variable an env file can carry.
var secretNames = proof.Names{App: secretSlot, Var: liveswap.ValidEnvKey}

// boxVar is the grammar of a variable in the box's own slot: notify's
// secret names (notify/config.go, secretRef), `NOTIFY_` then capitals,
// digits and underscores. The slot is notify's alone (#418's open
// question 1).
var boxVar = regexp.MustCompile(`^NOTIFY_[A-Z0-9_]*$`)

// errNotIdentity is a key file that is there and does not read as the
// box's (the Console commands table's text); age's own parse error is
// not passed on, since a line it could not read may be the key itself.
var errNotIdentity = errors.New("is not an age identity: move it aside, run motel box key again, then make rekey BOX_KEY=<the new key> in the repository")

// readIdentity reads the box's key: a regular file, not followed if a
// symlink, within maxIdentity, in age-keygen's format, holding exactly
// one X25519 identity.
func readIdentity(file string) (*age.X25519Identity, error) {
	data, err := readFile(file, maxIdentity, false)
	if err != nil {
		return nil, err
	}
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil || len(ids) != 1 {
		return nil, fmt.Errorf("%s %w", file, errNotIdentity)
	}
	id, ok := ids[0].(*age.X25519Identity)
	if !ok {
		return nil, fmt.Errorf("%s %w", file, errNotIdentity)
	}
	return id, nil
}

// identityTemp is the pattern of the temporaries ensureIdentity writes
// beside the key.
const identityTemp = ".identity-*"

// ensureIdentity is the box's key, made if there is none (DESIGN-box.md,
// "The box's key"). It holds an exclusive flock on the key's directory
// for its run, so that two runs never meet, and first removes any
// temporary a crashed run left: one may hold a key nobody uses, or be a
// second name for the box's own. A new key is written whole to a
// temporary in the same directory (0600, created exclusively), fsynced,
// then linked into place — a link, not a rename, so that an identity
// already there is never replaced — and the temporary removed and the
// directory fsynced. created reports whether this call made the key; an
// error after the link comes with the key, which is in place though not
// known durable. A key that is there and does not read is refused,
// never replaced: moving it aside is a person's decision, since every
// secret encrypted to it needs a rekey after.
func ensureIdentity(file string, now time.Time) (id *age.X25519Identity, created bool, err error) {
	dir := filepath.Dir(file)
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	left, err := filepath.Glob(filepath.Join(dir, identityTemp))
	if err != nil {
		return nil, false, err
	}
	for _, name := range left {
		if err := os.Remove(name); err != nil {
			return nil, false, err
		}
	}
	id, err = readIdentity(file)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return id, false, err
	}
	id, err = age.GenerateX25519Identity()
	if err != nil {
		return nil, false, err
	}
	tmp, err := os.CreateTemp(dir, identityTemp)
	if err != nil {
		return nil, false, err
	}
	body := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", now.UTC().Format(time.RFC3339), id.Recipient(), id)
	_, err = tmp.WriteString(body)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Link(tmp.Name(), file)
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) // best effort: the next run's sweep removes a leftover
		return nil, false, err
	}
	// The key is in place from here on.
	if err := os.Remove(tmp.Name()); err != nil {
		return id, true, err
	}
	return id, true, syncDir(dir)
}

// lockDir takes an exclusive flock on a directory, waiting for it; the
// returned func releases it.
func lockDir(dir string) (func(), error) {
	d, err := os.Open(dir) //nolint:gosec // the box key's fixed directory
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX); err != nil {
		_ = d.Close()
		return nil, err
	}
	return func() { _ = d.Close() }, nil // closing the descriptor releases the lock
}

// openedApp is one app's secrets, opened: the variables' names in the
// order the commit lists them, and the env file they render to.
type openedApp struct {
	name string
	vars []string
	file []byte
}

// openSecrets is step 15a (DESIGN-box.md): every value of every app
// opened with the box's key, held to the value rules, and each app's
// rendered to the env file the box will install. It runs as root, and
// only on secrets ProveSecrets proved from a chain whose every
// signature verified (I6): the bytes it decrypts are a signer's.
// caddyfile is the bundle's path, which names the directory the
// secrets sit in.
func openSecrets(id *age.X25519Identity, s *proof.Secrets, caddyfile string) ([]openedApp, error) {
	var out []openedApp
	for _, a := range s.Apps {
		app := openedApp{name: a.Name}
		shown := secretsShown(caddyfile) + "/" + a.Name
		pairs := make([]string, 0, len(a.Vars))
		for _, v := range a.Vars {
			plain, err := openSecret(id, v.Data, shown+"/"+v.Name+".age", secretsShown(caddyfile))
			if err != nil {
				return nil, err
			}
			value, err := secretValue(plain, a.Name+"/"+v.Name)
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, v.Name+"="+value)
			app.vars = append(app.vars, v.Name)
		}
		body, err := liveswap.RenderEnvFile(pairs)
		if err != nil {
			// secretValue and the proof's names admit only what renders.
			return nil, fmt.Errorf("rendering %s's env file: %w", a.Name, err)
		}
		// The cap is the values' lines; the header is the box's own.
		if len(body) > maxEnvFile {
			return nil, refused("app %s's env file would be %d bytes; the cap is %d", a.Name, len(body), maxEnvFile)
		}
		app.file = append([]byte(envFileHeader), body...)
		out = append(out, app)
	}
	return out, nil
}

// envFileHeader opens every env file the box writes: one constant line,
// so that the same values always render the same bytes and a rekey (new
// ciphertext, same values) leaves the file as it is.
const envFileHeader = "# Written by motel from the box's repository; a push that changes these secrets replaces this file.\n"

// ageIntro and ageArmor are how a binary age file and an armored one
// begin (age-encryption.org/v1).
const (
	ageIntro = "age-encryption.org/v1\n"
	ageArmor = "-----BEGIN AGE ENCRYPTED FILE-----"
)

// openSecret decrypts one value with the box's key. A file encrypted to
// other keys only is refused naming the box's, so that the run says
// which key to put first in the recipients file; anything else that
// does not decrypt whole is refused saying what it is, in the box's
// words, never age's: age's error for a file that is not an age file
// quotes its first line, and that file may be a value someone committed
// without encrypting it. Nothing of the plaintext is kept beyond the
// value.
func openSecret(id *age.X25519Identity, data []byte, shown, secrets string) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(data), id)
	var unmatched *age.NoIdentityMatchError
	switch {
	case errors.As(err, &unmatched):
		return nil, refused("%s is not encrypted to this box's key (%s): put that key first in %s/recipients and run make rekey", shown, id.Recipient(), secrets)
	case err != nil && bytes.HasPrefix(data, []byte(ageArmor)):
		return nil, refused("%s does not decrypt: it is ASCII-armored (age -a); the box reads age's binary form", shown)
	case err != nil && !bytes.HasPrefix(data, []byte(ageIntro)):
		return nil, refused("%s does not decrypt: it is not an age file", shown)
	case err != nil:
		return nil, refused("%s does not decrypt: its header is not one age reads, or was changed after it was encrypted", shown)
	}
	plain, err := readCapped(r, shown, proof.MaxSecretFile) // a payload is never longer than its file
	if err != nil {
		return nil, refused("%s does not decrypt: its payload was changed or cut short after it was encrypted", shown)
	}
	return plain, nil
}

// secretValue is the value a decrypted file holds (DESIGN-box.md,
// "Values and the env file"): its bytes, less one trailing newline (LF
// or CRLF), since `echo value | age …` adds one and a value cannot hold
// one. What an env file cannot carry is refused, naming <app>/<VAR>:
// another line break, a CR, a NUL, bytes that are not UTF-8. These are
// RenderEnvFile's rules, checked first so that the refusal names the
// secret. An empty value is a value.
func secretValue(plain []byte, name string) (string, error) {
	v := plain
	if s, ok := bytes.CutSuffix(v, []byte("\r\n")); ok {
		v = s
	} else if s, ok := bytes.CutSuffix(v, []byte("\n")); ok {
		v = s
	}
	switch {
	case bytes.ContainsAny(v, "\n\r"):
		return "", refused("%s: the value has a line break", name)
	case bytes.IndexByte(v, 0) >= 0:
		return "", refused("%s: the value has a NUL byte", name)
	case !utf8.Valid(v):
		return "", refused("%s: the value is not UTF-8", name)
	}
	return string(v), nil
}

// secretsShown is the secrets directory as a message names it: beside
// the Caddyfile, in the repository.
func secretsShown(caddyfile string) string {
	dir := path.Dir(caddyfile)
	if dir == "." {
		return "secrets"
	}
	return proof.Bound(dir + "/secrets")
}

// secretsFit holds the incoming file's app blocks to the secrets beside
// it (DESIGN-box.md, "Which checks run where"): every secrets directory
// names an app, every app with secrets reads its own env file and
// shadows none of them, and no app's env_file names a file in
// secretsDir the box will not write for it. These are mistakes a push
// should be told about before the app's next launch, not a boundary:
// the handler and `motel box describe` run them, root does not. An
// env_file's value is never quoted back, since it may hold a value the
// author meant to keep out of the run's output.
func secretsFit(shape *Shape, s *proof.Secrets, caddyfile string) error {
	dir := secretsShown(caddyfile)
	if err := notifyCarried(shape, s, dir); err != nil {
		return err
	}
	has := map[string]bool{}
	for _, a := range s.Apps {
		has[a.Name] = true
		if a.Name == boxSlot {
			if err := boxSlotFits(shape, a, dir); err != nil {
				return err
			}
			continue
		}
		block, ok := shape.AppEnv[a.Name]
		if !ok {
			return refused("%s/%s/ names no app in the new Caddyfile: add the app, or remove the directory", dir, a.Name)
		}
		want := managedEnvFile(a.Name)
		switch {
		case block.EnvFile == "":
			return refused("app %s has secrets and no env_file: add env_file %s to its block", a.Name, want)
		case filepath.Clean(block.EnvFile) != want:
			return refused("app %s has secrets, and its env_file is not %s: set it", a.Name, want)
		}
		for _, v := range a.Vars {
			switch {
			case v.Name == "SOCKET":
				return refused("secret SOCKET for app %s: motel sets SOCKET last, so it would never reach the app", a.Name)
			case block.hasEnv(v.Name):
				return refused("app %s sets %s with env and as a secret; env wins over env_file, so the secret would never reach it", a.Name, v.Name)
			}
		}
	}
	for _, name := range shape.Apps {
		file := shape.AppEnv[name].EnvFile
		if file == "" {
			continue
		}
		clean := filepath.Clean(file)
		rest, ok := strings.CutPrefix(clean, secretsDir+"/")
		if !ok && clean != secretsDir { // the directory itself is a mistake too
			continue
		}
		other, isEnv := strings.CutSuffix(rest, ".env")
		switch {
		case isEnv && other == name:
			if !has[name] {
				return refused("app %s's env_file is %s, and %s/%s/ holds no secret: add one, or remove the env_file line", name, clean, dir, name)
			}
		case isEnv && other == boxSlot:
			return refused("app %s's env_file is the box's own secrets file, which notify reads and no app is given: use %s", name, managedEnvFile(name))
		case isEnv && appName.MatchString(other):
			return refused("app %s's env_file is app %s's secrets file", name, other)
		default:
			return refused("app %s's env_file is in %s, where the box keeps each app's own file: use %s", name, secretsDir, managedEnvFile(name))
		}
	}
	return nil
}

// boxSlotFits holds the box's own slot, secrets/_box/, to the file
// beside it: notify's secret names alone, and a notify block that reads
// them from the file the box writes. Like the rest of secretsFit, a
// courtesy against a signer's mistake. A secrets_file line is compared
// as a path and never quoted back, as an env_file is.
func boxSlotFits(shape *Shape, a proof.SecretApp, dir string) error {
	for _, v := range a.Vars {
		if !boxVar.MatchString(v.Name) {
			return refused("%s/%s/%s.age: the box's own slot holds notify's channel URLs alone, each named NOTIFY_ then capitals, digits and underscores", dir, boxSlot, v.Name)
		}
	}
	switch {
	case shape.Notify == nil:
		return refused("%s/%s/ holds the box's notify secrets, and the new Caddyfile has no notify block to read them: add one, or remove the directory", dir, boxSlot)
	case shape.Notify.SecretsFile != "" && filepath.Clean(shape.Notify.SecretsFile) != managedEnvFile(boxSlot):
		return refused("%s/%s/ is written to %s, and the notify block's secrets_file is another file: remove the secrets_file line", dir, boxSlot, managedEnvFile(boxSlot))
	}
	return nil
}

// boxSecretNames are the variables of the box's own slot in s, in the
// commit's order: the names `motel validate` is told the push carries
// (NotifyPushedSecretsEnv).
func boxSecretNames(s *proof.Secrets) []string {
	var out []string
	for _, a := range s.Apps {
		if a.Name != boxSlot {
			continue
		}
		for _, v := range a.Vars {
			out = append(out, v.Name)
		}
	}
	return out
}

// notifyCarried holds a notify block that reads the box's own secrets
// file to the push: every secret a channel or heartbeat line names must
// be in secrets/_box/, and a load drops a channel whose secret is
// missing (notify/DESIGN-notify.md, "Secrets at load"): the mistake is
// told here, at the push, not found at the next boot. It reads the push
// alone, not the box: a box with no slot yet, whose _box.env an
// operator wrote by hand, is refused too though validate would read
// that file, and the remedy the refusal names, the slot, replaces the
// hand-written file (DESIGN-box.md, Accepted residuals). A
// secrets_file of the operator's own is the operator's, and nothing
// here checks it.
func notifyCarried(shape *Shape, s *proof.Secrets, dir string) error {
	n := shape.Notify
	if n == nil || (n.SecretsFile != "" && filepath.Clean(n.SecretsFile) != managedEnvFile(boxSlot)) {
		return nil
	}
	carried := map[string]bool{}
	for _, name := range boxSecretNames(s) {
		carried[name] = true
	}
	for _, name := range n.Secrets {
		if !carried[name] {
			return refused("the notify block reads secret %s, and %s/%s/ does not hold it: make secret APP=%s VAR=%s, or remove the line", proof.Bound(name), dir, boxSlot, boxSlot, proof.Bound(name))
		}
	}
	return nil
}
