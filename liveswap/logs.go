package liveswap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// An app's logs, as GET /_motel/apps/<app>/logs reads them for a person
// (motel_logs.go): the app's own lines from the motel user's journal,
// read entry by entry from journalctl's JSON, every read bounded. This
// is apart from journal.go's tail, which a failed deploy's response
// carries, and reads differently: by the app's identifier, not a
// launch's units, so that a crash loop's lines, which journald stores
// with no unit when the process was gone before it read them, are not
// lost.
//
// Which lines are the app's (logsArgs): those stored under the app's
// identifier (syslogIdentifier, every unit of the app writes under it),
// from the uid moteld runs as, which the app's units run as, by the
// stdout transport, the streams the manager opens for a unit's stdout
// and stderr. The identifier is what a stream's writer names, so two
// things hold it to the app: an app's sandbox never reaches journald's
// sockets (sandboxNeverReachable), so its only stream is its unit's,
// under its own identifier; and motel's own units of the uid write
// under identifiers no app's name makes (probeIdentifier, and
// backups/unit's motel_backup). Under both, an entry whose unit fields,
// which journald sets and no writer can, name a unit that is not one of
// the app's is passed over and not sent (belongsTo): no app makes one,
// and a process of the uid outside every sandbox (T5 in
// DESIGN-threat-model.md) can.

const (
	// logsLineMax is the most of a line's text an answer sends, after the
	// filter; a longer one is cut and ends in "…".
	logsLineMax = 4096
	// logsTextMax is the most text one answer holds: a read stops before
	// the line that would pass it, each line counted at most logsLineMax.
	logsTextMax = 64 << 10
	// logsEntryMax bounds one entry of journalctl's output. journald
	// stores at most 48 KiB of a line (LineMax=), which -o json writes,
	// as a byte array when it is not UTF-8, in at most four bytes a byte;
	// the fields asked for beside it are under 1 KiB. Past it the read
	// fails, named: an operator raised LineMax.
	logsEntryMax = 256 << 10
)

// logsFields are the fields a read asks journalctl for. Every entry
// carries __CURSOR and __REALTIME_TIMESTAMP whatever is asked.
const logsFields = "MESSAGE,_SYSTEMD_UNIT,_SYSTEMD_USER_UNIT"

// logsArgs is journalctl's argv for one read: of the app's identifier,
// moteld's uid and a count of the row's set, all the box's own; and
// after, a cursor held to journalCursor, one argument whatever it holds.
// A tail (after "") reads the newest n+1 entries, newest first, so that
// the read stops as soon as it has what it sends (a tail of 1,001 lines
// of 30 KB read whole costs journalctl 98 MB and 339 ms, the 17 the cap
// needs 12 MB and 9 ms, measured). A follow reads from the cursor's own
// entry, which the read checks is the one sent (readLogs), then n lines
// and one more. --all: without it journalctl prints a MESSAGE of 4 KiB
// or more as null.
func logsArgs(ident string, uid, n int, after string) []string {
	args := []string{"--user", "--no-pager", "--quiet", "-o", "json", "--all", "--output-fields=" + logsFields}
	if after == "" {
		args = append(args, "-r", "-n", strconv.Itoa(n+1))
	} else {
		args = append(args, "--cursor="+after, "-n", "+"+strconv.Itoa(n+2))
	}
	return append(args, "SYSLOG_IDENTIFIER="+ident, "_UID="+strconv.Itoa(uid), "_TRANSPORT=stdout")
}

// logEntry is one entry as read: when the journal stored it, the units
// journald attributed it to ("" for none), its text as valid UTF-8, and
// its cursor.
type logEntry struct {
	at             time.Time
	unit, userUnit string
	text           string
	cursor         string
}

// errLogsEntry is an entry of journalctl's output that is not one as
// the read asked for it: no cursor, no time, a MESSAGE that is neither
// a string nor bytes.
var errLogsEntry = errors.New("an entry journalctl printed is not one")

// errLogsEntryTooLong is an entry over logsEntryMax.
var errLogsEntryTooLong = fmt.Errorf("an entry of journalctl's output is over %d bytes: is journald's LineMax= raised?", logsEntryMax)

