package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// skinTone is a thumbs-up with a skin tone, and as the door draws it:
// the base, then the modifier written as an escape, ten cells.
const (
	skinTone      = "\U0001f44d\U0001f3fd"
	skinToneDrawn = "\U0001f44d" + `\U0001f3fd`
)

// nextCells is how many cells the first character of rest takes, by
// the test's own count of the characters these tests use, not by the
// screens' measure: one for ASCII, ten for the escape of a skin tone,
// none for a combining mark or a zero-width space, two for any other.
func nextCells(rest string) int {
	if strings.HasPrefix(rest, `\U0001f3fd`) {
		return 10
	}
	r, _ := utf8.DecodeRuneInString(rest)
	switch {
	case r < 0x80:
		return 1
	case unicode.Is(unicode.Mn, r), r == 0x200b:
		return 0
	}
	return 2
}

// unseen is s without its zero-width spaces, which the screen may
// keep in a cell of their own or with the character before.
func unseen(s string) string { return strings.ReplaceAll(s, "\u200b", "") }

// fitsOrCut holds a drawn row to want, the line as drawn whole: drawn
// whole when it fits in room cells, else cut with "…" as its last
// visible cell, within room, and not early: the next character of want
// would not have fitted before the mark. It says whether it was cut.
func fitsOrCut(t *testing.T, name string, r drawnRow, want string, room int) bool {
	t.Helper()
	text, w := unseen(r.text), unseen(want)
	if text == w && r.end <= room {
		return false
	}
	shown, ok := strings.CutSuffix(text, "…")
	rest, prefix := strings.CutPrefix(w, shown)
	switch {
	case !ok || r.last != "…" || !prefix || rest == "":
		t.Errorf("%s: drawn neither whole nor cut with … last: %q (want %q)", name, r.text, want)
	case r.end > room:
		t.Errorf("%s: drawn %d cells wide, past %d: %q", name, r.end, room, r.text)
	case r.end+nextCells(rest) <= room:
		t.Errorf("%s: cut early, at %d of %d cells, before %q: %q", name, r.end, room, []rune(rest)[0], r.text)
	}
	return true
}

// rowStarting is the first drawn row that begins with prefix.
func rowStarting(h *harness, prefix string) (drawnRow, bool) {
	for _, r := range screenRows(h) {
		if strings.HasPrefix(r.text, prefix) {
			return r, true
		}
	}
	return drawnRow{}, false
}

// TestFieldsCutInCells: a box's value on an app's screen and in a
// record's head, of wide characters, of skin tones the door writes as
// escapes, of combining marks and of zero-width spaces, is drawn whole
// when it fits in the window's cells and is otherwise cut at its edge,
// "…" last, at 60 and 80 columns, as the simulation screen's own cells
// show it.
func TestFieldsCutInCells(t *testing.T) {
	values := []struct {
		name, v, drawn string
		cut60, cut80   bool
	}{
		{"wide", strings.Repeat("鍵", 60), strings.Repeat("鍵", 60), true, true},
		{"skin tones", strings.Repeat(skinTone, 10), strings.Repeat(skinToneDrawn, 10), true, true},
		{"combining", strings.Repeat("e\u0301", 60), strings.Repeat("e\u0301", 60), true, false},
		{"zero-width", strings.Repeat("a\u200b", 50), strings.Repeat("a\u200b", 50), true, false},
	}
	for _, c := range values {
		for _, w := range []int{60, 80} {
			v, _ := json.Marshal(c.v)
			h := start(t, w, 30, func(f *demo.Fake) {
				f.Body("status/wiki.json", `{"app": "wiki", "phase": "idle", "running": false, "available_versions": [], "deploys": [],
					"last_deploy": {"version": "e9a0b3f2c118", "status": "failed", "deployed_by": "github:example/wiki@refs/heads/main", "started_at": "@-782s", "finished_at": "@-12m", "phase": "starting", "error": `+string(v)+`}}`)
			})
			want := map[int]bool{60: c.cut60, 80: c.cut80}[w]
			for _, view := range []model.View{app("wiki"), record("wiki", "e9a0b3f2c118")} {
				h.open(view)
				name := c.name + " at " + map[int]string{60: "60", 80: "80"}[w] + " on " + map[model.Screen]string{model.ScreenApp: "the app", model.ScreenRecord: "the record"}[view.Screen]
				r, ok := rowStarting(h, "error ")
				if !ok {
					t.Errorf("%s: no error row:\n%s", name, h.screen())
					continue
				}
				if cut := fitsOrCut(t, name, r, "error       "+c.drawn, w); cut != want {
					t.Errorf("%s: cut %v, want %v: %q", name, cut, want, r.text)
				}
			}
		}
	}
}

// leftPart is row y's text up to and with its last "…" before the
// right side, read from the screen's own cells, as a drawn row: its
// end is the cell after that "…".
func leftPart(h *harness, y int) (drawnRow, bool) {
	var r drawnRow
	found := false
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() {
		defer close(done)
		w, _ := h.scr.Size()
		var b strings.Builder
		for x := 0; x < w; {
			s, _, cw := h.scr.Get(x, y)
			b.WriteString(s)
			if s == "…" {
				r, found = drawnRow{text: b.String(), last: s, end: x + max(cw, 1)}, true
			}
			x += max(cw, 1)
		}
	})
	<-done
	return r, found
}

// TestTitleAndCrumbInCells: the title bar's and the crumb's right side
// is drawn whole, ending at the window's edge, and their left side is
// cut before it in cells, "…" last and not early, at 60 and 80
// columns, with wide characters and skin tones on the left. The real
// client refuses such a name and such an app (both are outside its
// grammars, which are ASCII); the fake does not hold them, so it is
// the screens' measure that is tested.
func TestTitleAndCrumbInCells(t *testing.T) {
	for _, c := range []struct{ name, v, drawn string }{
		{"wide", strings.Repeat("鍵", 30), strings.Repeat("鍵", 30)},
		{"skin tones", strings.Repeat(skinTone, 5), strings.Repeat(skinToneDrawn, 5)},
	} {
		for _, w := range []int{60, 80} {
			name := c.name + " at " + map[int]string{60: "60", 80: "80"}[w]
			h := start(t, w, 30, func(f *demo.Fake) {
				f.Body("whoami.json", `{"name":"`+c.v+`","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`)
				f.Body("apps.json", `{"apps": [{"name": "`+c.v+`"}, {"name": "blog"}]}`)
				f.Body("status/"+c.v+".json", `{"app": "x", "phase": "idle", "running": false, "available_versions": [], "deploys": []}`)
			})
			h.open(app(c.v))
			for _, row := range []struct {
				what, left, right string
				y                 int
			}{
				{"the title", "motel · deploy.example.com · v0.7.0 │ " + c.drawn + " · viewer of the box", "live · 0s ago", 0},
				{"the crumb", "apps › " + c.drawn, "as of 14:12:40 UTC · 0s ago", 1},
			} {
				r := screenRows(h)[row.y]
				if !strings.HasSuffix(r.text, " "+row.right) || r.end != w {
					t.Errorf("%s: %s's right side is not whole at the edge (it ends at %d): %q", name, row.what, r.end, r.text)
					continue
				}
				l, ok := leftPart(h, row.y)
				if !ok {
					t.Errorf("%s: %s's left side is not cut: %q", name, row.what, r.text)
					continue
				}
				fitsOrCut(t, name+", "+row.what, l, row.left, w-len([]rune(row.right))-1)
			}
		}
	}
}
