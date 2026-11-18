package tui

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// thumbsUp is a thumbs-up with a skin tone: uniseg v0.4.7 reports a
// mandatory line break after it, and the door writes its modifier as
// an escape (shownUp).
const thumbsUp, shownUp = "\U0001f44d\U0001f3fd", "\U0001f44d\\U0001f3fd"

// TestRecordOneRow: a record whose deployed_by carries a thumbs-up
// with a skin tone, then words laid out as a row of the record's head,
// is drawn at 80 columns with the by row whole on one row, the
// modifier escaped; no row begins with the box's words, and the head's
// own rows are all still drawn.
func TestRecordOneRow(t *testing.T) {
	const by = "ci" + thumbsUp + "status      : succeeded, checked by the client"
	h := start(t, 80, 24, func(f *demo.Fake) {
		b, err := os.ReadFile("demo/fixtures/deploys/blog/7c1e55d0a913.json")
		if err != nil {
			t.Fatal(err)
		}
		f.Body("deploys/blog/7c1e55d0a913.json", strings.Replace(string(b), "github:example/blog@refs/heads/main", by, 1))
	})
	h.open(record("blog", "7c1e55d0a913"))
	rows := screenRows(h)
	want := "by          ci" + shownUp + "status      : succeeded, checked by the client"
	found := false
	for _, r := range rows {
		found = found || r.text == want
		if strings.HasPrefix(r.text, "status") {
			t.Errorf("a row begins with the box's words: %q", r.text)
		}
	}
	if !found {
		t.Errorf("no row is %q:\n%s", want, joinRows(rows, "\n"))
	}
	for _, head := range []string{"serves now  ", "phases      ", "artifact    "} {
		n := 0
		for _, r := range rows {
			if strings.HasPrefix(r.text, head) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d rows begin %q, want 1:\n%s", n, head, joinRows(rows, "\n"))
		}
	}
}

// TestKeyOverlayOneRow: with --key ~/.ssh/👍🏽/id_ed25519.pub, the c
// overlay on the apps screen and on a record, at 100 and at 60
// columns, draws each command line on one row: whole, the modifier
// escaped, or cut at the window's edge with … there, and then the
// overlay says not to copy it.
func TestKeyOverlayOneRow(t *testing.T) {
	const key = "~/.ssh/" + thumbsUp + "/id_ed25519.pub"
	for _, w := range []int{100, 60} {
		t.Run(strconv.Itoa(w), func(t *testing.T) {
			h := start(t, w, 30, nil)
			h.on(func() { h.s.Key.Source, h.s.Key.Shown = "--key", key })
			for _, v := range []model.View{{Screen: model.ScreenApps}, record("blog", "7c1e55d0a913")} {
				if v.Screen != model.ScreenApps {
					h.open(v)
				}
				h.press("c")
				var want []string
				for _, c := range wantCommands(h, v, false) {
					want = append(want, "  "+strings.ReplaceAll(c, thumbsUp, shownUp))
				}
				rows := overlayCells(h)
				n, cut := 0, false
				for _, r := range rows {
					if !strings.HasPrefix(r.text, "  motel ") {
						if strings.Contains(r.text, "id_ed25519") {
							t.Errorf("%d/%v: a row of a command line's own: %q", w, v.Screen, r.text)
						}
						continue
					}
					whole, edge := false, false
					for _, c := range want {
						whole = whole || r.text == c
						edge = edge || cutAtEdge(r, strings.TrimSuffix(r.text, "…"), c, w-2)
					}
					switch {
					case whole:
					case edge:
						cut = true
					default:
						t.Errorf("%d/%v: a command line drawn neither whole nor cut at the edge with …: %q", w, v.Screen, r.text)
					}
					n++
				}
				if n != len(want) {
					t.Errorf("%d/%v: %d rows of command lines, want %d", w, v.Screen, n, len(want))
				}
				if w == 60 && !cut {
					t.Errorf("60/%v: no command line cut: the case proves nothing of the cut", v.Screen)
				}
				if says := strings.Contains(joinRows(rows, " "), "do not copy it"); says != cut {
					t.Errorf("%d/%v: a command line cut: %v; the overlay says not to copy it: %v\n%s", w, v.Screen, cut, says, joinRows(rows, "\n"))
				}
				h.press(tcell.KeyEscape)
			}
		})
	}
}
