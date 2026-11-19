package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/tui"
)

// TestClosedTerminalHelper is not a test of its own:
// TestClosedTerminalWithTheScreenUp runs the test binary with it on a
// pty, under TestClosedTerminalLeader, and closes the pty's master once
// the screen is up. What it says
// goes to a file, since the terminal is gone by then.
func TestClosedTerminalHelper(t *testing.T) {
	out := os.Getenv("MOTEL_TEST_CLOSED_HELPER")
	if out == "" {
		t.Skip("run by TestClosedTerminalWithTheScreenUp")
	}
	fb := newFakeBox(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), nil, Env{Terminal: true,
		Getenv: func(k string) string { return env[k] }, Stdout: &stdout, Stderr: &stderr, Dir: t.TempDir(), Home: home,
		Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: fb.srv.Client().Transport, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test",
	})
	if err := os.WriteFile(out, []byte("exit "+strconv.Itoa(code)+", stderr "+strconv.Quote(stderr.String())), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClosedTerminalLeader is not a test of its own either: it leads
// the pty's session for TestClosedTerminalWithTheScreenUp, ignoring
// SIGHUP, and runs the helper as its child, in its process group, on
// the terminal, so the kernel's hang-up, sent to the session's leader
// alone, never reaches the TUI. A shell would not do: bash runs the
// last command of -c in its own place, which makes the TUI the leader.
// (The TUI cannot ignore SIGHUP itself: it listens for it.)
func TestClosedTerminalLeader(t *testing.T) {
	if os.Getenv("MOTEL_TEST_CLOSED_LEADER") == "" {
		t.Skip("run by TestClosedTerminalWithTheScreenUp")
	}
	signal.Ignore(syscall.SIGHUP)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestClosedTerminalHelper$", "-test.count=1", "-test.timeout=40s")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// How it ended is in the helper's file.
	_ = cmd.Run()
}

// ioctl is the ioctl(2) of fd with a pointer argument.
func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// TestClosedTerminalWithTheScreenUp: the pty's master closed while the
// screen is up, as a terminal emulator does when its window is closed,
// ends motel as an interrupt does: exit 130, nothing said. The TUI
// runs under a leader of the pty's session that ignores SIGHUP
// (TestClosedTerminalLeader), so the kernel's SIGHUP (sent to the
// session's leader alone) never reaches it: what it sees is the read of
// the terminal ending, which a closed window's often wins the race
// against the signal (the trial: 18 of 20 ended with exit 2 and "the
// screen could not start: EOF").
func TestClosedTerminalWithTheScreenUp(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("NOT RUN: no /dev/ptmx (" + err.Error() + "); a closed terminal is not held here")
	}
	defer func() { _ = master.Close() }()
	var unlock int32
	var n uint32
	if err := ioctl(master.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Fatal(err)
	}
	if err := ioctl(master.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		t.Fatal(err)
	}
	ws := [4]uint16{24, 80, 0, 0}
	if err := ioctl(master.Fd(), syscall.TIOCSWINSZ, unsafe.Pointer(&ws)); err != nil {
		t.Fatal(err)
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(t.TempDir(), "result")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestClosedTerminalLeader$", "-test.count=1", "-test.timeout=50s")
	cmd.Env = append(os.Environ(), "MOTEL_TEST_CLOSED_LEADER=1", "MOTEL_TEST_CLOSED_HELPER="+result, "TERM=xterm")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	// The leader leads a process group of its own (a session's leader
	// does), the helper in it: the group goes when ctx ends, and when
	// the test does, so nothing either started outlives the test.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	_ = pts.Close()
	var mu sync.Mutex
	var tty bytes.Buffer
	up := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		var once sync.Once
		for {
			k, err := master.Read(buf)
			mu.Lock()
			tty.Write(buf[:k])
			// The alternate screen, which the TUI's screen opens on, and a
			// frame drawn on it.
			if s := tty.String(); strings.Contains(s, "\x1b[?1049h") && strings.Contains(s, "apps") {
				once.Do(func() { close(up) })
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-up:
	case <-ctx.Done():
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the screen did not open:\n%q", tty.String())
	}
	if err := master.Close(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err != nil && !errors.As(err, new(*exec.ExitError)) {
		t.Fatal(err)
	}
	got, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("the helper said nothing: %v", err)
	}
	if want := `exit ` + strconv.Itoa(client.ExitInterrupted) + `, stderr ""`; string(got) != want {
		t.Errorf("after the terminal closed with the screen up: %s; want %s", got, want)
	}
}

// TestRunFailureSays: a terminal that went away while the screen held
// it is an interrupt, said with nothing; any other end of the screen's
// run is a screen that could not start, exit 2.
func TestRunFailureSays(t *testing.T) {
	if err := runFailure(tui.ErrClosed); !errors.Is(err, errInterrupted) {
		t.Errorf("a closed terminal is said as %v", err)
	}
	var f *client.Failure
	if err := runFailure(errors.New("open /dev/tty: no such device or address")); !errors.As(err, &f) || f.Exit != client.ExitUsage || !strings.HasPrefix(f.Message, "the screen could not start: ") {
		t.Errorf("a screen that could not start is said as %v", err)
	}
}

// TestCanDrawHelper is not a test of its own: TestCanDrawNoTerminal
// runs the test binary with it in a session of its own, which has no
// controlling terminal, and reads what CanDraw said.
func TestCanDrawHelper(t *testing.T) {
	if os.Getenv("MOTEL_TEST_CANDRAW_HELPER") == "" {
		t.Skip("run by TestCanDrawNoTerminal")
	}
	_, _ = os.Stdout.WriteString("CANDRAW " + strconv.Quote(fmt.Sprint(CanDraw())) + "\n")
}

// TestCanDrawNoTerminal: with a TERM and a locale the screen can draw
// with, and no controlling terminal (a CI job, cron, ssh without -t),
// CanDraw fails at /dev/tty: for whatever reason opening it fails, ENXIO
// where the node exists, ENOENT in a sandbox that has none.
func TestCanDrawNoTerminal(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestCanDrawHelper$", "-test.count=1", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "MOTEL_TEST_CANDRAW_HELPER=1", "TERM=xterm-256color", "LC_ALL=", "LC_CTYPE=", "LANG=C.UTF-8")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, _ := cmd.CombinedOutput()
	_, line, found := strings.Cut(string(out), "CANDRAW ")
	line, _, _ = strings.Cut(line, "\n")
	got, err := strconv.Unquote(line)
	if want := "open /dev/tty: "; !found || err != nil || !strings.HasPrefix(got, want) || got == want {
		t.Errorf("CanDraw with no controlling terminal said %s, want a refusal beginning %q\n%s", line, want, out)
	}
}
