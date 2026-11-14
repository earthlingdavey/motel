package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func external(firing bool) Event {
	return Event{Name: "disk_failing", Kind: kindState, Severity: SeverityCritical, Instance: "/dev/sdb",
		Source: "external:smartd", Summary: "sdb reports reallocated sectors", Details: map[string]string{"sectors": "12"}, Firing: firing}
}

// restored is a test engine as a new process's: it reads the state
// directory's file, as configure does at notify's first Start.
func restored(t *testing.T, apps ...string) *testEngine {
	t.Helper()
	te := newTestEngine(t)
	te.mu.Lock()
	te.restoreExternal(apps)
	te.mu.Unlock()
	return te
}

// diskFailing is the producer's alert among those posted.
func diskFailing(as []amAlert) amAlert {
	for _, a := range as {
		if a.Labels["alertname"] == "disk_failing" {
			return a
		}
	}
	return amAlert{}
}

func journalStates(te *testEngine) string {
	var out []string
	for _, l := range te.lines("monitor: event") {
		out = append(out, fmt.Sprintf("%s:%v", l.ContextMap()["state"], l.ContextMap()["standing"]))
	}
	return strings.Join(out, ",")
}

// A producer's state outlives a restart of motel: the new process tells
// it again as a standing report (Alertmanager merges it with the alert
// it holds), and the producer's later resolve finds it and ends it
// after the hold.
func TestExternalStateOutlivesARestart(t *testing.T) {
	dir := t.TempDir()
	useStateDir(t, dir)
	file := filepath.Join(dir, externalName)

	before := restored(t)
	before.report(external(true))
	before.report(watchdog(true)) // liveswap's own: kept in alerts.json, not here
	before.drain()
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the file after a firing: %v, %v", fi, err)
	}
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "watchdog_throttled") || !strings.Contains(string(b), `"firing":true`) {
		t.Fatalf("the file holds %s", b)
	}
	before.report(external(true)) // a repeat: nothing changed, nothing written
	if fi2, err := os.Stat(file); err != nil || !os.SameFile(fi, fi2) {
		t.Fatal("a repeat of the firing rewrote the file")
	}

	after := restored(t)
	got := after.drain()
	if countAlert(got, "disk_failing") != 1 || !diskFailing(got).EndsAt.IsZero() || diskFailing(got).Annotations["detail_sectors"] != "12" {
		t.Fatalf("the standing report = %+v", got)
	}
	if s := journalStates(after); s != "firing:true" {
		t.Fatalf("journal = %s", s)
	}
	after.mu.Lock()
	held := after.states[external(false).key()] != nil
	after.mu.Unlock()
	if !held {
		t.Fatal("the producer's resolve would find nothing")
	}
	after.report(external(false))
	after.advance(31 * time.Second)
	after.tick()
	if got := after.drain(); countAlert(got, "disk_failing") != 1 || !diskFailing(got).EndsAt.Equal(after.at) {
		t.Fatalf("the resolve after the hold = %+v", got)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the file once nothing stands: %v", err)
	}
}

// A restart inside a recovery's hold: the recovery is held again and
// told after it, never at resolve_timeout.
func TestExternalRecoveryHeldAcrossARestart(t *testing.T) {
	useStateDir(t, t.TempDir())
	before := restored(t)
	before.report(external(true))
	before.drain()
	before.report(external(false))

	after := restored(t)
	if got := after.drain(); countAlert(got, "disk_failing") != 1 || !diskFailing(got).EndsAt.IsZero() {
		t.Fatalf("not kept alive: %+v", got)
	}
	after.advance(31 * time.Second)
	after.tick()
	if got := after.drain(); countAlert(got, "disk_failing") != 1 || !diskFailing(got).EndsAt.Equal(after.at) {
		t.Fatalf("the resolve after the hold = %+v", got)
	}
	if s := journalStates(after); s != "held:true,resolved:<nil>" {
		t.Fatalf("journal = %s", s)
	}
}

