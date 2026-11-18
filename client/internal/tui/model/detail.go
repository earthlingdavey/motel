package model

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// field is a labelled line: the label is the client's, and the value
// holds the box's words only as a checked token, a quotation or a
// capped value.
func field(label string, v ...Seg) Line {
	return append(Line{ownC(pad(label, 11)+" ", Label)}, v...)
}

// watchdogStates are liveswap/watchdog.go's states.
var watchdogStates = map[string]bool{"disabled": true, "idle": true, "grace": true, "watching": true, "backoff": true, "throttled": true}

// DeployCols are the overview's deploy table headings.
var DeployCols = []string{"VERSION", "RESULT", "WHERE", "FINISHED (UTC)", "TOOK", "BY"}

// DeployRow is one row of an app's deploys.
type DeployRow struct {
	Version string
	Mark    Mark
	Cells   []termtext.Text
}

// OverviewView is an app's screen: its state, and its deploys.
type OverviewView struct {
	// AsOf is the crumb's right side: how old the status is.
	AsOf Line
	// Failed says why the last read failed, beside the status read
	// before it; nil while it reads. The screen lays it on rows
	// (Sentence).
	Failed Line
	Fields []Line
	// Note is said in place of the fields: no answer yet, a refusal,
	// no version yet.
	Note    []Line
	OnDisk  Line
	Deploys []DeployRow
}

func (s *Session) asOf(at time.Time, now time.Time) Line {
	return Line{own("as of " + hms(at) + " UTC · " + Ago(now.Sub(at)) + " ago")}
}

// Overview is app's screen at now.
func (s *Session) Overview(app string, now time.Time) OverviewView {
	s.seen(now)
	var v OverviewView
	a := s.apps[app]
	switch {
	case a == nil || a.st == nil && a.err == nil:
		v.Note = []Line{{own("asking " + s.Host + " for " + app + "…")}}
		return v
	case a.st == nil || Forbidden(a.err) || NotFound(a.err):
		v.Note = []Line{{Seg{T: Capped(failureText(a.err), 4*LineMax)}}}
		if Forbidden(a.err) {
			v.Note = append(v.Note, s.askAgain(now))
		}
		return v
	}
	st := a.st.V
	v.AsOf = s.asOf(a.st.At, now)
	if a.err != nil {
		v.AsOf = append(v.AsOf, ownC(" · the last read failed", Warn))
		v.Failed = Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(a.err), 2*LineMax)}}
	}
	// The deploys first: an app whose first deploy failed has no
	// version, and its failed deploys are what there is to show.
	if len(st.AvailableVersions) > 0 {
		vs := make([]string, 0, 3)
		for i, ver := range st.AvailableVersions {
			if i == 3 {
				vs = append(vs, "+"+strconv.Itoa(len(st.AvailableVersions)-3))
				break
			}
			if slices.Contains(st.OrderedByMtime, ver) {
				ver = "~" + ver
			}
			vs = append(vs, short(ver))
		}
		v.OnDisk = Line{own("on disk: " + strings.Join(vs, " "))}
	}
	for _, d := range st.Deploys {
		r := DeployRow{Version: d.Version, Mark: MarkNone}
		if d.Status == "failed" {
			r.Mark = MarkLook
		}
		fin, took := "(unreadable)", "(unreadable)"
		switch f, ok := rawTime(d.FinishedAt); {
		case noTime(d.FinishedAt) || ok && f.IsZero():
			fin, took = "(none)", "-"
		case ok:
			fin, took = date(f, now), "-"
			if b, ok := rawTime(d.StartedAt); ok {
				if l, ok := span(b, f); ok {
					took = l
				}
			} else if !noTime(d.StartedAt) {
				took = "(unreadable)"
			}
		}
		r.Cells = []termtext.Text{termtext.From(d.Version), termtext.From(d.Status), termtext.From(d.Phase), termtext.From(fin), termtext.From(took), Capped(d.By, 60)}
		v.Deploys = append(v.Deploys, r)
	}
	if st.CurrentVersion == "" {
		v.Note = []Line{{own(app + " has no version yet. Its first deploy comes from CI. Nothing to show until then.")}}
		if len(v.Deploys) > 0 || st.LastDeploy != nil {
			v.Note = append([]Line{{own(app + " has no version yet: no deploy of it serves.")}}, lastDeploy(st.LastDeploy, now)...)
		}
		return v
	}
	row := s.row(app, now)
	state := Line{ownC(row.Cells[ColState].String(), row.Mark.Class()), own(" " + st.CurrentVersion)}
	if st.PID != 0 {
		state = append(state, own(" · pid "+strconv.Itoa(st.PID)))
	}
	if st.Unit != "" {
		state = append(state, own(" · unit "+st.Unit))
	}
	v.Fields = append(v.Fields, field("state", state...))
	if wd := st.Watchdog; wd != nil {
		// The client's check lets through the states it knows and its
		// own words for one the box's filter hid; anything else would
		// be shown as the box's value.
		w := Line{boxT(wd.State)}
		if watchdogStates[wd.State] {
			w = Line{own(wd.State)}
		}
		w = append(w, own(" · "+strconv.Itoa(wd.RestartsInWindow)+" restarts in its window"))
		if wd.Failures > 0 {
			w = append(w, own(" · "+strconv.Itoa(wd.Failures)+" failed checks in a row"))
		}
		if !wd.LastRestartAt.IsZero() {
			w = append(w, own(" · last restart "+clock(wd.LastRestartAt, now, false)+" UTC"))
		}
		v.Fields = append(v.Fields, field("watchdog", w...))
		if wd.LastRestartCause != "" {
			v.Fields = append(v.Fields, field("restarted", boxT(wd.LastRestartCause)))
		}
		if wd.LastFailure != "" {
			v.Fields = append(v.Fields, field("last check", boxT(wd.LastFailure)))
		}
	}
	v.Fields = append(v.Fields, lastDeploy(st.LastDeploy, now)...)
	v.Fields = append(v.Fields, s.metricLines(st, now)...)
	if d := st.LastDeploy; d != nil && d.ArtifactEntries > 0 {
		v.Fields = append(v.Fields, field("artifact", own(strconv.Itoa(d.ArtifactEntries)+" entries · "+mib(float64(d.ArtifactBytes))+" (the last deploy's)")))
	}
	return v
}

