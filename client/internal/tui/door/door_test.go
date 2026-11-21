package door

import (
	"bytes"
	"math"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/hostile"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// drawOnce draws p once on a real terminfo screen over a fake tty, and
// returns the bytes tcell wrote. then, when not nil, runs on the event
// loop before the frame is drawn, once the focus has settled: what a
// focus change would undo (the completion list a blur drops) is done
// there.
func drawOnce(t *testing.T, p tview.Primitive, focus tview.Primitive, then func()) []byte {
	t.Helper()
	tty := hostile.NewTTY()
	scr, err := hostile.Screen(tty)
	if err != nil {
		t.Fatal(err)
	}
	app := tview.NewApplication().SetScreen(scr).SetRoot(p, true)
	if focus != nil {
		app.SetFocus(focus)
	}
	if then == nil {
		then = func() {}
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = app.Run() }()
	app.QueueUpdateDraw(then)
	app.QueueUpdate(app.Stop) // from inside the loop, as the TUI stops
	<-done
	return tty.Bytes()
}

func names() []string {
	var n []string
	for k := range hostile.Payloads {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// widget is a widget holding text, the one to focus (nil: none), and
// what to do on the event loop once the focus has settled (nil:
// nothing).
type widget struct {
	p, focus tview.Primitive
	then     func()
}

// widgets puts text in every kind of widget the TUI hands text to, the
// way it hands it: through boxtext.go.
func widgets(ts []termtext.Text) map[string]func() widget {
	return map[string]func() widget{
		"table cell": func() widget {
			tb := tview.NewTable()
			for i, x := range ts {
				tb.SetCell(i, 0, Cell(x))
			}
			return widget{p: tb}
		},
		"text view, lines": func() widget {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
			var ls []model.Line
			for _, x := range ts {
				ls = append(ls, model.Line{{T: x, C: model.Bad}, {T: x}})
			}
			SetText(tv, Lines(ls, true))
			return widget{p: tv}
		},
		"text view, box lines": func() widget {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
			SetText(tv, Texts(ts))
			return widget{p: tv}
		},
		"framed title": func() widget {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
			tv.SetBorder(true)
			tv.SetTitle(" " + Inert(ts[0]).s + " ")
			return widget{p: tv}
		},
		"completions": func() widget {
			// As the screens use it (ui.go), and its list opened once the
			// field has the focus: a focus set after would blur the field,
			// and the blur closes the list, so nothing of it is drawn.
			f := tview.NewInputField().SetAutocompleteUseTags(false)
			var es []string
			for _, x := range ts {
				es = append(es, x.String())
			}
			SetCompletions(f, func(string) []string { return es })
			return widget{p: f, focus: f, then: func() { f.SetText("x"); f.Autocomplete() }}
		},
		"framed title, the TUI's own": func() widget {
			tv := tview.NewTextView()
			tv.SetBorder(true)
			SetTitle(tv.Box, ts[0].String())
			return widget{p: tv}
		},
		"input field": func() widget {
			f := tview.NewInputField()
			SetLabel(f, ts[0].String())
			SetInput(f, ts[len(ts)-1].String())
			return widget{p: f, focus: f}
		},
	}
}

// TestEscapeHolds puts every hostile string, through termtext and the
// one door, in every widget kind used, on a real terminfo screen: no
// attack reaches the bytes, and each widget draws the text it holds
// (the URL tag's words are on screen, as text), so the check is of what
// was drawn. It also holds the gap the door exists for: tview.Escape
// lets a URL tag through as a live OSC 8 link.
func TestEscapeHolds(t *testing.T) {
	var ts []termtext.Text
	for _, n := range names() {
		ts = append(ts, termtext.From(n+" <"+hostile.Payloads[n]+">"))
	}
	for name, mk := range widgets(ts) {
		w := mk()
		if found := hostile.Scan(drawOnce(t, w.p, w.focus, w.then)); len(found) > 0 {
			t.Errorf("%s: attacks reached the terminal: %v", name, found)
		}
	}
	for _, n := range names() {
		one := []termtext.Text{termtext.From(hostile.Payloads[n])}
		for name, mk := range widgets(one) {
			w := mk()
			out := drawOnce(t, w.p, w.focus, w.then)
			if found := hostile.Scan(out); len(found) > 0 {
				t.Errorf("%s alone in a %s: %v", n, name, found)
			}
			if n == "urltag" && !bytes.Contains(out, []byte("evil.example/tag")) {
				t.Errorf("a %s draws nothing of the text it holds: no attack in it is drawn either", name)
			}
		}
	}
	// The gap: the same URL tag after termtext and tview.Escape is a link.
	tag := termtext.From(hostile.Payloads["urltag"]).String()
	if found := hostile.Scan(drawOnce(t, tview.NewTable().SetCell(0, 0, tview.NewTableCell(tview.Escape(tag))), nil, nil)); !strings.Contains(strings.Join(found, ","), "osc8") {
		t.Errorf("tview.Escape no longer lets a URL tag through (%v): the door's reason has changed; re-read tview's tag parser", found)
	}
	// And its inverse: inert text shows the tag as text.
	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetText(Inert(termtext.From("[red]x[-]")).s)
	if got := tv.GetText(true); !strings.Contains(got, "red") {
		t.Errorf("inert text lost its tag's words: %q", got)
	}
}

// TestWholeAllocatesOnce: whole's allocations do not grow with the
// text, ASCII or not: a box can send a line of many thousands of
// characters, and each frame draws it. Seen to fail: with each cluster
// added to its segment one at a time (out[k] += c): 30,009 allocations
// for 30,000 zero-width spaces, or for 30,000 of e with an acute
// accent, against 1,009 for 1,000.
func TestWholeAllocatesOnce(t *testing.T) {
	for _, c := range []string{"\u200b", "\u00e9"} {
		allocs := func(n int) float64 {
			segs := []string{"level ", strings.Repeat(c, n) + "x", " ok"}
			return testing.AllocsPerRun(5, func() { whole(segs) })
		}
		if few, many := allocs(1000), allocs(30000); many > few {
			t.Errorf("whole of %q: %v allocations for 1,000, %v for 30,000", c, few, many)
		}
	}
}

// TestUnbreakWritesOnce: the bytes unbreak allocates grow with the
// text, not with its square: a box's line of many emoji with skin
// tones has an escape after each one, and each frame draws it. Seen to
// fail: with each escape written in by a copy of its segment
// (s[:i] + escape(r) + s[i+n:]): 35,516,600 bytes for 1,000 thumbs-up
// and skin tones, 559,185,616 for 4,000.
// The least of three runs, after one to warm up, is taken: another
// goroutine's allocations only add to a run.
func TestUnbreakWritesOnce(t *testing.T) {
	alloc := func(n int) uint64 {
		segs := []string{"level ", strings.Repeat("\U0001f44d\U0001f3fd", n)}
		want := strings.Repeat("\U0001f44d\\U0001f3fd", n)
		if got := unbreak(slices.Clone(segs)); got[0] != "level " || got[1] != want {
			t.Fatalf("unbreak of %d thumbs-up and skin tones: %q", n, got)
		}
		least := uint64(math.MaxUint64)
		for range 3 {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			unbreak(slices.Clone(segs))
			runtime.ReadMemStats(&after)
			least = min(least, after.TotalAlloc-before.TotalAlloc)
		}
		return least
	}
	if few, many := alloc(1000), alloc(4000); many > 8*few {
		t.Errorf("unbreak allocates %d bytes for 1,000 escapes, %d for 4,000", few, many)
	} else {
		t.Logf("%d bytes for 1,000 escapes, %d for 4,000", few, many)
	}
}

// TestRowbreakKeepsATest: rowbreak keeps a test file of its own, which
// keeps make test's time: without one, go test -cover given several
// packages at once instrumented it into the door's test binary (its
// package comment has the numbers).
func TestRowbreakKeepsATest(t *testing.T) {
	if _, err := os.Stat("rowbreak/rowbreak_test.go"); err != nil {
		t.Errorf("rowbreak has no test file of its own: %v", err)
	}
}
