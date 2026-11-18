package box

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
)

var errReload = errors.New("job for moteld.service failed")

// fmRow is one row of DESIGN-box.md's Failure-mode table, in one of its
// two columns: the write fails (fail), or the process dies once it is
// durable (crash). Each names the invariants it holds.
type fmRow struct {
	name  string
	holds string
	// scen is the push: "change" (a signed new file), "same" (a signed
	// commit of the installed file), "unsigned".
	scen    string
	fail    map[string]int
	crash   string // a crash after the first arrival at this point
	reloads []error
	// crashReload crashes the run inside the first reload, once it has
	// taken effect.
	crashReload bool
	// fullDisk: the first run ends in the full-disk end, the record and
	// the entry left, the new file on disk; the second run (no faults)
	// is the console's after freeing space.
	fullDisk   bool
	fullPhase  string // the record's phase at the full-disk end
	rerun      bool   // run again with no faults before checking
	journal    string // an error-level line containing this
	file       int    // the installed file at the end: 1 or 2
	advanced   bool   // the baseline is the pushed commit at the end
	phase, msg string // the result at the end ("" phase: none); msg "~x": contains x
	reloads2   int    // reloads over every run
}

func TestFailureModes(t *testing.T) {
	failed := "~the install failed before the Caddyfile changed: "
	rows := []fmRow{
		// result refused
		{name: "result refused/write fails", holds: "I2 I3", scen: "unsigned", fail: map[string]int{"result:refused": -1}, journal: "box result could not be written", file: 1},
		{name: "result refused/crash after", holds: "I2", scen: "unsigned", crash: "result:refused", file: 1, phase: phaseRefused, msg: "~is not signed"},
		// result verified
		{name: "result verified/write fails", holds: "I1 I2 I3", scen: "change", fail: map[string]int{"result:verified": -1}, journal: "box result could not be written", file: 1, phase: phaseFailed, msg: failed + "result:verified: no space"},
		{name: "result verified/write fails, failed too", holds: "I1 I2 I3", scen: "change", fail: map[string]int{"result:verified": -1, "result:failed": -1}, journal: "box result could not be written", file: 1},
		{name: "result verified/crash after", holds: "I1 I2 I3", scen: "change", crash: "result:verified", file: 1, phase: phaseFailed, msg: msgInterrupted},
		// record no_change
		{name: "record no_change/write fails", holds: "I1 I4 I5", scen: "same", fail: map[string]int{"record:no_change": -1}, file: 1, phase: phaseFailed, msg: failed + "record:no_change: no space"},
		{name: "record no_change/crash after", holds: "I4 I5 I8", scen: "same", crash: "record:no_change", file: 1, advanced: true, phase: phaseNoChange},
		// record installing
		{name: "record installing/write fails", holds: "I1 I5", scen: "change", fail: map[string]int{"record:installing": -1}, file: 1, phase: phaseFailed, msg: failed + "record:installing: no space"},
		{name: "record installing/crash after", holds: "I1 I5 I8", scen: "change", crash: "record:installing", file: 1, phase: phaseFailed, msg: msgInterrupted},
		// new file temp + rename
		{name: "new file/write fails", holds: "I1 I2", scen: "change", fail: map[string]int{"caddyfile:new": -1}, file: 1, phase: phaseFailed, msg: failed + "caddyfile:new: no space"},
		{name: "new file/crash after", holds: "I1 I5", scen: "change", crash: "caddyfile:new", file: 1, phase: phaseRolledBack, msg: msgRolledBackInterrupted, reloads2: 1},
		// record swapped
		{name: "record swapped/write fails", holds: "I1 I4", scen: "change", fail: map[string]int{"record:swapped": -1}, file: 1, phase: phaseRolledBack, msg: msgRolledBackUnrecorded, reloads2: 1},
		{name: "record swapped/write fails, the rollback's reload too", holds: "I1 I4", scen: "change", fail: map[string]int{"record:swapped": -1}, reloads: []error{errReload}, file: 1, phase: phaseUnknown, msg: msgPrevReloadFailed, reloads2: 1},
		{name: "record swapped/write fails, write-back too: full disk", holds: "I1 I2 I5", scen: "change",
			fail: map[string]int{"record:swapped": -1, "record:rolling_back": -1, "caddyfile:prev": -1}, fullDisk: true, fullPhase: phaseInstalling,
			journal: msgDiskFull, file: 1, phase: phaseRolledBack, msg: msgRolledBackInterrupted, reloads2: 1},
		{name: "record swapped/crash after", holds: "I1 I5", scen: "change", crash: "record:swapped", file: 1, phase: phaseRolledBack, msg: msgRolledBackInterrupted, reloads2: 1},
		// reload
		{name: "reload/fails", holds: "I1 I4", scen: "change", reloads: []error{errReload}, file: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		{name: "reload/fails, second reload too", holds: "I1 I4", scen: "change", reloads: []error{errReload, errReload}, file: 1, phase: phaseUnknown, msg: msgBothReloads, reloads2: 2},
		{name: "reload/crash after", holds: "I1 I4 I5", scen: "change", crashReload: true, file: 1, phase: phaseRolledBack, msg: msgRolledBackInterrupted, reloads2: 2},
		// record applied
		{name: "record applied/write fails", holds: "I1 I4", scen: "change", fail: map[string]int{"record:applied": -1}, file: 1, phase: phaseRolledBack, msg: msgRecordAfterReload, reloads2: 2},
		{name: "record applied/write fails, the rollback's reload too", holds: "I1 I4", scen: "change", fail: map[string]int{"record:applied": -1}, reloads: []error{nil, errReload}, file: 1, phase: phaseUnknown, msg: msgPrevReloadFailed, reloads2: 2},
		{name: "record applied/write fails, write-back too: full disk", holds: "I1 I2 I4 I5", scen: "change",
			fail: map[string]int{"record:applied": -1, "caddyfile:prev": -1}, fullDisk: true, fullPhase: phaseRollingBack,
			journal: msgDiskFull, file: 1, phase: phaseRolledBack, msg: msgRecordAfterReload, reloads2: 2},
		{name: "record applied/crash after", holds: "I4 I5 I8", scen: "change", crash: "record:applied", file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		// applied.json
		{name: "applied.json/write fails", holds: "I4", scen: "change", fail: map[string]int{"applied.json": -1}, journal: "box baseline could not be recorded", file: 2, phase: phaseUnknown, reloads2: 1},
		{name: "applied.json/write fails on no_change", holds: "I4", scen: "same", fail: map[string]int{"applied.json": -1}, journal: "box baseline could not be recorded", file: 1, phase: phaseUnknown},
		{name: "applied.json/crash after", holds: "I4 I5", scen: "change", crash: "applied.json", file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		// previous bytes back
		{name: "previous bytes back/fails once, retried", holds: "I1", scen: "change", reloads: []error{errReload}, fail: map[string]int{"caddyfile:prev": 1}, file: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		{name: "previous bytes back/fails twice: full disk", holds: "I1 I2 I5", scen: "change", reloads: []error{errReload}, fail: map[string]int{"caddyfile:prev": -1},
			fullDisk: true, fullPhase: phaseRollingBack, journal: msgDiskFull, file: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		{name: "previous bytes back/crash after", holds: "I1 I5", scen: "change", reloads: []error{errReload}, crash: "caddyfile:prev", file: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		// record rolling_back (the rollback's own record write)
		{name: "record rolling_back/write fails", holds: "I1", scen: "change", reloads: []error{errReload}, fail: map[string]int{"record:rolling_back": -1}, file: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		{name: "record rolling_back/crash after", holds: "I1 I5", scen: "change", reloads: []error{errReload}, crash: "record:rolling_back", file: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		// terminal result
		{name: "terminal result/write fails", holds: "I2 I3 I4", scen: "change", fail: map[string]int{"result:applied": -1}, journal: "box result could not be written", file: 2, advanced: true, phase: phaseVerified, reloads2: 1},
		{name: "terminal result/crash after", holds: "I2 I5", scen: "change", crash: "result:applied", file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		// record removed
		{name: "record removed/fails once, retried", holds: "I5", scen: "change", fail: map[string]int{"record:remove": 1}, file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		{name: "record removed/fails: next run removes it", holds: "I5", scen: "change", fail: map[string]int{"record:remove": -1}, rerun: true, journal: "box record could not be removed", file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		{name: "record removed/crash after", holds: "I2", scen: "change", crash: "record:remove", file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		// entry removed
		{name: "entry removed/fails once, retried", holds: "I2", scen: "change", fail: map[string]int{"entry:remove": 1}, file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		{name: "entry removed/fails: next run removes it", holds: "I2", scen: "change", fail: map[string]int{"entry:remove": -1}, rerun: true, journal: "box work entry could not be removed", file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		{name: "entry removed/crash after", holds: "I2", scen: "change", crash: "entry:remove", file: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		// take (in/ → work/)
		{name: "take/fails", holds: "I2 I3", scen: "change", fail: map[string]int{"take": -1}, file: 1, phase: phaseFailed, msg: failed + "take: no space"},
		{name: "take/crash after", holds: "I2 I3", scen: "change", crash: "take", file: 1, phase: phaseFailed, msg: msgInterrupted},
	}
	for _, row := range rows {
		t.Run(row.name+" ["+row.holds+"]", func(t *testing.T) {
			b := newTestBox(t)
			var head string
			switch row.scen {
			case "change":
				head = b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
			case "same":
				head = b.repo.commit(b.v1, &b.alice, b.base)
			case "unsigned":
				head = b.repo.commit(boxFile(2, b.alice), nil, b.base)
			}
			v2 := b.repo.files[head]
			id := b.push(b.repo.bundleFiles(head, b.base))
			b.sd.reloads = row.reloads
			h := hooks{}
			if row.fail != nil {
				h.fail = failAt(row.fail)
			}
			// I1 at every instant a test can see: the Caddyfile is one of
			// the two complete files.
			whole := func(when string) {
				t.Helper()
				if got := string(b.installed()); got != string(b.v1) && got != string(v2) {
					t.Fatalf("%s: the Caddyfile is neither file (I1):\n%s", when, got)
				}
			}
			switch {
			case row.crash != "":
				h.crash = crashAfter(row.crash, 1)
				if p := b.runCrash(h); p != row.crash {
					t.Fatalf("crashed at %s", p)
				}
				whole("after the crash")
				if err := b.run(hooks{}); err != nil {
					t.Fatalf("recovery: %v", err)
				}
			case row.crashReload:
				b.sd.onReload = func(n int) {
					if n == 1 {
						panic(crashed{point: "reload"})
					}
				}
				b.runCrash(h)
				b.sd.onReload = nil
				whole("after the crash")
				if err := b.run(hooks{}); err != nil {
					t.Fatalf("recovery: %v", err)
				}
			case row.fullDisk:
				if err := b.run(h); !errors.Is(err, errFullDisk) {
					t.Fatalf("first run: %v, want the full-disk end", err)
				}
				whole("at the full-disk end")
				if r := b.record(); r == nil || r.Phase != row.fullPhase {
					t.Fatalf("full disk leaves the record (phase %s): %+v", row.fullPhase, r)
				}
				if n := b.names(b.x("work")); len(n) != 1 {
					t.Fatalf("full disk leaves the entry in work/: %v", n)
				}
				if string(b.installed()) != string(v2) {
					t.Fatal("full disk: the new file should still be on disk")
				}
				if !b.errorLogged(row.journal) {
					t.Errorf("no error-level line %q", row.journal)
				}
				if err := b.run(hooks{}); err != nil {
					t.Fatalf("the console's run after freeing space: %v", err)
				}
			default:
				if err := b.run(h); err != nil {
					t.Fatalf("run: %v", err)
				}
				if row.rerun {
					whole("after the first run")
					if err := b.run(hooks{}); err != nil {
						t.Fatalf("second run: %v", err)
					}
				}
			}
			b.settled()
			want := b.v1
			if row.file == 2 {
				want = v2
			}
			if string(b.installed()) != string(want) {
				t.Errorf("installed file is not version %d", row.file)
			}
			if adv := b.applied().SHA == head; adv != row.advanced {
				t.Errorf("baseline advanced = %v, want %v (I4)", adv, row.advanced)
			}
			r := b.result(id)
			switch {
			case row.phase == "" && r != nil:
				t.Errorf("a result was written: %+v", r)
			case row.phase != "" && r == nil:
				t.Errorf("no result, want %s", row.phase)
			case r != nil && r.Phase != row.phase:
				t.Errorf("result phase %s (%s), want %s", r.Phase, r.Error, row.phase)
			case r != nil && strings.HasPrefix(row.msg, "~") && !strings.Contains(r.Error, row.msg[1:]):
				t.Errorf("result error %q, want it to contain %q", r.Error, row.msg[1:])
			case r != nil && row.msg != "" && !strings.HasPrefix(row.msg, "~") && r.Error != row.msg:
				t.Errorf("result error %q, want %q", r.Error, row.msg)
			}
			if row.journal != "" && !b.errorLogged(row.journal) {
				t.Errorf("no error-level line %q", row.journal)
			}
			if b.sd.reloaded != row.reloads2 {
				t.Errorf("%d reloads, want %d", b.sd.reloaded, row.reloads2)
			}
		})
	}
}

// Every result text the applier writes is the Message catalogue's, word
// for word: the two copies are written by hand, so a reworded one would
// otherwise drift from the table the workflow is built against.
func TestApplierMessagesAreCatalogued(t *testing.T) {
	// The section alone: some texts are also quoted in the States and
	// Transitions tables, which must not stand in for the catalogue.
	_, section, ok := strings.Cut(readDesign(t), "\n## Message catalogue\n")
	if !ok {
		t.Fatal("DESIGN-box.md has no Message catalogue section")
	}
	catalogue, _, _ := strings.Cut(section, "\n## ")
	for _, msg := range []string{
		msgNotRunning, msgStillStarting, msgNoSigner, msgInterrupted, msgInterruptedStopped,
		msgNoRecord, msgReloadFailed, msgRecordAfterReload, msgRolledBackUnrecorded,
		msgRolledBackInterrupted, msgBothReloads, msgPrevReloadFailed, msgChanged, msgDiskFull,
		msgInterruptedFiles, msgRolledBackUnrecordedFiles, msgRolledBackInterruptedFiles, msgEnvChanged, msgEnvDiskFull,
		msgNoKey, msgSecretsDir, msgEnvTooLarge, msgEnvNotRegular, msgKeyNotRoot,
	} {
		if !strings.Contains(catalogue, "`"+msg+"`") {
			t.Errorf("not in the Message catalogue: %q", msg)
		}
	}
}

// A run that leaves work on disk ends with one line naming the remedy and
// counting what it leaves (I2's exceptions); a late arrival in in/ is
// not a stop and gets none. The harness holds every run to the line's
// presence (checkLeftLine); these pin what it counts.
func TestLeftOnDiskLine(t *testing.T) {
	cases := []struct {
		name  string
		setup func(b *testBox) hooks
		left  string // the line's `left` field; "" for no line
	}{
		{"the lock cannot be taken", func(b *testBox) hooks {
			if err := os.Mkdir(b.x("lock"), 0o700); err != nil {
				b.t.Fatal(err)
			}
			return hooks{}
		}, "[in/: 1]"},
		{"the record does not read", func(b *testBox) hooks {
			if err := os.WriteFile(b.x("txn.json"), []byte("{"), 0o600); err != nil {
				b.t.Fatal(err)
			}
			return hooks{}
		}, "[txn.json in/: 1]"},
		{"an entry cannot be removed", func(b *testBox) hooks {
			return hooks{fail: failAt(map[string]int{"entry:remove": -1})}
		}, "[work/: 1]"},
		{"a bundle lands after the listing", func(b *testBox) hooks {
			late := b.repo.commit(boxFile(3, b.alice), &b.alice, b.base)
			return hooks{read: func(string) { b.push(b.repo.bundleFiles(late, b.base)) }}
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			head := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
			b.push(b.repo.bundleFiles(head, b.base))
			if err := b.run(c.setup(b)); err != nil {
				t.Fatal(err)
			}
			lines := b.logged(zapcore.ErrorLevel, msgLeftOnDisk)
			if c.left == "" {
				if len(lines) != 0 {
					t.Errorf("a late arrival got the left-on-disk line: %v", lines[0].ContextMap())
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("%d left-on-disk lines, want 1", len(lines))
			}
			if got := fmt.Sprint(lines[0].ContextMap()["left"]); got != c.left {
				t.Errorf("left = %s, want %s", got, c.left)
			}
		})
	}
}
