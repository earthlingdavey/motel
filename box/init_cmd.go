package box

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/smallhoursorg/motel/box/proof"
)

// runInitCmd is `moteld init <sha>` (DESIGN-box.md, "`moteld
// init`"), run as root inside the box's directory, dir: the Caddyfile
// there read once, walked, its path derived from its host, then the
// transaction with origin init, whose reload is the only check of the
// file. Every refusal before the transaction writes nothing; the exit
// status is 0 only when the box runs what was asked, applied or
// no_change.
func runInitCmd(ctx context.Context, euid int, a *Applier, dir, sha string, out io.Writer) error {
	if ctx == nil { // as runApply: a RunE called without cobra's Execute
		ctx = context.Background()
	}
	if euid != 0 {
		return errors.New("moteld init runs as root: it installs " + installedFile)
	}
	if err := commitArg(sha); err != nil {
		return err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// One buffer from the read to the install. The directory is the
	// operator's own, so a symlink at the name is followed.
	file, err := readFile(filepath.Join(dir, "Caddyfile"), proof.MaxCaddyfile, true)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("no Caddyfile here; run init from inside the box's directory")
	}
	if err != nil {
		return err
	}
	shape, err := Walk(file)
	if err != nil {
		return err
	}
	// The path is derived, so the directory must be the one it names:
	// the wrong box's directory, or one misnamed, is refused before
	// anything is touched rather than at the first push.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if name := filepath.Base(resolved); name != shape.Host {
		return fmt.Errorf("this directory is %s, and the Caddyfile is for %s: a box's directory is named for its host, so run init from %s/ in a copy of the repository", proof.Bound(name), shape.Host, shape.Host)
	}
	path := shape.Host + "/Caddyfile" // a bare hostname: one safe component
	if err := printf(out, "init: box_webhook on %s; %s; %s\n"+
		"init: this box's file is %s in the repository\n",
		shape.Host, counted(shape.Apps, "app"), counted(principals(shape.Signers), "signer"), path); err != nil {
		return err
	}
	a.lockWait = func() {
		_ = printf(out, "init: waiting for root's lock: the applier, or another init or baseline, holds it\n") // a notice; the outcome is what counts
	}
	// From the file the transaction replaces, before the swap: a file
	// that walks as no box's (the packaged one) names no host to warn
	// about.
	a.initPrev = func(prev []byte) {
		if was, err := Walk(prev); err == nil && was.Host != shape.Host {
			_ = printf(out, "init: this box was %s; this file names %s\n", was.Host, shape.Host) // a warning; the outcome is what counts
		}
	}
	run, err := a.runInit(ctx, record{ID: newRequestID(), Commit: sha, Path: path, BoxWebhook: shape.Host, Apps: shape.Apps}, file)
	var notUp *notUpError
	if errors.As(err, &notUp) {
		return errors.New(notUp.msg + "\n" + fmt.Sprintf(msgInitRemedy, sha, shellWord(dir)))
	}
	if err != nil {
		return err
	}
	if run.result == nil { // every end of install sets one; a nil is a bug
		return errors.New("init: the transaction ended with no outcome; journalctl says what it did")
	}
	commits := "this box applies commits descending from " + shortSHA(sha)
	if run.was != nil && run.was.SHA != sha {
		commits = "this box ran " + shortSHA(run.was.SHA) + "; it now applies commits descending from " + shortSHA(sha)
	}
	switch run.Phase {
	case phaseApplied:
		if err := printf(out, "init: installed %s\ninit: %s\n"+
			"init: reloaded. From here, this box takes config only as signed commits sent by your repository's workflow.\n",
			installedFile, commits); err != nil {
			return err
		}
	case phaseNoChange:
		if run.was != nil && run.was.SHA != sha {
			if err := printf(out, "init: %s\n", commits); err != nil {
				return err
			}
		}
		if err := printf(out, "init: unchanged; the baseline is now %s\n", shortSHA(sha)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s: %s", run.Phase, run.Error)
	}
	return initKey(a, out)
}

// initKey is init's last step (DESIGN-box.md, "The box's key"): the
// box's key for secrets, made if there is none, and its line. The
// install stands whatever happens here, so a failure is a warning and
// the exit status stays 0.
func initKey(a *Applier, out io.Writer) error {
	recipient, _, err := a.boxKey()
	if err != nil {
		return printf(out, "init: could not make the box's key for secrets: %s; moteld box key, as root, makes it\n", proof.Bound(err.Error()))
	}
	return printf(out, "init: secrets for this box encrypt to %s; moteld box key prints it again\n", recipient)
}

// counted is "no apps", "1 app (example)", "2 apps (a, b)": names held
// to a grammar (app names, principals), so they print as they are.
func counted(names []string, noun string) string {
	switch len(names) {
	case 0:
		return "no " + noun + "s"
	case 1:
		return "1 " + noun + " (" + names[0] + ")"
	}
	return fmt.Sprintf("%d %ss (%s)", len(names), noun, strings.Join(names, ", "))
}

func principals(s proof.Signers) []string {
	out := make([]string, len(s))
	for i, sg := range s {
		out[i] = sg.Principal
	}
	return out
}

// newRequestID is an id for init's own transaction: the swap's
// temporary and the record are named for it, as a push's are.
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails: crypto/rand crashes the program instead (Go 1.24)
	return hex.EncodeToString(b)
}

// shellWord is s as one word for the operator's shell: as it is when
// every byte is one no shell treats specially, else single-quoted.
func shellWord(s string) string {
	const plain = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+@"
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(plain, s[i]) < 0 {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