// A file that is not there is nothing standing; one that does not read,
// or holds what is not a producer's state, is said, taken as nothing,
// and removed, so the warning is said once.
func TestExternalFileMissingOrCorrupt(t *testing.T) {
	for name, content := range map[string]string{
		"missing":       "",
		"not JSON":      "{\"states\":[",
		"motel's own":   `{"states":[{"source":"liveswap","app":"blog","name":"watchdog_throttled","severity":"critical","summary":"x","firing":true}]}`,
		"a name absent": `{"states":[{"source":"external:smartd","severity":"critical","summary":"x","firing":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			useStateDir(t, dir)
			file := filepath.Join(dir, externalName)
			if content != "" {
				if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			te := restored(t)
			if got := te.drain(); len(got) != 0 {
				t.Fatalf("posted %+v", got)
			}
			warned := te.lines("monitor: the producers' states kept before this start could not be read; taken as none, and removed")
			if (len(warned) == 1) != (content != "") {
				t.Fatalf("warnings: %+v", warned)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("the file: %v", err)
			}
			te.report(external(true)) // and the writes go on
			if _, err := os.Stat(file); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A config that no longer runs the app resolves what was kept for it, as
// a reload would; a reload that takes the block out forgets the file, and
// a shutdown keeps it.
func TestExternalFileAtConfigureAndRelease(t *testing.T) {
	orig := configRunning
	t.Cleanup(func() { configRunning = orig })
	for _, reload := range []bool{false, true} {
		dir := t.TempDir()
		useStateDir(t, dir)
		useSocket(t, filepath.Join(t.TempDir(), "absent.sock"))
		file := filepath.Join(dir, externalName)
		gone := external(true)
		gone.App, gone.Name = "shop", "queue_stuck"
		before := restored(t, "blog", "shop")
		before.report(external(true))
		before.report(gone)

		e := newEngine()
		a := &App{BackupRecord: "off", cfg: &resolved{apps: []string{"blog"}}, logger: zap.NewNop()}
		p := &recPoster{}
		e.configure(a, p, nil, zap.NewNop())
		e.mu.Lock()
		_, box := e.states[external(true).key()]
		_, shop := e.states[gone.key()]
		e.mu.Unlock()
		if !box || shop {
			t.Fatalf("after the restore: box %v, shop %v", box, shop)
		}
		if b, _ := os.ReadFile(file); strings.Contains(string(b), "queue_stuck") || !strings.Contains(string(b), "disk_failing") {
			t.Fatalf("the file holds %s", b)
		}
		configRunning = func() bool { return reload }
		e.release(a)
		_, err := os.Stat(file)
		if reload != os.IsNotExist(err) {
			t.Fatalf("reload %v: the file: %v", reload, err)
		}
	}
}

// A state sent before setup made the state directory is written once it
// has: the next tick tries again, without a warning meanwhile.
func TestExternalStateWrittenOnceSetUp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	useStateDir(t, dir)
	te := restored(t)
	te.report(external(true))
	te.tick()
	if l := te.lines("monitor: the producers' states could not be kept for a restart: until the next change, a restart tells what the file still says"); len(l) != 0 {
		t.Fatalf("warned before setup: %+v", l)
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	te.advance(5 * time.Second)
	te.tick()
	if b, err := os.ReadFile(filepath.Join(dir, externalName)); err != nil || !strings.Contains(string(b), "disk_failing") {
		t.Fatalf("after setup: %s, %v", b, err)
	}
}

// A start into a config without the block forgets the file, as a
// reload's release does; a box setup has not run on is no error.
func TestSendNothingForgetsTheProducersStates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, externalName)
	if err := os.WriteFile(file, []byte(`{"states":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SendNothing(dir, filepath.Join(t.TempDir(), "absent.sock")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the file: %v", err)
	}
	if _, err := SendNothing(filepath.Join(dir, "absent"), filepath.Join(t.TempDir(), "absent.sock")); err != nil {
		t.Fatal(err)
	}
}
