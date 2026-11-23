package liveswap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The files motel keeps in an app's directory — the deploy records
// (deploys.go) and state.json (state.go) — are written and read by the
// two functions here, so the stores hold one set of rules rather than
// two copies of it. These names sit in the app dir itself, which no
// app can write from inside its sandbox (it writes only its release,
// `shared/` and `run/<nonce>/`), but nothing on disk says who made
// what stands at one of them or where a link there was meant to
// point: an operator's (`<root>/blog -> <root>/shop` by mistake)
// looks like any other, so following one could write or read a file
// that is not this app's (deploys.go, rule 3):
//
//   - A write goes to a fresh temp file under a random name in the
//     same directory (os.CreateTemp: O_EXCL, mode 0600 — never a fixed
//     name a link could already stand at), then is renamed over the
//     name, which replaces whatever stood there rather than writing
//     through it. A failed write removes its temp file; one interrupted
//     before the rename leaves it, for its store to remove. No fsync.
//   - A read does not follow a link at the name (O_NOFOLLOW), does not
//     block on a FIFO (O_NONBLOCK), and refuses anything but a regular
//     file, and a file larger than its store's bound: no file motel
//     wrote is.
//
// The directory is the caller's to vouch for: recordsDir checks the
// records'; state.json's is the app dir, and every save follows a
// launch whose bind sources were checked to be the app's own
// (resolveBindSources, sandbox.go).

// writeOwnFile replaces path with data under the write rule above.
// pattern names the temp file as os.CreateTemp does; it is how the
// store recognises a leftover of its own.
func writeOwnFile(path, pattern string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp) // best effort: the write's own error is the one to report
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close() // the write already failed; its error is the one to report
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeOwnLeftovers removes the temp files an interrupted
// writeOwnFile under pattern left in dir, by that pattern alone: the
// stores in an app dir each have their own. os.Remove unlinks the
// name, never what a link points at. Best effort: a leftover that
// cannot be removed costs a little disk, never a write, and a dir that
// cannot be listed is the following write's to report.
func removeOwnLeftovers(dir, pattern string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix, suffix, _ := strings.Cut(pattern, "*")
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}

// readOwnFile reads path whole under the read rule above; the
// modification time comes from the same open file as the bytes. what
// names the file in the refusal of one that is too large ("a record").
func readOwnFile(path string, maxBytes int, what string) ([]byte, time.Time, error) {
	// O_NONBLOCK as elf.go opens the command: a FIFO at the name would
	// otherwise hold the open — and whatever lock the caller holds —
	// for good.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // a path under an app's own dir, built by the caller
	if err != nil {
		// O_NOFOLLOW refuses a link at the name as ELOOP, which reads
		// as a loop; say what it is. Readlink answers only at a link
		// and follows none. (Not Lstat: gosec's G703 taint rule counts
		// Lstat as a sink and reaches this line in about half of its
		// runs — it walks a call graph built in map order — so a
		// directive here was "unused" in the other half.)
		if errors.Is(err, syscall.ELOOP) {
			if _, lerr := os.Readlink(path); lerr == nil {
				return nil, time.Time{}, fmt.Errorf("%s is a link (a planted link is not followed)", path)
			}
		}
		return nil, time.Time{}, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	if !fi.Mode().IsRegular() {
		return nil, time.Time{}, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(b) > maxBytes {
		return nil, time.Time{}, fmt.Errorf("%s is larger than %s can be", path, what)
	}
	return b, fi.ModTime(), nil
}
