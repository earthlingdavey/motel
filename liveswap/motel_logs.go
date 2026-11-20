package liveswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
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
// the safeguard: each line passes the app's filter on its own, then the
// body the final pass (filterLogs).
func (h *Handler) appLogs(w http.ResponseWriter, r *http.Request, app appHandle, q appQuery) error {
	c := app.ma.snapshot()
	if c.spec == nil || c.spec.deployLogLines == 0 {
		// Motel's words alone: no filter of the app's is needed.
		return RespondJSON(w, http.StatusOK, wire.Logs{Kept: wire.KeptOff, Lines: []wire.LogLine{}}, NewRedactor(nil, nil))
	}
	give, ok := takeLogsSlot(r.Context())
	if !ok {
		w.Header().Set("Retry-After", "1")
		return RespondJSON(w, http.StatusServiceUnavailable, wire.Error{Error: logsBusyMessage}, NewRedactor(nil, nil))
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
		return RespondJSON(w, http.StatusInternalServerError, wire.Error{Error: logsUnreadMessage}, NewRedactor(nil, nil))
	}
	if read.passedOver > 0 {
		// None an app makes (logs.go): a process of motel's uid outside
		// every sandbox wrote under the app's identifier.
		h.logger.Warn("logs: lines under an app's identifier from a unit that is not the app's were not sent",
			zap.String("app", app.ma.name), zap.Int("lines", read.passedOver), zap.String("units", read.passedUnit))
	}
	body := wire.Logs{Kept: wire.KeptOn, Lines: make([]wire.LogLine, len(read.lines)), More: read.more, Gap: read.gap, Cursor: read.cursor}
	var releases []string
	for i, e := range read.lines {
		rel, _ := unitRelease(e.userUnit, app.ma.name)
		body.Lines[i] = wire.LogLine{At: e.at, Release: rel, Text: e.text}
		if rel != "" && !slices.Contains(releases, rel) {
			releases = append(releases, rel)
		}
	}
	// The releases the lines name are versions from unit names (rule 4):
	// safe strings, as the status's are.
	f := app.ma.redactorFor(app.ma.status(), releases...)
	out, keys, ok := filterLogs(f, body)
	sent := true
	if !ok {
		// The filter withheld the body (its env_file unreadable: the
		// filter's sentence), or the final pass changed its shape: the
		// body, its lines filtered each, whole through the filter, its
		// cursor put back.
		out, keys, sent = filterLogsWhole(f, body)
	}
	// At Warn, as the refusals are: a level that quiets moteld's Info
	// lines leaves the record of who read what.
	if sent && len(read.lines) > 0 {
		fields := []zap.Field{zap.String("person", app.who), zap.String("app", app.ma.name), zap.Int("lines", len(read.lines)),
			zap.String("first", read.lines[0].cursor), zap.String("last", read.lines[len(read.lines)-1].cursor)}
		if read.gap {
			fields = append(fields, zap.Bool("gap", true))
		}
		if len(keys) > 0 {
			fields = append(fields, zap.Strings("redacted_env", keys))
		}
		h.logger.Warn("access: person read logs", fields...)
	}
	// The slot bounds the journal's children and the filter's work, not
	// a client's pace: a reader that stops reading holds no slot.
	giveBack()
	return respondFiltered(w, http.StatusOK, out)
}

// filterLogs is the logs body through the app's filter: each line's text
// through the whole filter on its own (filterLogLine), then the body
// without its cursor through the final pass alone (lastPass), which
// holds the times (rule 7), its shape checked unchanged, then the cursor
// and redacted_env, the keys any of it replaced, put back. The cursor is
// journald's position in the box's journal, of journalCursor's form,
// never the app's bytes: through the filter it is masked (32 hex
// digits), and a follower could not send it back. It reports false, for
// the caller to send the body whole through the filter, when the filter
// withholds (its env_file unreadable) or the final pass refused or
// changed the body's shape (a value of JSON's punctuation across two
// lines).
func filterLogs(f *Redactor, body wire.Logs) (string, []string, bool) {
	if f != nil && f.withhold != "" {
		return "", nil, false
	}
	seen := map[string]bool{}
	lines := make([]wire.LogLine, len(body.Lines))
	for i, l := range body.Lines {
		lines[i] = wire.LogLine{At: l.At, Release: l.Release, Text: filterLogLine(f, l.Text, seen)}
	}
	type words struct {
		Kept  string         `json:"kept"`
		Lines []wire.LogLine `json:"lines"`
		More  bool           `json:"more"`
		Gap   bool           `json:"gap"`
	}
	raw, err := json.Marshal(words{body.Kept, lines, body.More, body.Gap})
	if err != nil {
		return "", nil, false
	}
	out, ok := f.lastPass(raw, seen)
	if !ok {
		return "", nil, false
	}
	// Its shape as it went in: the same lines at the same times, the
	// same words; only the strings' text may differ.
	var got words
	dec := json.NewDecoder(bytes.NewReader([]byte(out)))
	dec.DisallowUnknownFields()
	if dec.Decode(&got) != nil || got.Kept != body.Kept || got.More != body.More || got.Gap != body.Gap || len(got.Lines) != len(lines) {
		return "", nil, false
	}
	for i := range got.Lines {
		if !got.Lines[i].At.Equal(lines[i].At) {
			return "", nil, false
		}
	}
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(out), &top) != nil {
		return "", nil, false
	}
	keys := reportedKeys(seen)
	for i, k := range keys {
		keys[i] = f.replaceKnown(k, seen, false) // a key name, not a body
	}
	// The pass's bytes as they are, in the wire type's order: no string
	// of them is written again.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(struct {
		Kept        json.RawMessage `json:"kept"`
		Lines       json.RawMessage `json:"lines"`
		More        json.RawMessage `json:"more"`
		Gap         json.RawMessage `json:"gap"`
		Cursor      string          `json:"cursor,omitempty"`
		RedactedEnv []string        `json:"redacted_env,omitempty"`
	}{top["kept"], top["lines"], top["more"], top["gap"], body.Cursor, keys}) != nil {
		return "", nil, false
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), keys, true
}

