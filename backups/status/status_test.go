package status

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/backups/record"
)

var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

const (
	idA = "aaaaaaaa11111111aaaaaaaa11111111aaaaaaaa11111111aaaaaaaa11111111"
	idB = "bbbbbbbb22222222bbbbbbbb22222222bbbbbbbb22222222bbbbbbbb22222222"
)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func at(d time.Duration) *time.Time { t := ago(d); return &t }

// snap is a snapshot made d ago that the listing seen ago held; a nil
// seen is a snapshot no listing has held.
func snap(id string, d time.Duration, seen *time.Time) *record.Snapshot {
	return &record.Snapshot{ID: id, Time: ago(d), Seen: seen}
}

// sound is an app a run backed up ten minutes ago, whose restore was
// proven two days ago, with a listing since that held both.
func sound() *record.App {
	s := snap(idA, 10*time.Minute, at(9*time.Minute))
	return &record.App{
		Class: record.OK, Looked: "/var/lib/liveswap/blog/shared",
		Snapshot: s, LastOK: s, LastSnapshot: s,
		Items:         []record.Item{{Kind: "files", Path: "uploads", OK: true}},
		RestoreProven: &record.Drill{Snapshot: *snap(idB, 50*time.Hour, at(9*time.Minute)), Time: ago(48 * time.Hour)},
	}
}

func of(apps map[string]*record.App) *record.Status {
	return &record.Status{Started: ago(11 * time.Minute), Finished: ago(9 * time.Minute), Apps: apps, Listed: at(9 * time.Minute)}
}

func with(change func(*record.App)) *record.Status {
	a := sound()
	change(a)
	return of(map[string]*record.App{"blog": a, "shop": sound()})
}

// checked is a sound record whose last check of the repository, d ago,
// read group 38/52 and came to class.
func checked(class record.CheckClass, detail string, d time.Duration) *record.Status {
	st := with(func(*record.App) {})
	st.LastCheck = &record.Check{Time: ago(d), Group: "38/52", Class: class, Detail: detail}
	return st
}

