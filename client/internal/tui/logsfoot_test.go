package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// The logs screen's footer in each of its forms.
const (
	footAll        = "↑↓ scroll  End follow  w wrap  c cmd  esc back  ? more"
	footFollowWrap = "End follow  w wrap  c cmd  esc back  ? more"
	footFollow     = "End follow  c cmd  esc back  ? more"
	footWrap       = "w wrap  c cmd  esc back  ? more"
	footNone       = "c cmd  esc back  ? more"
)

// whoami's answers: the fixture's grants, and none on logs.
const (
	whoamiGrants = `{"name":"alice@example.com","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"},{"role":"logs","scope":"app:blog"},{"role":"logs","scope":"app:hostile"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`
	whoamiViewer = `{"name":"alice@example.com","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`
)

// blogLines is blog's tail for these tests: 60 short lines, then one of
// 135 characters, the newest, which a window of 60 or 100 columns cuts
// or, with w, wraps.
func blogLines() string {
	var b strings.Builder
	b.WriteString(`{"kept":"on","more":false,"gap":false,"lines":[`)
	for i := range 60 {
		fmt.Fprintf(&b, `{"at":"@-%ds","release":"7c1e55d0a913","text":"line %d"},`, 90-i, i)
	}
	b.WriteString(`{"at":"@-1s","release":"7c1e55d0a913","text":"the long line: ` + strings.Repeat("abcdefghij", 12) + `"}]}`)
	return b.String()
}

// tallFailure is a failure whose sentence takes 14 rows or more at 60
// columns: 400 characters two cells wide.
var tallFailure = &model.Failure{Kind: model.FailNoAnswer, Message: strings.Repeat("字", 400)}

// footer is the screen's last row, read from its cells.
func footer(h *harness) string {
	rows := strings.Split(strings.TrimSuffix(h.screen(), "\n"), "\n")
	return strings.TrimSpace(rows[len(rows)-1])
}

// longLine says how the frame shows blog's long line: cut at the edge
// (its row ends with …), or wrapped (the row after it goes on, indented
// past the time).
func longLine(t *testing.T, h *harness) (cut, wrapped bool) {
	t.Helper()
	scr := h.screen()
	rows := strings.Split(scr, "\n")
	for i, r := range rows {
		if strings.Contains(r, "the long line: ") {
			return strings.HasSuffix(r, "…"), i+1 < len(rows) && strings.HasPrefix(rows[i+1], strings.Repeat(" ", 10)) && strings.TrimSpace(rows[i+1]) != ""
		}
	}
	t.Fatalf("the long line is not shown:\n%s", scr)
	return false, false
}

// whoamiAgain has whoami read again now, answering body.
func whoamiAgain(h *harness, body string) {
	h.fake.Body("whoami.json", body)
	h.now = h.now.Add(model.AgainAfter)
	h.on(func() { h.s.Engine.Again(h.now, model.Job{Kind: model.KWhoAmI}) })
	h.press()
}

// TestLogsFooterWithoutLines: while the logs screen says why it shows
// no line of the app, its footer lists none of its own keys, and none of
// them changes anything: when the lines show, the screen follows them
// and does not wrap them.
func TestLogsFooterWithoutLines(t *testing.T) {
	refused := &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this"}
	for _, c := range []struct {
		name  string
		setup func(*demo.Fake)
		says  string
		// lines makes the lines show.
		lines func(h *harness)
	}{
		{"no grant", func(f *demo.Fake) { f.Body("whoami.json", whoamiViewer) }, "needs a logs grant on blog", func(h *harness) { whoamiAgain(h, whoamiGrants) }},
		{"whoami not answered", func(f *demo.Fake) {
			f.Set("whoami", &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
		}, "whether this key may read blog's lines", func(h *harness) {
			h.fake.Set("whoami", nil)
			h.now = h.now.Add(model.AgainAfter)
			h.press()
		}},
		{"the box refused", func(f *demo.Fake) { f.Set("logs:blog", refused) }, "does not allow your key this", func(h *harness) {
			h.fake.Set("logs:blog", nil)
			h.now = h.now.Add(model.AgainAfter)
			h.press(tcell.KeyCtrlR)
		}},
		{"kept off", func(f *demo.Fake) {
			f.Body("logs/blog.json", `{"kept":"off","lines":[],"more":false,"gap":false}`)
		}, "keeps blog's output on the box", func(h *harness) {
			h.fake.Body("logs/blog.json", blogLines())
			h.now = h.now.Add(model.AgainAfter)
			h.press(tcell.KeyCtrlR)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, 60, 16, func(f *demo.Fake) {
				f.Body("logs/blog.json", blogLines())
				c.setup(f)
			})
			h.open(app("blog"))
			h.press("l")
			if s := h.screen(); !strings.Contains(s, c.says) {
				t.Fatalf("the screen:\n%s", s)
			}
			if got := footer(h); got != footNone {
				t.Errorf("the footer %q, want %q", got, footNone)
			}
			h.press("k", "k", tcell.KeyPgUp, tcell.KeyHome, "w", tcell.KeyEnd)
			c.lines(h)
			if s := h.screen(); !strings.Contains(s, "── following") {
				t.Errorf("with lines shown the view is not followed:\n%s", s)
			}
			if cut, wrapped := longLine(t, h); !cut || wrapped {
				t.Errorf("w changed the wrap: the long line cut %v, wrapped %v", cut, wrapped)
			}
			if got := footer(h); got != footAll {
				t.Errorf("with lines shown the footer %q, want %q", got, footAll)
			}
		})
	}
}