// filterLogsWhole is filterLogs' fallback: the body, each line filtered
// on its own as there, whole through the filter, as any body; then the
// keys the lines' filtering replaced added to its redacted_env, and its
// cursor put back. sent is false when the filter withheld the body (no
// line in it), and keys then nil.
func filterLogsWhole(f *Redactor, body wire.Logs) (out string, keys []string, sent bool) {
	seen := map[string]bool{}
	lines := make([]wire.LogLine, len(body.Lines))
	for i, l := range body.Lines {
		lines[i] = wire.LogLine{At: l.At, Release: l.Release, Text: filterLogLine(f, l.Text, seen)}
	}
	cursor := body.Cursor
	body.Lines, body.Cursor = lines, ""
	raw, err := json.Marshal(body)
	if err != nil {
		return `{"error":"[#0]"}`, nil, false
	}
	out = f.redactJSON(raw)
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(out), &top) != nil || top["lines"] == nil {
		return out, nil, false
	}
	names := reportedKeys(seen)
	for i, k := range names {
		names[i] = f.replaceKnown(k, seen, false) // a key name, not a body
	}
	out = withField(out, "redacted_env", names)
	if json.Unmarshal([]byte(out), &top) != nil {
		return out, nil, false
	}
	var had []string
	_ = json.Unmarshal(top["redacted_env"], &had)
	if cursor != "" {
		top["cursor"], _ = json.Marshal(cursor)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(top) != nil {
		return out, had, true
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), had, true
}

// logsCutMargin is the least the filter sees of a cut line past what is
// sent: the reach of a credential the shape and entropy layers know (a
// provider token, a JWT's segments, a 20-character generated run).
const logsCutMargin = 256

// tokenAlphabet is layer 4's (entropyTokenRe in redact.go).
const tokenAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+=_-"

// filterLogLine is one line's text through the app's filter, at most
// logsLineMax bytes of it sent. The filter sees the text cut to
// logsLineMax and a margin, the longest form it knows or logsCutMargin
// if more, so every value or credential that begins inside what is sent
// is whole when it is looked for. Where that cut fell, a value or token
// it split is a piece no layer can know, at the end of what the filter
// returns and, but for a token longer than the margin, shorter than it:
// the margin's bytes go, and the rest of such a token. Then what is left is cut to logsLineMax with
// "…" (a marker or plain text may be cut there, nothing a layer would
// have replaced). The keys it replaced are added to seen.
func filterLogLine(f *Redactor, text string, seen map[string]bool) string {
	longest := logsCutMargin
	if f != nil && len(f.secrets) > 0 {
		longest = max(longest, len(f.secrets[0].text)) // longest first
	}
	pre := logsLineMax + longest
	cut := len(text) > pre
	if cut {
		text = cutText(text, pre)
	}
	out, keys := f.Redact(text)
	for _, k := range keys {
		seen[k] = true
	}
	if cut {
		// And what is left of a token longer than the margin: a run of
		// layer 4's alphabet the cut ended, too short now to be judged.
		out = strings.TrimRight(cutText(out, max(len(out)-longest, 0)), tokenAlphabet)
	}
	if cut || len(out) > logsLineMax {
		out = cutText(out, logsLineMax-len("…")) + "…"
	}
	return out
}

// cutText is s cut to at most n bytes, on a rune's start.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// refusedLogs is when each person was last told refused an app's logs,
// by person and app: the journal line is written at most once a minute
// for each, as a throttled person's is. Entries older than a minute go
// once the map holds refusedLogsKeep.
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
// is told only here.
func (h *Handler) logsRefused(r *http.Request, person, app string, err error) {
	now := logsNow()
	key := [2]string{person, app}
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