func TestWhatStatusSays(t *testing.T) {
	for name, tc := range map[string]struct {
		st         *record.Status
		setUp      time.Time
		running    []Running
		unknown    []string
		runningErr error
		healthy    bool
		says       []string
		never      []string
	}{
		"no run yet": {
			st: &record.Status{Apps: map[string]*record.App{}}, healthy: true,
			says: []string{"pending first run"}, never: []string{"stale", "not proven"},
		},
		"declared, not deployed yet": {
			st: with(func(a *record.App) {
				*a = record.App{Class: record.Pending, Looked: "/var/lib/liveswap/blog/shared"}
			}), healthy: true,
			says:  []string{"blog: pending", "/var/lib/liveswap/blog/shared"},
			never: []string{"blog: stale", "blog: restore not proven"},
		},
		"fresh and proven": {
			st: with(func(*record.App) {}), healthy: true,
			says: []string{
				"blog: ok", "2026-09-20 14:50 UTC", "snapshot aaaaaaaa",
				"blog: restore last proven: 2026-09-18 15:00 UTC, snapshot bbbbbbbb",
				"/var/lib/liveswap/blog/shared",
			},
			never: []string{"stale", ": old", "no longer in the repository", idA},
		},
		"a minute short of stale": {
			st:      with(func(a *record.App) { a.LastOK = snap(idA, StaleAfter-time.Minute, at(9*time.Minute)) }),
			healthy: true, never: []string{"stale"},
		},
		"a minute past stale": {
			st:   with(func(a *record.App) { a.LastOK = snap(idA, StaleAfter+time.Minute, at(9*time.Minute)) }),
			says: []string{"blog: stale", "2026-09-20 11:59 UTC"}, never: []string{"shop: stale"},
		},
		"freshness is the last complete backup's, not the last snapshot's": {
			st: with(func(a *record.App) {
				a.Class, a.Detail = record.Incomplete, "restic could not read everything (exit 3)"
				a.LastOK = snap(idB, 30*time.Hour, at(9*time.Minute))
			}),
			says:  []string{"blog: incomplete", "exit 3", "blog: stale", "2026-09-19 09:00 UTC"},
			never: []string{"blog: ok"},
		},
		"a run that did not reach the app says so, and judges it by its last ok": {
			st: with(func(a *record.App) {
				a.Class, a.Snapshot, a.Items = record.NotRun, nil, nil
			}), healthy: true,
			says: []string{"blog: not run"}, never: []string{"blog: ok", "blog: stale"},
		},
		"never a complete backup": {
			st: with(func(a *record.App) {
				a.Class, a.Detail = record.Failed, "the repository's password is wrong (exit 12)"
				a.Snapshot, a.LastOK, a.LastSnapshot, a.RestoreProven = nil, nil, nil, nil
			}),
			says: []string{"blog: failed", "password is wrong", "blog: no complete backup yet"}, never: []string{"pending"},
		},
		"never proven, and why": {
			st: with(func(a *record.App) {
				a.RestoreProven = nil
				a.RestoreDrill = &record.Drill{Snapshot: *snap(idA, time.Hour, nil), Time: ago(time.Hour), Detail: "app.db: the copy is damaged"}
			}),
			says:  []string{"blog: restore not proven", "app.db: the copy is damaged", "motel-backup drill"},
			never: []string{"blog: restore last proven"},
		},
		// An app on record with no result — a record the engine never
		// writes, but one that reads — is nothing to be healthy about.
		"an app on record with no result": {
			st:   of(map[string]*record.App{"blog": nil, "shop": sound()}),
			says: []string{"blog: no result on record"},
		},
		// A drill that could not ask the repository has no snapshot to name.
		"a drill that could not ask": {
			st: with(func(a *record.App) {
				a.RestoreProven = nil
				a.RestoreDrill = &record.Drill{Time: ago(time.Hour), Detail: "the repository could not be asked for this app's snapshots: no"}
			}),
			says:  []string{"blog: restore not proven: the repository could not be asked", "motel-backup drill"},
			never: []string{"snapshot ,", "snapshot )"},
		},
		// Each part of a warning is cut on its own when the run writes it;
		// cut again as a whole, the last part — where the listing's
		// stderr is — would be lost.
		"a long warning is said whole": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Warning = strings.Repeat("shop no longer declares a backup. ", 8) + "the repository could not be listed. What restic said is in /var/lib/motel-backup/listing.err"
				return st
			}(),
			healthy: true,
			says:    []string{"What restic said is in /var/lib/motel-backup/listing.err"},
		},
		"no drill at all yet": {
			st:   with(func(a *record.App) { a.RestoreProven = nil }),
			says: []string{"blog: restore not proven", "motel-backup drill"},
		},
		"proven a minute short of old": {
			st:      with(func(a *record.App) { a.RestoreProven.Time = ago(ProvenOldAfter - time.Minute) }),
			healthy: true, never: []string{": old"},
		},
		"proven a minute past old": {
			st:   with(func(a *record.App) { a.RestoreProven.Time = ago(ProvenOldAfter + time.Minute) }),
			says: []string{"blog: restore last proven: 2026-09-12 14:59 UTC", ": old"},
		},
		// The newest snapshot is the one a restore would reach for: a drill
		// of it that proved nothing is not made good by an older proof.
		"a later drill that proved nothing, beside what was proven": {
			st: with(func(a *record.App) {
				a.RestoreDrill = &record.Drill{Snapshot: *snap(idA, time.Hour, nil), Time: ago(time.Hour), Detail: "out of room"}
			}),
			says: []string{"blog: restore last proven", "blog: restore not proven", "out of room"},
		},
		"the last complete backup is gone from the repository": {
			st:    with(func(a *record.App) { a.LastOK = snap(idA, 70*time.Minute, at(69*time.Minute)) }),
			says:  []string{"blog: snapshot aaaaaaaa is no longer in the repository", "2026-09-20 13:51 UTC"},
			never: []string{"shop: snapshot"},
		},
		"a snapshot no listing ever held": {
			st:   with(func(a *record.App) { a.LastOK = snap(idA, 70*time.Minute, nil) }),
			says: []string{"blog: snapshot aaaaaaaa is no longer in the repository"},
		},
		"no listing yet says nothing of vanishing": {
			st: func() *record.Status {
				st := with(func(a *record.App) { a.LastOK = snap(idA, 10*time.Minute, nil) })
				st.Listed = nil
				st.Apps["shop"].LastOK = snap(idA, 10*time.Minute, nil)
				st.Apps["blog"].RestoreProven.Snapshot.Seen, st.Apps["shop"].RestoreProven.Snapshot.Seen = nil, nil
				return st
			}(), healthy: true, never: []string{"no longer in the repository"},
		},
		"a listing that failed since says nothing of vanishing": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Listed = at(3 * time.Hour) // every Seen is newer
				return st
			}(), healthy: true, never: []string{"no longer in the repository"},
		},
		"a proven snapshot since pruned is said, and is still a proof": {
			st:      with(func(a *record.App) { a.RestoreProven.Snapshot.Seen = at(30 * time.Hour) }),
			healthy: true,
			says:    []string{"blog: restore last proven", "snapshot bbbbbbbb is no longer in the repository"},
		},
		"data missing": {
			st: with(func(a *record.App) {
				a.Class, a.Detail, a.Snapshot = record.DataMissing, "/var/lib/liveswap/blog/shared is gone, and was backed up before", nil
			}),
			says: []string{"blog: data missing", "was backed up before", "`motel-backup restore blog`, as root,"}, never: []string{"pending"},
		},
		"a unit three hours in is running, not failed": {
			st:      with(func(*record.App) {}),
			running: []Running{{Role: "upload", App: "blog", State: "activating", Since: ago(3 * time.Hour)}},
			healthy: true,
			says:    []string{"running: upload of blog, since 2026-09-20 12:00 UTC"},
			never:   []string{"failed", "no backup unit running"},
		},
		"a unit of the whole run": {
			st:      with(func(*record.App) {}),
			running: []Running{{Role: "plan", State: "activating", Since: ago(time.Minute)}},
			healthy: true, says: []string{"running: plan, since 2026-09-20 14:59 UTC"},
		},
		"the manager could not be asked": {
			st: with(func(*record.App) {}), runningErr: errors.New("dial unix /run/dbus/system_bus_socket: no such file"),
			healthy: true,
			says:    []string{"could not ask systemd what is running", "system_bus_socket"},
			never:   []string{"no backup unit running"},
		},
		"no backup unit running": {
			st: with(func(*record.App) {}), healthy: true, says: []string{"no backup unit running now"},
		},
		// Nothing else ages a record whose apps are all pending, or that
		// has none: a timer that died three weeks ago would read as "no
		// data yet", exit 0, for good.
		"the last run is weeks old and everything was pending": {
			st: func() *record.Status {
				st := of(map[string]*record.App{"blog": {Class: record.Pending, Looked: "/var/lib/liveswap/blog/shared"}})
				st.Started, st.Finished = ago(21*24*time.Hour), ago(21*24*time.Hour-time.Minute)
				return st
			}(),
			says: []string{"no run has finished since 2026-08-30 15:01 UTC"},
		},
		"the last run is weeks old and no app declared a backup": {
			st:   &record.Status{Started: ago(21 * 24 * time.Hour), Finished: ago(21 * 24 * time.Hour), Apps: map[string]*record.App{}},
			says: []string{"no app declares a backup", "no run has finished since"},
		},
		"no app declares a backup, as of a run just now": {
			st:      &record.Status{Started: ago(2 * time.Minute), Finished: ago(time.Minute), Apps: map[string]*record.App{}},
			healthy: true, says: []string{"no app declares a backup"}, never: []string{"no run has finished"},
		},
		"set up four hours ago, and still no run": {
			st: &record.Status{Apps: map[string]*record.App{}}, setUp: ago(4 * time.Hour),
			says: []string{"no run yet", "set up 2026-09-20 11:00 UTC"}, never: []string{"pending first run"},
		},
		"set up ten minutes ago": {
			st: &record.Status{Apps: map[string]*record.App{}}, setUp: ago(10 * time.Minute), healthy: true,
			says: []string{"pending first run"},
		},
		// A drill before any run writes a record with apps and no run.
		"a drill came first": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Started, st.Finished = time.Time{}, time.Time{}
				return st
			}(), healthy: true,
			says: []string{"no run yet"}, never: []string{"0001"},
		},
		"a unit that ended while it was being asked about has no time": {
			st:      with(func(*record.App) {}),
			running: []Running{{Role: "upload", App: "blog", State: "activating"}},
			healthy: true, says: []string{"running: upload of blog"}, never: []string{"0001", "since"},
		},
		"a unit whose name says nothing here is still running": {
			st: with(func(*record.App) {}), unknown: []string{"motel_backup_newrole_x_y.service"}, healthy: true,
			says: []string{"running: motel_backup_newrole_x_y.service"}, never: []string{"no backup unit running"},
		},
		"a drill that could not begin": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.LastDrill = &record.Drill{Time: ago(time.Hour), Detail: "the Caddyfile could not be turned into a plan (exit 1)"}
				return st
			}(),
			says: []string{"the last drill, 2026-09-20 14:00 UTC, could not begin: the Caddyfile could not be turned into a plan"},
		},
		"when a drill last ran": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.LastDrill = &record.Drill{Time: ago(time.Hour)}
				return st
			}(), healthy: true, says: []string{"last drill 2026-09-20 14:00 UTC"},
		},
		// A listing that fails once is a warning; one that has failed for
		// longer than a backup may be old is what keeps a snapshot pruned
		// off the box looking like the last good backup.
		"the repository has gone unlisted for an hour": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Unlisted = at(time.Hour)
				return st
			}(), healthy: true, says: []string{"the repository has not been listed since 2026-09-20 14:00 UTC"},
		},
		"the repository has gone unlisted for four hours": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Unlisted = at(4 * time.Hour)
				return st
			}(), says: []string{"the repository has not been listed since 2026-09-20 11:00 UTC", "more than 3 hours"},
		},
		// A restore backs an app up first, and that is never "the last
		// complete backup": ok, with no last ok.
		"backed up by a restore, and never by a run": {
			st: with(func(a *record.App) {
				a.LastOK, a.RestoreProven = nil, nil
			}),
			says:  []string{"blog: ok: snapshot aaaaaaaa", "made by a restore", "blog: no complete backup yet"},
			never: []string{"blog: ok:  ("},
		},
		"not attempted": {
			st: with(func(a *record.App) {
				a.Class, a.Detail, a.Snapshot = record.NotAttempted, "the repository's password is wrong (exit 12)", nil
			}),
			says: []string{"blog: not attempted: the repository's password is wrong"},
		},
		// The repository's own check: none yet is said, and is unhealthy
		// only once a box has been set up for longer than a week and a
		// day; a clean one is said, and ages; any other verdict is
		// unhealthy until a clean one.
		"no check yet, set up two days ago": {
			st: with(func(*record.App) {}), setUp: ago(48 * time.Hour), healthy: true,
			says: []string{"the repository has not been checked yet: the weekly drill checks it"},
		},
		"no check, set up nine days ago": {
			st: with(func(*record.App) {}), setUp: ago(9 * 24 * time.Hour),
			says: []string{"the repository has not been checked since backups were set up, 2026-09-11 15:00 UTC, more than 8 days ago; `motel-backup drill`, as root, checks it"},
		},
		// A box set up before the check existed: none on record, but a
		// drill lately, and the next drill checks. Not failing.
		"no check, set up nine days ago, a drill three days ago": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.LastDrill = &record.Drill{Time: ago(3 * 24 * time.Hour)}
				return st
			}(), setUp: ago(9 * 24 * time.Hour), healthy: true,
			says: []string{"the repository has not been checked yet: the weekly drill checks it"},
		},
		"no check, set up nine days ago, the last drill nine days ago": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.LastDrill = &record.Drill{Time: ago(9 * 24 * time.Hour)}
				return st
			}(), setUp: ago(30 * 24 * time.Hour),
			says: []string{"the repository has not been checked since backups were set up"},
		},
		// Drills that never come to a verdict — each stopped, week on
		// week — refresh last_drill; the date checks began is what ages.
		"checks begun nine days ago, none come to a verdict": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.LastDrill = &record.Drill{Time: ago(24 * time.Hour)}
				st.CheckSince = at(9 * 24 * time.Hour)
				return st
			}(), setUp: ago(30 * 24 * time.Hour),
			says: []string{"no check of the repository has come to a verdict since 2026-09-11 15:00 UTC, more than 8 days ago; `motel-backup drill`, as root, checks it"},
		},
		"checks begun three days ago": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.LastDrill = &record.Drill{Time: ago(24 * time.Hour)}
				st.CheckSince = at(3 * 24 * time.Hour)
				return st
			}(), setUp: ago(30 * 24 * time.Hour), healthy: true,
			says: []string{"the repository has not been checked yet: the weekly drill checks it"},
		},
		"checked clean lately": {
			st: checked(record.CheckClean, "", 48*time.Hour), setUp: ago(30 * 24 * time.Hour), healthy: true,
			says:  []string{"repository last checked 2026-09-18 15:00 UTC: its structure, and data group 38/52: clean"},
			never: []string{"not been checked"},
		},
		"checked clean, nine days ago": {
			st:   checked(record.CheckClean, "", 9*24*time.Hour),
			says: []string{"repository last checked 2026-09-11 15:00 UTC: its structure, and data group 38/52: clean: old, more than 8 days ago; `motel-backup drill`, as root, checks it"},
		},
		"checked and found damaged": {
			st:   checked(record.CheckDamaged, "restic check found 2 errors, in 2 damaged packs", 48*time.Hour),
			says: []string{"the repository check of 2026-09-18 15:00 UTC (data group 38/52): damaged: restic check found 2 errors, in 2 damaged packs"},
		},
		"a check that could not reach it": {
			st:   checked(record.CheckUnreachable, "the repository did not answer within 30s", 48*time.Hour),
			says: []string{"the repository check of 2026-09-18 15:00 UTC (data group 38/52): unreachable: the repository did not answer within 30s"},
		},
		"a run that ended early, and a warning": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Error, st.Warning = "the Caddyfile could not be turned into a plan (exit 1)", "the last record could not be read"
				return st
			}(),
			says: []string{"the last run ended early: the Caddyfile could not be turned into a plan", "warning: the last record could not be read"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := Report(Input{Record: tc.st, Now: now, SetUp: tc.setUp, Running: tc.running, Unrecognised: tc.unknown, RunningErr: tc.runningErr})
			out, healthy := strings.Join(Text(r), "\n"), r.Healthy
			sameVerdict(t, r)
			if healthy != tc.healthy {
				t.Errorf("healthy = %v, want %v:\n%s", healthy, tc.healthy, out)
			}
			for _, want := range tc.says {
				if !strings.Contains(out, want) {
					t.Errorf("does not say %q:\n%s", want, out)
				}
			}
			for _, not := range tc.never {
				if strings.Contains(out, not) {
					t.Errorf("says %q:\n%s", not, out)
				}
			}
		})
	}
}