// TestLogsWrapWhileAsking: w pressed while the first read is still
// asked, which the footer then offers, is kept: the lines arrive
// wrapped.
func TestLogsWrapWhileAsking(t *testing.T) {
	h := start(t, 60, 16, func(f *demo.Fake) { f.Body("logs/blog.json", blogLines()) })
	h.open(app("blog"))
	h.on(func() { h.u.openLogs("blog") })
	if s := h.screen(); !strings.Contains(s, "asking deploy.example.com for blog's newest lines") {
		t.Fatalf("not asking:\n%s", s)
	}
	if got := footer(h); got != footWrap {
		t.Errorf("asking, the footer %q, want %q", got, footWrap)
	}
	h.press("w") // and the read is answered
	if cut, wrapped := longLine(t, h); cut || !wrapped {
		t.Errorf("w while asking: the long line cut %v, wrapped %v", cut, wrapped)
	}
}

// TestLogsLiveWithNoLine: a follow with no line yet is still a live
// follow: End and w are offered, the scroll keys not, and w is kept for
// the lines that arrive.
func TestLogsLiveWithNoLine(t *testing.T) {
	h := start(t, 60, 16, func(f *demo.Fake) {
		f.Body("logs/blog.json", `{"kept":"on","lines":[],"more":false,"gap":false}`)
	})
	h.open(app("blog"))
	h.press("l")
	if s := h.screen(); !strings.Contains(s, "this screen waits for its first") || !strings.Contains(s, "── following") {
		t.Fatalf("not a follow with no line:\n%s", s)
	}
	if got := footer(h); got != footFollowWrap {
		t.Errorf("the footer %q, want %q", got, footFollowWrap)
	}
	h.press("w")
	h.fake.Body("logs/blog.json", blogLines())
	h.now = h.now.Add(model.LogsEvery)
	h.press()
	if cut, wrapped := longLine(t, h); cut || !wrapped {
		t.Errorf("w on a follow with no line: the long line cut %v, wrapped %v", cut, wrapped)
	}
	if got := footer(h); got != footAll {
		t.Errorf("with lines the footer %q, want %q", got, footAll)
	}
}

