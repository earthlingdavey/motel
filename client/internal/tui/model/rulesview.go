package model

import (
	"strconv"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The rules screens: the rules the box alerts on, and how close each
// is (GET /_motel/rules, `motel monitor rules`, for a viewer of the box;
// GET /_motel/apps/<app>/rules, `motel monitor rules --app <app>`, for a
// viewer of the app). The client judges nothing of its own: a mark
// repeats a state or a level the box said, and every number is the
// box's, written in the client's words with its age. A rule's name, a
// guard's meaning, a reading's filesystem and a word the client does
// not know are the box's: each on a row of its own after the client's
// label, or quoted, and capped, so none begins a row where the screen's
// own do (a head at column 0, a label at column 4). A row is cut at the
// window's edge, but a condition and a guard's meaning, which go on at
// column 6 (Sentence) rather than lose their end.

// RulesAsOf is how old the box's answer about the rules is, for the
// crumb, and that the last read failed when it did; app is "" for the
// box's.
func (s *Session) RulesAsOf(app string, now time.Time) Line {
	s.seen(now)
	st, err := s.rules(app)
	if st == nil {
		return nil
	}
	l := s.asOf(st.At, now)
	if err != nil {
		l = append(l, ownC(" · the last read failed", Warn))
	}
	return l
}

// rules is the rules answer held for app ("" the box's) and its newest
// read's failure.
func (s *Session) rules(app string) (*Got[wire.Rules], error) {
	if app == "" {
		return s.BoxRulesSt, s.BoxRulesErr
	}
	if a := s.apps[app]; a != nil {
		return a.rules, a.rulesErr
	}
	return nil, nil
}

// StoreSays is the box's word for its store in the client's words: the
// screen's and the command's first line.
func StoreSays(r wire.Rules) Line {
	switch r.Store {
	case wire.StoreOn:
		return Line{own("on · numbers as the box last read the store (at most every 30 s)")}
	case wire.StoreOff:
		if len(r.Alerts) == 0 {
			return Line{own("off: this box raises no alerts (its monitor block sends nothing to anyone, or it has none)")}
		}
		return Line{own("off: this box has no metrics store, so no rule of the store's is listed")}
	case wire.StoreUnanswered:
		return Line{ownC("not known", Warn), own(": the store did not answer the box in time, so the store's rules have no numbers")}
	}
	return Line{ownC("not known", Warn), own(": the box said "), quoted(r.Store), own(", so the store's rules' numbers are not known")}
}

// RulesFileSays is the line for a rules file that is not the config's,
// nil when it is.
func RulesFileSays(r wire.Rules) Line {
	var why string
	switch r.RulesFile {
	case "":
		return nil
	case wire.RulesFileDiffers:
		why = "is not this config's render"
	case wire.RulesFileMissing:
		why = "is not there"
	case wire.RulesFileUnreadable:
		why = "could not be read"
	default:
		return Line{ownC("not this config's, maybe", Warn), own(": the box said "), quoted(r.RulesFile), own(" of the file vmalert reads")}
	}
	if r.Store == wire.StoreOff {
		// No store rule is listed: the file may still hold some.
		return Line{ownC("the file vmalert reads "+why, Warn), own(": vmalert may run rules not listed here (the box's journal says why)")}
	}
	return Line{ownC("the file vmalert reads "+why, Warn), own(": the store's rules listed may not be the ones it runs (the box's journal says why)")}
}

// RuleBlock is one rule's rows: its head in the client's words (its
// mark, severity, kind and scope), then under it, each after the
// client's label, its name, what it fires on (a row of its own, so that
// an 80-column window keeps the longest, disk_low's two levels, whole),
// a severity that is not motel's, each guard's meaning, and each number
// with its age and state.
func RuleBlock(a wire.AlertListing, store string, now time.Time) []Line {
	var out []Line
	for _, r := range ruleRows(a, store, now) {
		out = append(out, r.l)
	}
	return out
}

// ruleRow is one of a rule's rows, and whether a window too narrow for
// it lays it on more rows (the condition, the client's words; a guard's
// meaning, motel's sentence), going on at column 6, two in from its
// label, where neither a head nor a labelled row begins, rather than
// cutting it.
type ruleRow struct {
	l    Line
	wrap bool
}

func ruleRows(a wire.AlertListing, store string, now time.Time) []ruleRow {
	m := ruleMark(a)
	// A rule the store evaluates (neither a check of motel's nor an
	// event) has no number when the store was not read: not known, never
	// a quiet "no number" beside the store's line.
	unread := store != wire.StoreOn && a.Source != "motel check" && a.Kind != "event" && len(a.Readings) == 0
	if unread {
		m = MarkUnknown
	}
	head := Line{ownC(m.Glyph()+" ", m.Class()), ownC(pad(SeverityWord(a.Severity), 9), m.Class()), own(ruleKind(a.Kind) + " · " + ruleScope(a))}
	rows := []ruleRow{{l: head}, {l: boxField("rule", boxT(a.Name))}}
	if c := ruleCondition(a); c != "" {
		rows = append(rows, ruleRow{l: boxField("fires", own(c)), wrap: true})
	}
	if a.Severity != "" && SeverityWord(a.Severity) != a.Severity {
		rows = append(rows, ruleRow{l: boxField("severity", boxT(a.Severity))})
	}
	for _, g := range a.Guards {
		l := Line{}
		if g.Op != "" && g.Value != nil {
			l = append(l, own(opWord(g.Op)+" "+number(*g.Value, 6)+": "))
		}
		rows = append(rows, ruleRow{l: boxField("guard", append(l, boxT(g.Meaning))...), wrap: true})
	}
	for _, r := range a.Readings {
		rows = append(rows, ruleRow{l: boxField("now", reading(a, r, now)...)})
	}
	switch {
	case unread:
		rows = append(rows, ruleRow{l: boxField("now", ownC("not known: the store's numbers were not read", Warn))})
	case len(a.Readings) == 0 && a.Kind != "event":
		rows = append(rows, ruleRow{l: boxField("now", ownC("no number", Dim))})
	}
	return rows
}

// reading is one number of a rule in the client's words: what it is of
// (an app, a checked name; a filesystem or a backup, the box's text),
// the number, its age, and the state the box said of it.
func reading(a wire.AlertListing, r wire.Reading, now time.Time) Line {
	var l Line
	switch {
	case r.App != "" && r.Instance != "":
		l = append(l, boxT(r.Instance), own(" "+r.App+" "))
	case r.App != "":
		l = append(l, own(r.App+" "))
	case r.Instance != "":
		l = append(l, boxT(r.Instance), own(" "))
	}
	v := "no number"
	switch {
	case r.Value == nil:
	case r.Threshold != nil:
		v = number(*r.Value, 6) + " of " + Amount(*r.Threshold, a.Unit) + " in " + durWords(r.WindowSeconds)
	default:
		v = Amount(*r.Value, a.Unit)
	}
	l = append(l, own(v+" ("+Ago(now.Sub(r.MeasuredAt))+" ago)"))
	switch r.Level {
	case "":
	case "warning", "critical":
		l = append(l, own(" · firing at "+r.Level))
	default:
		l = append(l, own(" · at the level "), quoted(r.Level))
	}
	switch r.State {
	case "", wire.RuleInactive:
		if r.State != "" {
			l = append(l, ownC(" · inactive", Dim))
		}
	case wire.RulePending, wire.RuleFiring:
		s := " · " + r.State
		if r.ActiveSince != nil {
			s += " since " + clock(*r.ActiveSince, now, false) + " UTC (" + since(*r.ActiveSince, now) + ")"
		}
		l = append(l, ownC(s, Warn))
	default:
		l = append(l, ownC(" · state not known", Warn), own(", the box said "), quoted(r.State))
	}
	return l
}

// ruleMark is the worst a rule's numbers say the box holds it: firing
// (or a disk at a level) by its severity, pending as a look, a state
// the client does not know as cannot tell, nothing otherwise.
func ruleMark(a wire.AlertListing) Mark {
	out := MarkNone
	worse := func(m Mark) {
		if m < out {
			out = m
		}
	}
	bySeverity := func(sev string) Mark {
		switch sev {
		case "critical":
			return MarkBroken
		case "warning":
			return MarkLook
		case "info":
			return MarkNone
		}
		return MarkUnknown
	}
	for _, r := range a.Readings {
		switch r.State {
		case "", wire.RuleInactive:
		case wire.RuleFiring:
			worse(bySeverity(a.Severity))
		case wire.RulePending:
			worse(MarkLook)
		default:
			worse(MarkUnknown)
		}
		if r.Level != "" {
			worse(bySeverity(r.Level))
		}
	}
	return out
}

func ruleKind(k string) string {
	switch k {
	case "threshold", "event":
		return k
	case "guarded":
		return "threshold with guards"
	}
	return "kind not known"
}

func ruleScope(a wire.AlertListing) string {
	switch {
	case a.Subject == "box":
		return "the box"
	case a.Subject == "app" && a.App != "":
		return "app " + a.App
	case a.Subject == "app" && len(a.Apps) > 0:
		return "apps " + strings.Join(a.Apps, ", ")
	case a.Subject == "app":
		return "every app"
	}
	return "scope not known"
}

// ruleCondition is what the rule fires on, in the client's words: ""
// for one that says no threshold or op.
func ruleCondition(a wire.AlertListing) string {
	var c string
	switch {
	case a.Kind == "event":
		return "an event: nothing to be close to"
	case len(a.Levels) > 0:
		var ls []string
		gap, same := a.Levels[0].Threshold-a.Levels[0].ClearsBelow, true
		for _, lv := range a.Levels {
			ls = append(ls, opWord(a.Op)+" "+Amount(lv.Threshold, a.Unit)+" "+SeverityWord(lv.Severity))
			same = same && lv.Threshold-lv.ClearsBelow == gap
		}
		c = strings.Join(ls, ", ")
		if same {
			c += ", each clears " + Amount(gap, a.Unit) + " under"
		} else {
			for i, lv := range a.Levels {
				ls[i] += " (clears under " + Amount(lv.ClearsBelow, a.Unit) + ")"
			}
			c = strings.Join(ls, ", ")
		}
	case a.Threshold != nil:
		c = opWord(a.Op) + " " + Amount(*a.Threshold, a.Unit)
	case a.Name == "watchdog_throttled":
		// motel's own: a spent budget is not firing (liveswap/watchdog.go);
		// it fires when a further restart is needed and the budget is spent.
		return "a further restart needed while its budget in its window is spent"
	case a.Op != "":
		c = opWord(a.Op) + " each number's own threshold"
	default:
		return ""
	}
	if a.ForSeconds > 0 {
		c += " for " + durWords(float64(a.ForSeconds))
	}
	return c
}

// opWord is a comparison motel writes, or "?" for one it does not.
func opWord(op string) string {
	switch op {
	case ">", ">=", "<", "<=":
		return op
	}
	return "?"
}

// Amount is v in a rule's unit, in the client's words: a share as a
// percentage, seconds as a duration, bytes in binary units. A unit the
// client does not know is quoted beside the number.
func Amount(v float64, unit string) string {
	g := number
	switch unit {
	case "share":
		return g(v*100, 4) + " %"
	case "percent":
		// Down, so that 79.96 is not read as the 80 it is under.
		t := float64(int64(v * 10))
		if t > v*10 {
			t--
		}
		return strings.TrimSuffix(strconv.FormatFloat(t/10, 'f', 1, 64), ".0") + " %"
	case "seconds":
		switch a := abs(v); {
		case a < 1 && a > 0:
			return g(v*1000, 3) + " ms"
		case a < 60:
			return g(v, 3) + " s"
		}
		return durWords(v)
	case "bytes":
		sign := ""
		if v < 0 {
			sign, v = "-", -v
		}
		for _, u := range []struct {
			n    float64
			name string
		}{{1 << 40, "TiB"}, {1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
			if v >= u.n {
				return sign + g(v/u.n, 4) + " " + u.name
			}
		}
		return sign + g(v, 3) + " bytes"
	case "requests/s", "cores", "restarts", "reads":
		return g(v, 4) + " " + unit
	case "":
		return g(v, 6)
	}
	return g(v, 6) + " (in " + termtext.Quote(unit).String() + ")"
}

// durWords is seconds as a duration, its two largest parts: 10m,
// 3h 59m, 1d 2h, 45s.
func durWords(sec float64) string {
	neg := sec < 0
	d := time.Duration(abs(sec) * float64(time.Second)).Truncate(time.Second)
	var parts []string
	for _, u := range []struct {
		d time.Duration
		s string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.d; n > 0 && len(parts) < 2 {
			parts = append(parts, strconv.Itoa(int(n))+u.s)
			d -= n * u.d
		} else if len(parts) > 0 {
			break
		}
	}
	out := strings.Join(parts, " ")
	if out == "" {
		out = "0s"
	}
	if neg {
		out = "-" + out
	}
	return out
}

// BoxRulesView is the rules screen of the box, its lines whole.
func (s *Session) BoxRulesView(now time.Time) []Line { return s.RulesViewFit("", now, 0, nil) }

// RulesViewFit is the rules screen of app ("" the box's), laid to width
// cells; 0 leaves each line one line. A line holding a box's value is
// cut to the width, but a condition and a guard's meaning, laid on rows
// going on at column 6, so a value never begins a row where the
// screen's own do; the client's sentences are laid on rows (Sentence).
func (s *Session) RulesViewFit(app string, now time.Time, width int, cells Cells) []Line {
	s.seen(now)
	st, err := s.rules(app)
	path := "GET /_motel/rules"
	if app != "" {
		path = "GET /_motel/apps/" + app + "/rules"
	}
	switch {
	case NotFound(err) && app == "":
		return Sentence(Line{own("Not said here: the box answered 404, so its moteld is older than this read (" + path + ").")}, width, cells)
	case NotFound(err):
		// A moteld older than the read, or no app of the name now: the
		// box's own words say which.
		return Sentence(Line{own("Not said here: the box answered 404 to " + path + " (a moteld older than this read, or no app of that name now): "), Seg{T: Capped(failureText(err), 2*LineMax)}}, width, cells)
	case st == nil && err != nil:
		l := Line{{T: Capped(failureText(err), 4*LineMax)}}
		if Forbidden(err) {
			l = append(l, s.askAgain(now)...)
		}
		return Sentence(l, width, cells)
	case st == nil:
		return Sentence(Line{own("asking " + s.Host + " which rules it alerts on…")}, width, cells)
	}
	r := st.V
	out := Sentence(field("store", StoreSays(r)...), width, cells)
	if l := RulesFileSays(r); l != nil {
		out = append(out, Sentence(field("rules file", l...), width, cells)...)
	}
	if len(r.Alerts) > 0 {
		heading := "RULES, as the box alerts on them"
		if app != "" {
			heading = "RULES that apply to " + app + ", with its numbers alone"
		}
		out = append(out, nil, cutTo(Line{ownC(heading, Label)}, width, cells))
	}
	for _, a := range r.Alerts {
		for _, row := range ruleRows(a, r.Store, now) {
			if row.wrap {
				out = append(out, Sentence(row.l, width, cells)...)
				continue
			}
			out = append(out, cutTo(row.l, width, cells))
		}
	}
	if err != nil {
		out = append(append(out, nil), Sentence(Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(err), 2*LineMax)}}, width, cells)...)
	}
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// number is v to n significant digits, not in an exponent's form
// (1000, not 1e+03; 0.0001234, not 1.234e-04), and a whole number under
// 1e15 exactly (12345 restarts); outside 1e-9 to 1e15 a box's number is
// left in the exponent's form, so a row cannot run to hundreds of
// digits.
func number(v float64, n int) string {
	if a := abs(v); a >= 1e15 || a != 0 && a < 1e-9 {
		return strconv.FormatFloat(v, 'g', n, 64)
	}
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', n, 64), 64)
	if err != nil {
		return strconv.FormatFloat(v, 'g', n, 64)
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}