// What an app wrote reaches a record through record.Text; a record
// written by hand, or by a later engine, has been through nothing.
func TestAReportHoldsNoControlCharacters(t *testing.T) {
	st := with(func(a *record.App) {
		a.Class, a.Detail = "fail\x1b[2Jed", "\x1b[2Jgone"
		a.Looked = "/var/lib/liveswap/blog/\x1b]0;x\x07shared"
		a.LastOK = &record.Snapshot{ID: "\x1b[2Jaaaa", Time: ago(time.Minute)}
		a.RestoreProven.Snapshot.ID = "\x1b[2Jbbbb"
	})
	st.Apps["sh\x1bop"] = sound()
	st.LastCheck = &record.Check{Time: ago(time.Hour), Group: "3\x1b[2J8/52", Class: "dam\x1b[2Jaged", Detail: "\x1b]0;x\x07found"}
	lines := Text(Report(Input{Record: st, Now: now}))
	if len(lines) == 0 {
		t.Fatal("nothing was said")
	}
	for _, l := range lines {
		if strings.ContainsAny(l, "\x1b\x07\n") {
			t.Fatalf("a control character reached the terminal: %q", l)
		}
	}
}

// sameVerdict is that the verdicts are the lines': healthy where no line
// is a problem, and an app healthy where no line of it is.
func sameVerdict(t *testing.T, r Result) {
	t.Helper()
	problem := map[string]bool{}
	for _, l := range r.Lines {
		if l.Level == Problem {
			problem[l.App] = true
		}
	}
	if r.Healthy == (len(problem) > 0) {
		t.Errorf("healthy = %v, with problems of %v", r.Healthy, problem)
	}
	for _, a := range r.Apps {
		if a.Healthy == problem[a.Name] {
			t.Errorf("%s: healthy = %v, and a line of it a problem: %v", a.Name, a.Healthy, problem[a.Name])
		}
	}
}

