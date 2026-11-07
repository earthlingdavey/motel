package liveswap

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The app's own output goes to the journal (StandardOutput=journal on
// the unit, see systemd_dbus.go), and a failed deploy's response
// carries the last lines of it: what a migration printed before it
// exited 3, the stack trace of an app that died on start. Reading the
// journal back means running journalctl — the product binary is built
// without cgo, so the sd-journal library is out — which is the one
// external program motel executes. The argument lists are built
// here, of unit names, a pid the manager recorded, stream ids journald
// printed, a count and a timestamp; the output is bounded
// (deployLogMaxBytes, and the line count the operator sets with
// deploy_log_lines); a journalctl that is missing or fails leaves the
// tail out and says why.
//
// Everything read here passes the response filter (redact.go) like
// any other body, and deploy_log_lines 0 keeps app output on the box
// entirely: no journal tail, and no health probe body either
// (failureDetail, app.go) — the exit status and the probe's status
// code are motel's observations, not the app's bytes, and stay.

// journalReader is the seam: the real reader runs journalctl, tests
// script one.
type journalReader interface {
	// tail returns up to n of the most recent lines a launch's units
	// wrote since a point in time, oldest first.
	tail(ctx context.Context, of tailOf, since time.Time, n int) ([]string, error)
}

// tailOf is whose lines a tail reads: a launch's units; the pid of
// the main process of the pre_start or app that failed (runner.Exit,
// exitError; 0 where none is known); and the identifier every unit of
// the app writes under.
type tailOf struct {
	units []string
	pid   int
	ident string
}

// journalctlPath is where Debian's systemd package puts journalctl —
// an absolute path, so what runs never depends on PATH.
const journalctlPath = "/usr/bin/journalctl"

// journalctlReader reads the motel user's own journal, where the
// units on its user manager write.
//
// journald names a line's unit from /proc/<pid> of the process that
// wrote it, when it gets to the line, and a line whose writer was
// reaped first is stored with no unit field at all: a child that
// prints its error and exits, every time; any line, while journald is
// behind. What every line a unit writes to stdout or stderr does
// carry, its children's included, is the unit's one stream: journald
// gives the stream its id, a client cannot set it, and only the
// unit's own processes hold the stream. So the tail is read in two
// runs: the streams first, from the lines that kept their unit and
// from those the failed process wrote — by its pid, as the manager
// recorded it, under the app's identifier — and then every line of
// those streams, with any line journald attributed to the units
// otherwise. A stream none of whose lines did either stays unknown,
// and its lines out of the tail: a wrapper script that runs its app
// without exec and prints nothing itself, when the app prints its
// error and exits; a pre_start that succeeded, every line of it read
// late (its pid is gone with its unit).
type journalctlReader struct{}

func (journalctlReader) tail(ctx context.Context, of tailOf, since time.Time, n int) ([]string, error) {
	if n <= 0 || len(of.units) == 0 {
		return nil, nil
	}
	found, err := runJournalctl(ctx, streamArgs(of, since, n), streamsMaxBytes)
	if err != nil {
		return nil, err
	}
	// The line count bounds the entries, not their size: a single entry
	// can be as long as journald lets a line be. The output streams
	// through a writer that keeps only the last bytes, more than the
	// response cap so that capTail still sees "there was more", and
	// memory is fixed whatever the app wrote. A dropped prefix leaves a
	// partial first line, whose first token the bound may have cut in
	// two: that token goes (dropUnfinished), since a fragment of an
	// env_file value is one the filter cannot match. One byte more is
	// read than kept, to tell an output of exactly the limit, which is
	// whole, from a longer one.
	const limit = 2 * deployLogMaxBytes
	out, err := runJournalctl(ctx, tailArgs(of.units, streamIDs(found), since, n), limit+1)
	if err != nil {
		return nil, err
	}
	text := boundedTail(out, limit)
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// boundedTail is the text of out, read through a tailWriter one byte
// larger than limit: an out longer than limit was cut by the read, so
// it is trimmed to limit and loses the token the cut may have split
// (dropUnfinished); one of at most limit bytes was read whole and keeps
// its first token.
func boundedTail(out []byte, limit int) string {
	cut := len(out) > limit
	if cut {
		out = out[len(out)-limit:]
	}
	text := strings.TrimRight(string(out), "\n")
	if cut {
		text = dropUnfinished(text, true)
	}
	return text
}

// dropUnfinished removes the token a read bound may have cut in two:
// the first token of s (atStart: a kept suffix) or its last (a kept
// prefix), up to or from the nearest whitespace, and puts an ellipsis
// where it was. A bound that fell inside an env_file value left a
// fragment the response filter cannot match and no heuristic can see
// (redact.go); without a whitespace to cut at, the whole of s goes. A
// value with a space in it can still be cut (DESIGN-liveswap.md,
// "Accepted residuals").
func dropUnfinished(s string, atStart bool) string {
	const marker = "…"
	if atStart {
		if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
			return marker + s[i:]
		}
		return marker
	}
	if i := strings.LastIndexAny(s, " \t\r\n"); i >= 0 {
		return s[:i+1] + marker
	}
	return marker
}

