package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/door"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// overlayRows is what the open overlay draws inside its border, row by
// row, a wide character once and trailing spaces cut.
func overlayRows(h *harness) (rows []string) {
	var cells []tcell.SimCell
	var w, hgt int
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() { cells, w, hgt = h.scr.(tcell.SimulationScreen).GetContents(); close(done) })
	<-done
	in := false
	for y := range hgt {
		switch string(cells[y*w].Runes) {
		case "╔":
			in = true
			continue
		case "╚":
			return rows
		}
		if !in {
			continue
		}
		var b strings.Builder
		for x := 1; x < w-1; x++ {
			r := string(cells[y*w+x].Runes)
			b.WriteString(r)
			if door.Width(termtext.From(r)) == 2 {
				x++
			}
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return rows
}

// drawnRow is a row inside the overlay's border as the screen holds
// it: its text (a wide character once, trailing spaces cut), the
// cells from the border to the end of its last drawn cell, and that
// cell's text.
type drawnRow struct {
	text, last string
	end        int
}

// cellRow is row y of scr from column x0 to x1 (not included), read
// from the screen's own cells: the screen says how wide each is.
func cellRow(scr tcell.Screen, y, x0, x1 int) drawnRow {
	var r drawnRow
	var b strings.Builder
	for x := x0; x < x1; {
		s, _, cw := scr.Get(x, y)
		b.WriteString(s)
		if s != " " && s != "" {
			r.last, r.end = s, x-x0+max(cw, 1)
		}
		x += max(cw, 1)
	}
	r.text = strings.TrimRight(b.String(), " ")
	return r
}

// screenRows is every row of the screen, read from its cells.
func screenRows(h *harness) (rows []drawnRow) {
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() {
		defer close(done)
		w, hgt := h.scr.Size()
		for y := range hgt {
			rows = append(rows, cellRow(h.scr, y, 0, w))
		}
	})
	<-done
	return rows
}

// overlayCells is the open overlay's rows inside its border, read from
// the screen's own cells.
func overlayCells(h *harness) (rows []drawnRow) {
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() {
		defer close(done)
		w, hgt := h.scr.Size()
		in := false
		for y := range hgt {
			switch s, _, _ := h.scr.Get(0, y); s {
			case "╔":
				in = true
				continue
			case "╚":
				return
			}
			if in {
				rows = append(rows, cellRow(h.scr, y, 1, w-1))
			}
		}
	})
	<-done
	return rows
}

// joinRows is the rows' text, joined by sep.
func joinRows(rows []drawnRow, sep string) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.text)
	}
	return strings.Join(out, sep)
}

// cutAtEdge says whether a row that ends in … after prefix, a part of
// the command line want as drawn, is cut at the edge of a window room
// cells wide: the row ends at the edge, or the next of want's
// characters is too wide to stand there before the mark. Widths are
// the test's own, of the only characters its command lines hold: one
// cell for ASCII, two for any other character, and ten for a skin tone
// the door writes as an escape (shownUp).
func cutAtEdge(r drawnRow, prefix, want string, room int) bool {
	rest, ok := strings.CutPrefix(want, prefix)
	if !ok || rest == "" || r.last != "…" || r.end > room {
		return false
	}
	next := 2
	switch {
	case strings.HasPrefix(rest, `\U0001f3fd`):
		next = 10
	case rest[0] < 0x80:
		next = 1
	}
	return r.end == room || r.end+next > room
}

// wantCommands is every command line the overlay over v shows, whole:
// the command, and in the command overlay its --json form and, on the
// apps screen, a row's.
func wantCommands(h *harness, v model.View, json bool) (out []string) {
	h.on(func() {
		c, _ := h.s.Command(v)
		out = []string{c}
		if json {
			return
		}
		args, _ := h.s.CommandArgs(v)
		i := slices.Index(args, "--")
		if i < 0 {
			i = len(args)
		}
		out = append(out, model.ShellLine(slices.Insert(slices.Clone(args), i, "--json")))
		if v.Screen == model.ScreenApps {
			a, _ := h.s.CommandArgs(model.View{Screen: model.ScreenApp, App: "x"})
			out = append(out, model.ShellLine(a[:len(a)-1])+" <app>")
		}
	})
	return out
}

