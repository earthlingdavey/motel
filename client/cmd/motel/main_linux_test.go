package main

import (
	"os"
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

// TestPtyIsATerminal: a pty's terminal end is a terminal, as a
// terminal emulator's is, so motel alone opens the screen there.
func TestPtyIsATerminal(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("NOT RUN: no /dev/ptmx (" + err.Error() + "); a pty is not held here")
	}
	defer func() { _ = master.Close() }()
	var unlock int32
	var n uint32
	for _, c := range []struct {
		req uintptr
		arg unsafe.Pointer
	}{{syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)}, {syscall.TIOCGPTN, unsafe.Pointer(&n)}} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), c.req, uintptr(c.arg)); e != 0 {
			t.Fatal(e)
		}
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pts.Close() }()
	if !terminal(pts) {
		t.Error("a pty is not taken for a terminal")
	}
}
