package liveswap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testHealthConfig() healthConfig {
	return healthConfig{
		path:     "/health",
		interval: time.Second,
		timeout:  time.Second,
		soak:     3 * time.Second,
		deadline: 30 * time.Second,
	}
}

func alwaysAlive() bool { return true }

// unixServer serves h on a unix socket, the way an app does under
// motel, and returns the socket path.
func unixServer(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

// testSocketRef is a socketRef for an app-side socket at path, pinned
// into a directory of its own the way appDirs.socketRef does.
func testSocketRef(t *testing.T, path string) *socketRef {
	t.Helper()
	return newSocketRef(path, filepath.Join(t.TempDir(), "pinned.sock"), "")
}

func statusHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

func TestProberPassesAfterSoak(t *testing.T) {
	sock := unixServer(t, statusHandler(http.StatusOK))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	start := clk.Now()
	if err := p.waitHealthy(context.Background(), testSocketRef(t, sock), alwaysAlive, testHealthConfig()); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	if elapsed := clk.Now().Sub(start); elapsed < 3*time.Second {
		t.Fatalf("returned before the soak elapsed: %v", elapsed)
	}
}

// soak 0 removes the soak, not the check: the gate passes on the
// first healthy probe, with no time on the clock, and an unhealthy
// probe before it is still waited out.
func TestProberSoakZeroPassesOnTheFirstHealthyProbe(t *testing.T) {
	var probes atomic.Int32
	sock := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	hc := testHealthConfig()
	hc.soak = 0
	start := clk.Now()
	if err := p.waitHealthy(context.Background(), testSocketRef(t, sock), alwaysAlive, hc); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	if n := probes.Load(); n != 2 {
		t.Fatalf("probes = %d, want 2: the failing one, then the first healthy one", n)
	}
	if elapsed := clk.Now().Sub(start); elapsed != hc.interval {
		t.Fatalf("elapsed %v, want one interval (%v): nothing after the healthy probe", elapsed, hc.interval)
	}
}

func TestProberFlappingResetsSoak(t *testing.T) {
	// Healthy, healthy, unhealthy, then healthy forever: the flap must
	// restart the soak window.
	var n atomic.Int64
	sock := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	start := clk.Now()
	if err := p.waitHealthy(context.Background(), testSocketRef(t, sock), alwaysAlive, testHealthConfig()); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	// Flap at probe 3 (t=2s) means soak restarts at t=3s and completes
	// no earlier than t=6s.
	if elapsed := clk.Now().Sub(start); elapsed < 6*time.Second {
		t.Fatalf("soak was not reset by the flap: %v", elapsed)
	}
}

func TestProberDeadlineExceeded(t *testing.T) {
	sock := unixServer(t, statusHandler(http.StatusServiceUnavailable))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	hc := testHealthConfig()
	hc.deadline = 5 * time.Second
	err := p.waitHealthy(context.Background(), testSocketRef(t, sock), alwaysAlive, hc)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want deadline error, got %v", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("deadline error should carry the last probe failure, got %v", err)
	}
}

func TestProberDeadlineDuringSoakNamesTheHealthyRun(t *testing.T) {
	// The app answers 503 for its first ten probes, then 200: healthy
	// from t=10s, so a 20s soak would finish at 30s — after the 25s
	// deadline. The verdict must say the app was healthy but not for
	// long enough, and must not carry the stale 503 as the cause.
	var n atomic.Int32
	sock := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) <= 10 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	hc := testHealthConfig()
	hc.soak = 20 * time.Second
	hc.deadline = 25 * time.Second
	err := p.waitHealthy(context.Background(), testSocketRef(t, sock), alwaysAlive, hc)
	if err == nil || !strings.Contains(err.Error(), "healthy for 15s of the 20s soak when the 25s deadline expired") {
		t.Fatalf("want a healthy-but-short verdict, got %v", err)
	}
	if strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "%!w") {
		t.Fatalf("verdict must not blame the pre-healthy probe or format a nil error: %v", err)
	}
	var pe *probeError
	if errors.As(err, &pe) {
		t.Fatalf("a probe from before the healthy run must not be the failure detail: %v", pe)
	}
}

func TestProberSoakMetOnTheDeadlineTickPasses(t *testing.T) {
	// Healthy from the first probe, soak 26s, deadline 27s (a config
	// Validate accepts), ticks every 5s: the tick at t=30s is the
	// first to satisfy the soak and also the first past the deadline,
	// and the soak must win. Ticks are interval-granular, so judging
	// the deadline first would refuse a config Validate accepts.
	sock := unixServer(t, statusHandler(http.StatusOK))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	hc := testHealthConfig()
	hc.interval = 5 * time.Second
	hc.soak = 26 * time.Second
	hc.deadline = 27 * time.Second
	if err := p.waitHealthy(context.Background(), testSocketRef(t, sock), alwaysAlive, hc); err != nil {
		t.Fatalf("a soak completed on the deadline tick must pass, got %v", err)
	}
}

