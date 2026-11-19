package tui

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/door"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// asDrawn is line as the overlay's kind of widget draws it alone, in a
// window wide enough for all of it, read back from the screen's cells
// (a wide character once, trailing spaces cut).
func asDrawn(line string) string {
	const w = 4096
	scr := tcell.NewSimulationScreen("UTF-8")
	_ = scr.Init()
	scr.SetSize(w, 1)
	tv := textView()
	door.SetText(tv, door.Lines([]model.Line{{{T: termtext.From(line)}}}, false))
	tv.SetRect(0, 0, w, 1)
	tv.Draw(scr)
	return cellRow(scr, 0, 0, w).text
}

// checkAnyOverlay holds the open overlay over v, read from the
// screen's cells, to client/README.md's rules for any command line,
// what it holds drawn as it is or not: each command line is drawn on
// one row, in its order, whole (as the line alone is drawn, asDrawn)
// when it fits the room inside the border, and otherwise cut, its last
// visible cell the mark … (a Prepend character before the mark takes it
// into its own cell), within the room and less than an escape's ten
// cells from its edge; the overlay says a line is cut exactly when one
// is, says one is not drawn as it runs exactly when one is not (its
// cells alone, asDrawn, are not the line), offers selecting the lines
// exactly when neither, and says what prints each such line whole (P
// the first and the --json one, c then P an apps row's); and its own
// words are drawn whole (each sentence is found on its rows other than
// the command lines, read as one line). A record whose version the grammar does not take has no
// command, and says so.
func checkAnyOverlay(t *testing.T, h *harness, v model.View, json bool, w int) {
	t.Helper()
	rows := overlayCells(h)
	// The overlay's own words are looked for on its other rows only: a
	// key path may spell any of them inside a command line.
	var prose []drawnRow
	for _, r := range rows {
		if !strings.HasPrefix(r.text, "  motel ") {
			prose = append(prose, r)
		}
	}
	text := joinRows(prose, " ")
	says := func(s string) bool { return strings.Contains(text, s) }
	var ok bool
	h.on(func() { _, ok = h.s.Command(v) })
	if !ok {
		if v.Screen != model.ScreenRecord || model.ValidVersion(v.Version) || !says("the box's filter hid this deploy's version: no motel command can ask for its record") {
			t.Errorf("no command over %v, and the overlay says:\n%s", v, joinRows(rows, "\n"))
		}
		return
	}
	want := wantCommands(h, v, json)
	room := w - 2
	cut, other := make([]bool, len(want)), make([]bool, len(want))
	k := 0
	for _, r := range rows {
		if !strings.HasPrefix(r.text, "  motel ") {
			continue
		}
		if k == len(want) {
			t.Errorf("a command line more than the %d drawn: %q", len(want), r.text)
			continue
		}
		line := "  " + want[k]
		whole := asDrawn(line)
		other[k] = whole != line
		fits := door.Width(termtext.From(line)) <= room
		switch {
		case fits && r.text != whole:
			t.Errorf("%q fits the %d cells and is drawn %q, not whole", want[k], room, r.text)
		case !fits && (!strings.HasSuffix(r.last, "…") || r.end > room || r.end < room-9):
			t.Errorf("%q is wider than the %d cells and is drawn %q, its last cell %q ending at %d: not cut at the edge with the mark", want[k], room, r.text, r.last, r.end)
		}
		cut[k] = !fits
		k++
	}
	if k < len(want) {
		t.Errorf("%d of the %d command lines drawn:\n%s", k, len(want), joinRows(rows, "\n"))
	}
	bad := func(i int) bool { return i < len(want) && (cut[i] || other[i]) }
	var sentences []struct {
		says string
		want bool
	}
	add := func(s string, want bool) {
		sentences = append(sentences, struct {
			says string
			want bool
		}{s, want})
	}
	if !json {
		add("the same read from a shell:", true)
		add("a line above is cut at the window's edge, marked …: do not copy it.", slices.Contains(cut, true))
		add("a line above is not drawn as it runs (a character written as an escape, or one not drawn at all): do not copy it.", slices.Contains(other, true))
		add("select it with the mouse, or press P to quit and print it below your prompt", !slices.Contains(cut, true) && !slices.Contains(other, true))
		add("P quits and prints the first command whole below your prompt", bad(0) || bad(1))
		add("the second is it with --json among its flags", bad(1))
		add("c then P on an app's own screen prints a row's command whole.", bad(2))
		if v.Screen == model.ScreenApps {
			add("This screen is that list, and a row for each app: each row is", true)
		}
	} else {
		add("the box's answer to", true)
		add("the line above is cut at the window's edge, marked …: do not copy it.", cut[0])
		add("the line above is not drawn as it runs (a character written as an escape, or one not drawn at all): do not copy it.", other[0])
		add("c then P prints it whole.", cut[0] || other[0])
		add("indented here; its strings as received, made safe to show;", true)
		add("lines over 2000 characters, and answers over 2000 lines, are cut for display", true)
		add("P prints it whole, made safe on a terminal", true)
	}
	for _, s := range sentences {
		if says(s.says) != s.want {
			t.Errorf("the overlay says %q: %v, want %v (cut %v, not as it runs %v)\n%s", s.says, !s.want, s.want, cut, other, joinRows(rows, "\n"))
		}
	}
}