// lastDeploy is the overview's lines of the last deploy, none when the
// box sends none.
func lastDeploy(d *wire.DeployResult, now time.Time) []Line {
	if d == nil {
		return nil
	}
	l := Line{ownC(d.Status, statusClass(d.Status))}
	if d.Phase != "" {
		l = append(l, own(" in "+d.Phase))
	}
	if d.FinishedAt.IsZero() {
		l = append(l, own(" (the box gives no time it finished)"))
	} else {
		t := " " + since(d.FinishedAt, now) + " (" + clock(d.FinishedAt, now, false) + " UTC"
		if took, ok := span(d.StartedAt, d.FinishedAt); ok {
			t += ", " + took
		}
		l = append(l, own(t+")"))
	}
	if d.By != "" {
		l = append(l, own(" by "), boxT(d.By))
	}
	out := []Line{field("deployed", l...)}
	if d.Error != "" {
		out = append(out, field("error", boxT(d.Error)))
	}
	if d.Note != "" {
		out = append(out, field("note", boxT(d.Note)))
	}
	return out
}

func statusClass(status string) Class {
	if status == "failed" {
		return Bad
	}
	return Plain
}

func (s *Session) metricLines(st wire.Status, now time.Time) []Line {
	m := st.Metrics
	switch {
	case m == nil:
		return []Line{field("metrics", own("this box keeps none"))}
	case m.Error != "":
		return []Line{field("metrics", own("the box says: "), quoted(m.Error))}
	}
	var out []Line
	cap := memCap(st)
	if mem := m.Memory; mem != nil && (mem.Now != nil || mem.Max != nil) {
		var l Line
		if mem.Now != nil {
			l = append(l, own(mib(*mem.Now)+" now"))
		}
		if mem.Max != nil {
			l = append(l, own(" · "+mib(*mem.Max)+" max"))
		}
		if cap > 0 {
			l = append(l, own(" · cap "+mib(cap)))
			if slices.Contains(st.CapsUnenforced, "memory_max") {
				l = append(l, own(" (not enforced)"))
			}
			if mem.Now != nil {
				// A share outside 0 to 1 (moteld sends none below 0) is
				// drawn at its end of the bar.
				f := min(max(*mem.Now/cap, 0), 1)
				n := int(f*20 + 0.5)
				l = append(l, own("  "+strings.Repeat("█", n)+strings.Repeat("░", 20-n)+" "+pct(*mem.Now/cap)+" of cap"))
			}
		}
		out = append(out, field("memory", l...))
	}
	if c := m.CPU; c != nil {
		l := Line{}
		if c.Avg5m != nil {
			l = append(l, own(num(*c.Avg5m, 2)+" cores (last 5 m) · "))
		}
		l = append(l, own(num(c.Avg, 2)+" avg"))
		if st.Caps != nil && st.Caps.CPUQuota > 0 {
			l = append(l, own(" · cap "+strconv.Itoa(st.Caps.CPUQuota)+"% of one CPU"))
		}
		out = append(out, field("cpu", l...))
	}
	if q := m.Requests; q != nil {
		l := Line{own(strconv.FormatUint(q.Total, 10) + " · " + num(q.PerSecond, 2) + "/s")}
		if q.Share5xx != nil {
			l = append(l, own(" · 5xx "+pct(*q.Share5xx)))
		}
		if q.P50 != nil {
			l = append(l, own(" · p50 "+num(*q.P50, 0)+" ms"))
		}
		if q.P95 != nil {
			l = append(l, own(" · p95 "+num(*q.P95, 0)+" ms"))
		}
		if q.CacheHitShare != nil {
			l = append(l, own(" · cache hits "+pct(*q.CacheHitShare)))
		}
		out = append(out, field("requests", l...))
	}
	l := Line{own("over "), boxT(m.Window)}
	if m.At != nil && !m.At.IsZero() {
		l = append(l, own(", newest point "+clock(*m.At, now, true)+" UTC (the store runs 30 s behind)"))
	}
	return append(out, field("metrics", l...))
}