func TestProberFailedProbePastTheDeadlineNamesTheHealthyRun(t *testing.T) {
	// 1s ticks, 503 for the first two probes, healthy from t=2 with a
	// 5s soak (due at t=7) and a 6s deadline: the tick at t=7 is past
	// the deadline with the soak due, so it probes — and that probe
	// answers 503. The verdict must name the 5s healthy run AND the
	// probe that failed, not read as an instance that was never well.
	var n atomic.Int32
	sock := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if k := n.Add(1); k <= 2 || k >= 8 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	hc := testHealthConfig()
	hc.soak = 5 * time.Second
	hc.deadline = 6 * time.Second
	err := p.waitHealthy(context.Background(), testSocketRef(t, sock), alwaysAlive, hc)
	if err == nil || !strings.Contains(err.Error(), "healthy for 5s of the 5s soak") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("want the healthy run and the failed probe named, got %v", err)
	}
	var pe *probeError
	if !errors.As(err, &pe) || pe.status != http.StatusServiceUnavailable {
		t.Fatalf("the failed probe must be the failure detail, got %v", err)
	}
	if got := n.Load(); got != 8 {
		t.Fatalf("probes issued = %d, want 8 (t=0..7)", got)
	}
}

func TestProberDoesNotProbePastADeadlineItCannotMeet(t *testing.T) {
	// Never healthy, deadline 5s, 1s ticks: probes at t=0..5, then the
	// tick at t=6 is past the deadline with no soak to complete and
	// must return the verdict without spending another probe.
	var n atomic.Int32
	sock := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	hc := testHealthConfig()
	hc.deadline = 5 * time.Second
	err := p.waitHealthy(context.Background(), testSocketRef(t, sock), alwaysAlive, hc)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want deadline error, got %v", err)
	}
	if got := n.Load(); got != 6 {
		t.Fatalf("probes issued = %d, want 6 (t=0..5): none past a deadline the soak cannot meet", got)
	}
}

func TestProberProcessDeath(t *testing.T) {
	sock := unixServer(t, statusHandler(http.StatusOK))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	var calls atomic.Int64
	dieAfterTwo := func() bool { return calls.Add(1) <= 2 }
	err := p.waitHealthy(context.Background(), testSocketRef(t, sock), dieAfterTwo, testHealthConfig())
	if err == nil || !strings.Contains(err.Error(), "process exited") {
		t.Fatalf("want process-exit error, got %v", err)
	}
}

func TestProberHealthOffSoaksOnLiveness(t *testing.T) {
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	hc := testHealthConfig()
	hc.path = "" // health_path off
	start := clk.Now()
	nobody := filepath.Join(t.TempDir(), "none.sock")
	if err := p.waitHealthy(context.Background(), testSocketRef(t, nobody), alwaysAlive, hc); err != nil {
		t.Fatalf("liveness-only soak failed: %v", err)
	}
	if elapsed := clk.Now().Sub(start); elapsed < 3*time.Second {
		t.Fatalf("soak not observed: %v", elapsed)
	}
}

func TestProberContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	err := p.waitHealthy(ctx, testSocketRef(t, filepath.Join(t.TempDir(), "none.sock")), alwaysAlive, testHealthConfig())
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("want cancellation error, got %v", err)
	}
}

// A socket nobody listens on reads as unhealthy, with the error
// carried in the verdict — the shape of an app that has not bound
// yet, or has exited.
func TestProbeOnceNobodyListening(t *testing.T) {
	p := &httpProber{clock: newFakeClock()}
	err := p.probeOnce(context.Background(), testSocketRef(t, filepath.Join(t.TempDir(), "none.sock")), "/health", time.Second)
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("want an error naming the missing socket, got %v", err)
	}
}

