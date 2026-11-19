package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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
// pty, and closes the pty's master once the screen is up. What it says
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
	code := Main(context.Background(), []string{"tui"}, Env{
		Getenv: func(k string) string { return env[k] }, Stdout: &stdout, Stderr: &stderr, Dir: t.TempDir(), Home: home,
		Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: fb.srv.Client().Transport, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test",
	})
	if err := os.WriteFile(out, []byte("exit "+strconv.Itoa(code)+", stderr "+strconv.Quote(stderr.String())), 0o600); err != nil {
		t.Fatal(err)
	}
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
// ends motel tui as an interrupt does: exit 130, nothing said. The TUI
// runs under a shell that leads the pty's session and ignores SIGHUP,
// so the kernel's SIGHUP (sent to the session's leader alone) never
// reaches it: what it sees is the read of the terminal ending, which a
// closed window's often wins the race against the signal (the trial: 18
// of 20 ended with exit 2 and "the screen could not start: EOF").
func TestClosedTerminalWithTheScreenUp(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("NOT RUN: no sh to lead the pty's session; a closed terminal is not held here")
	}
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("NOT RUN: no /dev/ptmx (" + err.Error() + "); a closed terminal is not held here")
	}
	defer master.Close()
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
	cmd := exec.CommandContext(ctx, sh, "-c", `trap "" HUP; "$0" -test.run='^TestClosedTerminalHelper$' -test.count=1`, self)
	cmd.Env = append(os.Environ(), "MOTEL_TEST_CLOSED_HELPER="+result, "TERM=xterm")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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
	if err := cmd.Wait(); err != nil && !errors.As(err, new(*exec.ExitError)) {
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
