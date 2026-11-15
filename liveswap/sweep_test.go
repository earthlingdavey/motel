package liveswap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestSweepUnknownApps(t *testing.T) {
	conn := newFakeSystemdConn()
	root := t.TempDir()
	// What the removed app left on disk while motel was down: no
	// managedApp will ever sweep it, so this sweep must. The configured
	// app's are left alone (its own sweeps own them).
	for _, app := range []string{"old", "demo"} {
		d := newAppDirs(root, app)
		must(t, os.MkdirAll(d.runDir("0a1b2c3d0a1b2c3d"), 0o750))
		must(t, os.MkdirAll(d.proxy, 0o750))
		must(t, os.WriteFile(filepath.Join(d.proxy, "0a1b2c3d0a1b2c3d.sock"), nil, 0o600))
	}
	running := unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true}
	conn.setStatus("motel-demo.v1.0a1b2c3d0a1b2c3d.service", running)     // configured: keep
	conn.setStatus("motel-old.v3.0a1b2c3d0a1b2c3d.service", running)      // removed app: stop
	conn.setStatus("motel-old.v2.0a1b2c3e0a1b2c3e.service", failedStatus) // removed app: reset
	conn.setStatus("motel-demo-api.v1.0a1b2c3d0a1b2c3d.service", running) // another configured app: keep
	conn.setStatus("motel-weird.service", running)                        // not ours: ignore
	configured := map[string]bool{"demo": true, "demo-api": true}
	swapSeam(t, &appConfiguredSeam, func(app string) bool { return configured[app] })
	err := sweepUnknownApps(context.Background(), conn, root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if stops := conn.stops(); len(stops) != 1 || stops[0] != "motel-old.v3.0a1b2c3d0a1b2c3d.service" {
		t.Fatalf("only the removed app's running unit must be stopped, got %v", stops)
	}
	conn.mu.Lock()
	lists := conn.listCalls
	conn.mu.Unlock()
	if lists != 2 { // one global listing + one per unknown app, not per unit
		t.Fatalf("each unknown app must be swept once, got %d listings", lists)
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != "motel-old.v2.0a1b2c3e0a1b2c3e.service" {
		t.Fatalf("the removed app's failed unit must be reset, got %v", rs)
	}
	for app, want := range map[string]bool{"old": false, "demo": true} {
		d := newAppDirs(root, app)
		_, runErr := os.Stat(d.runDir("0a1b2c3d0a1b2c3d"))
		_, pinErr := os.Stat(filepath.Join(d.proxy, "0a1b2c3d0a1b2c3d.sock"))
		if got := runErr == nil && pinErr == nil; got != want {
			t.Fatalf("%s's socket dirs present = %v, want %v (run: %v, pin: %v)", app, got, want, runErr, pinErr)
		}
	}
	conn.mu.Lock()
	conn.listErr = errors.New("dbus down")
	conn.mu.Unlock()
	if err := sweepUnknownApps(context.Background(), conn, root, zap.NewNop()); err == nil {
		t.Fatal("a listing failure must be reported")
	}
}

func TestSweepUnknownAppsDoesNothingWhileExiting(t *testing.T) {
	conn := newFakeSystemdConn()
	root := t.TempDir()
	conn.setStatus("motel-old.v3.0a1b2c3d0a1b2c3d.service", unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true})
	swapSeam(t, &caddyExitingSeam, func() bool { return true })
	if err := sweepUnknownApps(context.Background(), conn, root, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if len(conn.stops()) != 0 {
		t.Fatalf("shutdown drops pool references; units must survive it, stops=%v", conn.stops())
	}
}

func TestSweepUnknownAppsJudgesAgainstLiveConfig(t *testing.T) {
	// A reload adds "late" back while the sweep is still listing: the
	// sweep must judge against the ledger as it is right before acting
	// — never a capture from before.
	conn := newFakeSystemdConn()
	root := t.TempDir()
	running := unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true}
	conn.setStatus("motel-late.v1.0a1b2c3d0a1b2c3d.service", running)
	late := newAppDirs(root, "late")
	must(t, os.MkdirAll(late.runDir("0a1b2c3d0a1b2c3d"), 0o750))
	must(t, os.MkdirAll(late.proxy, 0o750))
	must(t, os.WriteFile(filepath.Join(late.proxy, "0a1b2c3d0a1b2c3d.sock"), nil, 0o600))
	var mu sync.Mutex
	configured := map[string]bool{} // "late" is not configured when the sweep starts
	// Ownership is not monotonic: the candidate that adopts "late"
	// mid-listing is itself cleaned up right after the sweep's veto,
	// so every later check finds the app unowned again. The veto must
	// still stand: a sweep that left a unit running prunes nothing.
	swapSeam(t, &appConfiguredSeam, func(app string) bool {
		mu.Lock()
		defer mu.Unlock()
		owned := configured[app]
		configured[app] = false
		return owned
	})
	conn.mu.Lock()
	conn.listHook = func() { mu.Lock(); configured["late"] = true; mu.Unlock() } // the reload lands mid-listing
	conn.mu.Unlock()
	if err := sweepUnknownApps(context.Background(), conn, root, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if len(conn.stops()) != 0 {
		t.Fatalf("an app the live config names must never be swept, got %v", conn.stops())
	}
	// Nor may its socket dirs be pruned: the unit the sweep left
	// running is the instance the reload is adopting.
	for _, p := range []string{late.runDir("0a1b2c3d0a1b2c3d"), filepath.Join(late.proxy, "0a1b2c3d0a1b2c3d.sock")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("an adopted app's %s was pruned: %v", p, err)
		}
	}
}