// RecordView is one deploy's record, laid out for reading a failure:
// the verdict, what serves now, the box's error, where the time went,
// the app's side, then the log tail.
type RecordView struct {
	Head []Line
	// TailTitle names the pane below, which holds the box's lines.
	TailTitle Line
	Tail      []termtext.Text
	Note      []Line
}

// Record is app's deploy of version at now.
func (s *Session) RecordView(app, version string, now time.Time) RecordView {
	var v RecordView
	g, err, ok := s.Record(app, version)
	switch {
	case !ok:
		v.Note = []Line{{own("asking " + s.Host + " for the record of " + version + "…")}}
		return v
	case g == nil || Forbidden(err) || NotFound(err):
		// A 403 (the grant removed) or a 404 (the record gone) is said
		// alone, as an app's screen says it: the record read before is
		// not one to show beside it.
		v.Note = []Line{{Seg{T: Capped(failureText(err), 4*LineMax)}}}
		return v
	case err != nil:
		// The record read before stays beside a read that got no answer
		// or could not be made; the crumb says how old it is.
		v.Head = append(v.Head, Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(err), 2*LineMax)}})
	}
	d := g.V
	took, tookOK := span(d.StartedAt, d.FinishedAt)
	// When it ran: the start, and the end after it, its date only when
	// the start's is another; a time the box did not give is said so.
	when := " · "
	switch {
	case d.StartedAt.IsZero() && d.FinishedAt.IsZero():
		when += "the box gives no times"
	case d.FinishedAt.IsZero():
		when += "started " + clock(d.StartedAt, now, true) + " UTC, the box gives no time it finished"
	case d.StartedAt.IsZero():
		when += "the box gives no time it started; finished " + clock(d.FinishedAt, now, true) + " UTC"
	default:
		end := clock(d.FinishedAt, now, true)
		if datePart(d.FinishedAt, d.StartedAt, false) == "" {
			end = hms(d.FinishedAt)
		}
		when += clock(d.StartedAt, now, true) + " → " + end + " UTC"
	}
	var verdict Line
	if d.Status == "failed" {
		w := "✖ FAILED"
		if d.Phase != "" {
			w += " in " + d.Phase
		}
		if tookOK {
			when = " after " + took + when
		}
		verdict = Line{ownC(w, Bad), own(when)}
	} else {
		if tookOK {
			when = " in " + took + when
		}
		verdict = Line{ownC("✓ SUCCEEDED", Good), own(when)}
	}
	v.Head = append(v.Head, verdict)
	if d.By != "" {
		v.Head = append(v.Head, field("by", boxT(d.By)))
	}
	if a := s.apps[app]; a != nil && a.st != nil {
		cur := a.st.V.CurrentVersion
		switch cur {
		case "":
			v.Head = append(v.Head, field("serves now", own("nothing (as of "+clock(a.st.At, now, true)+" UTC)")))
		case d.Version:
			v.Head = append(v.Head, field("serves now", own("this version (as of "+clock(a.st.At, now, true)+" UTC)")))
		default:
			v.Head = append(v.Head, field("serves now", own(cur+" (as of "+clock(a.st.At, now, true)+" UTC)")))
		}
	}
	if d.Error != "" {
		v.Head = append(v.Head, field("error", boxT(d.Error)))
	}
	if d.Note != "" {
		v.Head = append(v.Head, field("note", boxT(d.Note)))
	}
	if len(d.Phases) > 0 {
		var l Line
		for i, p := range d.Phases {
			if i > 0 {
				l = append(l, ownC(" › ", Dim))
			}
			l = append(l, own(p.Name+" "+num(p.Seconds, 1)+"s"))
			if i == len(d.Phases)-1 && d.Status == "failed" && p.Name == d.Phase {
				l = append(l, ownC(" ✖", Bad))
			}
		}
		v.Head = append(v.Head, field("phases", l...))
	}
	if dt := d.Detail; dt != nil {
		if dt.Exit != "" {
			v.Head = append(v.Head, field("exit", boxT(dt.Exit)))
		} else {
			v.Head = append(v.Head, field("exit", own("(none given)")))
		}
		if p := dt.Probe; p != nil {
			l := Line{own(strconv.Itoa(p.Status))}
			if p.Location != "" {
				// The app's own header, inside the client's sentence: a
				// quotation, so ", body:" after it is the client's.
				l = append(l, own(" from "), quoted(p.Location))
			}
			if p.Body != "" {
				l = append(l, own(", body:"))
			}
			v.Head = append(v.Head, field("probe", l...))
			for _, b := range termtext.Lines(p.Body) {
				v.Head = append(v.Head, Line{own("              "), Seg{T: Capped(b.String(), LineMax)}})
			}
		}
	}
	if len(d.RedactedEnv) > 0 {
		v.Head = append(v.Head, field("redacted", own("the box replaced the values of: "), boxT(strings.Join(d.RedactedEnv, ", "))))
	}
	if d.ArtifactEntries > 0 {
		v.Head = append(v.Head, field("artifact", own(strconv.Itoa(d.ArtifactEntries)+" entries · "+mib(float64(d.ArtifactBytes)))))
	}
	dt := d.Detail
	switch {
	case dt == nil:
	case dt.LogTailError != "":
		v.TailTitle = Line{own("no log tail: the box says: "), quoted(dt.LogTailError)}
	case len(dt.LogTail) == 0 && dt.Probe != nil:
		// The box sends no field that says so; an empty tail beside a
		// probe and no error is what deploy_log_lines 0 leaves.
		v.TailTitle = Line{own("no log tail: this app may keep its output on the box (deploy_log_lines 0)")}
	case len(dt.LogTail) > 0:
		for _, l := range dt.LogTail {
			ts := termtext.Lines(l)
			if l == "" {
				// Lines makes no line of nothing; an empty line of the
				// tail is a line.
				ts = []termtext.Text{{}}
			}
			for _, t := range ts {
				v.Tail = append(v.Tail, Capped(t.String(), TailMax))
			}
		}
		t := "log tail: " + strconv.Itoa(len(v.Tail)) + " lines"
		if dt.LogTailTruncated {
			t += ", earlier ones cut"
		}
		v.TailTitle = Line{own(t)}
	}
	return v
}

