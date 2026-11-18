//go:build integration

package box

import (
	"bytes"
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real systemctl against a real PID 1 (make test-integration's
// dev-systemd container): the words is-active prints for each state the
// applier tells apart, reload's verdict, and a deadline that ends a
// systemctl stuck on a reload job within its WaitDelay — the fake in
// the unit tests encodes these, so they are pinned here.
func TestIntegrationSystemctl(t *testing.T) {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Fatalf("needs systemd as PID 1 (make test-integration): %v", err)
	}
	ctl := func(args ...string) error {
		t.Helper()
		return exec.Command("systemctl", args...).Run()
	}
	unitFile := func(name, body string) string {
		t.Helper()
		unit := "box-it-" + name + "-" + randomID(t)[:8]
		path := filepath.Join("/run/systemd/system", unit+".service")
		if err := os.WriteFile(path, []byte("[Service]\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ctl("daemon-reload"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = ctl("kill", "--signal=KILL", unit)
			_ = ctl("stop", "--no-block", unit)
			_ = ctl("reset-failed", unit)
			_ = os.Remove(path)
			_ = ctl("daemon-reload")
		})
		return unit
	}
	ctx := context.Background()
	isActive := func(s systemctl, want string) {
		t.Helper()
		got, err := s.IsActive(ctx)
		if err != nil || got != want {
			t.Fatalf("is-active %s: %q, %v; want %q", s.unit, got, err, want)
		}
	}

	t.Run("active, reload, inactive", func(t *testing.T) {
		flag := "/run/box-it-reload-fails-" + randomID(t)[:8]
		unit := unitFile("main", "Type=oneshot\nRemainAfterExit=yes\nExecStart=/bin/true\nExecReload=/bin/sh -c 'test ! -e "+flag+"'\n")
		s := systemctl{unit: unit}
		isActive(s, "inactive")
		if err := ctl("start", unit); err != nil {
			t.Fatal(err)
		}
		isActive(s, "active")
		if err := s.Reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if err := os.WriteFile(flag, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(flag) }()
		if err := s.Reload(ctx); err == nil || !strings.Contains(err.Error(), "systemctl reload "+unit) {
			t.Fatalf("a failing reload: %v", err)
		}
		if err := ctl("stop", unit); err != nil {
			t.Fatal(err)
		}
		isActive(s, "inactive")
	})
	t.Run("failed", func(t *testing.T) {
		unit := unitFile("failed", "Type=oneshot\nExecStart=/bin/false\n")
		_ = ctl("start", unit)
		isActive(systemctl{unit: unit}, "failed")
	})
	t.Run("activating", func(t *testing.T) {
		unit := unitFile("activating", "Type=oneshot\nExecStart=/bin/sleep 120\n")
		if err := ctl("start", "--no-block", unit); err != nil {
			t.Fatal(err)
		}
		s := systemctl{unit: unit}
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err := s.IsActive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got == "activating" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("never activating: %q", got)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	// moteld init against the real manager, a stand-in unit for
	// moteld.service: stopped, it refuses with nothing written; once
	// started, the same command applies and reloads it.
	t.Run("init: refused while stopped, applied once started", func(t *testing.T) {
		reloads := "/run/box-it-init-reloads-" + randomID(t)[:8]
		defer func() { _ = os.Remove(reloads) }()
		unit := unitFile("init", "Type=oneshot\nRemainAfterExit=yes\nExecStart=/bin/true\nExecReload=/bin/sh -c 'echo >> "+reloads+"'\n")
		b := newTestBox(t)
		a := b.applier(hooks{})
		a.systemd = systemctl{unit: unit}
		file := boxFile(2, b.alice)
		sha := b.repo.commit(file, &b.alice, b.base)
		dir := initDir(t, file)
		snap := b.snapshot()
		var out strings.Builder
		if err := runInitCmd(ctx, 0, a, dir, sha, &out); err == nil || !strings.HasPrefix(err.Error(), msgNotRunning+"\n") {
			t.Fatalf("init on a stopped unit: %v", err)
		}
		if !maps.Equal(snap, b.snapshot()) {
			t.Fatal("a refused init changed the box")
		}
		if err := ctl("start", unit); err != nil {
			t.Fatal(err)
		}
		a = b.applier(hooks{}) // a new run, with a wait budget of its own
		a.systemd = systemctl{unit: unit}
		if err := runInitCmd(ctx, 0, a, dir, sha, &out); err != nil {
			t.Fatalf("init on a started unit: %v\n%s", err, out.String())
		}
		b.settled()
		if got, _ := os.ReadFile(reloads); string(got) != "\n" {
			t.Errorf("reloads: %q, want one", got)
		}
		if !bytes.Equal(b.installed(), file) || b.applied().SHA != sha {
			t.Error("init did not apply on a started unit")
		}
	})
	t.Run("a deadline ends a systemctl stuck on its reload job", func(t *testing.T) {
		unit := unitFile("stuck", "Type=oneshot\nRemainAfterExit=yes\nExecStart=/bin/true\nExecReload=/bin/sleep 120\n")
		if err := ctl("start", unit); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := runSystemctl(ctx, time.Second, "reload", unit)
		if err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("%v", err)
		}
		if took := time.Since(start); took > time.Second+childWaitDelay+2*time.Second {
			t.Fatalf("returned after %v", took)
		}
		// The reload job runs on in PID 1: is-active says `reloading`,
		// the word step 16 waits out and up() counts as up.
		isActive(systemctl{unit: unit}, "reloading")
	})
}
