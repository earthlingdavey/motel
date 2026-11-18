package tui

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestDeepJSONOnScreen: y on an app whose status holds one more field,
// 58 KB nested 9000 deep (the review's), takes under 200 MB and a
// second (about 6 MB, 56 under -race, which counts the frames drawn
// meanwhile; it was over 2 GB), and the overlay says, in the screen's cells, that its lines
// are indented as at IndentMax and how many it does not draw.
func TestDeepJSONOnScreen(t *testing.T) {
	deep := strings.Repeat("[", 9000) + strings.TrimSuffix(strings.Repeat("1,", 20000), ",") + strings.Repeat("]", 9000)
	body := `{"app": "wiki", "phase": "idle", "running": false, "available_versions": [], "deploys": [], "x": ` + deep + `}`
	h := start(t, 80, 24, func(f *demo.Fake) { f.Body("status/wiki.json", body) })
	h.open(app("wiki"))
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	at := time.Now()
	h.press("y")
	took := time.Since(at)
	runtime.ReadMemStats(&m1)
	if mb := (m1.TotalAlloc - m0.TotalAlloc) >> 20; mb >= 200 || took >= time.Second {
		t.Errorf("y on a %d-byte body nested 9000 deep took %v and %d MB", len(body), took, mb)
	}
	all := joinRows(overlayCells(h), " ")
	if !strings.Contains(all, "nested deeper than "+strconv.Itoa(model.IndentMax)+" levels and indented as at") {
		t.Errorf("the overlay does not say the deep lines are indented less:\n%s", all)
	}
	h.press(tcell.KeyEnd)
	all = joinRows(overlayCells(h), " ")
	// Its lines: { and six fields, the last opening the first [, 8999
	// more [, 20000 numbers, 9000 ], }.
	if more := 7 + 8999 + 20000 + 9000 + 1 - model.OverlayLines; !strings.Contains(all, "… "+strconv.Itoa(more)+" more lines not drawn here") {
		t.Errorf("the overlay's end does not count the %d lines not drawn:\n%s", more, all)
	}
}

// failAll makes every read the session makes fail with err, or answer
// again with nil.
func failAll(h *harness, err error) {
	for _, k := range append([]string{"whoami", "apps", "box"}, h.s.Names()...) {
		h.fake.Set(k, err)
	}
}

