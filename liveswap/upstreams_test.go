package liveswap

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deploys in this file bind a real socket: GetUpstreams pins the
// socket file by descriptor, and there is nothing to pin otherwise.
func newUpstreamsRig(t *testing.T) *testRig {
	t.Helper()
	rig := newTestRig(t)
	rig.prober.bind = true
	return rig
}

func TestGetUpstreamsBeforeFirstDeploy(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.markRecovered() // recovery found nothing to run: no wait (startwait_test.go)
	u := &Upstreams{App: "demo", ma: rig.ma}
	_, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	if err == nil || !strings.Contains(err.Error(), "no running version") {
		t.Fatalf("want no-version error, got %v", err)
	}
}

func TestGetUpstreamsReturnsActiveSocket(t *testing.T) {
	rig := newUpstreamsRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
	u := &Upstreams{App: "demo", ma: rig.ma}
	ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	if err != nil || len(ups) != 1 {
		t.Fatalf("upstreams: %v %v", ups, err)
	}
	// Caddy's unix//abs/path spelling: the network, a slash, then the
	// absolute path with its own leading slash — and the path is the
	// pinned name under proxy/, never the app-writable one under run/.
	inst := rig.ma.currentInstance()
	want := "unix/" + inst.sock.pinned
	if ups[0].Dial != want || !strings.HasPrefix(ups[0].Dial, "unix//") || !pathWithin(inst.sock.pinned, rig.spec.dirs.proxy) {
		t.Fatalf("dial = %q, want %q under %s", ups[0].Dial, want, rig.spec.dirs.proxy)
	}
	pinned, err := os.Lstat(inst.sock.pinned)
	must(t, err)
	bound, err := os.Lstat(inst.socket)
	must(t, err)
	if !os.SameFile(pinned, bound) {
		t.Fatal("the pinned name must be a hard link to the socket the app bound")
	}
}

// Promise: the proxy never follows the app-writable name. An app that
// replaces its socket with a symlink — to motel's admin socket, to
// a sibling's — gets a 502, not a connection through it.
func TestGetUpstreamsRefusesASymlinkUnderTheSocketName(t *testing.T) {
	rig := newTestRig(t) // no bind: nothing pinned yet when the symlink appears
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
	victim := filepath.Join(t.TempDir(), "admin.sock")
	bindSocket(t, victim)
	must(t, os.Symlink(victim, rig.ma.currentInstance().socket))
	u := &Upstreams{App: "demo", ma: rig.ma}
	_, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want a refusal naming the symlink, got %v", err)
	}
}

// Cutover is visible to the proxy immediately: after a second deploy,
// GetUpstreams returns the new socket with no re-provisioning.
func TestGetUpstreamsSeesCutover(t *testing.T) {
	rig := newUpstreamsRig(t)
	u := &Upstreams{App: "demo", ma: rig.ma}
	ctx := context.Background()

	assertDial := func(version string) string {
		t.Helper()
		ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
		if err != nil {
			t.Fatalf("GetUpstreams: %v", err)
		}
		inst := rig.ma.currentInstance()
		if inst == nil || inst.version != version {
			t.Fatalf("current instance = %+v, want version %s", inst, version)
		}
		if want := "unix/" + inst.sock.pinned; ups[0].Dial != want {
			t.Fatalf("GetUpstreams dial = %s, want %s", ups[0].Dial, want)
		}
		return ups[0].Dial
	}

	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1.tgz", version: "v1"}))
	v1 := assertDial("v1")
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/2.tgz", version: "v2"}))
	if v2 := assertDial("v2"); v2 == v1 {
		t.Fatalf("v2 was handed v1's socket %s: sockets are per instance", v1)
	}
	// v1 is retired: its pinned name is gone, so a request that still
	// holds it dials nobody — never another instance.
	if _, err := os.Lstat(strings.TrimPrefix(v1, "unix/")); !os.IsNotExist(err) {
		t.Fatalf("v1's pinned name must be removed after the cutover: %v", err)
	}
}
