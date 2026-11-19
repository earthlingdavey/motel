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

// The fixes for what the trial of the TUI against a real box found.

// TestDeployInFlight: while another version is deployed, an app's
// screen says the serving version is running, and the deploy in flight
// on a line of its own: its version, phase, when it started and by
// whom. The box's answer had phase "preparing" beside the running
// version, and the screen said "preparing <running version>".
func TestDeployInFlight(t *testing.T) {
	for _, sz := range [][2]int{{60, 16}, {80, 24}} {
		name := "app-deploying-" + strconv.Itoa(sz[0]) + "x" + strconv.Itoa(sz[1])
		t.Run(name, func(t *testing.T) {
			h := start(t, sz[0], sz[1], nil)
			h.open(app("api"))
			rows := screenRows(h)
			var state, deploying string
			for _, r := range rows {
				switch {
				case strings.HasPrefix(r.text, "state "):
					state = r.text
				case strings.HasPrefix(r.text, "deploying "):
					deploying = r.text
				}
			}
			if want := "state       running 3b8812aa0c4f"; !strings.HasPrefix(state, want) {
				t.Errorf("the state is drawn %q, want it to begin %q", state, want)
			}
			if want := "deploying   4c2d9e7b1a05 soaking started 25s ago (14:12"; !strings.HasPrefix(deploying, want) {
				t.Errorf("the deploy in flight is drawn %q, want it to begin %q", deploying, want)
			}
			golden(t, name, h.screen())
		})
	}
}

// TestMetricsSayOnlyWhatTheClientKnows: the metrics line says the
// newest point's time and its age, and nothing of how far behind the
// store runs, which the box does not say (the trial's ran 5 s behind;
// the screen said 30).
func TestMetricsSayOnlyWhatTheClientKnows(t *testing.T) {
	h := start(t, 60, 16, nil)
	h.open(app("blog"))
	var got string
	for _, r := range screenRows(h) {
		if strings.HasPrefix(r.text, "metrics ") {
			got = r.text
		}
	}
	if want := "metrics     over 1h, newest point 14:12:10 UTC (30s ago)"; got != want {
		t.Errorf("the metrics line is drawn %q, want %q", got, want)
	}
}

// TestLongTailLineCut: a log tail's line wider than the window is cut
// at its edge, "…" its last cell, as every other cut line is (the trial
// saw a 300-character line stop at column 80 unmarked); with w on, it
// wraps whole onto the rows below.
func TestLongTailLineCut(t *testing.T) {
	const rec = "deploys/blog/e9a0b3f2c118.json"
	b, err := os.ReadFile("demo/fixtures/" + rec)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 300)
	body := strings.Replace(string(b), `"> blog@1.4.0 start"`, `"`+long+`"`, 1)
	if body == string(b) {
		t.Fatal("the fixture's first log line moved")
	}
	for _, sz := range [][2]int{{60, 16}, {80, 24}} {
		h := start(t, sz[0], sz[1], func(f *demo.Fake) { f.Body(rec, body) })
		h.open(record("blog", "e9a0b3f2c118"))
		h.on(func() { h.u.tail.ScrollToBeginning() })
		var row drawnRow
		for _, r := range screenRows(h) {
			if strings.HasPrefix(r.text, "xxxx") {
				row = r
				break
			}
		}
		if want := strings.Repeat("x", sz[0]-1) + "…"; row.text != want {
			t.Errorf("%dx%d: the long line is drawn %q, want %q", sz[0], sz[1], row.text, want)
		}
		h.press("w")
		n := 0
		for _, r := range screenRows(h) {
			if strings.HasPrefix(r.text, "xxxx") {
				n++
				if strings.HasSuffix(r.text, "…") {
					t.Errorf("%dx%d: with w on, a row of the long line is cut: %q", sz[0], sz[1], r.text)
				}
			}
		}
		if n < 2 {
			t.Errorf("%dx%d: with w on, the long line is on %d rows", sz[0], sz[1], n)
		}
	}
}

// TestScrollingFootersSaySo: who am I and the box screen, which can be
// longer than the window and scroll by ↑ ↓, say so in their footers, as
// the stop screens and help do (the trial: who am I at 60x16 scrolled,
// and its footer named no key that scrolls it).
func TestScrollingFootersSaySo(t *testing.T) {
	for name, v := range map[string]model.View{"who am I": {Screen: model.ScreenMe}, "the box": {Screen: model.ScreenBox}} {
		h := start(t, 60, 16, nil)
		h.open(v)
		rows := screenRows(h)
		if got := rows[len(rows)-1].text; !strings.HasPrefix(got, "↑↓ scroll  ") {
			t.Errorf("%s: the footer is drawn %q, naming no key that scrolls it", name, got)
		}
	}
	// Who am I is longer than the window there, and ↓ scrolls it.
	h := start(t, 60, 16, nil)
	h.open(model.View{Screen: model.ScreenMe})
	before := screenRows(h)[2].text
	h.press(tcell.KeyDown)
	if after := screenRows(h)[2].text; after == before {
		t.Errorf("↓ on who am I at 60x16 did not scroll it: its first row stays %q", after)
	}
}

