// Package status says, from the record a run leaves and from what the
// manager says is running, whether the backups are fresh and whether a
// restore would work. It reads nothing itself and needs no privilege:
// the record holds no secret, and is for everyone to read.
package status

import (
	"fmt"
	"sort"
	"time"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

const (
	// StaleAfter is how old an app's last complete backup may be: two
	// hourly runs missed, and some grace.
	StaleAfter = 3 * time.Hour
	// ProvenOldAfter is how old a proven restore may be: the weekly
	// drill's period and a day.
	ProvenOldAfter = 8 * 24 * time.Hour
	// CheckOldAfter is how old the last clean check of the repository
	// may be, and how long a box set up may go without one: the weekly
	// drill's period and a day.
	CheckOldAfter = ProvenOldAfter
)

// Running is one unit of a run, a restore or a drill that the manager
// holds right now.
type Running struct {
	// Role is what the unit does (upload, fetch, …) and App whose data
	// it does it to; a unit of the whole run has no App.
	Role, App string
	// State is the manager's ActiveState. A oneshot is "activating" for
	// as long as its command runs.
	State string
	Since time.Time
}

// Input is what Report is told.
type Input struct {
	Record *record.Status
	Now    time.Time
	// SetUp is when backups were set up, where that is known.
	SetUp time.Time
	// Running are the units of a run, a restore or a drill that the
	// manager holds; Unrecognised the names of those it holds whose
	// name says nothing here; RunningErr why it could not be asked.
	Running      []Running
	Unrecognised []string
	RunningErr   error
	// Warnings are what was found of the credential file, said first,
	// each as a warning: line. Only root's look finds any.
	Warnings []string
}

// Result is what status decides: the lines it prints, and the facts of
// each, for whoever reads it as JSON (`motel-backup status --json`), and
// GET /_motel/backups's body, which a person with the viewer role on the
// box reads over HTTPS. Its JSON is a contract (backups/README.md
// "Status"); the record it is read from is not. The types are
// liveswap/wire's, which imports only the standard library, so that a
// client decodes the answer into the very type marshalled here; their
// fields are documented there.
type (
	Result = wire.Backups
	Line   = wire.BackupLine
	App    = wire.BackupApp
	Unit   = wire.BackupUnit
)

// The levels of a Line.
const (
	Info    = "info"
	Warning = "warning"
	Problem = "problem"
)

// The levels a problem alerts at: notify's severities.
const (
	AlertCritical = "critical"
	AlertWarning  = "warning"
)

// Alert is what moteld's monitor raises while a problem line of its kind
// stands, per app where the line is an app's, with the line as its
// text (notify's backup poller). A name is permanent once released:
// silences, routes and the rules listing name it.
type Alert struct{ Name, Severity string }

// Alerts are every rule's alert, each once: the names notify lists,
// lets a silence take and keeps from an outside producer.
// TestEveryRuleNamesAListedAlert holds Report's rules to this list,
// both ways.
var Alerts = []Alert{
	{"backup_never_ran", AlertCritical},
	{"backup_not_running", AlertCritical},
	{"backup_stale", AlertCritical},
	{"backup_never_complete", AlertCritical},
	{"backup_no_result", AlertCritical},
	{"backup_run_failed", AlertWarning},
	{"backup_data_missing", AlertWarning},
	{"backup_snapshot_gone", AlertCritical},
	{"backup_repository_unchecked", AlertCritical},
	{"backup_repository_check_failed", AlertCritical},
	{"backup_repository_unlisted", AlertCritical},
	{"backup_drill_failed", AlertCritical},
	{"backup_restore_proof_old", AlertCritical},
	{"backup_restore_unproven", AlertCritical},
}

// Text is the lines as status prints them.
func Text(r Result) []string {
	out := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		out[i] = l.Text
	}
	return out
}

// CouldNotTell is the Result of a look that could not be made, and
// why; the warnings found before it are said.
func CouldNotTell(now time.Time, warnings []string, why string) Result {
	r := newResult(now, warnings)
	r.CouldNotTell = record.Clean(why)
	return r
}

