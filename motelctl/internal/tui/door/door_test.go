package door

import (
	"sort"
	"strings"
	"testing"

	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/hostile"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

// drawOnce draws p once on a real terminfo screen over a fake tty, and
// returns the bytes tcell wrote.
func drawOnce(t *testing.T, p tview.Primitive, focus tview.Primitive) []byte {
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
	done := make(chan struct{})
	go func() { defer close(done); _ = app.Run() }()
	app.QueueUpdateDraw(func() {})
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

// widgets puts text in every kind of widget the TUI hands text to, the
// way it hands it: through boxtext.go.
func widgets(ts []termtext.Text) map[string]func() (tview.Primitive, tview.Primitive) {
	return map[string]func() (tview.Primitive, tview.Primitive){
		"table cell": func() (tview.Primitive, tview.Primitive) {
			tb := tview.NewTable()
			for i, x := range ts {
				tb.SetCell(i, 0, Cell(x))
			}
			return tb, nil
		},
		"text view, lines": func() (tview.Primitive, tview.Primitive) {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
			var ls []model.Line
			for _, x := range ts {
				ls = append(ls, model.Line{{T: x, C: model.Bad}, {T: x}})
			}
			SetText(tv, Lines(ls, true))
			return tv, nil
		},
		"text view, box lines": func() (tview.Primitive, tview.Primitive) {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
			SetText(tv, Texts(ts))
			return tv, nil
		},
		"framed title": func() (tview.Primitive, tview.Primitive) {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
			tv.SetBorder(true)
			tv.SetTitle(" " + Inert(ts[0]).s + " ")
			return tv, nil
		},
		"input field": func() (tview.Primitive, tview.Primitive) {
			f := tview.NewInputField()
			SetLabel(f, ts[0].String())
			SetInput(f, ts[len(ts)-1].String())
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
		ts = append(ts, termtext.From(n+" <"+hostile.Payloads[n]+">"))
	}
	for name, mk := range widgets(ts) {
		p, f := mk()
		if found := hostile.Scan(drawOnce(t, p, f)); len(found) > 0 {
			t.Errorf("%s: attacks reached the terminal: %v", name, found)
		}
	}
	for _, n := range names() {
		one := []termtext.Text{termtext.From(hostile.Payloads[n])}
		for name, mk := range widgets(one) {
			p, f := mk()
			if found := hostile.Scan(drawOnce(t, p, f)); len(found) > 0 {
				t.Errorf("%s alone in a %s: %v", n, name, found)
			}
		}
	}
	// The gap: the same URL tag after termtext and tview.Escape is a link.
	tag := termtext.From(hostile.Payloads["urltag"]).String()
	if found := hostile.Scan(drawOnce(t, tview.NewTable().SetCell(0, 0, tview.NewTableCell(tview.Escape(tag))), nil)); !strings.Contains(strings.Join(found, ","), "osc8") {
		t.Errorf("tview.Escape no longer lets a URL tag through (%v): the door's reason has changed; re-read tview's tag parser", found)
	}
	// And its inverse: inert text shows the tag as text.
	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetText(Inert(termtext.From("[red]x[-]")).s)
	if got := tv.GetText(true); !strings.Contains(got, "red") {
		t.Errorf("inert text lost its tag's words: %q", got)
	}
}
