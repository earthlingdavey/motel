package box

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func randomID(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// lyingRecord writes a record as a crashed run would have left it, with
// its work/ entry when the origin is the applier's.
func (b *testBox) lyingRecord(rec record) {
	b.t.Helper()
	if err := os.WriteFile(b.x("txn.json"), encodeJSON(rec), 0o600); err != nil {
		b.t.Fatal(err)
	}
	if rec.Origin == originApplier {
		if err := os.WriteFile(filepath.Join(b.x("work"), rec.ID+".tar"), []byte("taken"), 0o644); err != nil {
			b.t.Fatal(err)
		}
	}
}

// The States table (DESIGN-box.md): recovery acts on the phase after
// checking d, the installed file's digest, against what the phase
// implies — including every "d must be" violation.
func TestRecoveryStates(t *testing.T) {
	const (
		dPrev  = "prev"
		dNew   = "new"
		dOther = "other"
	)
	type row struct {
		phase    string
		origin   origin
		d        string
		state    string  // is-active's answer
		reloads  []error // reload results
		recError string  // a rolling_back record's error
		boxEnv   bool    // the set is _box.env alone: d and file are its, the Caddyfile stays prev
		file     string  // the installed file at the end: prev, new, other
		advanced bool
		result   string // the terminal phase: a result file, or for init the journal's (it writes none)
		msg      string
		reloaded int
	}
	rows := []row{
		{phase: phaseNoChange, d: dPrev, file: dPrev, advanced: true, result: phaseNoChange},
		{phase: phaseNoChange, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},

		{phase: phaseInstalling, d: dPrev, file: dPrev, result: phaseFailed, msg: msgInterrupted},
		{phase: phaseInstalling, d: dNew, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterrupted, reloaded: 1},
		{phase: phaseInstalling, d: dNew, state: "inactive", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		{phase: phaseInstalling, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},

		{phase: phaseSwapped, d: dNew, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterrupted, reloaded: 1},
		{phase: phaseSwapped, d: dNew, state: "failed", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		// The first reload's outcome is unknown, so "both failed" would overclaim.
		{phase: phaseSwapped, d: dNew, state: "active", reloads: []error{errReload}, file: dPrev, result: phaseUnknown, msg: msgPrevReloadFailed, reloaded: 1},
		{phase: phaseSwapped, d: dPrev, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterrupted, reloaded: 1},
		{phase: phaseSwapped, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},
		// init's records recover as the applier's: a swap whose reload
		// was never confirmed rolls back, whether or not motel runs.
		{phase: phaseSwapped, origin: originInit, d: dNew, state: "inactive", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		{phase: phaseSwapped, origin: originInit, d: dNew, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterrupted, reloaded: 1},
		{phase: phaseNoChange, origin: originInit, d: dPrev, file: dPrev, advanced: true, result: phaseNoChange},
		{phase: phaseApplied, origin: originInit, d: dNew, file: dNew, advanced: true, result: phaseApplied},

		{phase: phaseApplied, d: dNew, file: dNew, advanced: true, result: phaseApplied},
		{phase: phaseApplied, d: dPrev, file: dPrev, result: phaseUnknown, msg: msgChanged},
		{phase: phaseApplied, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},

		{phase: phaseRollingBack, d: dPrev, state: "active", recError: msgRecordAfterReload, file: dPrev, result: phaseRolledBack, msg: msgRecordAfterReload, reloaded: 1},
		{phase: phaseRollingBack, d: dNew, state: "active", recError: msgReloadFailed, file: dPrev, result: phaseRolledBack, msg: msgReloadFailed, reloaded: 1},
		// A record with no error (none the applier writes: a hand edit)
		// names no first reload: the reload succeeding, it reads as one a
		// crash cut short, for the Caddyfile or the box's own env file.
		{phase: phaseRollingBack, d: dNew, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterrupted, reloaded: 1},
		{phase: phaseRollingBack, boxEnv: true, d: dNew, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterruptedFiles, reloaded: 1},
		{phase: phaseRollingBack, d: dPrev, state: "active", reloads: []error{errReload}, file: dPrev, result: phaseUnknown, msg: msgPrevReloadFailed, reloaded: 1},
		{phase: phaseRollingBack, d: dPrev, state: "active", recError: msgReloadFailed, reloads: []error{errReload}, file: dPrev, result: phaseUnknown, msg: msgBothReloads, reloaded: 1},
		{phase: phaseRollingBack, d: dPrev, state: "active", recError: msgRecordAfterReload, reloads: []error{errReload}, file: dPrev, result: phaseUnknown, msg: msgPrevReloadFailed, reloaded: 1},
		{phase: phaseRollingBack, d: dNew, state: "active", recError: msgRolledBackUnrecorded, reloads: []error{errReload}, file: dPrev, result: phaseUnknown, msg: msgPrevReloadFailed, reloaded: 1},
		{phase: phaseRollingBack, d: dPrev, state: "inactive", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		{phase: phaseRollingBack, d: dNew, state: "inactive", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		{phase: phaseRollingBack, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},
	}
	for _, r := range rows {
		if r.origin == "" {
			r.origin = originApplier
		}
		name := r.phase + "/" + string(r.origin) + "/d=" + r.d + "/" + r.state
		if r.boxEnv {
			name += "/" + boxSlot + ".env"
		}
		t.Run(name, func(t *testing.T) {
			b := newTestBox(t)
			v2 := boxFile(2, b.alice)
			head := b.repo.commit(v2, &b.alice, b.base)
			next := v2
			if r.phase == phaseNoChange || r.boxEnv {
				next = b.v1
			}
			other := boxFile(9, b.alice)
			files := map[string][]byte{dPrev: b.v1, dNew: next, dOther: other}
			var set []envEntry
			var installed map[string]string
			var tree string
			if r.boxEnv {
				files = map[string][]byte{dPrev: []byte("NOTIFY_OPS_URL=old\n"), dNew: []byte("NOTIFY_OPS_URL=new\n"), dOther: []byte("NOTIFY_OPS_URL=other\n")}
				b.secretsDirAs(0o750 | os.ModeSetgid)
				b.putEnv(boxSlot, files[r.d], 0o640)
				b.writeInstalled(b.v1)
				set = []envEntry{{App: boxSlot, Prev: files[dPrev], PrevSHA256: digest(files[dPrev]), NewSHA256: digest(files[dNew])}}
				installed, tree = map[string]string{boxSlot: digest(files[dNew])}, strings.Repeat("e", 40)
			} else {
				b.writeInstalled(files[r.d])
			}
			if r.state != "" {
				b.sd.states = []string{r.state}
			}
			b.sd.reloads = r.reloads
			id := randomID(t)
			signer := b.alice.principal
			if r.origin == originInit {
				signer = "init"
			}
			b.lyingRecord(record{
				ID: id, Origin: r.origin, Phase: r.phase, Commit: head, Path: testPath, Signer: signer,
				Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(next), BoxWebhook: "deploy.example.com",
				Error: r.recError, Secrets: set, SecretsTree: tree, SecretsInstalled: installed,
			})
			// A temporary the crash left beside the Caddyfile goes too.
			if err := os.WriteFile(filepath.Join(b.path("etc/motel"), ".Caddyfile.box-"+id), []byte("half"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := b.run(hooks{}); err != nil {
				t.Fatal(err)
			}
			b.settled()
			switch {
			case r.boxEnv && (string(b.env(boxSlot)) != string(files[r.file]) || string(b.installed()) != string(b.v1)):
				t.Errorf("%s.env is not %s, or the Caddyfile not the one it was", boxSlot, r.file)
			case !r.boxEnv && string(b.installed()) != string(files[r.file]):
				t.Errorf("installed file is not %s", r.file)
			}
			if adv := b.applied().SHA == head; adv != r.advanced {
				t.Errorf("baseline advanced = %v, want %v", adv, r.advanced)
			}
			res := b.result(id)
			switch {
			case r.origin == originInit && res != nil:
				t.Errorf("a result was written for init: %+v", res)
			case r.origin == originInit:
				if phase, msg := b.outcomeLogged(id); phase != r.result || msg != r.msg {
					t.Errorf("journaled %s %q, want %s %q", phase, msg, r.result, r.msg)
				}
			case res == nil || res.Phase != r.result || res.Error != r.msg:
				t.Errorf("result %+v, want %s %q", res, r.result, r.msg)
			}
			if b.sd.reloaded != r.reloaded {
				t.Errorf("%d reloads, want %d", b.sd.reloaded, r.reloaded)
			}
		})
	}
}

// A record whose result is already terminal finished all but its
// removals (Failure-mode table, "terminal result", crash after).
func TestRecoveryTerminalResultRemovesRecord(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	head := b.repo.commit(v2, &b.alice, b.base)
	b.writeInstalled(v2)
	id := randomID(t)
	b.lyingRecord(record{ID: id, Origin: originApplier, Phase: phaseApplied, Commit: head, Path: testPath, Signer: "alice@example.com",
		Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
	if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseApplied}), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if b.sd.reloaded != 0 || b.applied().SHA != b.base {
		t.Error("a finished transaction was acted on again")
	}
}

// A record that does not read is kept and nothing is touched: not the
// file, not in/, not work/ (States table, "record unreadable").
func TestRecoveryUnreadableRecord(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       "{",
		"unknown phase":  `{"id":"0123456789abcdef0123456789abcdef","origin":"applier","phase":"verified"}`,
		"prev mismatch":  `{"id":"0123456789abcdef0123456789abcdef","origin":"applier","phase":"swapped","commit":"` + strings.Repeat("a", 40) + `","prev":"eA==","prev_sha256":"` + strings.Repeat("0", 64) + `","new_sha256":"` + strings.Repeat("1", 64) + `"}`,
		"unknown origin": `{"id":"0123456789abcdef0123456789abcdef","origin":"cron","phase":"swapped"}`,
		"no_change whose digests differ": string(encodeJSON(record{ID: "0123456789abcdef0123456789abcdef", Origin: originApplier, Phase: phaseNoChange,
			Commit: strings.Repeat("a", 40), Path: testPath, Prev: []byte("x"), PrevSHA256: digest([]byte("x")), NewSHA256: strings.Repeat("1", 64)})),
	} {
		t.Run(name, func(t *testing.T) {
			b := newTestBox(t)
			if err := os.WriteFile(b.x("txn.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			head := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
			b.push(b.repo.bundleFiles(head, b.base))
			if err := b.run(hooks{}); err != nil {
				t.Fatalf("an unreadable record is not the full-disk end: %v", err)
			}
			if data, _ := os.ReadFile(b.x("txn.json")); string(data) != body {
				t.Error("the record was changed")
			}
			if len(b.names(b.x("in"))) != 1 || string(b.installed()) != string(b.v1) {
				t.Error("something was taken or installed under an unreadable record")
			}
			if !b.errorLogged("txn.json cannot be read") {
				t.Error("no error-level line")
			}
		})
	}
}

// record.valid holds each rule on its own: one bad field, one reason.
func TestRecordValid(t *testing.T) {
	good := record{ID: strings.Repeat("ab", 16), Origin: originApplier, Phase: phaseSwapped, Commit: strings.Repeat("c", 40),
		Path: testPath, Prev: []byte("x"), PrevSHA256: digest([]byte("x")), NewSHA256: strings.Repeat("1", 64)}
	if err := good.valid(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(r *record){
		"origin":         func(r *record) { r.Origin = "cron" },
		"id":             func(r *record) { r.ID = "x" },
		"commit":         func(r *record) { r.Commit = strings.Repeat("c", 64) },
		"digest":         func(r *record) { r.NewSHA256 = "x" },
		"prev":           func(r *record) { r.Prev = []byte("y") },
		"path":           func(r *record) { r.Path = "../x" },
		"digests differ": func(r *record) { r.Phase = phaseNoChange },
		"not a record's": func(r *record) { r.Phase = phaseVerified },
	} {
		r := good
		edit(&r)
		if err := r.valid(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	same := good
	same.Phase, same.NewSHA256 = phaseNoChange, same.PrevSHA256
	if err := same.valid(); err != nil {
		t.Errorf("a no_change record with equal digests: %v", err)
	}
}

// No record: what work/ holds is settled by its result (States table,
// "(no record)"); what is not a bundle is removed with no result.
func TestRecoveryNoRecord(t *testing.T) {
	b := newTestBox(t)
	work := b.x("work")
	terminalID, verifiedID, bareID := randomID(t), randomID(t), randomID(t)
	for _, id := range []string{terminalID, verifiedID, bareID} {
		if err := os.WriteFile(filepath.Join(work, id+".tar"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b.x("out"), terminalID+".json"), encodeJSON(result{ID: terminalID, Phase: phaseRefused, Error: "no"}), 0o640); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("c", 40)
	if err := os.WriteFile(filepath.Join(b.x("out"), verifiedID+".json"), encodeJSON(result{ID: verifiedID, Phase: phaseVerified, Commit: head, Signer: "alice@example.com"}), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(work, "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tmp := range []string{b.x(".txn.json.tmp"), b.x(".applied.json.tmp"), filepath.Join(b.x("out"), "."+bareID+".json.tmp")} {
		if err := os.WriteFile(tmp, []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(terminalID); r.Phase != phaseRefused || r.Error != "no" {
		t.Errorf("a terminal result was rewritten: %+v", r)
	}
	if r := b.result(verifiedID); r.Phase != phaseFailed || r.Error != msgInterrupted || r.Commit != head || r.Signer != "alice@example.com" {
		t.Errorf("verified, no record: %+v", r)
	}
	if r := b.result(bareID); r == nil || r.Phase != phaseFailed || r.Error != msgInterrupted {
		t.Errorf("taken, no record: %+v", r)
	}
	if r := b.result("junk"); r != nil {
		t.Error("an entry that is not a bundle got a result")
	}
	for _, tmp := range []string{b.x(".txn.json.tmp"), b.x(".applied.json.tmp"), filepath.Join(b.x("out"), "."+bareID+".json.tmp")} {
		if exists(tmp) {
			t.Errorf("%s left behind", filepath.Base(tmp))
		}
	}
}
