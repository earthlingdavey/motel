package model

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The apps table's columns, in the order a narrow screen drops them:
// last the first (AGE folds into the title bar's word).
const (
	ColName = iota
	ColState
	ColVersion
	ColLast
	ColMem
	ColOfCap
	Col5xx
	ColP95
	ColAge
	NCols
)

// ColHeads are the columns' headings.
var ColHeads = [NCols]string{"NAME", "STATE", "VERSION", "LAST DEPLOY", "MEM", "OF CAP", "5xx", "P95", "AGE"}

// AppRow is one app on the apps screen. Its state is the box's facts
// only: the client judges nothing the box does not (no threshold of
// its own on memory or latency).
type AppRow struct {
	// Name is the app, a token of the box's app grammar.
	Name  string
	Mark  Mark
	Cells [NCols]termtext.Text
	Stale bool
	// Why is the "needs a look" line, empty when the row needs none.
	Why Line
	// Failed is the newest deploy's version when it failed.
	Failed string
	// state is STATE's word before a stale row's suffix: what the
	// strip counts by.
	state string
	mem   float64
	share float64
}

// Sorts are the apps screen's orders, which s cycles.
var Sorts = []string{"attention", "name", "memory", "5xx"}

// AppsView is the apps screen.
type AppsView struct {
	Rows []AppRow
	// Strip is every row's reason, in the rows' order; AllWell the line
	// said when none has one.
	Strip   []Line
	AllWell Line
	// Summary is the crumb's right side.
	Summary Line
	// Empty is said in place of the table when the box lists no app.
	Empty Line
	// ListFailed says, above the strip, how old the list held is and
	// why the last read of it failed; nil while it reads.
	ListFailed Line
}

// AppsView is the apps screen at now, filtered and sorted. A filter of
// "!" or "!ok", with or without a name after it, keeps only marked
// rows.
func (s *Session) AppsView(now time.Time, filter string, sort int) AppsView {
	var v AppsView
	switch {
	case s.List == nil && s.ListErr != nil:
		v.Empty = append(Line{own("no apps list: "), Seg{T: Capped(failureText(s.ListErr), 2*LineMax)}}, s.askAgain(now)...)
		return v
	case s.List == nil && s.Engine.Stopped() != Running:
		v.Empty = Line{own("not asking " + s.Host + " for the apps you may view: every read has stopped (above).")}
		return v
	case s.List == nil:
		v.Empty = Line{own("asking " + s.Host + " for the apps you may view…")}
		return v
	}
	if s.ListErr != nil {
		v.ListFailed = append(Line{ownC("list as of "+hms(s.List.At)+" UTC, "+Ago(now.Sub(s.List.At))+" ago · the last read failed: ", Warn),
			Seg{T: Capped(failureText(s.ListErr), 2*LineMax)}}, s.askAgain(now)...)
	}
	names := s.List.V
	if len(names) == 0 {
		v.Empty = Line{own("You may view no app on " + s.Host + ". "), own("Ask the operator for a grant line.")}
		return v
	}
	marked := false
	if rest, ok := strings.CutPrefix(filter, "!"); ok {
		marked, filter = true, strings.TrimPrefix(rest, "ok")
	}
	filter = strings.TrimSpace(filter)
	need, all, notRunning, asking := 0, 0, 0, 0
	for _, n := range names {
		r := s.row(n, now)
		if r.Mark < MarkNone {
			need++
		}
		switch r.state {
		case "running":
		case "asking…":
			asking++
		default:
			notRunning++
		}
		all++
		if marked && r.Mark >= MarkNone || filter != "" && !strings.Contains(n, filter) {
			continue
		}
		v.Rows = append(v.Rows, r)
	}
	switch Sorts[sort%len(Sorts)] {
	case "attention":
		slices.SortStableFunc(v.Rows, func(a, b AppRow) int { return int(a.Mark) - int(b.Mark) })
	case "memory":
		slices.SortStableFunc(v.Rows, func(a, b AppRow) int { return cmpDesc(a.mem, b.mem) })
	case "5xx":
		slices.SortStableFunc(v.Rows, func(a, b AppRow) int { return cmpDesc(a.share, b.share) })
	}
	for _, r := range v.Rows {
		if len(r.Why) > 0 {
			v.Strip = append(v.Strip, append(Line{ownC(r.Mark.Glyph()+" ", r.Mark.Class()), own(pad(r.Name, 12) + " ")}, r.Why...))
		}
	}
	switch {
	case asking > 0 && need == 0:
		v.AllWell = Line{own("asking the box for " + strconv.Itoa(asking) + " of " + strconv.Itoa(all) + " apps…")}
	case need == 0 && notRunning == 0:
		v.AllWell = Line{own("all " + strconv.Itoa(all) + " apps running; every newest deploy succeeded")}
	case need == 0:
		v.AllWell = Line{own("nothing needs a look; " + strconv.Itoa(notRunning) + " of " + strconv.Itoa(all) + " apps are not deployed or not yours to view")}
	}
	sum := strconv.Itoa(all) + " apps · " + strconv.Itoa(need) + " need a look · by " + Sorts[sort%len(Sorts)]
	if filter != "" || marked {
		sum = strconv.Itoa(len(v.Rows)) + " of " + sum
	}
	v.Summary = Line{own(sum)}
	return v
}