// FuzzOverlay: both overlays over each screen that reads the box, with
// any key path (as the client shows one, made safe by termtext), app,
// version, box and key source the client takes (client.ParseApp's name,
// client.CheckVersion's version, client.ResolveBox's --box), in a
// window from 60 to 200 columns, held to checkAnyOverlay. The fixtures
// answer for the app and version as for blog's. Seen to fail: with
// cutCells writing no mark (a key path holding a skin tone, at 60
// columns: the line's last cell is the thumbs-up); with the command
// overlay never saying a line is not drawn as it runs (the same key);
// and with the overlay's own words cut at the edge in place of laid on
// rows (the sentence that a line is cut is not found whole).
func FuzzOverlay(f *testing.F) {
	for _, k := range slices.Sorted(maps.Keys(asRunsKeys)) {
		f.Add(asRunsKeys[k].path, "blog", "7c1e55d0a913", "deploy.example.com", byte(0), byte(0))
		f.Add(asRunsKeys[k].path, "blog", "7c1e55d0a913", "deploy.example.com", byte(140), byte(0x03|0x08))
	}
	for _, s := range []struct {
		key, app, version, box string
		width, pick            byte
	}{
		{"~/.ssh/id_ed25519.pub", "blog", "7c1e55d0a913", "deploy.example.com", 20, 0x02 | 0x08},
		{"~/鍵鍵鍵鍵鍵鍵鍵鍵鍵鍵/k", "my-app-2", "-v1", "deploy.example.com:8443", 0, 0x04},
		{"~/.ssh/" + thumbsUp + "/id_ed25519", "blog", "v1", "deploy.example.com", 40, 0x01 | 0x08 | 0x10},
		{"~/.ssh/id_ed25519\u0d4e", "blog", "v1", "deploy.example.com", 15, 0x03},
		{"~/.ssh/\u1100\u1161\u0903\U0001f1e6\U0001f1e8", "blog", "v1", "deploy.example.com", 13, 0x00 | 0x20},
		{"/tmp/a;rm -rf x", "blog", strings.Repeat("v", 65), "deploy.example.com", 0, 0x02 | 0x08},
		{"--", "blog", "v1", "deploy.example.com", 140, 0x02},
		// Key paths that spell the overlay's own words (Copilot's on #591).
		{"c then P prints it whole.", "blog", "v1", "deploy.example.com", 140, 0x0c},
		{"a line above is not drawn as it runs (a character written as an escape, or one not drawn at all): do not copy it.", "blog", "v1", "deploy.example.com", 140, 0x03},
	} {
		f.Add(s.key, s.app, s.version, s.box, s.width, s.pick)
	}
	status, err := os.ReadFile("demo/fixtures/status/blog.json")
	if err != nil {
		f.Fatal(err)
	}
	record, err := os.ReadFile("demo/fixtures/deploys/blog/7c1e55d0a913.json")
	if err != nil {
		f.Fatal(err)
	}
	whoami, err := os.ReadFile("demo/fixtures/whoami.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, key, app, version, box string, width, pick byte) {
		if len(key) > 256 {
			return // a key path's length adds nothing past the widest window
		}
		if a, err := client.ParseApp(app); err != nil || a.Name != app || client.CheckVersion(version) != nil {
			return
		}
		if box == "" {
			return // ResolveBox would look further: MOTEL_BOX, MOTEL_URL, the repository
		}
		b, err := client.ResolveBox(context.Background(), client.BoxLookup{Flag: box})
		if err != nil {
			return
		}
		w := 60 + int(width)%141
		v := []model.View{{Screen: model.ScreenApps}, {Screen: model.ScreenApp, App: app}, {Screen: model.ScreenRecord, App: app, Version: version},
			{Screen: model.ScreenMe}, {Screen: model.ScreenBox}}[int(pick&7)%5]
		json := pick&0x08 != 0
		src := []string{"--key", "MOTEL_KEY", "the default file"}[int(pick>>4)%3]
		h := start(t, w, 50, func(f *demo.Fake) {
			f.Body("status/"+app+".json", string(status))
			f.Body("deploys/"+app+"/"+version+".json", string(record))
			f.Body("whoami.json", strings.Replace(string(whoami), `"deploy.example.com"`, `"`+b.Host+`"`, 1))
		}, func(s *model.Session) {
			s.Host, s.Addr = b.Host, b.Addr()
			s.Key.Source, s.Key.Shown = src, termtext.From(key).String()
		})
		if v.Screen != model.ScreenApps {
			h.open(v)
		}
		mode := "c"
		if json {
			mode = "y"
		}
		h.press(mode)
		checkAnyOverlay(t, h, v, json, w)
	})
}
