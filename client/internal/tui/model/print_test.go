package model_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// appsBody is a list the fake answers with, padded to indent to lines
// lines: nine of its own, and one for each element of pad.
func appsBody(lines int) string {
	pad := strings.TrimSuffix(strings.Repeat("1,", lines-9), ",")
	return `{"apps":[{"name":"blog"}],"pad":[` + pad + `]}`
}

func overlayText(s *model.Session, v model.View) []string {
	var out []string
	for _, l := range s.Overlay(v, true) {
		out = append(out, l.Text())
	}
	return out
}

// TestPrintForATerminal: what P prints of the box's answer for a
// terminal is the overlay's lines, made safe, every one and whole; the
// bytes as sent stay what Printable gives, for a pipe or a file. A C1
// control and a bidi override, which JSON lets a string hold raw, reach
// neither the overlay nor the terminal form raw.
func TestPrintForATerminal(t *testing.T) {
	s, f, now := session(t)
	long := strings.Repeat("x", model.TailMax+10)
	body := `{"apps":[{"name":"blog"}],"note":"c1 ` + "\u009b" + `31m bidi ` + "\u202e" + `abc","long":"` + long + `","pad":[` + strings.TrimSuffix(strings.Repeat("1,", model.OverlayLines+5), ",") + `]}`
	f.Body("apps.json", body)
	for range 3 {
		step(t, s, f, now)
	}
	v := model.View{Screen: model.ScreenApps}
	raw, ok := s.Printable(v, 2)
	if !ok || string(raw) != body+"\n" {
		t.Fatalf("P's bytes: %d, want the body and a line feed", len(raw))
	}
	shown, ok := s.PrintShown(v, 2)
	if !ok || shown != model.JSONShown([]byte(body)) {
		t.Fatalf("PrintShown is not JSONShown of the body")
	}
	if strings.ContainsAny(shown, "\u009b\u202e\x1b") || !strings.Contains(shown, `c1 \u009b31m bidi \u202eabc`) {
		t.Errorf("the controls in the terminal form: %q", shown[:200])
	}
	lines := strings.Split(strings.TrimSuffix(shown, "\n"), "\n")
	if want := 9 + 2 + model.OverlayLines + 5; len(lines) != want || !strings.HasSuffix(shown, "}\n") {
		t.Errorf("%d lines, want %d, every one", len(lines), want)
	}
	if !strings.Contains(shown, `"long": "`+long+`"`) {
		t.Error("the long line is cut in the terminal form")
	}
	for _, l := range overlayText(s, v) {
		if strings.ContainsAny(l, "\u009b\u202e") {
			t.Errorf("the overlay holds a control: %q", l)
		}
	}
	if _, ok := s.PrintShown(v, 1); ok {
		t.Error("the command line has a terminal form")
	}
}

// TestPrintLineFeed: P prints the body as sent and adds a line feed
// only when it ends without one.
func TestPrintLineFeed(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"apps":[{"name":"blog"}]}`, `{"apps":[{"name":"blog"}]}` + "\n"},
		{`{"apps":[{"name":"blog"}]}` + "\n", `{"apps":[{"name":"blog"}]}` + "\n"},
		{`{"apps":[{"name":"blog"}]}` + "\n\n", `{"apps":[{"name":"blog"}]}` + "\n\n"},
	} {
		s, f, now := session(t)
		f.Body("apps.json", c.body)
		for range 3 {
			step(t, s, f, now)
		}
		if p, ok := s.Printable(model.View{Screen: model.ScreenApps}, 2); !ok || string(p) != c.want {
			t.Errorf("%q printed %q, want %q", c.body, p, c.want)
		}
	}
}

// TestMoreLinesBoundary: an answer of OverlayLines lines is drawn
// whole; one more, and the overlay says exactly how many are not drawn.
func TestMoreLinesBoundary(t *testing.T) {
	if model.OverlayLines != 2000 {
		t.Fatalf("OverlayLines is %d", model.OverlayLines)
	}
	for _, c := range []struct {
		lines int
		more  string
	}{{2000, ""}, {2001, "… 1 more lines not drawn here"}, {2003, "… 3 more lines not drawn here"}} {
		s, f, now := session(t)
		f.Body("apps.json", appsBody(c.lines))
		for range 3 {
			step(t, s, f, now)
		}
		body, _, _ := s.Body(model.View{Screen: model.ScreenApps})
		all, cut, _ := model.JSONLines(body)
		if n := len(all) + cut; n != c.lines {
			t.Fatalf("the body indents to %d lines, want %d", n, c.lines)
		}
		text := overlayText(s, model.View{Screen: model.ScreenApps})
		last := text[len(text)-1]
		switch {
		case c.more == "" && strings.Contains(last, "more lines"):
			t.Errorf("%d lines: %q", c.lines, last)
		case c.more != "" && !strings.HasPrefix(last, c.more):
			t.Errorf("%d lines: the last line %q, want %q", c.lines, last, c.more)
		case c.more == "" && last != "}":
			t.Errorf("%d lines: drawn to %q, not the end", c.lines, last)
		}
		if drawn := cut == 0; drawn != (c.lines <= model.OverlayLines) {
			t.Errorf("%d lines: cut %s", c.lines, strconv.Itoa(cut))
		}
	}
}
