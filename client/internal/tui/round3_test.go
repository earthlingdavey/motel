package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/door"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestCtrlROnlyWhatIsShown: ctrl-r on who am I asks whoami alone, on
// help nothing, on the box the box alone, each pressed AgainAfter and
// more after the last read of it.
func TestCtrlROnlyWhatIsShown(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.press(":", "me", tcell.KeyEnter)
	for _, c := range []struct {
		where string
		want  model.Kind
		none  bool
	}{{"me", model.KWhoAmI, false}, {"help", 0, true}, {"box", model.KBox, false}} {
		if c.where != "me" {
			h.press(tcell.KeyEscape, ":", c.where, tcell.KeyEnter)
		}
		h.now = h.now.Add(model.AgainAfter + time.Second)
		h.pump()
		before := len(h.fake.Calls)
		h.press(tcell.KeyCtrlR)
		for _, call := range h.fake.Calls[before:] {
			if c.none || call.Job.Kind != c.want {
				t.Errorf("ctrl-r on %s read %v", c.where, call.Job)
			}
		}
		if !c.none && len(h.fake.Calls) == before {
			t.Errorf("ctrl-r on %s read nothing", c.where)
		}
	}
}

// TestRefusedAtTheSmallest: a refused key's screen in the smallest
// window, 60x16, with the client's own sentences for a 401 (its words,
// client.refused's, written out: no test of the TUI imports the
// client), the plain one and the one that names the clock. The footer
// is drawn whole, naming ↓; and ↓ brings every line of the page on
// screen, read from the screen's cells as it scrolls.
func TestRefusedAtTheSmallest(t *testing.T) {
	const (
		fp     = "SHA256:SAMPLE0000000000000000000000000000000000000"
		causes = "no person or signer line on the box lists it; your address is outside the from list on every grant you hold; deploy.example.com is not the box's main name; or the box's moteld is too old to take a key's token at all. Send the operator the line `motel key` prints."
		plain  = "deploy.example.com did not accept your key " + fp + " (~/.ssh/id_ed25519.pub). The box says no more to a key it does not accept, so it is one of these: " + causes
		clock  = "deploy.example.com did not accept your key " + fp + " (~/.ssh/id_ed25519.pub). This machine's clock differs from the box's by 3600 s (behind it; the box's Date is Sun, 15 Nov 2026 15:12:40 GMT), more than a token made here can bridge: if the box's Date is right, set this machine's clock. Or, as the box says no more, one of these: " + causes
		foot   = "↑↓ scroll  r ask again  P print person line  ? keys  q quit"
	)
	unspaced := func(s string) string { return strings.Join(strings.Fields(s), "") }
	for name, msg := range map[string]string{"plain": plain, "clock": clock} {
		h := start(t, 60, 16, func(f *demo.Fake) {
			f.Set("whoami", &model.Failure{Kind: model.FailSignedOut, Message: msg})
		})
		if h.top().Screen != model.ScreenRefused {
			t.Fatalf("%s: on %+v, not the refusal screen", name, h.top())
		}
		// The page's rows by where they stand in it, as ↓ scrolls it: the
		// body is the rows between the crumb and the footer.
		page := map[int]string{}
		var rows []drawnRow
		for range 60 {
			var off int
			h.on(func() { off, _ = h.u.page.GetScrollOffset() })
			rows = screenRows(h)
			for i, r := range rows[2 : len(rows)-1] {
				page[off+i] = r.text
			}
			h.press(tcell.KeyDown)
		}
		if got := rows[len(rows)-1].text; got != foot {
			t.Errorf("%s: the footer is drawn %q, want %q", name, got, foot)
		}
		var all strings.Builder
		for i := range len(page) {
			all.WriteString(page[i])
		}
		var want []model.Line
		h.on(func() { want = h.s.RefusedView(h.now) })
		for _, l := range want {
			var b strings.Builder
			for _, s := range l {
				b.WriteString(s.T.String())
			}
			if !strings.Contains(unspaced(all.String()), unspaced(b.String())) {
				t.Errorf("%s: ↓ does not bring this line on screen: %q", name, b.String())
			}
		}
	}
}

