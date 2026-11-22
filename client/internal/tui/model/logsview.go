package model

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The logs screen (l on an app's screen, :logs <app>): an app's own
// lines, the newest 200 when it opens, then followed every LogsEvery
// while it is shown, laid out as `motel logs` prints them (logs.go).

// LogsKeep is the most of an app's lines the logs screen holds, and
// LogsKeepBytes the most of their text as shown: older ones are
// dropped, and the screen says how many. A line is at most 4 KiB as the
// box sends it, but up to four times that made safe (an escape for each
// control byte), and eight with tabs, so 5,000 lines could be 160 MB.
const (
	LogsKeep      = 5000
	LogsKeepBytes = 16 << 20
)

// Grant is whether whoami lists a grant this key needs here.
type Grant int

// The grants.
const (
	// GrantNotKnown: whoami has not answered about this key here.
	GrantNotKnown Grant = iota
	Granted
	NoGrant
)

// LogsGrant says whether this key may read app's lines, by whoami's
// newest answer about this key here (the one held, when a later read of
// it failed): a logs grant on the app or on the box. Without one no logs
// read is sent, since the box writes a refused one in its journal.
func (s *Session) LogsGrant(app string) Grant {
	if !s.Trusted() {
		return GrantNotKnown
	}
	for _, r := range s.Me.V.Roles {
		if r.Role == "logs" && (r.Scope == "box" || r.Scope == "app:"+app) {
			return Granted
		}
	}
	return NoGrant
}

// logsCursor is the logs screen's cursor, the one part of it read off
// the UI's goroutine, by its read (readLogs): the app whose lines it
// follows, how far it has read (LogsFollow, in memory only), and gen,
// which a read takes beside them, so that its answer is taken only by
// the follow it was made for.
type logsCursor struct {
	mu  sync.Mutex
	app string
	gen int
	f   LogsFollow
}

// logsItem is one entry of the logs screen: a line of the app's, or a
// note of the client's among them.
type logsItem struct {
	line LogsLine
	note string
}

// logsData is the logs screen's lines and what it last read.
type logsData struct {
	app string
	gen int
	// items are the lines read since the screen opened, oldest first, and
	// the client's notes among them; first is the number of items[0]: each
	// is numbered as it is added, from 0.
	items []logsItem
	first int
	// lines is how many of items are lines; dropped, how many lines were
	// dropped for LogsKeep; arrived, how many were ever added.
	lines, dropped, arrived int
	// bytes is the text of the lines held, as shown.
	bytes int
	// run is how many lines the reads made at once in a row have brought.
	run int
	// skipped: the follow went on from a tail after a run (takeLogs),
	// whose note says lines were not read: the tail's own note is not
	// written, nor its lines at or before skipAfter, the newest held then.
	skipped   bool
	skipAfter time.Time
	// got is the newest answer, err the newest read's failure; full, the
	// newest that brought lines, which y shows.
	got, full *Got[wire.Logs]
	err       error
	// stopped: the box said the app keeps its output on the box, or named
	// what it keeps in a word this client does not know: nothing more is
	// read until ctrl-r.
	stopped bool
	// pos is where the person has the lines.
	pos logsPos
	// granted is whether whoami listed a logs grant on the app at its
	// last answer (Session.follow).
	granted bool
}

// logsPos is where the person has the logs screen's lines: at the end,
// following them (the zero logsPos), or held at a row: of the item
// numbered top, its row row; since is how many lines had arrived then.
type logsPos struct {
	held       bool
	top, row   int
	since      int
	lastHeight int
}

// LogsOpen is the logs screen opened for app (l, :logs): a fresh start,
// its newest lines read first. What an earlier opening read is dropped,
// and its cursor with it.
func (s *Session) LogsOpen(app string, now time.Time) {
	s.logsFresh(app)
	s.LogsShown(app, now)
}

// LogsShown says which app's logs screen is in front, "" for none (help
// or an overlay over it is not leaving it: its lines are kept, and it
// follows on from its cursor when it is in front again). A logs screen
// of another app than the lines held is a fresh start.
func (s *Session) LogsShown(app string, now time.Time) {
	s.logsOn = app
	if app != "" && (s.logs == nil || s.logs.app != app) {
		s.logsFresh(app)
	}
	s.logsAsk(now)
}

// logsFresh drops every line held and the cursor, for app.
func (s *Session) logsFresh(app string) {
	s.logsCur.mu.Lock()
	s.logsCur.gen++
	s.logsCur.app, s.logsCur.f = app, LogsFollow{}
	gen := s.logsCur.gen
	s.logsCur.mu.Unlock()
	// The newest read's failure is said until a read is answered, since
	// the engine holds a refused read, and waits out a backoff, whatever
	// was opened since. A read the box refused (a 403) starts as not
	// granted, so the next whoami answer that lists the grant asks it
	// again (Session.follow), at each such answer while the 403 stands.
	failed := s.logsFailed[app]
	s.logs = &logsData{app: app, gen: gen, granted: s.LogsGrant(app) == Granted && !Forbidden(failed), err: failed}
}

// logsAsk tells the engine which app's lines to follow: the logs
// screen's, while it is in front, whoami lists a logs grant on it, and
// the box has not said that nothing is to be read.
func (s *Session) logsAsk(now time.Time) {
	app := s.logsOn
	if app == "" || s.LogsGrant(app) != Granted || s.logs == nil || s.logs.stopped {
		app = ""
	}
	s.Engine.LogsOpen(app, now)
}

// LogsAgain is ctrl-r on the logs screen: a follow the box stopped
// (kept off, a word not known) reads again.
func (s *Session) LogsAgain(now time.Time) {
	if s.logs != nil {
		s.logs.stopped = false
	}
	s.logsAsk(now)
}

// readLogs is the logs screen's read: a tail, then the lines after the
// cursor the follow has read to.
func readLogs(ctx context.Context, s *Session, c Client, j Job) (func(*Session, time.Time), error) {
	s.logsCur.mu.Lock()
	gen, after := s.logsCur.gen, ""
	if s.logsCur.app == j.App {
		after = s.logsCur.f.After()
	}
	s.logsCur.mu.Unlock()
	g, err := c.Logs(ctx, j.App, after)
	// Whether to read again at once, said to the engine before it records
	// this read, by the rule the follow moves by (LogsFollow.Take).
	f := LogsFollow{after: after}
	s.Engine.logsMore(j, err == nil && g.V.Kept == wire.KeptOn && f.Take(g.V).Again)
	return func(s *Session, at time.Time) { s.takeLogs(j, gen, g, err, at) }, err
}

// takeLogs is a logs read's end, on the UI's goroutine: its lines and
// what the answer says among them, and where the follow reads next. An
// answer to a follow the screen has since started afresh is not taken,
// but a failure is: the engine holds a refused read until ctrl-r, and
// waits out another's backoff, whatever the screen started since, so
// the screen says it.
func (s *Session) takeLogs(j Job, gen int, g Got[wire.Logs], err error, at time.Time) {
	if out, _ := Classify(err); out == Stopped {
		// Every read stopped (a 401, a signature that failed): the stop
		// screen says why, and after r the screen reads on; it is not this
		// read's failure to say.
		return
	}
	d := s.logs
	if err != nil {
		if s.logsFailed == nil {
			s.logsFailed = map[string]error{}
		}
		s.logsFailed[j.App] = err
	}
	if err != nil && (d == nil || d.app != j.App || d.gen != gen) {
		if d != nil && d.app == j.App && d.got == nil && d.err == nil {
			// Opened afresh since, nothing read yet: it is this failure's.
			d.err = err
		}
		return
	}
	if d == nil || d.app != j.App || d.gen != gen {
		return
	}
	if err != nil {
		if Forbidden(err) || NotFound(err) {
			// The box's last word on them is a refusal: what was read of
			// them goes, as a status's does (readStatus), and the cursor; a
			// read asked again is a tail. The engine holds the read until
			// ctrl-r, so the refusal is said at each opening till then.
			s.logsFresh(d.app)
			d = s.logs
		}
		// What was read is kept beside any other failure.
		d.err = err
		return
	}
	g.At = at
	d.got, d.err = &g, nil
	if len(g.V.Lines) > 0 {
		d.full = &g
	}
	delete(s.logsFailed, d.app)
	if g.V.Kept != wire.KeptOn {
		// Kept off, or a word this client does not know: nothing to follow
		// (said by the screen), and nothing more is read.
		d.stopped = true
		s.logsAsk(at)
		return
	}
	s.logsCur.mu.Lock()
	turn := LogsTurn{}
	if s.logsCur.gen == gen {
		turn = s.logsCur.f.Take(g.V)
	}
	s.logsCur.mu.Unlock()
	if turn.Gap {
		d.note(LogsGapSays(d.app, len(g.V.Lines) == 0, turn.Older))
	}
	lines := g.V.Lines
	if d.skipped {
		// The tail after a skip may hold lines already shown: they are
		// not shown twice.
		lines = slices.DeleteFunc(slices.Clone(lines), func(l wire.LogLine) bool { return !l.At.After(d.skipAfter) })
	}
	if turn.Older && !d.skipped {
		d.note(LogsOlderSays(wire.LogsLines[0]) + ", and this screen does not page back (motel logs --lines " + LogsMostLines() + " may reach further)")
	}
	d.skipped = false
	for _, l := range LogsLines(lines) {
		d.items = append(d.items, logsItem{line: l})
		d.lines++
		d.arrived++
		d.bytes += len(l.Text.String())
	}
	if turn.Again {
		d.run += len(g.V.Lines)
	} else {
		d.run = 0
	}
	if d.run >= LogsKeep {
		// A follow so far behind that what it reads next would be dropped
		// before it is shown: it goes on from the app's newest lines, read
		// at once, as the lines left were to be.
		d.note("lines skipped: this follow fell more than the " + count(LogsKeep) + " lines this screen holds behind the app; it goes on from the newest, and the lines between were not read")
		d.run, d.skipped = 0, true
		d.skipAfter = d.lastAt()
		s.logsCur.mu.Lock()
		if s.logsCur.gen == gen {
			s.logsCur.f = LogsFollow{}
		}
		s.logsCur.mu.Unlock()
	}
	d.trim()
}

// lastAt is the time of the newest line held.
func (d *logsData) lastAt() time.Time {
	for i := len(d.items) - 1; i >= 0; i-- {
		if d.items[i].note == "" {
			return d.items[i].line.At
		}
	}
	return time.Time{}
}

// note is a note of the client's among the lines.
// A note already among those written since the last line is not
// written again: answers that bring no line (a gap whose entries were
// all passed over, with older ones left or not) would otherwise add
// notes each, which neither bound counts.
func (d *logsData) note(s string) {
	for i := len(d.items) - 1; i >= 0 && d.items[i].note != ""; i-- {
		if d.items[i].note == s {
			return
		}
	}
	d.items = append(d.items, logsItem{note: s})
}

// trim drops the oldest lines past LogsKeep or LogsKeepBytes, and a
// note left first, which was about lines dropped.
func (d *logsData) trim() {
	n := 0
	for ; n < len(d.items) && (d.lines > LogsKeep || d.bytes > LogsKeepBytes || d.dropped > 0 && d.items[n].note != ""); n++ {
		if d.items[n].note == "" {
			d.lines--
			d.dropped++
			d.bytes -= len(d.items[n].line.Text.String())
		}
	}
	if n > 0 {
		d.items = append([]logsItem(nil), d.items[n:]...)
		d.first += n
	}
}

// count is n with a comma between thousands.
func count(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// LogsAsOf is the logs screen's crumb: how old the newest answer is, and
// that the last read failed when it did.
func (s *Session) LogsAsOf(app string, now time.Time) Line {
	s.seen(now)
	d := s.logs
	if d == nil || d.app != app || d.got == nil {
		return nil
	}
	l := s.asOf(d.got.At, now)
	if d.err != nil {
		l = append(l, ownC(" · the last read failed", Warn))
	}
	return l
}

// LogsViewFit is the logs screen of app laid to width cells and height
// rows (the page's): what the screen says of the follow, a rule that
// says whether it follows or is held, and under it as many of the lines
// as fit, each laid out as `motel logs` prints it. A line wider than the
// window is cut at its edge, the cut marked; with wrap, it goes on to
// rows of its own, each indented past the time, so no part of it begins
// where a line or a divider does.
func (s *Session) LogsViewFit(app string, now time.Time, width, height int, cells Cells, wrap bool) []Line {
	s.seen(now)
	head, d := s.logsHead(app, now, width, cells)
	if d == nil {
		return head
	}
	// The head, the rule and the lines, within the page: a head taller
	// than the page is cut at its foot.
	if len(head)+1 >= height {
		return head[:min(len(head), max(height, 1))]
	}
	rows := height - len(head) - 1
	d.pos.lastHeight = rows
	var rule string
	switch {
	case s.Engine.Idle(now) && !d.stopped:
		rule = "── paused (idle 10m): any key reads on "
	case d.pos.held:
		n := d.arrived - d.pos.since
		lines := " new lines since"
		if n == 1 {
			lines = " new line since"
		}
		rule = "── held: " + count(n) + lines + " · End follows "
	case d.stopped:
		rule = "── not following: nothing more is asked "
	default:
		rule = "── following: a read every " + strconv.Itoa(int(LogsEvery/time.Second)) + " s "
	}
	out := append(slices.Clip(head), cutTo(Line{ownC(rule+strings.Repeat("─", max(width-len([]rune(rule)), 0)), Dim)}, width, cells))
	if len(d.items) == 0 {
		return out
	}
	i, r := d.top(rows, width, cells, wrap)
	for len(out) < len(head)+1+rows && i < len(d.items) {
		rs := d.rows(i, width, cells, wrap)
		out = append(out, rs[r:]...)
		i, r = i+1, 0
	}
	return out[:min(len(out), len(head)+1+rows)]
}

// logsHead is what the logs screen says above the lines, laid to width,
// and the lines held when there are lines to show (nil when the screen
// says why there are none).
func (s *Session) logsHead(app string, now time.Time, width int, cells Cells) ([]Line, *logsData) {
	say := func(l Line) []Line { return Sentence(l, width, cells) }
	switch s.LogsGrant(app) {
	case GrantNotKnown:
		return say(Line{own("whether this key may read " + app + "'s lines is not known until whoami answers about it here: nothing is asked until then.")}), nil
	case NoGrant:
		return say(append(Line{own("reading " + app + "'s lines needs a logs grant on " + app + " or on the box, and whoami (as of " + hms(s.Me.At) + " UTC) lists none for this key here, so nothing is asked. The operator gives one in the box's Caddyfile (grant <name> logs app " + app + "); :me lists what you hold.")}, s.askAgain(now)...)), nil
	}
	d := s.logs
	if d == nil || d.app != app {
		return say(Line{own("asking " + s.Host + " for " + app + "'s newest lines…")}), nil
	}
	switch {
	case d.got == nil && d.err == nil:
		return say(Line{own("asking " + s.Host + " for " + app + "'s newest lines…")}), nil
	case d.got == nil:
		l := Line{{T: Capped(failureText(d.err), 4*LineMax)}}
		switch {
		case NotFound(d.err):
			l = append(l, own(" (a moteld older than this read, or no app of this name now)"))
		case Forbidden(d.err):
			l = append(l, s.askAgain(now)...)
		}
		return say(l), nil
	}
	var out []Line
	if d.err != nil {
		out = say(Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(d.err), 2*LineMax)}})
	}
	switch k := d.got.V.Kept; {
	case k == wire.KeptOff:
		out = append(out, say(append(Line{own(LogsOffSays(s.Host, app) + ". Nothing more is asked.")}, s.askAgain(now)...))...)
	case k != wire.KeptOn:
		out = append(out, say(append(Line{ownC("not known: ", Warn), own(s.Host + " names what it keeps of " + app + "'s output in a word this client does not know, "), quoted(k), own(", so nothing more is asked.")}, s.askAgain(now)...))...)
	case d.arrived == 0 && len(d.items) == 0:
		// None, and no note: a tail whose older lines were not sent holds
		// lines, as the note says.
		out = append(out, say(Line{own(LogsNoneSays(s.Host, app) + "; this screen waits for its first.")})...)
	}
	if len(d.items) == 0 && d.got.V.Kept != wire.KeptOn {
		return out, nil
	}
	if d.dropped > 0 {
		out = append(out, say(Line{own("this screen holds the newest " + count(LogsKeep) + " lines, and at most " + strconv.Itoa(LogsKeepBytes>>20) + " MiB of their text: " + count(d.dropped) + " older ones were dropped.")})...)
	}
	return out, d
}