// TestTitleWordsAt60: every word the title bar's right side carries is
// drawn whole at the smallest window, 60 columns, ending at its edge,
// with the left side cut before it: read from the screen's cells.
func TestTitleWordsAt60(t *testing.T) {
	refuse := func(k model.Fail) func(*harness) {
		return func(h *harness) {
			failAll(h, &model.Failure{Kind: k, Message: "no"})
			h.press(tcell.KeyCtrlR)
		}
	}
	silent := func(h *harness, d time.Duration, word string) {
		failAll(h, &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
		h.now = h.now.Add(d)
		for range 100 {
			// A person at the keys: no idle pause.
			h.s.Engine.Touch(h.now)
			h.pump()
			if _, r := h.s.Title(h.now); strings.HasPrefix(r.Text(), word) {
				break
			}
			h.now = h.now.Add(time.Second)
		}
		h.on(func() {})
	}
	forbidden := &model.Failure{Kind: model.FailForbidden, Message: "no"}
	for _, c := range []struct {
		word  string
		setup func(*demo.Fake)
		get   func(*harness)
	}{
		{"live · 0s ago", nil, nil},
		{"asking…", func(f *demo.Fake) {
			for _, k := range []string{"whoami", "apps"} {
				f.Set(k, &model.Failure{Kind: model.FailNotSent, Message: "not sent"})
			}
		}, nil},
		{"TOUCH YOUR KEY to sign", nil, func(h *harness) { h.on(func() { h.s.Signing, h.s.Key.Hardware = true, true }) }},
		{"SIGNED OUT: the box refused the key (401) · r asks again", nil, refuse(model.FailSignedOut)},
		{"NOT SIGNED: r signs again", nil, refuse(model.FailNotSigned)},
		{"STOPPED: the certificate did not verify · r tries again", nil, refuse(model.FailCertificate)},
		{"STOPPED: answered about another key or box · r asks again", nil, func(h *harness) {
			h.fake.Body("whoami.json", otherKey)
			h.now = h.now.Add(model.MeAlways)
			h.pump()
		}},
		{"PAUSED (idle 10m): any key resumes", nil, func(h *harness) {
			h.now = h.now.Add(model.Idle)
			h.on(func() {})
		}},
		{"slowed by the box (429) until 15:12:40 UTC · 1h left", nil, func(h *harness) {
			failAll(h, &model.Failure{Kind: model.FailSlowed, Message: "slow", RetryAfter: time.Hour})
			h.press(tcell.KeyCtrlR)
		}},
		// The longest: a last answer 23 hours old.
		{"STALE · 23h ago: no answer since 14:12:40, retry in ", nil, func(h *harness) { silent(h, 23*time.Hour, "STALE") }},
		{"slow · 20s ago", nil, func(h *harness) { silent(h, 20*time.Second, "slow") }},
		{"back · ", nil, func(h *harness) {
			silent(h, 20*time.Second, "slow")
			failAll(h, nil)
			h.now = h.now.Add(10 * time.Second)
			h.pump()
		}},
		{"REFUSED: no read answered yet", func(f *demo.Fake) {
			f.Set("whoami", forbidden)
			f.Set("apps", forbidden)
		}, nil},
		{"refused · last answer ", nil, func(h *harness) {
			failAll(h, forbidden)
			h.now = h.now.Add(20 * time.Second)
			h.press(tcell.KeyCtrlR)
		}},
	} {
		h := start(t, 60, 24, c.setup)
		if c.get != nil {
			c.get(h)
		}
		_, right := h.s.Title(h.now)
		r := screenRows(h)[0]
		if !strings.HasSuffix(r.text, " "+right.Text()) || r.end != 60 {
			t.Errorf("%q (%d cells): not drawn whole at the edge of 60 columns: %q", right.Text(), len([]rune(right.Text())), r.text)
		}
		if !strings.HasPrefix(right.Text(), c.word) {
			t.Errorf("%q: the title's word is %q", c.word, right.Text())
		}
		t.Logf("%2d cells: %s", len([]rune(right.Text())), right.Text())
	}
}

// TestFailureSentenceWrapped: on who am I and the box screen a
// failure's sentence, the client's real ones, one quoting the box, is
// on screen whole at 60 and 80 columns, laid on rows at spaces, every
// row after its first indented, none past the window; labelled lines
// stay one row each. Read from the screen's cells.
func TestFailureSentenceWrapped(t *testing.T) {
	const timeout = "deploy.example.com timed out on port 443: no whole answer in 30 s. If moteld is down, as root on the box: systemctl status moteld; journalctl -u moteld -n 50. The box's off-box heartbeat also reports a stopped moteld."
	const busy = `deploy.example.com answered 409; the box says: "the app is being installed; the installer has not finished its work yet, try again in a minute or two"`
	for _, w := range []int{60, 80} {
		for _, c := range []struct {
			name, lead, why string
			screen          model.Screen
			key             string
			held            bool
		}{
			{"the box, no answer yet", "", timeout, model.ScreenBox, "box", false},
			{"the box, read before", "the last read failed: ", busy, model.ScreenBox, "box", true},
			{"who am I, no answer yet", "who you are is not known: ", busy, model.ScreenMe, "whoami", false},
			{"who am I, read before", "the last read failed: ", timeout, model.ScreenMe, "whoami", true},
		} {
			kind := model.FailThis
			if c.why == timeout {
				kind = model.FailNoAnswer
			}
			fail := &model.Failure{Kind: kind, Message: c.why}
			h := start(t, w, 40, func(f *demo.Fake) {
				if !c.held {
					f.Set(c.key, fail)
				}
			})
			h.open(model.View{Screen: c.screen})
			if c.held {
				h.fake.Set(c.key, fail)
				h.now = h.now.Add(model.MeAlways)
				h.press(tcell.KeyCtrlR)
			}
			rows := screenRows(h)[2:]
			name := c.name + " at " + strconv.Itoa(w)
			at := -1
			for i, r := range rows {
				if strings.HasPrefix(r.text, c.lead+strings.Fields(c.why)[0]) {
					at = i
				}
			}
			if at < 0 {
				t.Errorf("%s: no row begins the sentence:\n%s", name, h.screen())
				continue
			}
			parts := []string{rows[at].text}
			for _, r := range rows[at+1:] {
				if !strings.HasPrefix(r.text, "  ") || strings.HasPrefix(r.text, "   ") {
					break
				}
				parts = append(parts, strings.TrimPrefix(r.text, "  "))
			}
			if got := strings.Join(parts, " "); got != c.lead+c.why {
				t.Errorf("%s: the sentence on screen is\n%q\nwant\n%q", name, got, c.lead+c.why)
			}
			if len(parts) < 2 {
				t.Errorf("%s: the sentence is on one row: %q", name, parts[0])
			}
			for _, r := range rows {
				if r.end > w {
					t.Errorf("%s: a row past the window: %q", name, r.text)
				}
			}
			if c.screen == model.ScreenBox && c.held && !strings.HasPrefix(rows[0].text, "runs commit ") || c.screen == model.ScreenMe && c.held && !strings.HasPrefix(rows[0].text, "you are     ") {
				t.Errorf("%s: the labelled lines moved: %q", name, rows[0].text)
			}
		}
	}
}