// newResult is one with nothing decided yet but the warnings, said
// first.
func newResult(now time.Time, warnings []string) Result {
	r := Result{At: stamp(now), Apps: []App{}, Running: []Unit{}, UnrecognisedUnits: []string{}, Lines: []Line{}}
	for _, w := range warnings {
		r.Lines = append(r.Lines, Line{Level: Warning, Text: "warning: " + record.Clean(w)})
	}
	return r
}

// stamp is a time as the JSON gives it: in UTC, to the second. Every
// verdict is reckoned from the times as they are, as the text's always
// were, so an age read off the JSON may differ from it by under a
// second at the boundary.
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

func stampOf(t time.Time) *time.Time { s := stamp(t); return &s }

// Report is what status prints, and whether the last run is recent and
// every app that has anything to back up has a fresh complete backup
// and a restore proven lately: the text and the JSON are both of it.
//
// Freshness is the last complete backup's, whatever is in progress and
// whatever a later, incomplete snapshot holds; and a snapshot that a
// listing of the repository since did not hold is not a backup.
func Report(in Input) Result {
	st, now, running, runningErr := in.Record, in.Now, in.Running, in.RunningErr
	r := newResult(now, in.Warnings)
	r.Healthy = true
	// of is the app the lines said are of, and facts what is decided of
	// it.
	of, facts := "", (*App)(nil)
	add := func(level, format string, a ...any) {
		r.Lines = append(r.Lines, Line{App: of, Level: level, Text: fmt.Sprintf(format, a...)})
	}
	say := func(format string, a ...any) { add(Info, format, a...) }
	// bad is a rule that makes the box unhealthy: the alert the monitor
	// raises while it stands, and its level, are written at the rule.
	bad := func(alert, severity, format string, a ...any) {
		r.Healthy = false
		if facts != nil {
			facts.Healthy = false
		}
		add(Problem, format, a...)
		l := &r.Lines[len(r.Lines)-1]
		l.Alert, l.Severity = alert, severity
	}
	when, short := record.When, record.Short

	// The run itself is aged, not only each backup: a record whose apps
	// are all pending, or that has none, would otherwise read "nothing
	// to back up yet" for as long as no run replaces it — a timer that
	// died weeks ago included.
	switch {
	case st.Started.IsZero() && !in.SetUp.IsZero() && now.Sub(in.SetUp) > StaleAfter:
		bad("backup_never_ran", AlertCritical, "no run yet, and backups were set up %s, more than %d hours ago", when(in.SetUp), int(StaleAfter.Hours()))
	case st.Started.IsZero():
		say("no run yet: pending first run")
	default:
		say("last run %s", when(st.Started))
		if len(st.Apps) == 0 && st.Error == "" {
			say("no app declares a backup; nothing is backed up")
		}
		if last := st.Finished; now.Sub(last) > StaleAfter {
			bad("backup_not_running", AlertCritical, "no run has finished since %s, more than %d hours ago: what follows is as of then", when(last), int(StaleAfter.Hours()))
		}
	}
	if d := st.LastDrill; d != nil && d.Detail != "" {
		bad("backup_drill_failed", AlertCritical, "the last drill, %s, could not begin: %s", when(d.Time), record.Text(d.Detail))
	} else if d != nil {
		say("last drill %s", when(d.Time))
	}
	// The repository itself, as the weekly drill last checked it. None
	// yet is said, and is unhealthy a week and a day after the first
	// drill that checks — or, before there has been one, after setup with
	// no drill either: a new box is not failing, nor one set up before the
	// check was, whose next drill checks.
	old := int(CheckOldAfter.Hours() / 24)
	switch c := st.LastCheck; {
	case c == nil && st.CheckSince != nil && now.Sub(*st.CheckSince) > CheckOldAfter:
		// Drills that check have run, and none came to a verdict — each
		// stopped, week on week — however recent the last of them.
		bad("backup_repository_unchecked", AlertCritical, "no check of the repository has come to a verdict since %s, more than %d days ago; `motel-backup drill`, as root, checks it", when(*st.CheckSince), old)
	case c == nil && st.CheckSince == nil && !in.SetUp.IsZero() && now.Sub(in.SetUp) > CheckOldAfter && (st.LastDrill == nil || now.Sub(st.LastDrill.Time) > CheckOldAfter):
		bad("backup_repository_unchecked", AlertCritical, "the repository has not been checked since backups were set up, %s, more than %d days ago; `motel-backup drill`, as root, checks it", when(in.SetUp), old)
	case c == nil && !st.Started.IsZero():
		say("the repository has not been checked yet: the weekly drill checks it")
	case c == nil:
	case c.Class == record.CheckClean && now.Sub(c.Time) > CheckOldAfter:
		bad("backup_repository_unchecked", AlertCritical, "repository last checked %s: its structure, and data group %s: clean: old, more than %d days ago; `motel-backup drill`, as root, checks it", when(c.Time), record.Text(c.Group), old)
	case c.Class == record.CheckClean:
		say("repository last checked %s: its structure, and data group %s: clean", when(c.Time), record.Text(c.Group))
	default:
		bad("backup_repository_check_failed", AlertCritical, "the repository check of %s (data group %s): %s: %s", when(c.Time), record.Text(c.Group), record.Text(string(c.Class)), record.Text(c.Detail))
	}
	// One listing unanswered is the run's warning, below. Unanswered for
	// longer than a backup may be old, it is what would keep a snapshot
	// pruned off the box looking like the last good backup.
	if u := st.Unlisted; u != nil && now.Sub(*u) > StaleAfter {
		bad("backup_repository_unlisted", AlertCritical, "the repository has not been listed since %s, more than %d hours ago: whether it still holds the snapshots below is not known", when(*u), int(StaleAfter.Hours()))
	} else if u != nil {
		say("the repository has not been listed since %s", when(*u))
	}
	if st.Error != "" {
		bad("backup_run_failed", AlertWarning, "the last run ended early: %s", record.Text(st.Error))
	}
	if st.Warning != "" {
		add(Warning, "warning: %s", record.Clean(st.Warning))
	}

	// gone: a listing was answered after the snapshot was last seen.
	gone := func(s *record.Snapshot) bool {
		return st.Listed != nil && (s.Seen == nil || s.Seen.Before(*st.Listed))
	}
	lastSeen := func(s *record.Snapshot) string {
		if s.Seen == nil {
			return "no listing has held it"
		}
		return "last seen there " + when(*s.Seen)
	}

	names := make([]string, 0, len(st.Apps))
	for n := range st.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	r.Apps = make([]App, len(names))
	for i, name := range names {
		app, n := st.Apps[name], record.Text(name)
		of, facts = n, &r.Apps[i]
		*facts = App{Name: n, Healthy: true}
		if app == nil {
			// Never written by a run, but a record that reads: no result
			// is nothing to be healthy about.
			bad("backup_no_result", AlertCritical, "%s: no result on record, until the next run writes the record again", n)
			continue
		}
		facts.Result = record.Text(string(app.Class))
		if app.LastOK != nil {
			facts.LastCompleteBackup = stampOf(app.LastOK.Time)
			facts.Fresh = !gone(app.LastOK) && now.Sub(app.LastOK.Time) <= StaleAfter
		}
		// Decided once, here: the lines below say what these are.
		provenRecently := false
		if p := app.RestoreProven; p != nil {
			provenRecently = now.Sub(p.Time) <= ProvenOldAfter
			facts.RestoreProven = stampOf(p.Time)
			facts.RestoreProvenLately = provenRecently && app.RestoreDrill == nil
		}
		at := record.Text(app.Looked)
		if app.Class == record.Pending {
			say("%s: pending first run: no data at %s yet", n, at)
			continue
		}
		switch {
		case app.Class == record.OK && app.LastOK != nil:
			say("%s: ok: last complete backup %s, snapshot %s (data at %s)", n, when(app.LastOK.Time), short(app.LastOK.ID), at)
		case app.Class == record.OK && app.Snapshot != nil:
			// A restore backs up what it is about to restore over, and that
			// is never the last complete backup.
			say("%s: ok: snapshot %s, made by a restore of what it then restored over (data at %s)", n, short(app.Snapshot.ID), at)
		case app.Class == record.NotRun:
			say("%s: not run: %s (data at %s)", n, record.Text(app.Detail), at)
		case app.Class == record.DataMissing:
			bad("backup_data_missing", AlertWarning, "%s: data missing: %s; `motel-backup restore %s`, as root, puts its last snapshot back", n, record.Text(app.Detail), n)
		default:
			// The last run did not back this app up, however fresh the
			// run before it.
			bad("backup_run_failed", AlertWarning, "%s: %s: %s (data at %s)", n, record.Text(string(app.Class)), record.Text(app.Detail), at)
		}
		switch {
		case app.LastOK == nil:
			bad("backup_never_complete", AlertCritical, "%s: no complete backup yet", n)
		case gone(app.LastOK):
			bad("backup_snapshot_gone", AlertCritical, "%s: snapshot %s is no longer in the repository (%s): it was the last complete backup, made %s", n, short(app.LastOK.ID), lastSeen(app.LastOK), when(app.LastOK.Time))
		case !facts.Fresh:
			bad("backup_stale", AlertCritical, "%s: stale: the last complete backup is from %s, more than %d hours ago, snapshot %s", n, when(app.LastOK.Time), int(StaleAfter.Hours()), short(app.LastOK.ID))
		case app.Class != record.OK:
			say("%s: last complete backup %s, snapshot %s", n, when(app.LastOK.Time), short(app.LastOK.ID))
		}
		if app.LastSnapshot == nil {
			continue // nothing in the repository to prove a restore of
		}
		if p := app.RestoreProven; p != nil {
			proven := fmt.Sprintf("%s: restore last proven: %s, snapshot %s", n, when(p.Time), short(p.Snapshot.ID))
			if !provenRecently {
				bad("backup_restore_proof_old", AlertCritical, "%s: old, more than %d days ago; `motel-backup drill`, as root, proves one now", proven, int(ProvenOldAfter.Hours()/24))
			} else {
				say("%s", proven)
			}
			// Pruned since, off the box: what was proven was proven.
			if gone(&p.Snapshot) {
				say("%s: snapshot %s is no longer in the repository (%s); the restore proven was of it", n, short(p.Snapshot.ID), lastSeen(&p.Snapshot))
			}
		}
		// The newest snapshot is the one a restore reaches for: a drill of
		// it that proved nothing is not made good by an older proof.
		switch d := app.RestoreDrill; {
		case d != nil:
			snapOf := ""
			if d.Snapshot.ID != "" { // one it could not ask the repository for has none
				snapOf = "snapshot " + short(d.Snapshot.ID) + ", "
			}
			bad("backup_drill_failed", AlertCritical, "%s: restore not proven: %s (%s%s); `motel-backup drill`, as root, tries again", n, record.Text(d.Detail), snapOf, when(d.Time))
		case app.RestoreProven == nil:
			bad("backup_restore_unproven", AlertCritical, "%s: restore not proven: no drill of it has run; `motel-backup drill`, as root, proves it", n)
		}
	}
	of, facts = "", nil

	// Only `motel_backup_*` units are looked for: a run's, a restore's,
	// a drill's, a setup's. The command that starts them is not one, so
	// whenever it is at work itself — between two units, for the whole of
	// a restore's question — no unit is running, though it holds the run
	// lock and a second run, drill or restore is refused.
	switch {
	case runningErr != nil:
		say("could not ask systemd what is running: %s", record.Text(runningErr.Error()))
	case len(running) == 0 && len(in.Unrecognised) == 0:
		say("no backup unit running now")
	}
	for _, u := range running {
		unit := Unit{Role: record.Text(u.Role), App: record.Text(u.App), State: record.Text(u.State)}
		what := unit.Role
		if unit.App != "" {
			what += " of " + unit.App
		}
		// A oneshot is "activating" for as long as its command runs; any
		// other state is said as the manager says it.
		if u.State != "activating" {
			what += " (" + unit.State + ")"
		}
		of = unit.App
		// No time: the unit ended while the manager was being asked.
		if u.Since.IsZero() {
			say("running: %s", what)
		} else {
			unit.Since = stampOf(u.Since)
			say("running: %s, since %s", what, when(u.Since))
		}
		r.Running = append(r.Running, unit)
	}
	of = ""
	for _, name := range in.Unrecognised {
		r.UnrecognisedUnits = append(r.UnrecognisedUnits, record.Text(name))
		say("running: %s", record.Text(name))
	}
	return r
}