// checkOverlay holds the open overlay over v to the rules, and says
// whether a command line was cut: each command line is drawn whole, or
// cut at the edge (where one more character and the mark would not
// fit) with "…" there; the overlay says a line is cut, and
// how to get it whole, exactly when one is; its own words are never cut
// (each sentence is found whole, its rows read on one line); and in y,
// it says a line of the answer is wider than the window exactly when
// one is (by door.Width, the screens' measure).
func checkOverlay(t *testing.T, h *harness, v model.View, json bool, w int) (cut, wide bool) {
	t.Helper()
	rows := overlayRows(h)
	want := wantCommands(h, v, json)
	seen := map[string]bool{}
	for _, r := range rows {
		s, ok := strings.CutPrefix(r, "  motel ")
		if !ok {
			continue
		}
		s = "motel " + s
		if slices.Contains(want, s) {
			seen[s] = true
			continue
		}
		head, marked := strings.CutSuffix(s, "…")
		found := false
		for _, c := range want {
			if !marked || !strings.HasPrefix(c, head) || c == head {
				continue
			}
			// Cut at the edge: one more character and the mark would
			// not fit.
			next := []rune(c[len(head):])[0]
			if door.Width(termtext.From("  "+head+string(next)+"…")) > w-2 {
				seen[c], cut, found = true, true, true
			}
		}
		if !found {
			t.Errorf("a command line drawn neither whole nor cut with a mark at the edge: %q", r)
		}
	}
	for _, c := range want {
		if !seen[c] {
			t.Errorf("the command %q is not drawn, whole or marked as cut", c)
		}
	}
	text := strings.Join(rows, " ")
	says := func(sentence string) bool { return strings.Contains(text, sentence) }
	if says("do not copy it") != cut {
		t.Errorf("a command line cut: %v; the overlay says not to copy it: %v\n%s", cut, !cut, strings.Join(rows, "\n"))
	}
	var sentences []string
	if !json {
		sentences = []string{"the same read from a shell:"}
		if v.Screen == model.ScreenApps {
			sentences = append(sentences, "This screen is that list, and a row for each app: each row is")
		}
		if cut {
			sentences = append(sentences, "a line above is cut at the window's edge, marked …: do not copy it.")
			if !says("P quits and prints the first command whole below your prompt") && !says("c then P on an app's own screen prints a row's command whole.") {
				t.Errorf("a command line is cut, and the overlay does not say what prints it whole:\n%s", strings.Join(rows, "\n"))
			}
		} else {
			sentences = append(sentences, "select it with the mouse, or press P to quit and print it below your prompt")
		}
	} else {
		sentences = []string{"the box's answer to", "indented here; its strings as received, made safe to show;",
			"lines over 2000 characters, and answers over 2000 lines, are cut for display",
			"P prints it whole, made safe on a terminal"}
		if cut {
			sentences = append(sentences, "the line above is cut at the window's edge, marked …: do not copy it. c then P prints it whole.")
		}
		var body []byte
		h.on(func() { body, _, _ = h.s.Body(v) })
		lines, _, _ := model.JSONLines(body)
		for _, l := range lines {
			wide = wide || door.Width(l) > w-2
		}
		const wideSays = "a line below is wider than the window and cut at its edge, unmarked: P prints every line whole"
		if says(wideSays) != wide {
			t.Errorf("a line of the answer wider than the window: %v; the overlay says so: %v", wide, !wide)
		}
	}
	for _, s := range sentences {
		if !says(s) {
			t.Errorf("the overlay's words %q are not drawn whole:\n%s", s, strings.Join(rows, "\n"))
		}
	}
	return cut, wide
}