// TestPromptFooters: while the : line or the / filter is open, the
// footer says the keys that work there, not the screen's own, which
// would be typed into the line (the trial: "enter open  / filter  : go
// to …" under an open filter).
func TestPromptFooters(t *testing.T) {
	for _, c := range []struct {
		keys []any
		want string
	}{
		{[]any{"/"}, "enter keep the filter  esc clear it and close"},
		{[]any{"/", "bl"}, "enter keep the filter  esc clear it and close"},
		{[]any{":"}, "enter run it  tab complete  esc close"},
		{[]any{":", "st"}, "enter run it  tab complete  esc close"},
		{[]any{"/", tcell.KeyEscape}, "enter open  / filter  : go to  y json  c cmd  ? more  q quit"},
		{[]any{":", tcell.KeyEscape}, "enter open  / filter  : go to  y json  c cmd  ? more  q quit"},
	} {
		h := start(t, 60, 16, nil)
		h.press(c.keys...)
		rows := screenRows(h)
		if got := rows[len(rows)-1].text; got != c.want {
			t.Errorf("after %q: the footer is drawn %q, want %q", c.keys, got, c.want)
		}
	}
}

// TestHelpHangs: at 60 columns, an entry of the keys overlay wider than
// the window goes on under its own words, not at the left edge where it
// reads as a new entry (the trial: "else the app" under "d …"), read
// from the screen's cells as ↓ scrolls it, over the apps screen and
// over a stop screen.
func TestHelpHangs(t *testing.T) {
	signedOut := func(f *demo.Fake) {
		f.Set("whoami", &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com did not accept your key"})
	}
	for name, setup := range map[string]func(*demo.Fake){"over the apps": nil, "over a stop": signedOut} {
		h := start(t, 60, 16, setup)
		h.press("?")
		text := stopHelp()
		if h.top().Screen != model.ScreenStopHelp {
			h.on(func() { text = h.u.help() })
		}
		page := map[int]string{}
		for range 60 {
			var off int
			h.on(func() { off, _ = h.u.page.GetScrollOffset() })
			rows := screenRows(h)
			for i, r := range rows[2 : len(rows)-1] {
				page[off+i] = r.text
			}
			h.press(tcell.KeyDown)
		}
		rows := make([]string, len(page))
		for i := range rows {
			rows[i] = page[i]
		}
		long := 0
		for _, l := range strings.Split(text, "\n") {
			// Where the entry's words begin: after its name and the run of
			// two spaces or more that ends it.
			entry := strings.TrimLeft(l, " ")
			i := strings.Index(entry, "  ")
			if i <= 0 || len([]rune(l)) <= 60 {
				continue
			}
			col := i + len(l) - len(entry) + len(entry[i:]) - len(strings.TrimLeft(entry[i:], " "))
			long++
			words := strings.Fields(l[col:])
			at := -1
			for i, r := range rows {
				if strings.HasPrefix(r, l[:col]+words[0]) {
					at = i
					break
				}
			}
			if at < 0 {
				t.Errorf("%s: %q is not on the page", name, l)
				continue
			}
			got := strings.Fields(rows[at][col:])
			for i := at + 1; len(got) < len(words) && i < len(rows); i++ {
				if !strings.HasPrefix(rows[i], strings.Repeat(" ", col)) {
					t.Errorf("%s: %q goes on at the left edge: %q", name, l, rows[i])
					break
				}
				got = append(got, strings.Fields(rows[i])...)
			}
			if strings.Join(got, " ") != strings.Join(words, " ") {
				t.Errorf("%s: %q is drawn as %q", name, l, strings.Join(got, " "))
			}
		}
		if long == 0 {
			t.Errorf("%s: no entry is wider than 60 columns: the test holds nothing", name)
		}
	}
}

// TestOneApp: the apps screen's crumb counts one app as "1 app", and
// "1 of 1 app" under a filter.
func TestOneApp(t *testing.T) {
	h := start(t, 80, 24, func(f *demo.Fake) { f.Body("apps.json", `{"apps": [{"name": "blog"}]}`) })
	h.open(model.View{Screen: model.ScreenApps})
	if got := screenRows(h)[1].text; !strings.HasSuffix(got, " 1 app · 0 need a look · by attention") {
		t.Errorf("the crumb is drawn %q", got)
	}
	h.press("/", "bl", tcell.KeyEnter)
	if got := screenRows(h)[1].text; !strings.HasSuffix(got, " 1 of 1 app · 0 need a look · by attention") {
		t.Errorf("under a filter, the crumb is drawn %q", got)
	}
}

// TestCaddyfileDigestWhole: at 80 columns the box screen shows the
// Caddyfile's sha256 whole, the one value there a person compares with
// something (the trial: 60 of its 64 characters, cut).
func TestCaddyfileDigestWhole(t *testing.T) {
	h := start(t, 80, 24, nil)
	h.open(model.View{Screen: model.ScreenBox})
	const digest = "1f38639fad43624b85cdc2cac1778918713a061084bca6487683ec73f2b2"
	for _, r := range screenRows(h) {
		if strings.HasPrefix(r.text, "Caddyfile ") {
			if f := strings.Fields(r.text); len(f[len(f)-1]) != 64 || !strings.HasPrefix(f[len(f)-1], digest) {
				t.Errorf("the Caddyfile's line is drawn %q, its digest not whole", r.text)
			}
			return
		}
	}
	t.Error("no Caddyfile line on the box screen")
}

// TestColonRemedyFirst: what a mistyped : command gets back begins with
// where to find what : takes, so the remedy is on screen beside the
// crumb's right side at 60 columns, where the trial saw the sentence
// cut before it.
func TestColonRemedyFirst(t *testing.T) {
	for _, w := range []int{60, 80} {
		h := start(t, w, 16, nil)
		h.press(":", "bogus", tcell.KeyEnter)
		if got := screenRows(h)[1].text; !strings.HasPrefix(got, "? lists the : views") {
			t.Errorf("%d columns: the crumb's row is drawn %q", w, got)
		}
	}
}
