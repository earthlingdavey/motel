package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Where the box keeps its state (DESIGN-box.md, "Paths, owners, and
// who may touch what"). The handler reads applied.json, which the
// applier (apply.go), `init` and `moteld box baseline` write
// with the type below, so that the reader and the writers cannot
// disagree about a field. The applier writes the results and the
// transaction record too (results.go, txn.go); the markers are the
// handler's admission's, which the applier only reads.
const (
	installedFile = "/etc/motel/Caddyfile"
	exchangeDir   = "/var/lib/motel-box"
)

// Read caps on the exchange tree (DESIGN-box.md, "Caps").
const (
	// maxApplied caps applied.json: its path is up to 4 KiB, which
	// JSON's escapes can grow sixfold.
	maxApplied = 64 << 10
	// maxBody is a bundle as posted: the gzip tarball in in/.
	maxBody = 16 << 20
	// maxMarker caps a marker, stage/<id>.auth.
	maxMarker = 4 << 10
	// maxResult caps a result, out/<id>.json, as the handler reads it;
	// the applier never writes a larger one (results.go). One push may
	// remove 8192 secrets and add 8192 others: 16384 names of some 200
	// bytes, beside apps under 1 MiB and the diff's 384 KiB.
	maxResult = 6 << 20
)

// isRequestID reports whether s is a request id: 32 lowercase hex, the
// workflow's random choice for one push (DESIGN-box.md, Glossary).
func isRequestID(s string) bool {
	return len(s) == 32 && isLowerHex(s)
}

// parseRequestID is s as the id the box uses, or ok=false if s is not
// one (isRequestID). The id is rebuilt here, each character taken from a
// constant alphabet by its value, so no byte of the caller's string —
// a header, a query — reaches a path or a journal line: only its
// meaning does. CodeQL's taint tracking sees the same break.
func parseRequestID(s string) (string, bool) {
	if !isRequestID(s) {
		return "", false
	}
	const digits = "0123456789abcdef"
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		b[i] = digits[strings.IndexByte(digits, s[i])]
	}
	return string(b), true
}

// present reports whether something stands at path, without following a
// symlink there. Unlike the applier's exists, a lookup that fails for
// any reason but ENOENT is an error: the handler answers it as the box's
// trouble, never as a yes.
func present(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// isDigest reports whether s is a sha256 digest as the box writes one:
// 64 lowercase hex.
func isDigest(s string) bool {
	return len(s) == 64 && isLowerHex(s)
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// applied is applied.json, the baseline: the commit the box runs, and
// the env files it manages — the only ones it may remove, at the digests
// the fast path and the out-of-band flag compare. Both secrets fields
// are absent in a file a box without secrets wrote, which reads as none.
type applied struct {
	SHA         string            `json:"sha"`
	Path        string            `json:"path"`
	SHA256      string            `json:"sha256"`
	Signer      string            `json:"signer"`
	When        time.Time         `json:"when"`
	SecretsTree string            `json:"secrets_tree,omitempty"`
	Secrets     map[string]string `json:"secrets,omitempty"`
	// Found is the installed Caddyfile's digest as `moteld box
	// baseline` found it, beside the sha256 it keeps from the record
	// before; only GET / reads it, to say a console edit since the
	// baseline. No other writer sets it, and a baseline of a release
	// before it has none.
	Found string `json:"found_sha256,omitempty"`
}

var errNotRegular = errors.New("not a regular file")

// readFile reads one file whole, at most limit bytes: opened without
// blocking (a FIFO standing at the name cannot hang the request) and,
// unless follow, without following a symlink in the last component;
// held to a regular file by the descriptor it opened; read through a
// reader that stops one byte past the cap, since a stat is not a
// bound. A missing file is fs.ErrNotExist, for the caller to tell
// from the rest.
func readFile(path string, limit int64, follow bool) ([]byte, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !follow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0) //nolint:gosec // a fixed path of the box's
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", path, errNotRegular)
	}
	return readCapped(f, path, limit)
}

// readCapped reads r to its end, at most limit bytes: through a reader
// that stops one byte past the cap, so more is refused, never cut.
func readCapped(r io.Reader, name string, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, &tooLargeError{name: name, limit: limit}
	}
	return b, nil
}

// tooLargeError is a read that reached its cap: the applier refuses a
// bundle for it by name.
type tooLargeError struct {
	name  string
	limit int64
}

func (e *tooLargeError) Error() string {
	return fmt.Sprintf("%s: larger than %d bytes", e.name, e.limit)
}

// readApplied reads applied.json, refusing a symlink at its name (the
// exchange tree's directories come from tmpfiles.d, in a base
// directory the motel uid cannot write).
func readApplied(dir string) (*applied, error) {
	path := filepath.Join(dir, "applied.json")
	b, err := readFile(path, maxApplied, false)
	if err != nil {
		return nil, err
	}
	var a applied
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := a.valid(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &a, nil
}