// TestOverlayFits opens both overlays on every screen that reads the
// box, at 60, 80 and 100 columns, with the default key, a --key path,
// and a --key path of wide characters (two cells each), and holds each
// to checkOverlay. Where the second review found a fault, it also says
// what is cut: the y header's command at 80 with --key; the command at
// 80 with a wide key path on the box screen; nothing on the apps
// screen at 60.
func TestOverlayFits(t *testing.T) {
	keys := map[string]string{"default": "", "key": "~/.ssh/id_ed25519", "wide": "~/鍵鍵鍵鍵鍵鍵鍵鍵鍵鍵/k"}
	views := []struct {
		name string
		v    model.View
	}{
		{"apps", model.View{Screen: model.ScreenApps}},
		{"app", app("blog")},
		{"record", record("blog", "7c1e55d0a913")},
		{"me", model.View{Screen: model.ScreenMe}},
		{"box", model.View{Screen: model.ScreenBox}},
	}
	wantCut := map[string]bool{"80/key/record/y": true, "80/wide/box/c": true, "60/default/apps/c": false}
	for _, w := range []int{60, 80, 100} {
		for _, k := range []string{"default", "key", "wide"} {
			t.Run(strconv.Itoa(w)+"/"+k, func(t *testing.T) {
				h := start(t, w, 24, nil)
				if keys[k] != "" {
					h.on(func() { h.s.Key.Source, h.s.Key.Shown = "--key", keys[k] })
				}
				for _, c := range views {
					if c.v.Screen != model.ScreenApps {
						h.open(c.v)
					}
					for _, mode := range []string{"c", "y"} {
						name := strconv.Itoa(w) + "/" + k + "/" + c.name + "/" + mode
						h.press(mode)
						cut, _ := checkOverlay(t, h, c.v, mode == "y", w)
						if want, ok := wantCut[name]; ok && cut != want {
							t.Errorf("%s: a command line cut: %v, want %v", name, cut, want)
						}
					}
					h.press(tcell.KeyEscape)
				}
			})
		}
	}
}

// TestOverlayLongAnswerLine: a 110-character string in who am I's
// answer, at 80 columns, is wider than the window, and the y overlay
// says so and that P prints it whole.
func TestOverlayLongAnswerLine(t *testing.T) {
	h := start(t, 80, 24, func(f *demo.Fake) {
		f.Body("whoami.json", `{"name":"alice@example.com","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m","note":"`+strings.Repeat("a", 107)+`END"}`)
	})
	v := model.View{Screen: model.ScreenMe}
	h.open(v)
	h.press("y")
	if _, wide := checkOverlay(t, h, v, true, 80); !wide {
		t.Error("the 110-character line is not wider than the window: the test proves nothing")
	}
}

// TestOverlayScreens draws both overlays over a record, with --key, at
// 60, 80 and 100 columns, and the apps screen's command overlay at the
// same widths.
func TestOverlayScreens(t *testing.T) {
	for _, sz := range [][2]int{{60, 24}, {80, 24}, {100, 30}} {
		size := strconv.Itoa(sz[0]) + "x" + strconv.Itoa(sz[1])
		t.Run(size, func(t *testing.T) {
			h := start(t, sz[0], sz[1], nil)
			h.on(func() { h.s.Key.Source, h.s.Key.Shown = "--key", "~/.ssh/id_ed25519" })
			h.press("c")
			golden(t, "overlay-apps-command-"+size, h.screen())
			h.press(tcell.KeyEscape)
			h.open(record("blog", "7c1e55d0a913"))
			h.press("c")
			golden(t, "overlay-record-command-"+size, h.screen())
			h.press("y")
			golden(t, "overlay-record-json-"+size, h.screen())
		})
	}
}

// TestOverlayResize: the y overlay, open over a record with --key
// while the window is made narrower, wider, and narrower again, is laid
// out for the window's width each time: its command cut at the edge at
// 80 and 60 columns, and whole at 100.
func TestOverlayResize(t *testing.T) {
	h := start(t, 80, 24, nil)
	h.on(func() { h.s.Key.Source, h.s.Key.Shown = "--key", "~/.ssh/id_ed25519" })
	v := record("blog", "7c1e55d0a913")
	h.open(v)
	h.press("y")
	for _, w := range []int{80, 60, 100, 80} {
		h.on(func() { h.scr.(*sizedSim).SetSize(w, 24) })
		if cut, _ := checkOverlay(t, h, v, true, w); cut != (w < 100) {
			t.Errorf("at %d columns: the command cut: %v, want %v", w, cut, w < 100)
		}
	}
}
