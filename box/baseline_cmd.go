package box

import (
	"context"
	"errors"
	"io"
	"io/fs"
)

// signerBaseline is applied.json's signer after a baseline, whose
// sha256 is the record before's and no claim about the commit it
// names: the mark the handler's status reads, to compare the installed
// file with found_sha256 instead.
const signerBaseline = "baseline"

// runBaseline is `moteld box baseline <sha>` (DESIGN-box.md, "`box
// baseline`"): a trust reset, for a history rewritten below the commit
// the box runs or a chain past the cap. Under root's lock, after
// recovery has settled any record, one atomic write of applied.json and
// no record (I5): sha from the argument; path and sha256 as found, so a
// hand edit made since the last apply is still out of band at the next
// push; signer "baseline"; when now; found_sha256 the installed file's
// digest, for GET /. Before init there is nothing to
// reset.
func runBaseline(ctx context.Context, euid int, a *Applier, sha string, out io.Writer) error {
	if ctx == nil { // as runApply: a RunE called without cobra's Execute
		ctx = context.Background()
	}
	if euid != 0 {
		return errors.New("moteld box baseline runs as root: it rewrites the box's baseline")
	}
	if err := commitArg(sha); err != nil {
		return err
	}
	a.lockWait = func() {
		_ = printf(out, "baseline: waiting for root's lock: the applier, or another init or baseline, holds it\n") // a notice; the outcome is what counts
	}
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.recoverAll(ctx); err != nil {
		return err
	}
	old, err := readApplied(a.x(""))
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New(msgNoBaseline)
	}
	if err != nil {
		return err
	}
	if old.SHA == sha {
		// Rewriting it would only lose who verified it, and when. A
		// retry after a write whose rename landed but whose directory
		// fsync failed comes here: the fsync is owed (I8).
		if err := syncDir(a.x("")); err != nil {
			return err
		}
		return printf(out, "baseline: this box already runs %s; nothing changed\n", shortSHA(sha))
	}
	// The env files it manages are kept as found, as the digest is: the
	// next push decides them by the full chain. The installed file's
	// digest is recorded apart, for GET / alone; one that cannot be read
	// is left out, and GET / then says nothing of an edit.
	found, _ := a.installedDigest() // "" on any error
	if err := a.putApplied(applied{SHA: sha, Path: old.Path, SHA256: old.SHA256, Signer: signerBaseline, When: a.clock.Now().UTC(),
		SecretsTree: old.SecretsTree, Secrets: old.Secrets, Found: found}); err != nil {
		return err
	}
	return printf(out, "baseline: this box ran %s; it now applies commits descending from %s\n", shortSHA(old.SHA), shortSHA(sha))
}