// rows is item i laid out to width: a note of the client's on rows of
// its own (Sentence); a line after its divider when it has one, then the
// line, cut at width, or with wrap on as many rows as it takes, each
// after the first indented past the time. Width 0 is each line whole.
func (d *logsData) rows(i, width int, cells Cells, wrap bool) []Line {
	it := d.items[i]
	if it.note != "" {
		return Sentence(Line{ownC("-- "+it.note+" --", Dim)}, width, cells)
	}
	var prev *LogsLine
	if i > 0 && d.items[i-1].note == "" {
		prev = &d.items[i-1].line
	}
	var out []Line
	if div, ok := LogsDivider(prev, it.line); ok {
		out = Sentence(Line{ownC("-- ", Dim), Seg{T: div, C: Dim}, ownC(" --", Dim)}, width, cells)
	}
	clock := it.line.Clock() + "  "
	row := Line{ownC(clock, Dim), Seg{T: it.line.Text}}
	room := width - len(clock)
	if !wrap || width <= 0 || room < 1 || row.Width(cells) <= width {
		return append(out, cutTo(row, width, cells))
	}
	indent := strings.Repeat(" ", len(clock))
	for k, p := range splitCells(it.line.Text.String(), room, cells) {
		if k == 0 {
			out = append(out, Line{ownC(clock, Dim), Seg{T: termtext.From(p)}})
			continue
		}
		out = append(out, Line{own(indent), Seg{T: termtext.From(p)}})
	}
	return out
}

