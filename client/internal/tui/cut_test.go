package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// The details above an app's deploys and a record's log tail get at
// most half the window's height (#566): when they have more lines, the
// last row that shows says how many are not shown and that y shows the
// whole answer, so no line is dropped unsaid.

const moreSaid = " more lines: y shows the answer; then P prints it whole"

// detailRows is the rows of the screen's details: from under the crumb
// to the row above the line that opens the table or the tail.
func detailRows(t *testing.T, h *harness, opens string) []string {
	t.Helper()
	var out []string
	for _, r := range screenRows(h)[2:] {
		if strings.HasPrefix(r.text, opens) {
			return out
		}
		out = append(out, r.text)
	}
	t.Fatalf("no row opens with %q:\n%s", opens, h.screen())
	return nil
}

// cutSaid is the number the last row says is not shown, or -1 when that
// row is no such notice.
func cutSaid(rows []string) int {
	if len(rows) == 0 {
		return -1
	}
	n, ok := strings.CutPrefix(strings.TrimRight(rows[len(rows)-1], " "), "… ")
	if !ok {
		return -1
	}
	n, ok = strings.CutSuffix(n, moreSaid)
	if !ok {
		return -1
	}
	i, err := strconv.Atoi(n)
	if err != nil {
		return -1
	}
	return i
}

// TestDetailsCutSaid: an app's details and a record's, cut to half the
// window, end in a row that counts the lines not shown, the row's own
// among them; where every line fits, no row says so.
func TestDetailsCutSaid(t *testing.T) {
	for _, c := range []struct {
		name  string
		view  model.View
		opens string
	}{
		{"an app", app("api"), "─ DEPLOY"},
		{"a record", record("hostile", "v7"), "── log tail"},
	} {
		// At 100x60 the details have their 30 rows, more than either has.
		h := start(t, 100, 60, nil)
		h.open(c.view)
		all := detailRows(t, h, c.opens)
		if n := cutSaid(all); n != -1 {
			t.Fatalf("%s at 100x60: the details fit and still say %d more lines:\n%s", c.name, n, h.screen())
		}
		if len(all) <= 8 {
			t.Fatalf("%s: %d lines of details, which 60x16's 8 rows hold; the test needs more", c.name, len(all))
		}
		h = start(t, 60, 16, nil)
		h.open(c.view)
		rows := detailRows(t, h, c.opens)
		if len(rows) != 8 {
			t.Errorf("%s at 60x16: the details take %d rows, want half the window, 8:\n%s", c.name, len(rows), h.screen())
		}
		if got, want := cutSaid(rows), len(all)-len(rows)+1; got != want {
			t.Errorf("%s at 60x16: the last row of the details says %d more lines, want %d:\n%s", c.name, got, want, h.screen())
		}
	}
}

// TestFailedSentenceCutSaid: an app's "the last read failed" sentence
// that fills half the window (a 409 quoting 200 bidi overrides, laid on
// rows at 60x16) leaves its last row to count what is not shown: the
// sentence's end and the fields under it. 60x80 gives the details 40
// rows, room for the sentence laid at 60 columns and every field.
func TestFailedSentenceCutSaid(t *testing.T) {
	long := strings.Repeat("\u202e", 200)
	fail := &model.Failure{Kind: model.FailThis, Message: demo.Host + ` answered 409; the box says: "` + long + `"`}
	details := func(hgt int) []string {
		h := start(t, 60, hgt, nil)
		h.open(app("blog"))
		h.fake.Set("blog", fail)
		h.now = h.now.Add(model.MeAlways)
		h.press(tcell.KeyCtrlR)
		rows := detailRows(t, h, "─ DEPLOY")
		if len(rows) == 0 || !strings.HasPrefix(rows[0], "the last read failed: ") {
			t.Fatalf("60x%d: the details do not open with the failed sentence:\n%s", hgt, h.screen())
		}
		return rows
	}
	all := details(80)
	if n := cutSaid(all); n != -1 || len(all) <= 8 {
		t.Fatalf("60x80: %d rows of details, the last saying %d more lines; want more than 8, none cut", len(all), n)
	}
	rows := details(16)
	if len(rows) != 8 {
		t.Errorf("60x16: the details take %d rows, want 8", len(rows))
	}
	if got, want := cutSaid(rows), len(all)-len(rows)+1; got != want {
		t.Errorf("60x16: the last row says %d more lines, want %d", got, want)
	}
}

// TestCutSaidInColour: in colour the notice is drawn as the overlay's
// own cut is, in the warning's colour, not as one more field.
func TestCutSaidInColour(t *testing.T) {
	h := start(t, 60, 16, nil)
	h.on(func() { h.u.Colour = true })
	h.open(app("api"))
	rows := screenRows(h)
	for y, r := range rows {
		if !strings.HasSuffix(r.text, moreSaid) {
			continue
		}
		var fg tcell.Color
		h.on(func() { _, st, _ := h.scr.Get(0, y); fg, _, _ = st.Decompose() })
		if fg != tcell.ColorYellow {
			t.Errorf("the notice %q is drawn in %v, want yellow", r.text, fg)
		}
		return
	}
	t.Fatalf("no row says what is cut:\n%s", h.screen())
}