// journalWindow is what both runs share. A second of slack: the
// deploy's clock and the journal's need not agree to the millisecond,
// and a line a moment before the start is harmless where a line lost
// is not. One line more than asked, so the caller can tell "all of
// it" from "there was more" (capTail sets the flag). The streams are
// learnt in the same window: a launch's units run one after the
// other, so a stream with none of its named lines among the last n+1
// has none of its lines among the tail's last n+1 either.
func journalWindow(since time.Time, n int) []string {
	return []string{"--user", "--no-pager", "--quiet", "-n", strconv.Itoa(n + 1),
		"--since", "@" + strconv.FormatInt(since.Add(-time.Second).Unix(), 10)}
}

// streamArgs asks for the stream ids of the lines that name the
// launch: the unit field, matched directly rather than through -u
// (which also selects the manager's own lines about the unit,
// "Starting …", "Main process exited, code=exited, status=2" — what
// `exit` already says); or a main process's pid together with the
// app's identifier. Several matches of one field are OR'd, different
// fields AND'd, and "+" ORs the two groups.
func streamArgs(of tailOf, since time.Time, n int) []string {
	args := append(journalWindow(since, n), "-o", "cat", "--output-fields=_STREAM_ID")
	args = append(args, unitMatches(of.units)...)
	if of.pid > 0 && of.ident != "" {
		args = append(args, "+", "_PID="+strconv.Itoa(of.pid), "SYSLOG_IDENTIFIER="+of.ident)
	}
	return args
}

// tailArgs asks for the lines themselves: the units', and every line
// of their streams.
func tailArgs(units, streams []string, since time.Time, n int) []string {
	args := append(journalWindow(since, n), "-o", "cat")
	args = append(args, unitMatches(units)...)
	if len(streams) > 0 {
		args = append(args, "+")
		for _, s := range streams {
			args = append(args, "_STREAM_ID="+s)
		}
	}
	return args
}

func unitMatches(units []string) []string {
	m := make([]string, 0, len(units))
	for _, u := range units {
		m = append(m, "_SYSTEMD_USER_UNIT="+u)
	}
	return m
}

// streamsMaxBytes bounds the first run's output: an entry prints as
// its stream id and a newline, 33 bytes, and deploy_log_lines is at
// most 1000.
const streamsMaxBytes = 64 * 1024

// streamIDRe is a stream id as journald prints one.
var streamIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// streamIDs reads journalctl's output, an entry a line, for the
// distinct stream ids, in the order first met. What is not one — the
// first line cut by the byte bound, the empty line of an entry with no
// stream — is passed over: each id becomes an argument of the next run.
func streamIDs(out []byte) []string {
	var ids []string
	seen := map[string]bool{}
	for _, id := range strings.Split(string(out), "\n") {
		if !streamIDRe.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// runJournalctl runs journalctl, keeping the last limit bytes of what
// it prints.
func runJournalctl(ctx context.Context, args []string, limit int) ([]byte, error) {
	out := &tailWriter{max: limit}
	stderr := &tailWriter{max: 1024}
	cmd := exec.CommandContext(ctx, journalctlPath, args...) //nolint:gosec // a fixed program by absolute path; the arguments are validated unit names, pids the manager recorded, stream ids of journald's shape, a count and a timestamp built here, never request input
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(string(stderr.buf)); msg != "" {
			return nil, fmt.Errorf("journalctl: %s", msg)
		}
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	return out.buf, nil
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	if len(p) >= w.max {
		w.buf = append(w.buf[:0], p[len(p)-w.max:]...)
		return len(p), nil
	}
	if over := len(w.buf) + len(p) - w.max; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// deployLogMaxBytes bounds the tail in bytes whatever deploy_log_lines
// says: a response is read in a CI log, not a log viewer.
const deployLogMaxBytes = 8 * 1024

// journalTailTimeout bounds the journalctl run: a failed deploy's
// response must not hang on the journal.
const journalTailTimeout = 5 * time.Second

// capTail keeps the last lines that fit both caps, and reports whether
// anything was dropped. A last line that alone exceeds the byte cap —
// a one-line JSON stack trace — is kept as its tail with a marker,
// not dropped whole.
func capTail(lines []string, maxLines, maxBytes int) ([]string, bool) {
	truncated := false
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}
	size := 0
	for i := len(lines) - 1; i >= 0; i-- {
		size += len(lines[i]) + 1
		if size > maxBytes {
			if i == len(lines)-1 {
				const marker = "…"
				keep := maxBytes - len(marker) - 1
				if keep < 0 {
					keep = 0
				}
				// Cut on a rune boundary: a UTF-8 sequence split at
				// its start would reach the response as U+FFFD.
				start := len(lines[i]) - keep
				for start < len(lines[i]) && !utf8.RuneStart(lines[i][start]) {
					start++
				}
				lines = []string{marker + lines[i][start:]}
			} else {
				lines = lines[i+1:]
			}
			truncated = true
			break
		}
	}
	if len(lines) == 0 {
		return nil, truncated
	}
	return lines, truncated
}