// NameMax is the most of an app's name the apps table shows. A longer
// one (63 characters are allowed) is cut in the middle, its start and
// its end kept, so that names which share a start still differ on
// screen; the whole name is what is opened, filtered and completed.
const NameMax = 20

func cutName(name string) string {
	r := []rune(name)
	if len(r) <= NameMax {
		return name
	}
	const end = 7
	return string(r[:NameMax-1-end]) + "…" + string(r[len(r)-end:])
}

func cmpDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

func pad(s string, n int) string {
	if r := len([]rune(s)); r < n {
		return s + strings.Repeat(" ", n-r)
	}
	return s
}

// row derives an app's row from the box's facts, the first rule that
// holds giving its state.
func (s *Session) row(name string, now time.Time) (r AppRow) {
	r = AppRow{Name: name, Mark: MarkNone}
	r.Cells[ColName] = termtext.From(cutName(name))
	for c := ColVersion; c < ColAge; c++ {
		r.Cells[c] = termtext.From("-")
	}
	a := s.apps[name]
	if a == nil || a.st == nil && a.err == nil {
		r.state, r.Cells[ColState] = "asking…", termtext.From("asking…")
		return r
	}
	at := a.errAt
	if a.st != nil {
		at = a.st.At
	}
	// Stale is judged against the interval the row's next read was due
	// by when its last answer came: three reads that failed, or three
	// that should have come and did not (an interval's grace: the first
	// read after an app's first answer waits for its place in the
	// cycle). A held row is not being asked, so it is never stale.
	misses, held, every, woke := s.Engine.Row(name)
	r.Stale = !held && (woke || misses >= StaleAfter || Stale(now.Sub(at), every))
	r.Cells[ColAge] = termtext.From(Ago(now.Sub(at)))
	if r.Stale {
		r.Cells[ColAge] = termtext.From("stale")
		// The word goes in STATE too, which no width drops: a dimmed
		// row alone would say it by style only.
		defer func() { r.Cells[ColState] = termtext.From(r.Cells[ColState].String() + " · stale") }()
	}
	set := func(m Mark, state string, why ...Seg) {
		r.Mark, r.state, r.Cells[ColState], r.Why = m, state, termtext.From(state), why
	}
	out, _ := Classify(a.err)
	switch {
	case Forbidden(a.err):
		set(MarkNothing, "no access")
		return r
	case a.err != nil && (a.st == nil || out == Refused):
		set(MarkUnknown, "unknown", own("cannot tell: "), Seg{T: Capped(failureText(a.err), LineMax)})
		return r
	}
	st := a.st.V
	r.Cells[ColVersion] = termtext.From(short(st.CurrentVersion))
	if st.CurrentVersion == "" {
		r.Cells[ColVersion] = termtext.From("-")
	}
	var wd wire.Watchdog
	if st.Watchdog != nil {
		wd = *st.Watchdog
	}
	switch {
	case st.CurrentVersion == "":
		set(MarkNothing, "not deployed")
	case wd.State == "throttled":
		why := Line{own("throttled: " + strconv.Itoa(wd.RestartsInWindow) + " restarts in its window")}
		if wd.LastRestartCause != "" {
			why = append(why, own("; last cause "), quoted(wd.LastRestartCause))
		}
		set(MarkBroken, "throttled", why...)
	case !st.Running:
		set(MarkBroken, "stopped", own("stopped: "+short(st.CurrentVersion)+" is not running"))
	case wd.State == "backoff":
		set(MarkLook, "backoff", own("backoff: the watchdog waits to restart it; "+strconv.Itoa(wd.Failures)+" failed checks in a row"))
	case st.Phase != "idle":
		set(MarkMoving, st.Phase, own("deploying: "+st.Phase+" (seen "+Ago(now.Sub(at))+" ago)"))
	default:
		set(MarkNone, "running")
	}
	if len(st.Deploys) > 0 {
		d := st.Deploys[0]
		// The box's own word: a status is a token of its grammar.
		word := d.Status
		if d.Status == "failed" {
			r.Failed = d.Version
		}
		ago := "?"
		if t, ok := rawTime(d.FinishedAt); ok {
			ago = Ago(now.Sub(t))
		}
		last := pad(word, 10) + pad(ago, 4)
		if d.Phase != "" {
			last += " " + d.Phase
		}
		r.Cells[ColLast] = termtext.From(last)
		if d.Status == "failed" && r.Mark > MarkLook {
			why := Line{own("newest deploy " + short(d.Version) + " failed")}
			if d.Phase != "" {
				why[0] = own(why[0].T.String() + " in " + d.Phase)
			}
			why = append(why, own(" "+ago+" ago"))
			if st.CurrentVersion != "" && st.Running {
				why = append(why, own("; "+short(st.CurrentVersion)+" serves"))
			}
			r.Mark, r.Why = MarkLook, why
		}
	}
	if m := st.Metrics; m != nil && m.Error == "" {
		if m.Memory != nil && m.Memory.Now != nil {
			r.mem = *m.Memory.Now
			r.Cells[ColMem] = termtext.From(num(r.mem/(1<<20), 0) + "M")
			if c := memCap(st); c > 0 {
				r.Cells[ColOfCap] = termtext.From(pct(r.mem / c))
			}
		}
		if q := m.Requests; q != nil {
			if q.Share5xx != nil {
				r.share = *q.Share5xx
				r.Cells[Col5xx] = termtext.From(pct(r.share))
			}
			if q.P95 != nil {
				r.Cells[ColP95] = termtext.From(num(*q.P95, 0) + "ms")
			}
		}
	}
	return r
}

// Stale says whether an answer this old is stale for a row read every
// every: older than StaleAfter intervals and one of grace.
func Stale(age, every time.Duration) bool { return age > (StaleAfter+1)*every }

// memCap is the memory cap in bytes: the unit's, else the store's.
func memCap(st wire.Status) float64 {
	if st.Caps != nil && st.Caps.MemoryMax > 0 {
		return float64(st.Caps.MemoryMax)
	}
	if m := st.Metrics; m != nil && m.MemoryMax != nil {
		return *m.MemoryMax
	}
	return 0
}

// failureText is what the client says of err: its own sentence, with
// what the box sent in it already made safe or quoted.
func failureText(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Message
	}
	return "no answer"
}