// decodeEntry reads one entry of -o json. MESSAGE is a string, or an
// array of byte values when it is not UTF-8 or holds a control
// character, each run of whose bytes that are not UTF-8 becomes one
// U+FFFD. A MESSAGE
// that is absent or null is an error, never an empty line: with --all
// journalctl prints every MESSAGE.
func decodeEntry(raw []byte) (logEntry, error) {
	var e struct {
		Cursor   string          `json:"__CURSOR"`
		Realtime string          `json:"__REALTIME_TIMESTAMP"`
		Message  json.RawMessage `json:"MESSAGE"`
		Unit     json.RawMessage `json:"_SYSTEMD_UNIT"`
		UserUnit json.RawMessage `json:"_SYSTEMD_USER_UNIT"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Cursor == "" {
		return logEntry{}, errLogsEntry
	}
	us, err := strconv.ParseInt(e.Realtime, 10, 64)
	if err != nil || us < 0 {
		return logEntry{}, errLogsEntry
	}
	text, ok := entryText(e.Message)
	if !ok {
		return logEntry{}, errLogsEntry
	}
	unit, ok := entryField(e.Unit)
	if !ok {
		return logEntry{}, errLogsEntry
	}
	userUnit, ok := entryField(e.UserUnit)
	if !ok {
		return logEntry{}, errLogsEntry
	}
	return logEntry{at: time.UnixMicro(us).UTC(), unit: unit, userUnit: userUnit, text: text, cursor: e.Cursor}, nil
}

// entryText is a MESSAGE as -o json writes it: a string, or an array of
// byte values.
func entryText(raw json.RawMessage) (string, bool) {
	switch {
	case len(raw) == 0 || raw[0] == 'n':
		return "", false
	case raw[0] == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return strings.ToValidUTF8(s, "\uFFFD"), true
	case raw[0] == '[':
		var nums []int
		if json.Unmarshal(raw, &nums) != nil {
			return "", false
		}
		b := make([]byte, len(nums))
		for i, v := range nums {
			if v < 0 || v > 255 {
				return "", false
			}
			b[i] = byte(v)
		}
		return strings.ToValidUTF8(string(b), "\uFFFD"), true
	}
	return "", false
}

// entryField is a unit field: absent, or a string. journald sets each
// once, so anything else (an array of several) is no entry the read
// asked for.
func entryField(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// readEntry is one line of journalctl's output, without its newline, of
// at most limit bytes; io.EOF with what was left when the output ends.
func readEntry(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > limit+1 {
			return nil, errLogsEntryTooLong
		}
		buf = append(buf, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSuffix(buf, []byte("\n")), err
	}
}

// logsSource is the seam: the real one runs journalctl, tests script
// one.
type logsSource interface {
	// entries runs journalctl with args and calls each with every entry
	// it prints, in order, until each says stop or the output ends; a
	// stop ends journalctl.
	entries(ctx context.Context, args []string, each func(raw []byte) (more bool)) error
}

// journalctlLogs reads the motel user's journal through journalctl,
// with an empty environment (journalctlCommand), its output read
// through a pipe a bounded entry at a time, so journalctl runs no
// further ahead than the pipe holds, and is killed once the read has
// what it sends.
type journalctlLogs struct{}

func (journalctlLogs) entries(parent context.Context, args []string, each func([]byte) bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cmd := journalctlCommand(ctx, args)
	stderr := &tailWriter{max: 1024}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("journalctl: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("journalctl: %w", err)
	}
	br := bufio.NewReaderSize(out, 64<<10)
	stopped, read := false, 0
	var readErr error
	for {
		raw, err := readEntry(br, logsEntryMax)
		if len(raw) > 0 {
			read++
			if !each(raw) {
				stopped = true
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	if stopped || readErr != nil {
		cancel() // the rest is not read: journalctl is killed
	}
	werr := cmd.Wait()
	switch {
	case readErr != nil:
		return readErr
	case stopped:
		return nil
	case parent.Err() != nil:
		return fmt.Errorf("journalctl: %w", parent.Err())
	case werr == nil:
		return nil
	}
	if msg := strings.TrimSpace(string(stderr.buf)); msg != "" {
		return fmt.Errorf("journalctl: %s", msg)
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) && ee.ExitCode() == 1 && read == 0 {
		// journalctl's word for a read that matched no entry.
		return nil
	}
	return fmt.Errorf("journalctl: %w", werr)
}

// logsQuery is one read: the app, its identifier, the uid its units run
// as, the count of the row's set, and the cursor to follow from ("" for
// a tail).
type logsQuery struct {
	app, ident string
	uid, n     int
	after      string
}

// logsRead is what one read found: the lines to send, oldest first; the
// cursor of the last entry the read took ("" when it took none); more,
// when it stopped with an entry left unread or journalctl gave every
// entry asked for; gap, when the cursor was not honoured and the lines
// are a fresh tail; and how many entries it passed over (belongsTo).
type logsRead struct {
	lines      []logEntry
	cursor     string
	more, gap  bool
	passedOver int
	// passedUnit is the unit field of the first entry passed over, for
	// the journal.
	passedUnit string
}

// readLogs is one read's rules. A follow (q.after set) reads from the
// cursor's own entry, and is honoured only when that entry's cursor is
// the one sent, byte for byte: journalctl seeks by any time or sequence
// number a cursor names, a made-up one among them, and lands on the next
// entry of the app's after another app's cursor or a vacuumed one. Not
// honoured, the answer is a fresh tail with gap: what any tail gives, in
// one round trip. A follow keeps the oldest lines after the cursor, a
// tail the newest, each up to q.n lines and logsTextMax of text; the
// cursor is the last entry taken, so a follower's next read begins
// after it and no line is skipped or sent twice.
func readLogs(ctx context.Context, src logsSource, q logsQuery) (logsRead, error) {
	if q.after != "" {
		read, honoured, err := takeLogs(ctx, src, q, q.after)
		if err != nil || honoured {
			return read, err
		}
	}
	read, _, err := takeLogs(ctx, src, q, "")
	read.gap = q.after != ""
	return read, err
}

// takeLogs is one run of journalctl: a follow from after, or a tail.
// honoured is whether a follow's first entry was the cursor sent.
func takeLogs(ctx context.Context, src logsSource, q logsQuery, after string) (read logsRead, honoured bool, err error) {
	limit := q.n + 1 // the lines, and one more to know there is more
	if after != "" {
		limit++ // the cursor's own entry first
	}
	seen, text := 0, 0
	var bad error
	first := after != ""
	err = src.entries(ctx, logsArgs(q.ident, q.uid, q.n, after), func(raw []byte) bool {
		e, err := decodeEntry(raw)
		if err != nil {
			bad = err
			return false
		}
		seen++
		if first {
			first, honoured = false, e.cursor == after
			return honoured
		}
		if len(read.lines) == q.n {
			read.more = true
			return false
		}
		if !belongsTo(e, q.app, q.uid) {
			if read.passedOver == 0 {
				read.passedUnit = e.unit + " " + e.userUnit
			}
			read.passedOver++
			if after != "" || read.cursor == "" {
				read.cursor = e.cursor
			}
			return true
		}
		size := min(len(e.text), logsLineMax)
		if text+size > logsTextMax {
			read.more = true
			return false
		}
		text += size
		read.lines = append(read.lines, e)
		if after != "" || read.cursor == "" {
			read.cursor = e.cursor // a follow: the last taken; a tail: the newest
		}
		return true
	})
	if bad != nil {
		return logsRead{}, false, bad
	}
	if err != nil {
		return logsRead{}, false, err
	}
	if after != "" && !honoured {
		return logsRead{}, false, nil
	}
	// Every entry asked for came, and some were passed over: there may be
	// more than journalctl was asked for.
	read.more = read.more || seen == limit
	if after == "" {
		// Read newest first; sent oldest first.
		for i, j := 0, len(read.lines)-1; i < j; i, j = i+1, j-1 {
			read.lines[i], read.lines[j] = read.lines[j], read.lines[i]
		}
	}
	// A cursor given out is one the next read takes: never one the key's
	// grammar would refuse, which a later systemd's could be.
	if read.cursor != "" && !journalCursor.re.MatchString(read.cursor) {
		return logsRead{}, false, fmt.Errorf("journalctl gave a cursor not of the form a read takes: %.160q", read.cursor)
	}
	return read, true, nil
}

// belongsTo is whether an entry's units, which journald sets from the
// writer's cgroup and no writer can, are none, or the app's: a user unit
// one of the app's (the manager's own process is user@<uid>.service on
// the system manager). An entry journald stored with no unit, a process
// gone before it was read, is the app's by the match fields alone.
func belongsTo(e logEntry, app string, uid int) bool {
	if e.userUnit != "" && !unitBelongsTo(e.userUnit, app) {
		return false
	}
	return e.unit == "" || e.unit == "user@"+strconv.Itoa(uid)+".service"
}