// JSONLines is a body as received, indented, each line made safe and
// capped: the box's answer is never re-encoded from the client's
// types.
//
// At most OverlayLines are made, for the screen: cut says how many more
// there were. indented is false for a body that would not indent,
// which is shown line by line as it came.
func JSONLines(body []byte) (out []termtext.Text, cut int, indented bool) {
	out, cut, indented, _ = jsonLines(body)
	return out, cut, indented
}

// jsonLines is JSONLines, and whether a line it made is nested deeper
// than IndentMax levels, and so indented less than its depth.
func jsonLines(body []byte) (out []termtext.Text, cut int, indented, deep bool) {
	var all []termtext.Text
	if !json.Valid(body) {
		all = termtext.Lines(string(body))
	} else {
		// Only what the overlay draws is written: a line is cut a little
		// past TailMax runes (four bytes a rune, the most UTF-8 takes),
		// which Capped then cuts and marks, and nothing after the
		// OverlayLines'th line is written, only counted.
		w := indenter{lines: OverlayLines, lineBytes: 4 * (TailMax + 1)}
		w.indent(body)
		all, cut, indented, deep = termtext.Lines(string(w.b)), w.total()-OverlayLines, true, w.deep
	}
	if len(all) > OverlayLines {
		all, cut = all[:OverlayLines], len(all)-OverlayLines
	}
	for _, l := range all {
		out = append(out, Capped(l.String(), TailMax))
	}
	return out, max(cut, 0), indented, deep
}

