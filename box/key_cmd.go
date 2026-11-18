package box

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/smallhoursorg/motel/box/proof"
)

// `moteld box key` (DESIGN-box.md, "The box's key"): the box's key
// for secrets, made when there is none, and its recipient printed. Run
// by postinstall on a live system, by init after its transaction, and
// by the console. The rules:
//
//  1. The identity is never overwritten: ensureIdentity links a new one
//     into place, so one that exists stays, and one that does not parse
//     is refused with nothing written.
//  2. age.pub is rewritten only when it is missing, names another key,
//     or is not a 0644 file of root's (the handler could not read it, or
//     another account could change it): a temporary in /etc/motel,
//     renamed over it, the directory
//     fsynced (I8), under the flock on /etc/motel/age/ that guards the
//     identity, and from the identity as read under that lock — so two
//     runs never write it from different keys; a temporary a crashed run
//     left is removed first.
//  3. Standard output is the `age1…` line alone, so that
//     `$(ssh root@box moteld box key)` is the key; what it did goes to
//     standard error.

const msgKeyNotRoot = "moteld box key runs as root: it reads the box's key for secrets"

// agePubTemp is the pattern of age.pub's temporaries, beside it (Paths).
const agePubTemp = ".age.pub.box-*"

// runKey is `moteld box key`.
func runKey(euid int, a *Applier, out, errOut io.Writer) error {
	if euid != 0 {
		return errors.New(msgKeyNotRoot)
	}
	recipient, created, err := a.boxKey()
	if err != nil {
		return err
	}
	if err := printf(out, "%s\n", recipient); err != nil {
		return err
	}
	if created {
		return printf(errOut, "box key: made %s; its key above is the first line of %s/secrets/recipients\n", identityFile, a.boxHost())
	}
	return printf(errOut, "box key: this box's key for secrets\n")
}

// boxKey makes the box's key if there is none, writes age.pub if it is
// missing or another key's, and returns the recipient. created reports
// whether this call made the identity.
func (a *Applier) boxKey() (recipient string, created bool, err error) {
	if _, created, err = ensureIdentity(filepath.Join(a.root, identityFile), a.clock.Now()); err != nil {
		return "", created, err
	}
	recipient, err = a.writeAgePub()
	return recipient, created, err
}

// writeAgePub is rule 2, from the identity as it stands under the lock —
// not as ensureIdentity found it, since another run may have come
// between the two — and returns its recipient.
func (a *Applier) writeAgePub() (string, error) {
	unlock, err := lockDir(filepath.Dir(filepath.Join(a.root, identityFile)))
	if err != nil {
		return "", err
	}
	defer unlock()
	id, err := readIdentity(filepath.Join(a.root, identityFile))
	if err != nil {
		return "", err
	}
	recipient := id.Recipient().String()
	return recipient, a.putAgePub(recipient)
}

// putAgePub writes age.pub as rule 2 says, under the caller's lock.
func (a *Applier) putAgePub(recipient string) error {
	pub := filepath.Join(a.root, agePubFile)
	dir := filepath.Dir(pub)
	left, err := filepath.Glob(filepath.Join(dir, agePubTemp))
	if err != nil {
		return err
	}
	for _, name := range left {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	line := recipient + "\n"
	if fi, err := os.Lstat(pub); err == nil && fi.Mode() == 0o644 && ownedByUs(fi) {
		if b, err := readFile(pub, maxAgePub, false); err == nil && string(b) == line {
			return nil
		}
	}
	tmp, err := os.CreateTemp(dir, agePubTemp)
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(line)
	if err == nil {
		err = tmp.Chmod(0o644) // after the write: the umask has no say
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), pub)
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) // best effort: the next run removes a leftover
		return err
	}
	return syncDir(dir)
}

// ownedByUs reports whether fi is owned as a file this process makes is:
// root:root on the box, where only root runs `box key`.
func ownedByUs(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid() && int(st.Gid) == os.Getegid()
}

// boxHost is the box_webhook host of the Caddyfile the box runs, for the
// line that says where the key goes, or `<host>` when that file is no
// box's yet (the package's, before init).
func (a *Applier) boxHost() string {
	installed, err := readFile(a.caddyfile(), proof.MaxCaddyfile, true)
	if err != nil {
		return "<host>"
	}
	shape, err := Walk(installed)
	if err != nil {
		return "<host>"
	}
	return shape.Host
}