// TestStopClosesTheLine: a stop closes the : line and any overlay; on
// the stop screen only r, q, ? and the keys that scroll it act; its
// crumb says stopped; P is not offered for a stop that shows no person
// line.
func TestStopClosesTheLine(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.press("y", ":", "sta")
	h.fake.Set("blog", &model.Failure{Kind: model.FailNotSigned, Message: "~/.ssh/id_ed25519.pub did not sign"})
	h.on(func() { h.s.Engine.Again(h.now, model.Job{Kind: model.KStatus, App: "blog"}) })
	h.pump()
	var page string
	h.on(func() { page, _ = h.u.row2.GetFrontPage() })
	if page != "crumb" || h.u.overlay != 0 || h.top().Screen != model.ScreenRefused {
		t.Fatalf("after a stop: row 2 %q, overlay %d, top %+v", page, h.u.overlay, h.top())
	}
	h.press(":", "apps", tcell.KeyEnter, "c", "P", tcell.KeyEscape)
	if h.top().Screen != model.ScreenRefused || h.u.overlay != 0 || h.u.Print != "" {
		t.Fatalf("keys acted on the stop screen: top %+v overlay %d print %q", h.top(), h.u.overlay, h.u.Print)
	}
	s := h.screen()
	if !strings.Contains(s, "\nstopped") || strings.Contains(s, "first run") || strings.Contains(s, "P quit") {
		t.Errorf("the stop screen shows:\n%s", s)
	}
}

// TestNotThisKeyScreen: an answer about another key stops the session
// on one screen that says so: no other read, no overlay, no P.
func TestNotThisKeyScreen(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.press(":", "me", tcell.KeyEnter, "y")
	h.fake.Body("whoami.json", `{"name":"mallory@example.com","key":"SHA256:OTHER00000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`)
	h.now = h.now.Add(model.AgainAfter) // ctrl-r reads whoami again no sooner
	h.press(tcell.KeyCtrlR)
	s := h.screen()
	if h.top().Screen != model.ScreenRefused || h.u.overlay != 0 || !strings.Contains(s, `"SHA256:OTHER`) || strings.Contains(s, "mallory") || strings.Contains(s, "P quit") {
		t.Fatalf("the screen shows:\n%s", s)
	}
	before := len(h.fake.Calls)
	h.now = h.now.Add(time.Hour)
	h.pump()
	if len(h.fake.Calls) != before {
		t.Errorf("%d reads after the stop", len(h.fake.Calls)-before)
	}
}

// TestColonWordsAndTab: one word too many is said and not run (:q x
// does not quit); Tab with nothing to complete keeps the line.
func TestColonWordsAndTab(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.press(":", "status blog extra", tcell.KeyEnter)
	if s := h.screen(); h.top().Screen != model.ScreenApps || !strings.Contains(s, `one word too many: "extra"`) {
		t.Fatalf(":status blog extra:\n%s", s)
	}
	h.press(":", "q x", tcell.KeyEnter)
	select {
	case <-h.done:
		t.Fatal(":q x quit")
	default:
	}
	h.press(":", "zzz", tcell.KeyTab, tcell.KeyTab, tcell.KeyBacktab)
	var page string
	h.on(func() { page, _ = h.u.row2.GetFrontPage() })
	if page != "prompt" || h.u.prompt.GetText() != "zzz" {
		t.Fatalf("Tab with nothing to complete: row 2 %q, text %q", page, h.u.prompt.GetText())
	}
}

// TestBigOverlay: a 4 MiB answer opens, and keys move it, quickly: the
// overlay is made once and draws at most OverlayLines.
func TestBigOverlay(t *testing.T) {
	h := start(t, 100, 30, func(f *demo.Fake) {
		f.Body("apps.json", `{"apps": [{"name": "blog"}, {"name": "shop"}], "pad": [`+strings.Repeat(`"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",`, 45000)+`"x"]}`)
	})
	start := time.Now()
	h.press("y")
	opened := time.Since(start)
	if s := h.screen(); !strings.Contains(s, "P quit and print") || !strings.Contains(s, "the box's answer to") {
		t.Errorf("no JSON overlay with P:\n%s", s)
	}
	start = time.Now()
	for range 10 {
		h.press(tcell.KeyDown)
	}
	keys := time.Since(start) / 10
	t.Logf("open %s, a key %s", opened, keys)
	var last string
	h.on(func() {
		ls := h.s.Overlay(model.View{Screen: model.ScreenApps}, true, 98, door.Width, door.AsIs)
		last = ls[len(ls)-1].Text()
	})
	if !strings.Contains(last, "more lines not drawn here: P quits and prints the whole answer") {
		t.Errorf("the overlay's last line is %q", last)
	}
	if opened > 20*time.Second || keys > 5*time.Second {
		t.Errorf("open %s, a key %s: too slow", opened, keys)
	}
}