// JSONShown is a body as P prints it to a terminal: the lines the JSON
// overlay shows, indented (at most IndentMax levels deep) and made
// safe, every one and each whole, a line feed after each. By JSON's
// rules a body holds no raw C0 control, but it may hold C1 controls and
// bidi overrides, which a terminal acts on; what the command's --json
// prints goes where stdout is not a terminal (Printable).
func JSONShown(body []byte) string {
	text := string(body)
	if json.Valid(body) {
		var w indenter
		w.indent(body)
		text = string(w.b)
	}
	all := termtext.Lines(text)
	lines := make([]string, 0, len(all)+1)
	for _, l := range all {
		lines = append(lines, l.String())
	}
	return strings.Join(append(lines, ""), "\n")
}

// OverlayLines is the most of an answer's lines the JSON overlay draws:
// tview's text view draws slower the further it is scrolled, and a body
// of megabytes made every key slow. P prints all of it.
const OverlayLines = 2000

// IndentMax is the most levels of nesting the answer is indented by, on
// the overlay and as P prints it: a line nested deeper is indented as
// at IndentMax. Each level is two spaces on every line inside it, so a
// small body nested thousands deep would otherwise be gigabytes of
// spaces; moteld's answers nest five deep at most.
const IndentMax = 10

// indenter writes a valid JSON body indented as json.Indent indents it
// with two spaces and no prefix, but no deeper than IndentMax levels; it
// writes at most lines lines (0: all), and at most lineBytes bytes of a
// line (0: all), and counts the rest.
type indenter struct {
	lines, lineBytes int
	b                []byte
	// n is the line feeds of the whole text, cur the bytes of the line
	// being written; endsNL says the whole text ends with a line feed.
	n, cur int
	endsNL bool
	// deep says a line written is nested deeper than IndentMax.
	deep bool
}

// total is how many lines termtext.Lines would make of the whole text.
func (w *indenter) total() int {
	if w.endsNL {
		return w.n
	}
	return w.n + 1
}

func (w *indenter) put(c byte) {
	w.endsNL = c == '\n'
	if c == '\n' {
		w.n, w.cur = w.n+1, 0
	}
	if w.lines > 0 && w.n >= w.lines || w.lineBytes > 0 && w.cur >= w.lineBytes {
		return
	}
	w.b = append(w.b, c)
	if c != '\n' {
		w.cur++
	}
}

func (w *indenter) newline(depth int) {
	w.put('\n')
	if w.lines > 0 && w.n >= w.lines {
		return
	}
	if depth > IndentMax {
		w.deep, depth = true, IndentMax
	}
	for range 2 * depth {
		w.put(' ')
	}
}

// indent writes body, which json.Valid holds valid: white space outside
// strings is dropped, as json.Indent drops it, but for what follows the
// value, which it keeps as it is.
func (w *indenter) indent(body []byte) {
	// depth is the indent's, which, as json.Indent's, grows only when an
	// object or an array is found not empty; nest is the brackets open.
	depth, nest := 0, 0
	need, inStr, esc, begun := false, false, false, false
	for i, c := range body {
		if inStr {
			w.put(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
				if nest == 0 {
					w.rest(body[i+1:])
					return
				}
			}
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if nest == 0 && begun {
				// The end of a number or a literal that is the whole body.
				w.rest(body[i:])
				return
			}
			continue
		}
		if need && c != '}' && c != ']' {
			need, depth = false, depth+1
			w.newline(depth)
		}
		begun = true
		switch c {
		case '{', '[':
			need, nest = true, nest+1
			w.put(c)
		case ',':
			w.put(c)
			w.newline(depth)
		case ':':
			w.put(c)
			w.put(' ')
		case '}', ']':
			if need {
				need = false
			} else {
				depth--
				w.newline(depth)
			}
			w.put(c)
			if nest--; nest == 0 {
				w.rest(body[i+1:])
				return
			}
		case '"':
			inStr = true
			w.put(c)
		default:
			w.put(c)
		}
	}
}

// rest is what follows the body's value, white space, kept as it is.
func (w *indenter) rest(b []byte) {
	for _, c := range b {
		w.put(c)
	}
}
