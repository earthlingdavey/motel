package model

import (
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
	AsOf   Line
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
	}
	if st.CurrentVersion == "" {
		v.Note = []Line{{own(app + " has no version yet. Its first deploy comes from CI. Nothing to show until then.")}}
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
		// The client holds no grammar for the watchdog's state, so a
		// word it does not know is shown as the box's value.
		w := Line{boxT(wd.State)}
		if watchdogStates[wd.State] {
			w = Line{own(wd.State)}
		}
		w = append(w, own(" · "+strconv.Itoa(wd.RestartsInWindow)+" restarts in its window"))
		if wd.Failures > 0 {
			w = append(w, own(" · "+strconv.Itoa(wd.Failures)+" failed checks in a row"))
		}
		if !wd.LastRestartAt.IsZero() {
			w = append(w, own(" · last restart "+hm(wd.LastRestartAt)+" UTC"))
		}
		v.Fields = append(v.Fields, field("watchdog", w...))
		if wd.LastRestartCause != "" {
			v.Fields = append(v.Fields, field("restarted", boxT(wd.LastRestartCause)))
		}
		if wd.LastFailure != "" {
			v.Fields = append(v.Fields, field("last check", boxT(wd.LastFailure)))
		}
	}
	if d := st.LastDeploy; d != nil {
		l := Line{ownC(d.Status, statusClass(d.Status))}
		if d.Phase != "" {
			l = append(l, own(" in "+d.Phase))
		}
		l = append(l, own(" "+Ago(now.Sub(d.FinishedAt))+" ago ("+hm(d.FinishedAt)+" UTC, "+secs(d.FinishedAt.Sub(d.StartedAt))+")"))
		if d.By != "" {
			l = append(l, own(" by "), boxT(d.By))
		}
		v.Fields = append(v.Fields, field("deployed", l...))
		if d.Error != "" {
			v.Fields = append(v.Fields, field("error", boxT(d.Error)))
		}
		if d.Note != "" {
			v.Fields = append(v.Fields, field("note", boxT(d.Note)))
		}
	}
	v.Fields = append(v.Fields, s.metricLines(st)...)
	if d := st.LastDeploy; d != nil && d.ArtifactEntries > 0 {
		v.Fields = append(v.Fields, field("artifact", own(strconv.Itoa(d.ArtifactEntries)+" entries · "+mib(float64(d.ArtifactBytes))+" (the last deploy's)")))
	}
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
		if f, ok := rawTime(d.FinishedAt); ok {
			fin = f.UTC().Format("01-02 15:04")
			if b, ok := rawTime(d.StartedAt); ok {
				took = secs(f.Sub(b))
			}
		}
		r.Cells = []termtext.Text{termtext.From(d.Version), termtext.From(d.Status), termtext.From(d.Phase), termtext.From(fin), termtext.From(took), Capped(d.By, 60)}
		v.Deploys = append(v.Deploys, r)
	}
	return v
}

func statusClass(status string) Class {
	if status == "failed" {
		return Bad
	}
	return Plain
}

func (s *Session) metricLines(st wire.Status) []Line {
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
				f := min(*mem.Now/cap, 1)
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
	if m.At != nil {
		l = append(l, own(", newest point "+hms(*m.At)+" UTC (the store runs 30 s behind)"))
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
	case err != nil:
		v.Note = []Line{{Seg{T: Capped(failureText(err), 4*LineMax)}}}
		return v
	}
	d := g.V
	took := secs(d.FinishedAt.Sub(d.StartedAt))
	span := " · " + hms(d.StartedAt) + " → " + hms(d.FinishedAt) + " UTC"
	var verdict Line
	if d.Status == "failed" {
		w := "✖ FAILED"
		if d.Phase != "" {
			w += " in " + d.Phase
		}
		verdict = Line{ownC(w, Bad), own(" after " + took + span)}
	} else {
		verdict = Line{ownC("✓ SUCCEEDED", Good), own(" in " + took + span)}
	}
	v.Head = append(v.Head, verdict)
	if d.By != "" {
		v.Head = append(v.Head, field("by", boxT(d.By)))
	}
	if a := s.apps[app]; a != nil && a.st != nil {
		cur := a.st.V.CurrentVersion
		switch cur {
		case "":
			v.Head = append(v.Head, field("serves now", own("nothing (as of "+hms(a.st.At)+" UTC)")))
		case d.Version:
			v.Head = append(v.Head, field("serves now", own("this version (as of "+hms(a.st.At)+" UTC)")))
		default:
			v.Head = append(v.Head, field("serves now", own(cur+" (as of "+hms(a.st.At)+" UTC)")))
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
				l = append(l, own(" from "), boxT(p.Location))
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
			for _, t := range termtext.Lines(l) {
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