// What a status screen shows of an app, beside its lines.
func TestWhatStatusDecidesOfAnApp(t *testing.T) {
	for name, tc := range map[string]struct {
		st                     *record.Status
		fresh, provenLately    bool
		lastComplete, provenAt *time.Time
	}{
		"fresh and proven": {
			st: with(func(*record.App) {}), fresh: true, provenLately: true,
			lastComplete: at(10 * time.Minute), provenAt: at(48 * time.Hour),
		},
		"stale": {
			st:    with(func(a *record.App) { a.LastOK = snap(idA, StaleAfter+time.Minute, at(9*time.Minute)) }),
			fresh: false, provenLately: true, lastComplete: at(StaleAfter + time.Minute), provenAt: at(48 * time.Hour),
		},
		"gone from the repository": {
			st:    with(func(a *record.App) { a.LastOK = snap(idA, 10*time.Minute, at(time.Hour)) }),
			fresh: false, provenLately: true, lastComplete: at(10 * time.Minute), provenAt: at(48 * time.Hour),
		},
		"proven too long ago": {
			st: with(func(a *record.App) {
				a.RestoreProven.Time = ago(ProvenOldAfter + time.Minute)
			}), fresh: true, provenLately: false, lastComplete: at(10 * time.Minute), provenAt: at(ProvenOldAfter + time.Minute),
		},
		"a drill since failed": {
			st: with(func(a *record.App) {
				a.RestoreDrill = &record.Drill{Snapshot: *a.LastOK, Time: ago(time.Hour), Detail: "no room"}
			}), fresh: true, provenLately: false, lastComplete: at(10 * time.Minute), provenAt: at(48 * time.Hour),
		},
		"never backed up, never proven": {
			st: with(func(a *record.App) {
				a.Class, a.Snapshot, a.LastOK, a.LastSnapshot, a.RestoreProven = record.Failed, nil, nil, nil, nil
			}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := Report(Input{Record: tc.st, Now: now})
			sameVerdict(t, r)
			if len(r.Apps) != 2 || r.Apps[0].Name != "blog" || r.Apps[1].Name != "shop" {
				t.Fatalf("apps = %+v", r.Apps)
			}
			a := r.Apps[0]
			if a.Fresh != tc.fresh || a.RestoreProvenLately != tc.provenLately {
				t.Errorf("fresh = %v, proven lately = %v; want %v, %v", a.Fresh, a.RestoreProvenLately, tc.fresh, tc.provenLately)
			}
			if !sameTime(a.LastCompleteBackup, tc.lastComplete) || !sameTime(a.RestoreProven, tc.provenAt) {
				t.Errorf("last complete backup %v, proven %v; want %v, %v", a.LastCompleteBackup, a.RestoreProven, tc.lastComplete, tc.provenAt)
			}
			if a.LastCompleteBackup != nil && a.LastCompleteBackup.Location() != time.UTC {
				t.Errorf("a time not in UTC: %v", a.LastCompleteBackup)
			}
		})
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// The verdict is the true age's, to the nanosecond, as the text's
// always was: half a second past stale is stale, though the seconds the
// JSON gives read exactly StaleAfter.
func TestTheVerdictIsTheTrueAgesNotTheSeconds(t *testing.T) {
	when := now.Add(900 * time.Millisecond)
	st := with(func(a *record.App) {
		a.LastOK = &record.Snapshot{ID: idA, Time: when.Add(-StaleAfter - 500*time.Millisecond), Seen: at(9 * time.Minute)}
	})
	r := Report(Input{Record: st, Now: when})
	sameVerdict(t, r)
	if a := r.Apps[0]; a.Fresh || a.Healthy || !strings.Contains(strings.Join(Text(r), "\n"), "blog: stale") {
		t.Errorf("half a second past stale: fresh %v, healthy %v:\n%s", a.Fresh, a.Healthy, strings.Join(Text(r), "\n"))
	}
	if age := r.At.Sub(*r.Apps[0].LastCompleteBackup); age != StaleAfter {
		t.Errorf("the seconds read %v", age)
	}
}

// TestTheUnitsChangeNoVerdict: what the manager holds, or could not be
// asked, is said and never judged, so a look that does not ask it
// (notify's backup poller, which raises an alert per problem line)
// takes the same verdict and the same problem lines as one that does.
func TestTheUnitsChangeNoVerdict(t *testing.T) {
	problems := func(r Result) (out []string) {
		for _, l := range r.Lines {
			if l.Level == Problem {
				out = append(out, l.Text)
			}
		}
		return out
	}
	for name, st := range map[string]*record.Status{
		"healthy":  with(func(*record.App) {}),
		"stale":    with(func(a *record.App) { a.LastOK = snap(idA, 4*time.Hour, at(9*time.Minute)) }),
		"unlisted": func() *record.Status { st := with(func(*record.App) {}); st.Unlisted = at(4 * time.Hour); return st }(),
		"check":    checked(record.CheckUnreachable, "no answer", time.Hour),
	} {
		bare := Report(Input{Record: st, Now: now})
		for _, in := range []Input{
			{Record: st, Now: now, Running: []Running{{Role: "upload", App: "blog", State: "activating", Since: ago(time.Minute)}, {Role: "run", State: "deactivating"}}, Unrecognised: []string{"motel_backup_x.service"}},
			{Record: st, Now: now, RunningErr: errors.New("no bus")},
		} {
			r := Report(in)
			if r.Healthy != bare.Healthy || strings.Join(problems(r), "\n") != strings.Join(problems(bare), "\n") {
				t.Errorf("%s: with units: healthy %v, %q; without: healthy %v, %q", name, r.Healthy, problems(r), bare.Healthy, problems(bare))
			}
		}
	}
}

// TestEveryRuleNamesAListedAlert: each rule that makes the box unhealthy
// (a call of bad) names its alert as a string that starts backup_, and
// its level as AlertCritical or AlertWarning, at the rule; and the
// pairs the rules write are Alerts, both ways, each name at one level.
// A rule added with a name Alerts lacks, or a problem line made other
// than by bad, fails here, not on a box.
func TestEveryRuleNamesAListedAlert(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "status.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	levels := map[string]string{"AlertCritical": AlertCritical, "AlertWarning": AlertWarning}
	written, calls, problems := map[Alert]bool{}, 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := c.Fun.(*ast.Ident)
		if ok && id.Name == "add" && len(c.Args) > 0 {
			if lvl, ok := c.Args[0].(*ast.Ident); ok && lvl.Name == "Problem" {
				problems++
			}
		}
		if !ok || id.Name != "bad" {
			return true
		}
		calls++
		at := fset.Position(c.Pos())
		lit, ok := c.Args[0].(*ast.BasicLit)
		lvl, isIdent := c.Args[1].(*ast.Ident)
		if !ok || lit.Kind != token.STRING || !isIdent || levels[lvl.Name] == "" {
			t.Errorf("%s: a rule's alert is a string and its level AlertCritical or AlertWarning, written at the rule", at)
			return true
		}
		name, _ := strconv.Unquote(lit.Value)
		if !strings.HasPrefix(name, "backup_") {
			t.Errorf("%s: %s does not start backup_", at, name)
		}
		written[Alert{name, levels[lvl.Name]}] = true
		return true
	})
	if calls == 0 {
		t.Fatal("no rule found: bad's shape changed")
	}
	// A problem line is bad's alone: one made otherwise names no alert.
	if problems != 1 {
		t.Errorf("add(Problem, …) is called %d times: a problem line is made by bad alone", problems)
	}
	listed, names := map[Alert]bool{}, map[string]bool{}
	for _, a := range Alerts {
		if names[a.Name] {
			t.Errorf("%s is listed twice", a.Name)
		}
		names[a.Name], listed[a] = true, true
	}
	for a := range written {
		if !listed[a] {
			t.Errorf("a rule raises %s at %s, which Alerts does not list", a.Name, a.Severity)
		}
	}
	for _, a := range Alerts {
		if !written[a] {
			t.Errorf("Alerts lists %s at %s, which no rule raises", a.Name, a.Severity)
		}
	}
}

