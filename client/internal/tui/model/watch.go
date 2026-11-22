package model

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The watch (#576): what fires that this person may see, read whatever
// screen is open, so the title bar can say it on every screen and the
// bell can ring for a new alert. A viewer of the box reads the box's
// alerts (GET /_motel/alerts) every WatchEvery; a person with app
// grants only reads each app's alerts in the background, beside its
// status. Which follows whoami's roles as they change (Session.follow).
//
// The watch keeps its own state, apart from BoxAlertsSt and an app's
// alerts, which a `monitor: unanswered` answer replaces: for each read
// it takes, the last known set of what was counted and when, why the
// count is not known when it is not, and whether its newest read
// failed. What was new at an answer goes to the alarm (alarm.go).

// Watch is which reads the watch makes.
type Watch int

// The watches.
const (
	// WatchNone: none (whoami not answered yet, or no grant here).
	WatchNone Watch = iota
	// WatchBox: a viewer of the box: the box's alerts, every WatchEvery.
	WatchBox
	// WatchApps: a person with app grants only: each listed app's
	// alerts, every max(AppWatchEvery, the status's interval).
	WatchApps
)

const (
	// WatchEvery is a viewer of the box's alerts read, whatever screen
	// is open: a state alert waits 30 s in Alertmanager before any
	// channel is told, so the TUI sees it before a chat message does.
	WatchEvery = 15 * time.Second
	// AppWatchEvery is the shortest interval of a background app's
	// alerts read.
	AppWatchEvery = 30 * time.Second
	// PausedWatchEvery is the shortest interval of the watch's reads
	// while the TUI is paused (Idle): the token's life, not the read's
	// pace, sets how often a key signs, so a minute costs a viewer of
	// the box 480 reads in 8 hours and no more signatures than 15 s.
	PausedWatchEvery = 60 * time.Second
)

// Unknown is why the watch's count is not known.
type Unknown int

// The reasons.
const (
	// Known: it is.
	Known Unknown = iota
	// NotAsked: no read of it has ended yet.
	NotAsked
	// WasRefused: its read was refused (a 403, a 404, a 409).
	WasRefused
	// Unanswered: the box's Alertmanager did not answer the box.
	Unanswered
	// NotAWord: the box said a monitor word the client does not know.
	NotAWord
	// FailedFirst: its read failed before any answer was known.
	FailedFirst
	// Forgotten: the session dropped what the box said (an answer about
	// another key or box).
	Forgotten
	// NoWhoAmI: whoami has not answered as the client takes it, so
	// what may be watched is not known.
	NoWhoAmI
)

// watchState is the session's watch.
type watchState struct {
	mode Watch
	// forgotten: the session dropped what the box said; not known until
	// whoami answers again.
	forgotten bool
	// srcs are the reads taken: "" the box's alerts, an app's name its
	// alerts.
	srcs map[string]*watchSrc
}

// watchSrc is one read's part of the watch.
type watchSrc struct {
	// known: an answer was taken that said what fires (monitor on or
	// off): set, at, n, worst, more and off are that answer's.
	known bool
	// set is the keys counted at the last known answer, and, while the
	// box says it holds more than it sends, those of the answers before
	// it not in it: one left off the end is never taken as gone.
	set   map[string]bool
	at    time.Time
	n     int
	worst Mark
	// more: the box held more alerts than it sent (BoxAlertsMax).
	more bool
	// off: the box has no monitor: nothing can fire.
	off bool
	// why is why the count is not known now; Known when it is.
	why Unknown
	// failed: the newest read got no answer, or one the client could
	// not use, or a 429, or stopped every read: the count is old.
	failed bool
	// every is the read's interval when its last known answer came: the
	// next was due by it, whatever the interval is now (an app's screen
	// opened makes its alerts' 15 s).
	every time.Duration
}

// alertKey is an alert as the watch tells one from another across
// reads: its name, app, instance, severity and when it began. A refire
// is a new start, a severity change a new alert; Alertmanager keeps the
// earliest start of one held across moteld's restarts. A row the box's
// filter withheld has no name or instance: its key is its app, its
// severity and its start, which hold while it stays withheld. The
// fields are the box's text, any byte in them: the key is them encoded
// as a JSON list, so no two lists of fields make one key.
func alertKey(a wire.BoxAlert) string {
	b, _ := json.Marshal([]string{a.Name, a.App, a.Instance, a.Severity, a.StartsAt.UTC().Format(time.RFC3339Nano)}) // strings: nothing to fail
	return string(b)
}

// counted says whether the watch counts a: one the alerts screen marks
// (critical, warning, a severity or a state the client does not know),
// as the box screen's line counts what fires. One held quiet (a silence,
// the deploy mute) and one of severity info are not.
func counted(a wire.BoxAlert) bool { return alertMark(a) != MarkNone }

// take is a known answer at at: its rows, whether the box held more
// than it sent, and whether it has no monitor. It returns the keys new
// at it: counted now and not in the last known set; none at the first.
// The keys are a set: identical rows (a ChannelFailed told to several
// channels) count once.
func (w *watchSrc) take(rows []wire.BoxAlert, more, off bool, at time.Time) []string {
	set, present := map[string]bool{}, map[string]bool{}
	n, worst := 0, MarkLook
	for _, a := range rows {
		k := alertKey(a)
		present[k] = true
		if !counted(a) || set[k] {
			continue
		}
		set[k] = true
		n++
		if alertMark(a) == MarkBroken {
			worst = MarkBroken
		}
	}
	var fresh []string
	if w.known {
		for k := range set {
			if !w.set[k] {
				fresh = append(fresh, k)
			}
		}
		slices.Sort(fresh)
	}
	if more {
		for k := range w.set {
			if !present[k] {
				set[k] = true
			}
		}
	}
	w.known, w.set, w.at, w.n, w.worst, w.more, w.off = true, set, at, n, worst, more, off
	w.why, w.failed = Known, false
	return fresh
}

// follow sets the watch by whoami's roles: a viewer of the box watches
// the box's alerts, a person with app grants only each app's. A change
// of watch starts it afresh: the first answer of the new one is
// nothing new. A read the box refused (a 403) is asked again when
// whoami lists its role, not left until ctrl-r: a grant limited by
// `from` refuses it on another network and allows it back home.
func (s *Session) follow(now time.Time) {
	w := WatchNone
	switch {
	case !s.Trusted():
	case s.BoxViewer():
		w = WatchBox
	case len(s.Me.V.Roles) > 0:
		w = WatchApps
	}
	changed := w != s.watch.mode || s.watch.forgotten
	if changed {
		// What arrived under the watch left is not new to the one taken:
		// its first answer is nothing new.
		s.watch = watchState{mode: w}
		s.alarm.unseen = nil
		s.Engine.SetWatch(w, now)
	}
	// A read held by a refusal from before a change of watch (a 404 on a
	// screen opened then) is asked again too: the watch starts afresh.
	again := func(err error) bool { return Forbidden(err) || changed && NotFound(err) }
	switch w {
	case WatchBox:
		if again(s.BoxAlertsErr) {
			s.Engine.Again(now, Job{Kind: KBoxAlerts})
		}
	case WatchApps:
		for _, r := range s.Me.V.Roles {
			if n, ok := strings.CutPrefix(r.Scope, "app:"); ok {
				if a := s.apps[n]; a != nil && again(a.alertsErr) {
					s.Engine.Again(now, Job{Kind: KAlerts, App: n})
				}
			}
		}
	}
	// The logs screen's read follows the logs grant: made while whoami
	// lists one; held by a 403, it is asked again by a whoami answer that
	// lists the grant, since a refusal starts the read as not granted
	// (logsFresh): after a grant removed and given back, and also at each
	// whoami answer while the box refuses a grant whoami lists, a refused
	// read and a journal line each time (the accepted row in
	// client/README.md).
	if d := s.logs; d != nil {
		granted := s.LogsGrant(d.app) == Granted
		if granted && !d.granted && Forbidden(d.err) {
			s.Engine.Again(now, Job{Kind: KLogs, App: d.app})
		}
		d.granted = granted
	}
	s.logsAsk(now)
}

// watching says whether j is the watch's read: a refusal of it goes in
// the mark, not in the title bar's word.
func (s *Session) watching(j Job) bool {
	return j.Kind == KBoxAlerts && s.watch.mode == WatchBox || j.Kind == KAlerts && s.watch.mode == WatchApps && slices.Contains(s.watched(), j.App)
}

// watched is the reads the watch counts now: "" for the box's alerts,
// or each listed app whoami lists a grant on (a grant removed counts at
// whoami's answer, not at the list's next); nil when it counts none.
func (s *Session) watched() []string {
	switch s.watch.mode {
	case WatchBox:
		return []string{""}
	case WatchApps:
		var out []string
		for _, n := range s.Names() {
			if s.Me != nil && slices.ContainsFunc(s.Me.V.Roles, func(r wire.RoleHeld) bool { return r.Scope == "app:"+n }) {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

// watchList is the apps list answered: an app no longer listed leaves
// the watch, with what arrived on it, so one listed again starts from a
// first answer.
func (s *Session) watchList() {
	names := s.Names()
	for n := range s.watch.srcs {
		if n != "" && !slices.Contains(names, n) {
			delete(s.watch.srcs, n)
		}
	}
	for k := range s.alarm.unseen {
		if k.src != "" && !slices.Contains(names, k.src) {
			delete(s.alarm.unseen, k)
		}
	}
}

// unseen is how many of what arrived after since are not yet seen, of
// the reads the watch counts now: one of an app no longer listed is not.
func (s *Session) unseen(since time.Time) int {
	n, srcs := 0, s.watched()
	for k, at := range s.alarm.unseen {
		if slices.Contains(srcs, k.src) && (since.IsZero() || at.After(since)) {
			n++
		}
	}
	return n
}

// watchRead takes the read j's end into the watch, when it is the
// watch's read: its answer (BoxAlertsSt, or the app's alerts, already
// applied), or its failure err. A new alert goes to the alarm.
func (s *Session) watchRead(j Job, at time.Time, err error) {
	if !s.watching(j) {
		return
	}
	name := j.App
	if s.watch.srcs == nil {
		s.watch.srcs = map[string]*watchSrc{}
	}
	src := s.watch.srcs[name]
	if src == nil {
		src = &watchSrc{why: NotAsked}
		s.watch.srcs[name] = src
	}
	if err != nil {
		switch out, _ := Classify(err); {
		case out == Cancelled:
		case out == Refused && !notSent(err) && !unusable(err):
			src.why = WasRefused
		default:
			src.failed = true
			if !src.known {
				src.why = FailedFirst
			}
		}
		return
	}
	var rows []wire.BoxAlert
	var monitor string
	more := false
	if j.Kind == KBoxAlerts {
		if s.BoxAlertsSt == nil {
			return
		}
		b := s.BoxAlertsSt.V
		rows, monitor, more = b.Alerts, b.Monitor, b.MoreAlerts
	} else {
		a := s.apps[j.App]
		if a == nil || a.alerts == nil {
			return
		}
		monitor = a.alerts.V.Monitor
		for _, al := range a.alerts.V.Alerts {
			rows = append(rows, wire.BoxAlert{Alert: al, App: j.App})
		}
	}
	var fresh []string
	switch monitor {
	case wire.MonitorOn:
		fresh = src.take(rows, more, false, at)
		src.every = s.Engine.WatchInterval(j, at)
	case wire.MonitorOff:
		fresh = src.take(nil, false, true, at)
		src.every = s.Engine.WatchInterval(j, at)
	case wire.MonitorUnanswered:
		src.why, src.failed = Unanswered, false
		return
	default:
		src.why, src.failed = NotAWord, false
		return
	}
	s.alarm.took(name, src.set, fresh, at)
}

// unusable says whether err is an answer the client could not use.
func unusable(err error) bool { k, ok := failKind(err); return ok && k == FailUnusable }

// Firing is the watch's one value for the title bar's mark.
type Firing struct {
	// Why is Known when the count is; else why not.
	Why Unknown
	// N is how many alerts fire that this person may see, counted as
	// the alerts screen marks them; More, that the box held more than it
	// sent, so N is the least; Worst the mark of the worst of them.
	N     int
	More  bool
	Worst Mark
	// Off: the box has no monitor, so nothing can fire.
	Off bool
	// At is when the count was read (the oldest of its reads'); Old,
	// that it may no longer hold: the reads paused or stopped, the
	// watch's newest read failed, or its answer is older than its
	// interval and WaitingAfter (a read that waits, a signature).
	At  time.Time
	Old bool
	// New is how many of them arrived at an answer and are not yet seen.
	New int
	// Nothing: there is nothing this person may watch (no grant).
	Nothing bool
}

// Firing is the watch's value at now.
func (s *Session) Firing(now time.Time) Firing {
	s.seen(now)
	w := &s.watch
	stopped := s.Engine.Stopped() != Running
	switch {
	case w.forgotten:
		return Firing{Why: Forgotten}
	case w.mode == WatchNone && s.Me == nil && (s.MeErr != nil || stopped):
		return Firing{Why: NoWhoAmI}
	case w.mode == WatchNone && s.Me == nil:
		return Firing{Why: NotAsked}
	case w.mode == WatchNone:
		return Firing{Nothing: true}
	}
	if w.mode == WatchApps && s.List == nil {
		if stopped || s.ListErr != nil {
			return Firing{Why: FailedFirst}
		}
		return Firing{Why: NotAsked}
	}
	names := s.watched()
	f := Firing{Why: Known, Worst: MarkLook, Off: len(names) > 0}
	pending, why := false, Known
	for _, n := range names {
		src := w.srcs[n]
		switch {
		case src == nil || src.why == NotAsked:
			pending = true
		case src.why != Known:
			if why == Known {
				why = src.why
			}
		default:
			f.N += src.n
			f.More = f.More || src.more
			f.Off = f.Off && src.off
			if src.worst == MarkBroken && src.n > 0 {
				f.Worst = MarkBroken
			}
			if f.At.IsZero() || src.at.Before(f.At) {
				f.At = src.at
			}
			job := Job{Kind: KBoxAlerts}
			if w.mode == WatchApps {
				job = Job{Kind: KAlerts, App: n}
			}
			if src.failed || now.Sub(src.at) > max(src.every, s.Engine.WatchInterval(job, now))+WaitingAfter {
				f.Old = true
			}
		}
	}
	switch {
	case why != Known:
		return Firing{Why: why}
	case pending && stopped:
		return Firing{Why: FailedFirst}
	case pending:
		return Firing{Why: NotAsked}
	}
	if stopped && !f.At.IsZero() {
		// With nothing to read (no app listed) nothing is old.
		f.Old = true
	}
	f.New = s.unseen(time.Time{})
	return f
}