// Deciding an app's socket dirs are nobody's, and pruning them, is one
// step under the app's ownerLock — the lock recovery holds while it
// adopts — so a reload adopting the app cannot slip between the check
// and the prune. Pinned by holding the lock: the sweep must wait for
// that app, and only that app.
func TestSweepUnknownAppsPruneWaitsForRecovery(t *testing.T) {
	conn := newFakeSystemdConn()
	root := t.TempDir()
	gone, other := newAppDirs(root, "gone"), newAppDirs(root, "other")
	must(t, os.MkdirAll(gone.runDir("0a1b2c3d0a1b2c3d"), 0o750))
	must(t, os.MkdirAll(other.runDir("0a1b2c3d0a1b2c3d"), 0o750))
	swapSeam(t, &appConfiguredSeam, func(string) bool { return false })

	mu := ownerLock("gone")
	mu.Lock()
	done := make(chan error, 1)
	go func() { done <- sweepUnknownApps(context.Background(), conn, root, zap.NewNop()) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(gone.runDir("0a1b2c3d0a1b2c3d")); err != nil {
		mu.Unlock()
		t.Fatal("the sweep pruned while recovery held the app's ownership lock")
	}
	mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, d := range []appDirs{gone, other} {
		if _, err := os.Stat(d.runDir("0a1b2c3d0a1b2c3d")); !os.IsNotExist(err) {
			t.Fatalf("once the lock is free the unowned dirs are pruned (%s): %v", d.app, err)
		}
	}
}

// A root that cannot be read is reported, not treated as swept.
func TestSweepUnknownAppsReportsAnUnreadableRoot(t *testing.T) {
	conn := newFakeSystemdConn()
	root := filepath.Join(t.TempDir(), "root-is-a-file")
	must(t, os.WriteFile(root, nil, 0o600))
	if err := sweepUnknownApps(context.Background(), conn, root, zap.NewNop()); err == nil {
		t.Fatal("an unreadable root must be reported")
	}
	if err := sweepUnknownApps(context.Background(), conn, filepath.Join(t.TempDir(), "absent"), zap.NewNop()); err != nil {
		t.Fatalf("a missing root is nothing to prune, not an error: %v", err)
	}
}

// An app whose units are all gone never appears in the manager's
// listing; if it is no longer configured either, the start-time sweep
// is the only thing that will ever prune what it left under the root.
// A configured app's leftovers are its own managedApp's business.
func TestSweepUnknownAppsPrunesAppsWithNoUnitsLeft(t *testing.T) {
	conn := newFakeSystemdConn()
	root := t.TempDir()
	for _, app := range []string{"gone", "demo"} {
		d := newAppDirs(root, app)
		must(t, os.MkdirAll(d.runDir("0a1b2c3d0a1b2c3d"), 0o750))
		must(t, os.MkdirAll(d.proxy, 0o750))
		must(t, os.WriteFile(filepath.Join(d.proxy, "0a1b2c3d0a1b2c3d.sock"), nil, 0o600))
		must(t, os.WriteFile(d.alerts, []byte(`{"refused":true}`), 0o600))
	}
	must(t, os.WriteFile(filepath.Join(root, "not-an-app-dir.txt"), nil, 0o600))
	swapSeam(t, &appConfiguredSeam, func(app string) bool { return app == "demo" })
	if err := sweepUnknownApps(context.Background(), conn, root, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	for app, want := range map[string]bool{"gone": false, "demo": true} {
		d := newAppDirs(root, app)
		_, runErr := os.Stat(d.runDir("0a1b2c3d0a1b2c3d"))
		_, pinErr := os.Stat(filepath.Join(d.proxy, "0a1b2c3d0a1b2c3d.sock"))
		if got := runErr == nil && pinErr == nil; got != want {
			t.Fatalf("%s's socket dirs present = %v, want %v", app, got, want)
		}
		// What a removed app's watchdog reported goes with it, so an app
		// of the name added back later reports nothing stale.
		if _, err := os.Stat(d.alerts); (err == nil) != want {
			t.Fatalf("%s's alerts.json present = %v, want %v", app, err == nil, want)
		}
	}
	if _, err := os.Stat(newAppDirs(root, "gone").app); err != nil {
		t.Fatal("only the socket dirs are pruned; the app's directory and releases stay")
	}
}

// The start-time sweep prunes an unconfigured app's socket dirs only as
// the directories they name: a stray unit of `demo`, whose dir is a
// link to the configured shop's, gets its unit stopped and shop's live
// socket and pin left alone.
func TestSweepUnknownAppsDoesNotPruneThroughALinkedAppDir(t *testing.T) {
	conn := newFakeSystemdConn()
	root := t.TempDir()
	shop := newAppDirs(root, "shop")
	must(t, os.MkdirAll(shop.runDir("0a1b2c3d0a1b2c3d"), 0o750))
	must(t, os.MkdirAll(shop.proxy, 0o750))
	pin := filepath.Join(shop.proxy, "0a1b2c3d0a1b2c3d.sock")
	must(t, os.WriteFile(pin, nil, 0o600))
	must(t, os.Symlink("shop", filepath.Join(root, "demo")))
	conn.setStatus("motel-demo.v1.0b1b2c3d0a1b2c3d.service", unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true})
	swapSeam(t, &appConfiguredSeam, func(app string) bool { return app == "shop" })
	if err := sweepUnknownApps(context.Background(), conn, root, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if stops := conn.stops(); len(stops) != 1 {
		t.Fatalf("demo's stray unit must still be stopped, got %v", stops)
	}
	for _, f := range []string{shop.runDir("0a1b2c3d0a1b2c3d"), pin} {
		if _, err := os.Lstat(f); err != nil {
			t.Fatalf("shop's %s was pruned through demo's link: %v", f, err)
		}
	}
}

// The two seams the sweep reads are swapped by tests while a sweep may
// still be running: App.Start launches it on a goroutine nothing joins
// (bounded only by unknownAppSweepTimeout), so one test's restore can
// land while a sweep started by an earlier test is still reading. Both
// must therefore tolerate a concurrent read and swap, and the two loops
// below deliberately share no happens-before edge — that pairing is
// exactly what -race reports if either seam is not an atomic.
//
// Both verdicts installed here are the ones that make a sweep do
// nothing (exiting, and every app still owned): the seams are global,
// so a sweep leaked by an earlier test and running through this one
// must find the safe answer, never a licence to stop and prune.
func TestSweepSeamsTolerateAConcurrentSwap(t *testing.T) {
	const rounds = 1000
	swapSeam(t, &caddyExitingSeam, func() bool { return true })
	swapSeam(t, &appConfiguredSeam, func(string) bool { return true })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < rounds; i++ {
			_ = caddyExiting()
			_ = appConfigured("demo")
		}
	}()
	for i := 0; i < rounds; i++ {
		exiting := func() bool { return true }
		caddyExitingSeam.Store(&exiting)
		configured := func(string) bool { return true }
		appConfiguredSeam.Store(&configured)
	}
	<-done
}