// splitCells is t, safe text, cut into pieces of at most room cells as
// cells measures them, between two characters and never inside one of
// termtext's escapes (\x1b, \u202e), which would read as other text on
// two rows; a piece is at least one character, or one escape, so one
// wider than room has a piece of its own.
func splitCells(t string, room int, cells Cells) []string {
	// inside[i]: byte i is inside an escape, after its backslash, so no
	// piece may end before it.
	inside := make([]bool, len(t)+1)
	for _, m := range escapeRe.FindAllStringIndex(t, -1) {
		for i := m[0] + 1; i < m[1]; i++ {
			inside[i] = true
		}
	}
	var out []string
	for at := 0; at < len(t); {
		// The ends a piece may have: after each of the next characters, as
		// many as could fit in room cells and more (a mark takes none).
		var ends []int
		for i := range t[at:] {
			if e := at + i; i > 0 && !inside[e] {
				ends = append(ends, e)
			}
			if len(ends) == 4*room {
				break
			}
		}
		if len(ends) < 4*room {
			ends = append(ends, len(t))
		}
		lo, hi := 0, len(ends)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if cells(termtext.From(t[at:ends[mid]])) <= room {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		out = append(out, t[at:ends[lo]])
		at = ends[lo]
	}
	return out
}

// escapeRe is the escapes termtext writes for what it will not pass.
var escapeRe = regexp.MustCompile(`\\x[0-9a-f]{2}|\\u[0-9a-f]{4}`)

// top is the row the view begins at: held, where it was held (the
// oldest line held, when that one was dropped); following, the row that
// shows the last row of all at the view's foot.
func (d *logsData) top(rows, width int, cells Cells, wrap bool) (int, int) {
	if !d.pos.held {
		last := len(d.items) - 1
		return d.up(last, len(d.rows(last, width, cells, wrap))-1, rows-1, width, cells, wrap)
	}
	i := d.pos.top - d.first
	if i < 0 {
		return 0, 0
	}
	i = min(i, len(d.items)-1)
	return i, min(d.pos.row, len(d.rows(i, width, cells, wrap))-1)
}

// up is the row n rows above row r of item i, the first of all at most.
func (d *logsData) up(i, r, n, width int, cells Cells, wrap bool) (int, int) {
	for n > 0 {
		if r >= n {
			return i, r - n
		}
		n -= r + 1
		if i == 0 {
			return 0, 0
		}
		i--
		r = len(d.rows(i, width, cells, wrap)) - 1
	}
	return i, r
}

// down is the row n rows below row r of item i, and whether it is past
// the last row of all, or is it.
func (d *logsData) down(i, r, n, width int, cells Cells, wrap bool) (int, int, bool) {
	for {
		c := len(d.rows(i, width, cells, wrap))
		if r+n < c {
			return i, r + n, i == len(d.items)-1 && r+n == c-1
		}
		n -= c - r
		if i == len(d.items)-1 {
			return i, c - 1, true
		}
		i, r = i+1, 0
	}
}

// LogsScroll moves the logs screen's lines by n rows, up when n is
// negative, laid to width as the last frame drew them: scrolled up, the
// view is held where it is while lines arrive; scrolled down until the
// newest row is in view, it follows them again.
func (s *Session) LogsScroll(app string, n, width int, cells Cells, wrap bool) {
	d := s.logs
	if d == nil || d.app != app || len(d.items) == 0 || n == 0 {
		return
	}
	rows := max(d.pos.lastHeight, 1)
	i, r := d.top(rows, width, cells, wrap)
	if n < 0 {
		ui, ur := d.up(i, r, -n, width, cells, wrap)
		if ui == i && ur == r {
			// At the oldest row already: nothing scrolled, nothing held.
			return
		}
		i, r = ui, ur
		since := d.pos.since
		if !d.pos.held {
			since = d.arrived
		}
		d.pos = logsPos{held: true, top: d.first + i, row: r, since: since, lastHeight: rows}
		return
	}
	// The view's foot after the move: at the newest row, it follows.
	bi, br, _ := d.down(i, r, rows-1, width, cells, wrap)
	if _, _, end := d.down(bi, br, n, width, cells, wrap); end {
		d.pos = logsPos{lastHeight: rows}
		return
	}
	i, r, _ = d.down(i, r, n, width, cells, wrap)
	d.pos.top, d.pos.row = d.first+i, r
}

// LogsPage is a page of the logs screen's lines (PgUp is -1, PgDn 1).
func (s *Session) LogsPage(app string, by, width int, cells Cells, wrap bool) {
	if d := s.logs; d != nil && d.app == app {
		s.LogsScroll(app, by*max(d.pos.lastHeight-1, 1), width, cells, wrap)
	}
}

// LogsHome holds the logs screen's lines at the oldest held; LogsEnd
// follows them again, at the newest.
func (s *Session) LogsHome(app string, width int, cells Cells, wrap bool) {
	if d := s.logs; d != nil && d.app == app && len(d.items) > 0 {
		if i, r := d.top(max(d.pos.lastHeight, 1), width, cells, wrap); i == 0 && r == 0 {
			// At the oldest row already: nothing scrolled, nothing held.
			return
		}
		since := d.pos.since
		if !d.pos.held {
			since = d.arrived
		}
		d.pos = logsPos{held: true, top: d.first, since: since, lastHeight: d.pos.lastHeight}
	}
}

// LogsEnd follows the logs screen's lines again, at the newest.
func (s *Session) LogsEnd(app string) {
	if d := s.logs; d != nil && d.app == app {
		d.pos = logsPos{lastHeight: d.pos.lastHeight}
	}
}