// Promise: motel dials the socket by its pinned name under proxy/,
// never by the app-writable one. A symlink planted under the app's
// name is refused (it is not a socket); once pinned, replacing the
// name changes nothing about where motel connects; and a retired
// instance is never re-pinned, its pinned name removed.
func TestSocketRefPinsTheSocketAndRefusesASymlink(t *testing.T) {
	victim := unixServer(t, statusHandler(http.StatusTeapot)) // "the admin socket"
	dir := t.TempDir()
	planted := filepath.Join(dir, "app.sock")
	must(t, os.Symlink(victim, planted))
	p := &httpProber{clock: newFakeClock()}
	err := p.probeOnce(context.Background(), testSocketRef(t, planted), "/health", time.Second)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlink under the socket name must be refused, got %v", err)
	}

	// The real thing: bound, then pinned by the first dial.
	must(t, os.Remove(planted))
	ln, err := net.Listen("unix", planted)
	must(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	srv := httptest.NewUnstartedServer(statusHandler(http.StatusOK))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	bound, err := os.Lstat(planted)
	must(t, err)
	sock := testSocketRef(t, planted)
	must(t, p.probeOnce(context.Background(), sock, "/health", time.Second))

	// Now the app swaps the name for a symlink to the victim: the pin
	// still reaches the real socket, and the victim never sees a probe.
	must(t, os.Remove(planted))
	must(t, os.Symlink(victim, planted))
	if err := p.probeOnce(context.Background(), sock, "/health", time.Second); err != nil {
		t.Fatalf("the pinned socket must still be reached after the name is swapped: %v", err)
	}
	addr, err := sock.dial()
	if err != nil || addr != sock.pinned || addr == planted {
		t.Fatalf("dial = %q, %v; want the pinned name %s", addr, err, sock.pinned)
	}
	if pinStat, err := os.Lstat(addr); err != nil || pinStat.Mode()&os.ModeSocket == 0 || !os.SameFile(pinStat, bound) {
		t.Fatalf("the pinned name must be a hard link to the bound socket's inode: %v", err)
	}

	sock.retire()
	if _, err := sock.dial(); err == nil {
		t.Fatal("a retired socketRef must not re-pin")
	}
	if _, err := os.Lstat(addr); !os.IsNotExist(err) {
		t.Fatalf("retire must remove the pinned name: %v", err)
	}
}

// The probe requests the health path over the socket, whatever host
// the URL names, and never follows a redirect: a 3xx is "not 2xx".
func TestProbeOnceUsesThePathAndRefusesRedirects(t *testing.T) {
	var gotPath atomic.Value
	sock := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		if r.URL.Path == "/health" {
			http.Redirect(w, r, "/ok", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	p := &httpProber{clock: newFakeClock()}
	err := p.probeOnce(context.Background(), testSocketRef(t, sock), "/health", time.Second)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("a redirect must read as unhealthy, got %v", err)
	}
	var pe *probeError
	if !errors.As(err, &pe) || pe.status != 302 || pe.location != "/ok" {
		t.Fatalf("the probe error must carry the status and the redirect target: %+v", pe)
	}
	if got := gotPath.Load(); got != "/health" {
		t.Fatalf("probed %v, want /health (the redirect target must never be fetched)", got)
	}
}

// The process dies after answering a probe: the verdict is the exit,
// and the answer it gave is kept alongside for the deploy's detail.
func TestProberProcessDeathKeepsLastProbe(t *testing.T) {
	sock := unixServer(t, statusHandler(http.StatusInternalServerError))
	clk := newFakeClock()
	p := &httpProber{clock: clk}
	var calls atomic.Int64
	dieAfterTwo := func() bool { return calls.Add(1) <= 2 }
	err := p.waitHealthy(context.Background(), testSocketRef(t, sock), dieAfterTwo, testHealthConfig())
	if !errors.Is(err, errProcessExited) {
		t.Fatalf("want the process-exit sentinel, got %v", err)
	}
	var pe *probeError
	if !errors.As(err, &pe) || pe.status != http.StatusInternalServerError {
		t.Fatalf("the last answered probe should ride along: %v", err)
	}
}

// A failing probe's body is read only up to probeBodyRead bytes, less
// the token that bound cut in two: the excerpt a deploy's detail keeps
// is a diagnosis, not a page, and is cut to probeBodyExcerpt once the
// known values are replaced (TestFailureDetailCutsAfterTheKnownValues).
func TestProbeOnceKeepsABoundedBodyExcerpt(t *testing.T) {
	big := strings.Repeat("word1 ", probeBodyRead)
	cut := big[:probeBodyRead]
	cut = cut[:strings.LastIndexByte(cut, ' ')+1] + "…"
	exact := big[:probeBodyRead]
	for _, tc := range []struct{ name, body, want string }{
		{"a longer body loses the cut token", big, cut},
		{"a body of exactly the read is whole", exact, exact},
	} {
		sock := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(tc.body))
		}))
		p := &httpProber{clock: newFakeClock()}
		err := p.probeOnce(context.Background(), testSocketRef(t, sock), "/health", time.Second)
		var pe *probeError
		if !errors.As(err, &pe) || pe.status != http.StatusInternalServerError {
			t.Fatalf("%s: want a probe error with the status, got %v", tc.name, err)
		}
		if pe.body != tc.want {
			t.Fatalf("%s: body read is %d bytes ending %q, want %d ending %q", tc.name, len(pe.body), pe.body[len(pe.body)-12:], len(tc.want), tc.want[len(tc.want)-12:])
		}
	}
}