// TestLogsHoldNotShown: a view held where the screen no longer shows
// lines (the head filling a short window, the grant gone) is let go by
// End, which the footer offers; the scroll keys are not offered there
// and move nothing; with the lines shown again the screen follows.
func TestLogsHoldNotShown(t *testing.T) {
	h := start(t, 60, 40, func(f *demo.Fake) { f.Body("logs/blog.json", blogLines()) })
	h.open(app("blog"))
	h.press("l")
	h.press("k")
	if s := h.screen(); !strings.Contains(s, "── held") {
		t.Fatalf("k did not hold:\n%s", s)
	}
	h.fake.Set("logs:blog", tallFailure)
	h.now = h.now.Add(model.LogsEvery)
	h.press()
	if s := h.screen(); !strings.Contains(s, "the last read failed") || !strings.Contains(s, "── held") {
		t.Fatalf("at 60x40 the failure and the held lines:\n%s", s)
	}
	h.on(func() { h.scr.(*sizedSim).SetSize(60, 16) })
	if s := h.screen(); strings.Contains(s, "── ") {
		t.Fatalf("at 60x16 the head leaves the lines a row:\n%s", s)
	}
	if got := footer(h); got != footFollowWrap {
		t.Errorf("held, the head filling the page: the footer %q, want %q", got, footFollowWrap)
	}
	h.press(tcell.KeyEnd)
	h.on(func() { h.scr.(*sizedSim).SetSize(60, 40) })
	if s := h.screen(); !strings.Contains(s, "── following") {
		t.Errorf("End with the head filling the page did not let the hold go:\n%s", s)
	}
	// From following there, the scroll keys hold nothing.
	h.on(func() { h.scr.(*sizedSim).SetSize(60, 16) })
	h.press("k", tcell.KeyPgUp, tcell.KeyHome)
	h.on(func() { h.scr.(*sizedSim).SetSize(60, 40) })
	if s := h.screen(); !strings.Contains(s, "── following") {
		t.Errorf("a scroll key with the head filling the page held the view:\n%s", s)
	}

	// The grant gone while held: End alone.
	h = start(t, 60, 16, func(f *demo.Fake) { f.Body("logs/blog.json", blogLines()) })
	h.open(app("blog"))
	h.press("l")
	h.press("k")
	whoamiAgain(h, whoamiViewer)
	if s := h.screen(); !strings.Contains(s, "needs a logs grant on blog") {
		t.Fatalf("the grant gone:\n%s", s)
	}
	if got := footer(h); got != footFollow {
		t.Errorf("held, the grant gone: the footer %q, want %q", got, footFollow)
	}
	h.press(tcell.KeyEnd)
	whoamiAgain(h, whoamiGrants)
	if s := h.screen(); !strings.Contains(s, "── following") {
		t.Errorf("End with the grant gone did not let the hold go:\n%s", s)
	}
	// From following, the grant gone, the scroll keys hold nothing.
	whoamiAgain(h, whoamiViewer)
	if got := footer(h); got != footNone {
		t.Errorf("following, the grant gone: the footer %q, want %q", got, footNone)
	}
	h.press("k", tcell.KeyPgUp, tcell.KeyHome)
	whoamiAgain(h, whoamiGrants)
	if s := h.screen(); !strings.Contains(s, "── following") {
		t.Errorf("a scroll key with the grant gone held the view:\n%s", s)
	}
}

// TestLogsFooterByFrame: the footer goes by the frame's own layout, at
// the first frame after opening, after each resize, after the screen is
// opened again, and for a second app after a first, held, of which
// nothing is kept.
func TestLogsFooterByFrame(t *testing.T) {
	if got := logsFoot(model.LogsKeysOn{Scroll: true, Follow: true, Wrap: true}); got != footers[model.ScreenLogs] || got != footAll {
		t.Errorf("the full footer %q, footers has %q", got, footers[model.ScreenLogs])
	}
	h := start(t, 60, 40, func(f *demo.Fake) { f.Body("logs/blog.json", blogLines()) })
	h.open(app("blog"))
	h.on(func() { h.u.openLogs("blog") })
	if got := footer(h); got != footWrap {
		t.Errorf("the first frame, asking: the footer %q, want %q", got, footWrap)
	}
	h.press()
	if got := footer(h); got != footAll {
		t.Errorf("the lines shown: the footer %q, want %q", got, footAll)
	}
	h.fake.Set("logs:blog", tallFailure)
	h.now = h.now.Add(model.LogsEvery)
	h.press()
	for _, c := range []struct {
		h    int
		want string
	}{{16, footFollowWrap}, {40, footAll}, {16, footFollowWrap}, {30, footAll}} {
		h.on(func() { h.scr.(*sizedSim).SetSize(60, c.h) })
		if got := footer(h); got != c.want {
			t.Errorf("at 60x%d the footer %q, want %q", c.h, got, c.want)
		}
	}
	h.fake.Set("logs:blog", nil)
	h.on(func() { h.scr.(*sizedSim).SetSize(60, 16) })
	h.now = h.now.Add(5 * model.LogsEvery)
	h.press()
	h.press("k")
	if s := h.screen(); !strings.Contains(s, "── held") {
		t.Fatalf("k did not hold:\n%s", s)
	}
	// Another app: nothing of blog's hold.
	h.press(tcell.KeyEscape)
	h.on(func() { h.u.openLogs("hostile") })
	if got := footer(h); got != footWrap {
		t.Errorf("hostile, asking, after blog held: the footer %q, want %q", got, footWrap)
	}
	h.press()
	if got := footer(h); got != footAll {
		t.Errorf("hostile's lines: the footer %q, want %q", got, footAll)
	}
	// blog opened again: a fresh start.
	h.press(tcell.KeyEscape)
	h.on(func() { h.u.openLogs("blog") })
	if got := footer(h); got != footWrap {
		t.Errorf("blog opened again, asking: the footer %q, want %q", got, footWrap)
	}
	h.now = h.now.Add(model.LogsEvery)
	h.press()
	if s := h.screen(); !strings.Contains(s, "── following") || footer(h) != footAll {
		t.Errorf("blog opened again: the footer %q:\n%s", footer(h), s)
	}
}
