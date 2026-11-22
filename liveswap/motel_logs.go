package liveswap

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// logsReadsAtOnce bounds the logs reads the process runs at once, for
// every app and both webhooks (a failed deploy's tail, journal.go, takes
// none): each is a journalctl of 11 to 13 MB
// (measured) and the read's own few MB in moteld, for at most
// journalTailTimeout, on a unit with no MemoryMax. A read that finds
// them all taken waits logsSlotWait for one, then is told to come back.
const (
	logsReadsAtOnce = 4
	logsSlotWait    = time.Second
)

var logsSlots = make(chan struct{}, logsReadsAtOnce)

// takeLogsSlot is one of logsSlots, waited for at most logsSlotWait, and
// the function that gives it back; false when none came, or the request
// ended first.
func takeLogsSlot(ctx context.Context) (func(), bool) {
	give := func() { <-logsSlots }
	select {
	case logsSlots <- struct{}{}:
		return give, true
	default:
	}
	t := time.NewTimer(logsSlotWait)
	defer t.Stop()
	select {
	case logsSlots <- struct{}{}:
		return give, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

// The logs row's own sentences: fixed words, naming nothing of the
// request's or the app's.
const (
	logsBusyMessage   = "the box is running as many logs reads as it runs at once; retry after 1 s"
	logsUnreadMessage = "the app's journal could not be read; the box's journal says why"
)

// appLogs is GET /_motel/apps/<app>/logs[?lines=][&after=]: the handle's
// app's own lines from the box's journal, for a person whose grants give
// the app's logs (Decide's, before the app was looked for). lines is the
// most to send, of wire.LogsLines; after, a cursor an earlier answer
// gave, held to journalCursor, makes the read a follow (logs.go,
// readLogs). An app that keeps its output on the box (deploy_log_lines
// 0) is answered kept "off" and nothing is read. Every read that sends
// lines is a line in the box's journal: who, the app, how many, and the
// cursors of the first and the last, from which the operator can read on
// the box exactly what was sent (journalctl --cursor=<first>).
//
// What a reader is sent is what the app printed: its customers' data and
// the secrets it fetched while running, which no filter can know
// (DESIGN-threat-model.md). The filter is a net under who may read, not
// the safeguard: each line's text passes the app's filter here, once,
// as text, where it enters the answer (door.logLine, redact.go). A
// line's time and release, the cursor and the words kept, more and gap
// are moteld's own, and pass none. With the app's env_file unreadable
// each line's text is the filter's sentence and the rest is sent. The
// route's refusals are fixed sentences of moteld's: journalctl's own
// words go to the box's journal alone. No pass runs over the body.
func (h *Handler) appLogs(w http.ResponseWriter, r *http.Request, app appHandle, q appQuery) error {
	c := app.ma.snapshot()
	if c.spec == nil || c.spec.deployLogLines == 0 {
		// Motel's words alone: no filter of the app's is needed.
		return RespondJSON(w, http.StatusOK, wire.Logs{Kept: wire.KeptOff, Lines: []wire.LogLine{}})
	}
	give, ok := takeLogsSlot(r.Context())
	if !ok {
		w.Header().Set("Retry-After", "1")
		return RespondJSON(w, http.StatusServiceUnavailable, wire.Error{Error: logsBusyMessage})
	}
	given := false
	giveBack := func() {
		if !given {
			given = true
			give()
		}
	}
	defer giveBack()
	n, _ := strconv.Atoi(wire.LogsLines[0])
	if v, err := strconv.Atoi(q["lines"]); err == nil && slices.Contains(wire.LogsLines, q["lines"]) {
		n = v
	}
	src := c.logs
	if src == nil {
		src = journalctlLogs{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), journalTailTimeout)
	defer cancel()
	read, err := readLogs(ctx, src, logsQuery{app: app.ma.name, ident: syslogIdentifier(app.ma.name), uid: os.Getuid(), n: n, after: q["after"]})
	if err != nil && r.Context().Err() != nil {
		// The client went away, and its read with it: nothing to say.
		return nil
	}
	if err != nil {
		h.logger.Error("logs: an app's journal could not be read", zap.String("app", app.ma.name), zap.String("person", app.who), zap.Error(err))
		return RespondJSON(w, http.StatusInternalServerError, wire.Error{Error: logsUnreadMessage})
	}
	if read.passedOver > 0 {
		// None an app makes (logs.go): a process of motel's uid outside
		// every sandbox wrote under the app's identifier.
		h.logger.Warn("logs: lines under an app's identifier from a unit that is not the app's were not sent",
			zap.String("app", app.ma.name), zap.Int("lines", read.passedOver), zap.String("units", read.passedUnit))
	}
	// The releases the lines name are versions from unit names, valid
	// as versions (unitRelease): names the heuristics leave alone.
	var releases []string
	rels := make([]string, len(read.lines))
	for i, e := range read.lines {
		rels[i], _ = unitRelease(e.userUnit, app.ma.name)
		if rels[i] != "" && !slices.Contains(releases, rels[i]) {
			releases = append(releases, rels[i])
		}
	}
	d := app.ma.door(releases...)
	body := wire.Logs{Kept: wire.KeptOn, Lines: make([]wire.LogLine, len(read.lines)), More: read.more, Gap: read.gap, Cursor: read.cursor}
	for i, e := range read.lines {
		body.Lines[i] = wire.LogLine{At: e.at, Release: rels[i], Text: d.logLine(e.text)}
	}
	body.RedactedEnv = d.keys()
	// At Warn, as the refusals are: a level that quiets moteld's Info
	// lines leaves the record of who read what. None when the env_file
	// could not be read: no line's text was sent.
	if len(read.lines) > 0 && d.unread == "" {
		fields := []zap.Field{zap.String("person", app.who), zap.String("app", app.ma.name), zap.Int("lines", len(read.lines)),
			zap.String("first", read.lines[0].cursor), zap.String("last", read.lines[len(read.lines)-1].cursor)}
		if read.gap {
			fields = append(fields, zap.Bool("gap", true))
		}
		if len(body.RedactedEnv) > 0 {
			fields = append(fields, zap.Strings("redacted_env", body.RedactedEnv))
		}
		h.logger.Warn("access: person read logs", fields...)
	}
	// The slot bounds the journal's children and the filter's work, not
	// a client's pace: a reader that stops reading holds no slot.
	giveBack()
	return RespondJSON(w, http.StatusOK, body)
}

// logsCutMargin is the least the filter sees of a cut line past what is
// sent: the reach of a credential the shape and entropy layers know,
// whose rule needs its end (a PEM block of an RSA key of 4,096 bits on
// one line, about 3.3 KB; a JWT's three segments; a provider token).
// A line is at most 48 KiB as journald stores it, so the filter reads
// at most 12 KiB of one, 16 such lines an answer.
const logsCutMargin = 8 << 10

// tokenAlphabet is layer 4's (entropyTokenRe in redact.go).
const tokenAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+=_-"

// commonStart is the bytes a and b begin with alike, ended on a rune's
// start: a rune they share the first bytes of and not the rest is not
// kept.
func commonStart(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return cutText(a, n)
}

// cutText is s cut to at most n bytes, on a rune's start when one is
// within a rune's length before n; at n otherwise (bytes that are no
// UTF-8, a binary dump's run of continuation bytes, which ToValidUTF8
// makes U+FFFD after).
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for i := n; i >= 0 && i > n-utf8.UTFMax; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return s[:n]
}

// refusedLogs is when each person was last told refused an app's logs,
// by person and app: the journal line is written at most once a minute
// for each app of this box, and once a minute for every name that is no
// app here together, as a throttled person's is. Entries older than a
// minute go once the map holds refusedLogsKeep.
var refusedLogs = struct {
	sync.Mutex
	last map[[2]string]time.Time
}{last: map[[2]string]time.Time{}}

const refusedLogsKeep = 256

// logsNow is the clock of the refusals' minute.
var logsNow = time.Now

// logsRefused writes a journal line for a person Decide refused an
// app's logs: the person, the app the path named (an app or not, as
// Decide does not ask), the address the grants' lists were held to, and
// what was needed. A logs grant with a from list, asked from outside it,
// is told only here. A person so writes at most one line a minute for
// each app and one more: a name that is no app here (any the grammar
// takes) shares the key "", which no app's name is.
func (h *Handler) logsRefused(r *http.Request, person, app string, err error) {
	now := logsNow()
	key := [2]string{person, ""}
	if h.app.managedApp(app) != nil {
		key[1] = app
	}
	refusedLogs.Lock()
	if at, ok := refusedLogs.last[key]; ok && now.Sub(at) < time.Minute {
		refusedLogs.Unlock()
		return
	}
	refusedLogs.last[key] = now
	if len(refusedLogs.last) > refusedLogsKeep {
		for k, at := range refusedLogs.last {
			if now.Sub(at) >= time.Minute {
				delete(refusedLogs.last, k)
			}
		}
	}
	refusedLogs.Unlock()
	addr, why := clientAddr(r)
	shown := why
	if addr.IsValid() {
		shown = addr.String()
	}
	needs := "forbidden"
	var fb *access.Forbidden
	if errors.As(err, &fb) {
		needs = fb.Needs + " " + fb.Scope
	}
	h.logger.Warn("access: person refused logs", zap.String("person", person), zap.String("app", app), zap.String("address", shown), zap.String("needs", needs))
}