// TestEachRuleRaisesItsAlert: the alerts each of Report's eighteen rules
// raises alone, by name and app, at its listed level; a line that is no
// problem names none. And an app whose runs have failed since its first
// is critical (backup_never_complete), not a warning alone.
func TestEachRuleRaisesItsAlert(t *testing.T) {
	const day = 24 * time.Hour
	edit := func(change func(*record.Status)) *record.Status {
		st := with(func(*record.App) {})
		change(st)
		return st
	}
	weeksOld := of(map[string]*record.App{"blog": {Class: record.Pending, Looked: "/var/lib/liveswap/blog/shared"}})
	weeksOld.Started, weeksOld.Finished = ago(21*day), ago(21*day-time.Minute)
	for _, c := range []struct {
		rule  string
		st    *record.Status
		setUp time.Time
		want  []string
	}{
		{"1 no run since setup", &record.Status{Apps: map[string]*record.App{}}, ago(4 * time.Hour), []string{"backup_never_ran"}},
		{"2 no run finished lately", weeksOld, time.Time{}, []string{"backup_not_running"}},
		{"3 a drill that could not begin", edit(func(st *record.Status) { st.LastDrill = &record.Drill{Time: ago(time.Hour), Detail: "no plan"} }), time.Time{}, []string{"backup_drill_failed"}},
		{"4 no check came to a verdict", edit(func(st *record.Status) { st.CheckSince = at(9 * day) }), time.Time{}, []string{"backup_repository_unchecked"}},
		{"5 never checked since setup", with(func(*record.App) {}), ago(9 * day), []string{"backup_repository_unchecked"}},
		{"6 a clean check that is old", checked(record.CheckClean, "", 9*day), time.Time{}, []string{"backup_repository_unchecked"}},
		{"7 a check that is not clean", checked(record.CheckUnreachable, "no answer", time.Hour), time.Time{}, []string{"backup_repository_check_failed"}},
		{"8 not listed for hours", edit(func(st *record.Status) { st.Unlisted = at(4 * time.Hour) }), time.Time{}, []string{"backup_repository_unlisted"}},
		{"9 the run ended early", edit(func(st *record.Status) { st.Error = "the repository refused" }), time.Time{}, []string{"backup_run_failed"}},
		{"10 no result", of(map[string]*record.App{"blog": nil, "shop": sound()}), time.Time{}, []string{"backup_no_result/blog"}},
		{"11 data missing", with(func(a *record.App) { a.Class, a.Detail, a.Snapshot = record.DataMissing, "gone", nil }), time.Time{}, []string{"backup_data_missing/blog"}},
		{"12 the app failed", with(func(a *record.App) { a.Class, a.Detail = record.Failed, "exit 1" }), time.Time{}, []string{"backup_run_failed/blog"}},
		{"13 no complete backup", with(func(a *record.App) { a.LastOK = nil }), time.Time{}, []string{"backup_never_complete/blog"}},
		{"14 the last complete snapshot gone", with(func(a *record.App) { a.LastOK = snap(idA, 70*time.Minute, at(69*time.Minute)) }), time.Time{}, []string{"backup_snapshot_gone/blog"}},
		{"15 stale", with(func(a *record.App) { a.LastOK = snap(idA, 4*time.Hour, at(9*time.Minute)) }), time.Time{}, []string{"backup_stale/blog"}},
		{"16 a proof that is old", with(func(a *record.App) { a.RestoreProven.Time = ago(9 * day) }), time.Time{}, []string{"backup_restore_proof_old/blog"}},
		{"17 a drill that proved nothing", with(func(a *record.App) {
			a.RestoreDrill = &record.Drill{Snapshot: *snap(idA, time.Hour, nil), Time: ago(time.Hour), Detail: "out of room"}
		}), time.Time{}, []string{"backup_drill_failed/blog"}},
		{"18 never drilled", with(func(a *record.App) { a.RestoreProven = nil }), time.Time{}, []string{"backup_restore_unproven/blog"}},
		{"failed since its first run", with(func(a *record.App) {
			a.Class, a.Detail = record.Failed, "exit 1"
			a.Snapshot, a.LastOK, a.LastSnapshot, a.RestoreProven = nil, nil, nil, nil
		}), time.Time{}, []string{"backup_run_failed/blog", "backup_never_complete/blog"}},
		{"healthy", with(func(*record.App) {}), time.Time{}, nil},
	} {
		r := Report(Input{Record: c.st, Now: now, SetUp: c.setUp})
		var got []string
		for _, l := range r.Lines {
			if l.Level != Problem {
				if l.Alert != "" || l.Severity != "" {
					t.Errorf("%s: %q names %s, and is no problem", c.rule, l.Text, l.Alert)
				}
				continue
			}
			i := slices.IndexFunc(Alerts, func(a Alert) bool { return a.Name == l.Alert })
			if i < 0 || Alerts[i].Severity != l.Severity {
				t.Errorf("%s: %q raises %s at %s, not as listed", c.rule, l.Text, l.Alert, l.Severity)
			}
			k := l.Alert
			if l.App != "" {
				k += "/" + l.App
			}
			got = append(got, k)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: raises %v, want %v", c.rule, got, c.want)
		}
	}
}

// TestNoCompleteBackupInTheRepositoryIsCritical: an app with no complete
// backup in the repository is critical, whether it never had one or its
// last one is gone from it (the owner's decision, 2026-11-22, #649):
// status says "snapshot gone" in place of "stale", so no other line ages
// such an app.
func TestNoCompleteBackupInTheRepositoryIsCritical(t *testing.T) {
	for alert, st := range map[string]*record.Status{
		"backup_never_complete": with(func(a *record.App) { a.LastOK = nil }),
		"backup_snapshot_gone":  with(func(a *record.App) { a.LastOK = snap(idA, 30*time.Hour, at(29*time.Hour)) }),
	} {
		var problems []Line
		for _, l := range Report(Input{Record: st, Now: now}).Lines {
			if l.Level == Problem {
				problems = append(problems, l)
			}
		}
		if len(problems) != 1 || problems[0].Alert != alert || problems[0].App != "blog" || problems[0].Severity != AlertCritical {
			t.Errorf("%s: problems %+v, want blog's alone, critical", alert, problems)
		}
	}
}
