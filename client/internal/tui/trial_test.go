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
			if want := "deploying   4c2d9e7b1a05 soaking · started 25s ago (14:12"; !strings.HasPrefix(deploying, want) {
				t.Errorf("the deploy in flight is drawn %q, want it to begin %q", deploying, want)
			}
			golden(t, name, h.screen())
			// While the upload is received (no phase yet) nothing stands
			// between the version and its start.
			h.fake.Body("status/api.json", strings.Replace(apiStatus(t), `"phase": "soaking"`, `"phase": "idle"`, 1))
			h.press(tcell.KeyCtrlR)
			for _, r := range screenRows(h) {
				if strings.HasPrefix(r.text, "deploying ") {
					deploying = r.text
				}
			}
			if want := "deploying   4c2d9e7b1a05 started 25s ago (14:12"; !strings.HasPrefix(deploying, want) {
				t.Errorf("uploading: the deploy in flight is drawn %q, want it to begin %q", deploying, want)
			}
		})
	}
}

func apiStatus(t *testing.T) string {
	b, err := os.ReadFile("demo/fixtures/status/api.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
// where the : views are listed, so the remedy is on screen beside the
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

// TestRefusedRecordCutMarked: a record the box refused says why on one
// row, cut at the window's edge with "…" when it is longer, so a
// sentence long in escapes is never cut unmarked. Laid on rows (as
// 62d41e87 had it), a 403 quoting 200 bidi overrides filled the head's
// half of the window at 60x16 and lost its closing quote and the
// client's words after it, unmarked.
func TestRefusedRecordCutMarked(t *testing.T) {
	long := strings.Repeat("\u202e", 200)
	for _, sz := range [][2]int{{60, 16}, {80, 24}} {
		h := start(t, sz[0], sz[1], func(f *demo.Fake) {
			f.Set("blog@nosuch", &model.Failure{Kind: model.FailForbidden, Message: demo.Host + ` refused this (HTTP 403) and did not say what it needs; the box says: "` + long + `". ` + "`motel whoami` lists the roles you hold from here."})
		})
		h.open(record("blog", "nosuch"))
		rows := screenRows(h)
		if got := rows[2]; !strings.HasPrefix(got.text, demo.Host+" refused this (HTTP 403)") || got.last != "…" || got.end != sz[0] {
			t.Errorf("%dx%d: the refusal's row is drawn %q, ending %q at %d", sz[0], sz[1], got.text, got.last, got.end)
		}
		if got := rows[3].text; strings.Contains(got, `\u`) {
			t.Errorf("%dx%d: the refusal goes on to the next row: %q", sz[0], sz[1], got)
		}
	}
}

// TestUploadInFlight: while a push is uploading (the box sends
// "deploying" with its phase still idle), the apps row says the deploy
// as the app's screen does: ⟳ and "deploying", in the strip and counted
// in the crumb as a row in any other phase is, not "running". An older
// box, which sends a phase and no "deploying", is shown as before.
func TestUploadInFlight(t *testing.T) {
	b, err := os.ReadFile("demo/fixtures/status/api.json")
	if err != nil {
		t.Fatal(err)
	}
	idle := strings.Replace(string(b), `"phase": "soaking"`, `"phase": "idle"`, 1)
	old := strings.Replace(string(b), `"deploying"`, `"x_deploying"`, 1)
	if idle == string(b) || old == string(b) {
		t.Fatal("the api fixture moved")
	}
	for _, c := range []struct{ name, body, word, strip string }{
		{"uploading", idle, "deploying", "deploying 4c2d9e7b1a05 (seen 0s ago)"},
		{"an older box", old, "soaking", "deploying: soaking (seen 0s ago)"},
	} {
		for _, sz := range [][2]int{{60, 16}, {80, 24}} {
			h := start(t, sz[0], sz[1], func(f *demo.Fake) { f.Body("status/api.json", c.body) })
			h.open(model.View{Screen: model.ScreenApps})
			rows := screenRows(h)
			if got := rows[1].text; !strings.Contains(got, " 12 apps · 5 need a look · ") {
				t.Errorf("%s %dx%d: the crumb is drawn %q", c.name, sz[0], sz[1], got)
			}
			var row string
			for _, r := range rows[2:] {
				if f := strings.Fields(r.text); len(f) > 2 && f[1] == "api" && !strings.Contains(r.text, "(seen") {
					row = r.text
				}
			}
			if f := strings.Fields(row); len(f) < 3 || f[0] != "⟳" || f[2] != c.word {
				t.Errorf("%s %dx%d: the api row is drawn %q, want ⟳ and %q", c.name, sz[0], sz[1], row, c.word)
			}
			// The strip shows its first reason: under a filter, api's.
			h.press("/", "api", tcell.KeyEnter)
			if strip := screenRows(h)[2].text; !strings.HasPrefix(strip, "⟳ api") || !strings.Contains(strip, c.strip) {
				t.Errorf("%s %dx%d: the strip says %q, want %q", c.name, sz[0], sz[1], strip, c.strip)
			}
			h.open(app("api"))
			for _, r := range screenRows(h) {
				if strings.HasPrefix(r.text, "state ") && !strings.HasPrefix(r.text, "state       running 3b8812aa0c4f") {
					t.Errorf("%s %dx%d: the app's state is drawn %q", c.name, sz[0], sz[1], r.text)
				}
			}
		}
	}
}

// TestFirstDeployInFlight: an app with no version yet and a deploy in
// flight says the deploy on its screen, the "deploying" line under a
// sentence that is true while it runs, and its apps row says it (⟳ and
// its phase), as `motel status` does; the screen said "no version
// yet: no deploy of it serves" and nothing of the deploy, the row "not
// deployed". With no deploy before it, the screen said its first
// deploy comes from CI and there is nothing to show.
func TestFirstDeployInFlight(t *testing.T) {
	b, err := os.ReadFile("demo/fixtures/status/api.json")
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Replace(strings.Replace(string(b), `"current_version": "3b8812aa0c4f",`, ``, 1), `"running": true`, `"running": false`, 1)
	none := strings.Replace(strings.Replace(first, `"deploys": [`, `"x_deploys": [`, 1), `"last_deploy"`, `"x_last_deploy"`, 1)
	if first == string(b) || none == first {
		t.Fatal("the api fixture moved")
	}
	for name, body := range map[string]string{"after a deploy": first, "the first": none} {
		for _, sz := range [][2]int{{60, 16}, {80, 24}} {
			h := start(t, sz[0], sz[1], func(f *demo.Fake) { f.Body("status/api.json", body) })
			h.open(app("api"))
			rows := screenRows(h)
			if got, want := rows[2].text, "api has no version yet: a deploy of it is in flight."; got != want {
				t.Errorf("%s %dx%d: the sentence is drawn %q, want %q", name, sz[0], sz[1], got, want)
			}
			if got, want := rows[3].text, "deploying   4c2d9e7b1a05 soaking · started 25s ago (14:12"; !strings.HasPrefix(got, want) {
				t.Errorf("%s %dx%d: the row under it is drawn %q, want it to begin %q", name, sz[0], sz[1], got, want)
			}
			h.open(model.View{Screen: model.ScreenApps})
			var row string
			for _, r := range screenRows(h)[2:] {
				if f := strings.Fields(r.text); len(f) > 2 && f[1] == "api" {
					row = r.text
				}
			}
			if f := strings.Fields(row); len(f) < 4 || f[0] != "⟳" || f[2] != "soaking" {
				t.Errorf("%s %dx%d: the api row is drawn %q, want ⟳ and soaking", name, sz[0], sz[1], row)
			}
		}
	}
}

// TestCountsOfOne: every count in the apps screen's own sentences (the
// crumb and the strip) reads as English at one: "1 needs a look", "1 app
// running", "1 of 2 apps is", "1 failed check", "1 restart", where
// they said "1 need a look", "all 1 apps running", "1 of 2 apps are",
// "1 failed checks" and "1 restarts".
func TestCountsOfOne(t *testing.T) {
	read := func(path string) string {
		b, err := os.ReadFile("demo/fixtures/" + path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	blog, billing := read("status/blog.json"), read("status/billing.json")
	backoff := strings.Replace(blog, `"state": "watching"`, `"state": "backoff", "consecutive_failures": 1`, 1)
	throttled := strings.Replace(billing, `"restarts_in_window": 5`, `"restarts_in_window": 1`, 1)
	if backoff == blog || throttled == billing {
		t.Fatal("the fixtures moved")
	}
	for _, c := range []struct {
		name, apps, crumb, strip string
		setup                    func(*demo.Fake)
	}{
		{"one app running", `["blog"]`, " 1 app · 0 need a look · by attention", "1 app running; its newest deploy succeeded", nil},
		{"one to look at", `["mailer", "blog"]`, " 2 apps · 1 needs a look · by attention", "✖ mailer       stopped: 9f02bb1c4e7a is not running", nil},
		{"one not deployed", `["blog", "wiki"]`, " 2 apps · 0 need a look · by attention", "nothing needs a look; 1 of 2 apps is not deployed or not yours to view", nil},
		{"one failed check", `["blog"]`, " 1 app · 1 needs a look · by attention", "▲ blog         backoff: the watchdog waits to restart it; 1 failed check in a row",
			func(f *demo.Fake) { f.Body("status/blog.json", backoff) }},
		{"one restart", `["billing"]`, " 1 app · 1 needs a look · by attention", `✖ billing      throttled: 1 restart in its window; last cause "probe: timeout"`,
			func(f *demo.Fake) { f.Body("status/billing.json", throttled) }},
	} {
		var names []string
		for _, n := range strings.Split(strings.Trim(c.apps, "[]"), ", ") {
			names = append(names, `{"name": `+n+`}`)
		}
		for _, sz := range [][2]int{{60, 16}, {80, 24}} {
			h := start(t, sz[0], sz[1], func(f *demo.Fake) {
				f.Body("apps.json", `{"apps": [`+strings.Join(names, ", ")+`]}`)
				if c.setup != nil {
					c.setup(f)
				}
			})
			h.open(model.View{Screen: model.ScreenApps})
			rows := screenRows(h)
			if got := rows[1].text; !strings.HasSuffix(got, c.crumb) {
				t.Errorf("%s %dx%d: the crumb is drawn %q, want it to end %q", c.name, sz[0], sz[1], got, c.crumb)
			}
			// At 60 columns the strip's row is cut at the window's edge.
			if got := rows[2].text; got != c.strip && (rows[2].last != "…" || !strings.HasPrefix(c.strip, strings.TrimSuffix(got, "…"))) {
				t.Errorf("%s %dx%d: the strip is drawn %q, want %q", c.name, sz[0], sz[1], got, c.strip)
			}
		}
	}
}

// TestDeployAfterAFailure: a deploy in flight is said as one on the apps
// screen whatever the newest record says: with a failed record the
// newest (shop's), the row is marked ⟳ and the strip says the deploy,
// as it does after a success; the failure stays in LAST DEPLOY, and the
// row still counts in "need a look". The trial saw the row marked ▲ and
// the strip saying the old failure through a relaunch and a rollback.
func TestDeployAfterAFailure(t *testing.T) {
	b, err := os.ReadFile("demo/fixtures/status/shop.json")
	if err != nil {
		t.Fatal(err)
	}
	inFlight := `"phase": "soaking",
 "deploying": {"version": "e9a0b3f2c118", "deployed_by": "local:bob-laptop", "started_at": "@-20s"},`
	body := strings.Replace(string(b), `"phase": "idle",`, inFlight, 1)
	if body == string(b) {
		t.Fatal("the shop fixture moved")
	}
	for _, sz := range [][2]int{{60, 16}, {80, 24}} {
		h := start(t, sz[0], sz[1], func(f *demo.Fake) { f.Body("status/shop.json", body) })
		h.open(model.View{Screen: model.ScreenApps})
		rows := screenRows(h)
		if got := rows[1].text; !strings.Contains(got, " 12 apps · 5 need a look · ") {
			t.Errorf("%dx%d: the crumb is drawn %q", sz[0], sz[1], got)
		}
		var row string
		for _, r := range rows[3:] {
			if f := strings.Fields(r.text); len(f) > 2 && f[1] == "shop" {
				row = r.text
			}
		}
		if f := strings.Fields(row); len(f) < 5 || f[0] != "⟳" || f[2] != "soaking" || !strings.Contains(row, " → e9a0b3f2c118 failed") {
			t.Errorf("%dx%d: the shop row is drawn %q, want ⟳, soaking and its failed newest deploy", sz[0], sz[1], row)
		}
		h.press("/", "shop", tcell.KeyEnter)
		t.Logf("%dx%d:\n%s\n%s", sz[0], sz[1], screenRows(h)[2].text, row)
		if strip := screenRows(h)[2].text; !strings.HasPrefix(strip, "⟳ shop         deploying") {
			t.Errorf("%dx%d: the strip says %q, want the deploy in flight", sz[0], sz[1], strip)
		}
	}
}

// TestRowVersionInFlight: while a deploy is in flight the apps row's
// VERSION says the version in flight, marked as on its way ("→ "), and
// not the running one, so the phase in STATE is not read as the running
// version's; the strip's line names it too. An older box, which sends a
// phase and no "deploying", is shown as before; a version the box's
// filter hid is shown as hidden. The trial saw "⟳ example soaking
// a2b3326f1c84" with a2b3326f1c84 serving and another version soaking.
func TestRowVersionInFlight(t *testing.T) {
	b, err := os.ReadFile("demo/fixtures/status/api.json")
	if err != nil {
		t.Fatal(err)
	}
	api := string(b)
	edit := func(s, old, new string) string {
		out := strings.Replace(s, old, new, 1)
		if out == s {
			t.Fatalf("the api fixture moved: no %q", old)
		}
		return out
	}
	const hidden = "[hidden by the box's filter]"
	for _, c := range []struct {
		name, body, version, strip string
	}{
		{"in flight", api, "→ 4c2d9e7b1a05", "deploying 4c2d9e7b1a05: soaking (seen 0s ago)"},
		{"uploading", edit(api, `"phase": "soaking"`, `"phase": "idle"`), "→ 4c2d9e7b1a05", "deploying 4c2d9e7b1a05 (seen 0s ago)"},
		{"a relaunch", edit(api, `"version": "4c2d9e7b1a05"`, `"version": "3b8812aa0c4f"`), "→ 3b8812aa0c4f", "deploying 3b8812aa0c4f: soaking (seen 0s ago)"},
		{"the first", edit(edit(api, `"current_version": "3b8812aa0c4f",`, ``), `"running": true`, `"running": false`), "→ 4c2d9e7b1a05", "deploying 4c2d9e7b1a05: soaking (seen 0s ago)"},
		{"hidden", edit(api, `"version": "4c2d9e7b1a05"`, `"version": "`+hidden+`"`), "→ [hidden by …", "deploying [hidden by …: soaking (seen 0s ago)"},
		{"an older box", edit(api, `"deploying"`, `"x_deploying"`), "3b8812aa0c4f", "deploying: soaking (seen 0s ago)"},
	} {
		for _, sz := range [][2]int{{60, 16}, {80, 24}} {
			h := start(t, sz[0], sz[1], func(f *demo.Fake) { f.Body("status/api.json", c.body) })
			h.open(model.View{Screen: model.ScreenApps})
			row := find(screenRows(h)[3:], "⟳ api ")
			h.press("/", "api", tcell.KeyEnter)
			rows := screenRows(h)
			strip := rows[2].text
			t.Logf("%s %dx%d:\n%s\n%s", c.name, sz[0], sz[1], strip, row)
			if !strings.HasPrefix(row, "⟳ api ") || !strings.Contains(row, " "+c.version+" ") {
				t.Errorf("%s %dx%d: the api row is drawn %q, want VERSION %q", c.name, sz[0], sz[1], row, c.version)
			}
			if c.version[0] != '3' && strings.Contains(row, "3b8812aa0c4f") && c.name != "a relaunch" {
				t.Errorf("%s %dx%d: the api row names the running version beside the phase: %q", c.name, sz[0], sz[1], row)
			}
			if want := "⟳ api          " + c.strip; strip != want && (rows[2].last != "…" || !strings.HasPrefix(want, strings.TrimSuffix(strip, "…"))) {
				t.Errorf("%s %dx%d: the strip says %q, want %q", c.name, sz[0], sz[1], strip, want)
			}
		}
	}
	// A row with no deploy in flight keeps its running version, unmarked.
	for _, sz := range [][2]int{{60, 16}, {80, 24}} {
		h := start(t, sz[0], sz[1], nil)
		h.open(model.View{Screen: model.ScreenApps})
		row := find(screenRows(h)[3:], "  blog ")
		t.Logf("no deploy %dx%d:\n%s", sz[0], sz[1], row)
		if !strings.Contains(row, " 7c1e55d0a913 ") || strings.Contains(row, "→") {
			t.Errorf("no deploy %dx%d: the blog row is drawn %q", sz[0], sz[1], row)
		}
	}
}

// find is the text of the first row that begins with prefix.
func find(rows []drawnRow, prefix string) string {
	for _, r := range rows {
		if strings.HasPrefix(r.text, prefix) {
			return r.text
		}
	}
	return ""
}
