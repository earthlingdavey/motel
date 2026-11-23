package model_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// drive makes the reads due at now, at most n, as the TUI's driver
// does: the engine records each, then its answer is applied.
func drive(s *model.Session, f *demo.Fake, now time.Time, n int) {
	for range n {
		j, ok, _ := s.Engine.Take(now)
		if !ok {
			return
		}
		apply, err := s.Fetch(context.Background(), f, j)
		s.Engine.Done(j, now, err)
		apply()
	}
}

// logsCalls is the cursor each logs read sent, in order.
func logsCalls(f *demo.Fake) []string {
	var out []string
	for _, c := range f.Calls {
		if c.Job.Kind == model.KLogs {
			out = append(out, c.After)
		}
	}
	return out
}

// logsBody is a logs answer of n lines, the first numbered from, each a
// second apart from at.
func logsBody(at time.Time, from, n int, more, gap bool, cursor string) string {
	l := wire.Logs{Kept: wire.KeptOn, Lines: []wire.LogLine{}, More: more, Gap: gap, Cursor: cursor}
	for i := range n {
		l.Lines = append(l.Lines, wire.LogLine{At: at.Add(time.Duration(from+i) * time.Second), Release: "7c1e55d0a913", Text: wire.AppText(fmt.Sprintf("line %d", from+i))})
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// logsOpen is a session over the fake, whoami and the list read, with
// the logs screen of app opened at now.
func logsOpen(t *testing.T, app string, setup func(*demo.Fake)) (*model.Session, *demo.Fake, *time.Time) {
	t.Helper()
	s, f, now := session(t)
	if setup != nil {
		setup(f)
	}
	drive(s, f, *now, 2)
	s.LogsOpen(app, *now)
	drive(s, f, *now, 20)
	return s, f, now
}

func logsText(s *model.Session, now time.Time, width, height int) string {
	return text(s.LogsViewFit("blog", now, width, height, runes, false))
}

// TestLogsScreenFollows: the logs screen reads a tail when it opens (the
// fixture's newest lines, older ones not sent said first), then every
// LogsEvery the lines after the cursor the last answer gave, each new
// line at the foot of the view; a follow that stopped with lines left
// reads again at once; kept off is said and nothing more is read, until
// ctrl-r.
func TestLogsScreenFollows(t *testing.T) {
	s, f, now := logsOpen(t, "blog", nil)
	if got := logsCalls(f); len(got) != 1 || got[0] != "" {
		t.Fatalf("opening read %q, want one tail", got)
	}
	v := logsText(s, *now, 100, 30)
	for _, want := range []string{"── following: a read every 2 s", "-- older lines were not sent: a read sends at most 200 lines", "-- 2026-11-15 UTC, release e9a0b3f2c118 --",
		"-- 2026-11-15 UTC, release not kept by the journal --", "-- 2026-11-15 UTC, release 7c1e55d0a913 --", "GET / 200 11ms"} {
		if !strings.Contains(v, want) {
			t.Errorf("no %q:\n%s", want, v)
		}
	}
	var first wire.Logs
	if err := json.Unmarshal([]byte(must(t, s, "blog")), &first); err != nil {
		t.Fatal(err)
	}
	f.Body("logs/blog.json", logsBody(*now, 0, 1, false, false, "c2"))
	drive(s, f, *now, 5)
	if n := len(logsCalls(f)); n != 1 {
		t.Errorf("%d reads within LogsEvery of the first", n)
	}
	*now = now.Add(model.LogsEvery)
	drive(s, f, *now, 5)
	if got := logsCalls(f); len(got) != 2 || got[1] != first.Cursor {
		t.Fatalf("the follow sent %q, want the tail's cursor %q", got, first.Cursor)
	}
	rows := strings.Split(strings.TrimRight(logsText(s, *now, 100, 30), "\n"), "\n")
	if last := rows[len(rows)-1]; !strings.HasSuffix(last, "line 0") {
		t.Errorf("the new line is not at the foot: %q", last)
	}
	// More after a follow: the next read at once, with its cursor.
	*now = now.Add(model.LogsEvery)
	f.Body("logs/blog.json", logsBody(*now, 1, 2, true, false, "c3"))
	drive(s, f, *now, 1)
	f.Body("logs/blog.json", logsBody(*now, 3, 1, false, false, "c4"))
	drive(s, f, *now, 5)
	if got := logsCalls(f); len(got) != 4 || got[2] != "c2" || got[3] != "c3" {
		t.Errorf("after more: %q, want c2 then c3 at once", got)
	}
	drive(s, f, now.Add(model.LogsEvery-100*time.Millisecond), 20)
	if n := len(logsCalls(f)); n != 4 {
		t.Errorf("after a read with no more, %d reads within LogsEvery", n)
	}
	// Kept off: said, and nothing more asked until ctrl-r.
	*now = now.Add(model.LogsEvery)
	f.Body("logs/blog.json", `{"kept":"off","lines":[],"more":false,"gap":false}`)
	for range 10 {
		drive(s, f, *now, 5)
		*now = now.Add(time.Second)
	}
	if n := len(logsCalls(f)); n != 5 {
		t.Errorf("%d reads, want 5: none after kept off", n)
	}
	v = logsText(s, *now, 0, 30)
	for _, want := range []string{"deploy.example.com keeps blog's output on the box (deploy_log_lines 0): it sends no line of it. Nothing more is asked.", "── not following", "line 3"} {
		if !strings.Contains(v, want) {
			t.Errorf("kept off: no %q:\n%s", want, v)
		}
	}
	s.LogsAgain(*now)
	s.Engine.Again(*now, model.Job{Kind: model.KLogs, App: "blog"})
	drive(s, f, *now, 5)
	if n := len(logsCalls(f)); n != 6 {
		t.Errorf("ctrl-r: %d reads, want 6", n)
	}
	// Left, nothing is read; opened again, a fresh tail.
	s.LogsShown("", *now)
	*now = now.Add(time.Minute)
	drive(s, f, *now, 20)
	if n := len(logsCalls(f)); n != 6 {
		t.Errorf("%d reads with the screen closed", n)
	}
	f.Body("logs/blog.json", logsBody(*now, 9, 1, false, false, "c9"))
	s.LogsOpen("blog", *now)
	drive(s, f, *now, 20)
	if got := logsCalls(f); len(got) != 7 || got[6] != "" {
		t.Errorf("opened again: %q, want a tail", got)
	}
	if v := logsText(s, *now, 100, 30); strings.Contains(v, "line 3") || !strings.Contains(v, "line 9") {
		t.Errorf("opened again, the lines of the last opening are kept:\n%s", v)
	}
}

func must(t *testing.T, s *model.Session, app string) string {
	t.Helper()
	b, _, ok := s.Body(model.View{Screen: model.ScreenLogs, App: app})
	if !ok {
		t.Fatal("no answer held")
	}
	return string(b)
}

// TestLogsNeedAGrant: without a logs grant on the app, whoami's answer
// says so, the screen says which grant, and nothing is asked; before
// whoami answers about this key, nothing is asked either. A grant whoami
// lists later starts the read.
func TestLogsNeedAGrant(t *testing.T) {
	viewer := `{"name":"alice@example.com","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`
	s, f, now := logsOpen(t, "blog", func(f *demo.Fake) { f.Body("whoami.json", viewer) })
	for range 10 {
		*now = now.Add(time.Second)
		drive(s, f, *now, 10)
	}
	if n := len(logsCalls(f)); n != 0 {
		t.Errorf("%d logs reads without a grant", n)
	}
	if v := logsText(s, *now, 0, 30); !strings.Contains(v, "reading blog's lines needs a logs grant on blog or on the box, and whoami (as of 14:00:00 UTC) lists none for this key here, so nothing is asked.") {
		t.Errorf("no grant:\n%s", v)
	}
	if _, _, have := s.Body(model.View{Screen: model.ScreenLogs, App: "blog"}); have {
		t.Error("an answer to show without a grant")
	}
	f.Body("whoami.json", strings.Replace(viewer, `"scope":"box"}`, `"scope":"box"},{"role":"logs","scope":"app:blog"}`, 1))
	*now = now.Add(model.AgainAfter)
	s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
	drive(s, f, *now, 10)
	if got := logsCalls(f); len(got) != 1 || got[0] != "" {
		t.Errorf("a grant listed: %q, want a tail", got)
	}
	// whoami not yet known: nothing asked.
	s2, f2, now2 := session(t)
	f2.Set("whoami", &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
	drive(s2, f2, *now2, 2)
	s2.LogsOpen("blog", *now2)
	drive(s2, f2, *now2, 10)
	if n := len(logsCalls(f2)); n != 0 {
		t.Errorf("%d logs reads before whoami answered", n)
	}
	if v := logsText(s2, *now2, 0, 30); !strings.Contains(v, "not known until whoami answers") {
		t.Errorf("whoami not known:\n%s", v)
	}
	if got := s2.NoAnswer(model.View{Screen: model.ScreenLogs, App: "blog"}); !strings.Contains(got, "before whoami has answered") {
		t.Errorf("y before whoami answered says %q", got)
	}
}

// TestLogsLayout: each row of the lines is a line's time, a divider or
// a note of the client's at the left edge, or, wrapped, the rest of a
// line or a note indented past where either begins; every control
// character an app wrote is an escape; cut at the edge, a line is
// marked.
func TestLogsLayout(t *testing.T) {
	s, _, now := logsOpen(t, "hostile", nil)
	for _, wrap := range []bool{false, true} {
		v := s.LogsViewFit("hostile", *now, 60, 200, runes, wrap)
		lines := 0
		for _, l := range v[1:] {
			r := l.Text()
			switch {
			case len(r) > 10 && r[2] == ':' && r[5] == ':' && r[8:10] == "  ":
				lines++
			case strings.HasPrefix(r, "-- "):
			case wrap && strings.HasPrefix(r, "          "):
			default:
				t.Errorf("wrap %v: a row that is no line, divider or note: %q", wrap, r)
			}
			if wrap && partEscape.MatchString(r) {
				t.Errorf("a row ends inside an escape: %q", r)
			}
			if strings.ContainsFunc(r, func(c rune) bool { return c < 0x20 || c == 0x7f || c >= 0x80 && c < 0xa0 || c == '\u202e' }) {
				t.Errorf("a control character reached the screen: %q", r)
			}
			if !wrap && utf8.RuneCountInString(r) > 60 {
				t.Errorf("a row wider than the window: %q", r)
			}
		}
		if lines != 10 {
			t.Errorf("wrap %v: %d lines' first rows, want 10", wrap, lines)
		}
		joined := text(v)
		if !strings.Contains(joined, `osc52 \x1b]52;c;`) || !strings.Contains(joined, `\u202elmth.exe`) {
			t.Errorf("wrap %v: the escapes are not shown:\n%s", wrap, joined)
		}
		if !wrap && !strings.Contains(joined, "xxxxxxxx…\n") {
			t.Errorf("a long line is not cut and marked:\n%s", joined)
		}
		for _, r := range strings.Split(joined, "\n") {
			if strings.Contains(r, "a line of its") && !strings.HasPrefix(r, "          ") {
				t.Errorf("wrap %v: a padded line's rest begins a row: %q", wrap, r)
			}
		}
	}
}

// partEscape is a row that ends with part of one of termtext's escapes.
var partEscape = regexp.MustCompile(`\\(x[0-9a-f]?|u[0-9a-f]{0,3})$`)

// TestLogsBudget: the logs screen's follow is counted in the budget the
// other apps share: with it open, a viewer of the box's other apps keep
// 30 s to 23 apps, as the README says, and not to 24.
func TestLogsBudget(t *testing.T) {
	for _, c := range []struct {
		n    int
		at30 bool
	}{{23, true}, {24, false}} {
		e := model.NewEngine(t0)
		e.SetWatch(model.WatchBox, t0)
		e.LogsOpen("app-0", t0)
		e.SetApps(names(c.n), t0)
		if got := e.Interval("app-1") == model.BackgroundEvery; got != c.at30 {
			t.Errorf("%d apps: every %s", c.n, e.Interval("app-1"))
		}
	}
}

// TestLogsHeldAndCapped: scrolled up, the view stays on the rows it
// showed while lines arrive, and the rule counts them; End follows
// again. Past LogsKeep lines the oldest are dropped, and said.
func TestLogsHeldAndCapped(t *testing.T) {
	s, f, now := logsOpen(t, "blog", func(f *demo.Fake) { f.Body("logs/blog.json", logsBody(t0, 0, 1000, false, false, "c1")) })
	view := func() []string {
		return strings.Split(strings.TrimRight(logsText(s, *now, 80, 12), "\n"), "\n")
	}
	s.LogsScroll("blog", -3, 80, runes, false)
	held := view()
	for i := 1; i <= 5; i++ {
		f.Body("logs/blog.json", logsBody(t0, 1000*i, 1000, false, false, fmt.Sprintf("c%d", i+1)))
		*now = now.Add(model.LogsEvery)
		drive(s, f, *now, 5)
	}
	got := view()
	if !strings.Contains(got[2], "held: 5,000 new lines since · End follows") {
		t.Errorf("the rule: %q", got[2])
	}
	if strings.Join(got[:2], " ") != "this screen holds the newest 5,000 lines, and at most 16 MiB of their text:   1,000 older ones were dropped." {
		t.Errorf("the head: %q", got[:2])
	}
	// What it showed was dropped: it holds at the oldest line held.
	if !strings.Contains(got[4], "line 1000") {
		t.Errorf("held past the cap, the view begins %q", got[3:5])
	}
	_ = held
	s.LogsEnd("blog")
	if got := view(); !strings.HasSuffix(got[len(got)-1], "line 5999") || !strings.Contains(got[2], "following") {
		t.Errorf("End: %q", got)
	}
	// Held within what is kept: the same rows, whatever arrives.
	s.LogsScroll("blog", -20, 80, runes, false)
	view()
	f.Body("logs/blog.json", logsBody(t0, 6000, 1, false, false, "c8"))
	*now = now.Add(model.LogsEvery)
	drive(s, f, *now, 5)
	if got := view(); !strings.Contains(got[2], "held: 1 new line since · End follows") {
		t.Errorf("one line since: %q", got[2])
	}
	s.LogsEnd("blog")
	view()
	s.LogsScroll("blog", -20, 80, runes, false)
	before := view()
	f.Body("logs/blog.json", logsBody(t0, 6001, 2, false, false, "c9"))
	*now = now.Add(model.LogsEvery)
	drive(s, f, *now, 5)
	after := view()
	if strings.Join(before[3:], "\n") != strings.Join(after[3:], "\n") || !strings.Contains(after[2], "held: 2 new lines since") {
		t.Errorf("held, the view moved:\n%s\n---\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	// Scrolled down to the newest row: following again.
	s.LogsScroll("blog", 100, 80, runes, false)
	if got := view(); !strings.Contains(got[2], "following") || !strings.HasSuffix(got[len(got)-1], "line 6002") {
		t.Errorf("scrolled to the end: %q", got)
	}
}

// TestLogsBounds: a row up with every row in view holds nothing; a
// head taller than the page is cut to it.
func TestLogsBounds(t *testing.T) {
	s, f, now := logsOpen(t, "blog", func(f *demo.Fake) { f.Body("logs/blog.json", logsBody(t0, 0, 3, false, false, "c1")) })
	_ = f
	logsText(s, *now, 80, 20)
	s.LogsScroll("blog", -1, 80, runes, false)
	s.LogsHome("blog", 80, runes, false)
	if v := logsText(s, *now, 80, 20); !strings.Contains(v, "── following") || !strings.Contains(v, "line 0") {
		t.Errorf("a row up, and Home, with every row in view:\n%s", v)
	}
	// y gives the newest answer that brought lines, not an empty follow.
	f.Body("logs/blog.json", logsBody(t0, 0, 0, false, false, ""))
	drive(s, f, now.Add(model.LogsEvery), 20)
	if b := must(t, s, "blog"); !strings.Contains(b, "line 2") {
		t.Errorf("y after an empty follow: %s", b)
	}
	// A step of the clock back moves its time too.
	v := model.View{Screen: model.ScreenLogs, App: "blog"}
	_, at0, _ := s.Body(v)
	s.LogsAsOf("blog", now.Add(-time.Hour))
	if _, at1, _ := s.Body(v); at0.Sub(at1) < time.Hour {
		t.Errorf("a step back of an hour moved y's answer by %s", at0.Sub(at1))
	}
	s.LogsShown("", *now)
	f.Set("logs:blog", &model.Failure{Kind: model.FailNoAnswer, Message: strings.Repeat("no answer ", 40)})
	s.LogsShown("blog", *now)
	drive(s, f, now.Add(model.LogsEvery), 20)
	if rows := s.LogsViewFit("blog", now.Add(model.LogsEvery), 20, 3, runes, false); len(rows) != 3 {
		t.Errorf("a head taller than the page: %d rows for 3", len(rows))
	}
}

// TestLogsFallenBehind: a follow whose reads at once have brought
// LogsKeep lines and still have more goes on from a tail, said once.
func TestLogsFallenBehind(t *testing.T) {
	s, f, now := logsOpen(t, "blog", func(f *demo.Fake) { f.Body("logs/blog.json", logsBody(t0, 0, 1000, true, false, "c0")) })
	*now = now.Add(model.LogsEvery)
	for i := 1; i <= 6; i++ {
		f.Body("logs/blog.json", logsBody(t0, 1000*i, 1000, true, false, fmt.Sprintf("c%d", i)))
		if i == 6 {
			// The tail after the skip: some of its lines were shown.
			f.Body("logs/blog.json", logsBody(t0, 5900, 200, true, false, "c6"))
		}
		// One read at a time, at the same time: the logs read among those
		// due, made at once.
		for n, k := len(logsCalls(f)), 0; len(logsCalls(f)) == n; k++ {
			if k == 30 {
				t.Fatalf("read %d not made at once: %q", i, logsCalls(f))
			}
			drive(s, f, *now, 1)
		}
	}
	if got := logsCalls(f); strings.Join(got, ",") != ",c0,c1,c2,c3,c4," {
		t.Errorf("the reads sent %q: a tail, five follows at once, then a tail", got)
	}
	v := logsText(s, *now, 0, 6000)
	skip := strings.Index(v, "-- lines skipped: this follow fell more than the 5,000 lines this screen holds behind the app")
	if skip < 0 || strings.Count(v, "-- lines skipped") != 1 || strings.Contains(v[skip:], "older lines were not sent") {
		t.Errorf("the notes: skipped at %d, %d times; the tail's own note after it: %v", skip, strings.Count(v, "-- lines skipped"), skip >= 0 && strings.Contains(v[skip:], "older lines were not sent"))
	}
	if strings.Count(v, "  line 5950\n") != 1 || !strings.Contains(v, "  line 6099\n") {
		t.Errorf("the tail after the skip: line 5950 %d times, line 6099 %v", strings.Count(v, "  line 5950\n"), strings.Contains(v, "  line 6099\n"))
	}
}

// TestLogsBusy: a busy box's 503 (FailBusy) has the logs read asked
// again after its wait, the rest of the session not slowed and the box
// not said to refuse; a gap is said among the lines.
func TestLogsBusy(t *testing.T) {
	s, f, now := logsOpen(t, "blog", nil)
	*now = now.Add(model.LogsEvery)
	f.Set("logs:blog", &model.Failure{Kind: model.FailBusy, Message: "deploy.example.com answered 503", RetryAfter: 5 * time.Second})
	drive(s, f, *now, 20)
	f.Set("logs:blog", nil)
	if n := len(logsCalls(f)); n != 2 {
		t.Fatalf("%d reads, want the tail and the busy one", n)
	}
	drive(s, f, now.Add(5*time.Second-100*time.Millisecond), 20)
	if n := len(logsCalls(f)); n != 2 {
		t.Errorf("read again within the box's 5 s")
	}
	if s.Engine.RateNow() != model.Rate {
		t.Errorf("the rate slowed to %v", s.Engine.RateNow())
	}
	if _, r := s.Title(*now); strings.Contains(r.Text(), "refused") || strings.Contains(r.Text(), "slowed") {
		t.Errorf("a busy box said in the title bar: %q", r.Text())
	}
	if v := logsText(s, *now, 0, 30); !strings.Contains(v, "the last read failed: deploy.example.com answered 503") {
		t.Errorf("busy:\n%s", v)
	}
	f.Body("logs/blog.json", logsBody(*now, 0, 1, false, true, "c7"))
	*now = now.Add(5 * time.Second)
	drive(s, f, *now, 5)
	if v := logsText(s, *now, 0, 30); !strings.Contains(v, "-- a gap: the box no longer holds the line") || strings.Contains(v, "the last read failed") {
		t.Errorf("a gap:\n%s", v)
	}
	// Ten idle minutes: the reads pause, and the rule says so.
	*now = now.Add(model.Idle)
	if v := logsText(s, *now, 0, 30); !strings.Contains(v, "── paused (idle 10m): any key reads on") {
		t.Errorf("paused:\n%s", v)
	}
}

// TestLogsEmptyTailWithOlder: a tail that sent no line but says older
// ones were left is not said as a journal holding none.
func TestLogsEmptyTailWithOlder(t *testing.T) {
	s, _, now := logsOpen(t, "blog", func(f *demo.Fake) { f.Body("logs/blog.json", logsBody(t0, 0, 0, true, false, "")) })
	if v := logsText(s, *now, 0, 30); strings.Contains(v, "holds no line") || !strings.Contains(v, "older lines were not sent") {
		t.Errorf("an empty tail with older lines:\n%s", v)
	}
}

// TestLogsGrantBackAfterAnotherApp: a read refused (403), its grant
// removed, then given back while another app's logs were open, is asked
// again when whoami lists the grant once more.
func TestLogsGrantBackAfterAnotherApp(t *testing.T) {
	who := func(roles string) string {
		return `{"name":"alice@example.com","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[` + roles + `],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`
	}
	grants := `{"role":"viewer","scope":"box"},{"role":"logs","scope":"app:blog"},{"role":"logs","scope":"app:shop"}`
	s, f, now := logsOpen(t, "blog", func(f *demo.Fake) {
		f.Body("whoami.json", who(grants))
		f.Set("logs:blog", &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this"})
	})
	whoami := func(body string) {
		f.Body("whoami.json", body)
		*now = now.Add(model.AgainAfter)
		s.Engine.Again(*now, model.Job{Kind: model.KWhoAmI})
		drive(s, f, *now, 20)
	}
	whoami(who(`{"role":"viewer","scope":"box"},{"role":"logs","scope":"app:shop"}`))
	s.LogsOpen("shop", *now)
	drive(s, f, *now, 20)
	f.Set("logs:blog", nil)
	whoami(who(grants))
	s.LogsOpen("blog", *now)
	drive(s, f, *now, 20)
	whoami(who(grants))
	var blog int
	for _, c := range f.Calls {
		if c.Job.Kind == model.KLogs && c.Job.App == "blog" {
			blog++
		}
	}
	if blog != 2 {
		t.Errorf("blog's lines read %d times, want the refused one and one when the grant came back", blog)
	}
}

// TestLogsRefusedAfterReopening: a refusal applied after the screen was
// opened afresh (the read recorded, then esc and l before its answer is
// applied) is said, since the engine holds the read; and a note the
// same as the last is not written twice.
func TestLogsRefusedAfterReopening(t *testing.T) {
	s, f, now := logsOpen(t, "blog", nil)
	*now = now.Add(model.LogsEvery)
	f.Set("logs:blog", &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this"})
	j := model.Job{Kind: model.KLogs, App: "blog"}
	for range 20 {
		k, ok, _ := s.Engine.Take(*now)
		if !ok {
			t.Fatal("the logs read was not taken")
		}
		apply, err := s.Fetch(context.Background(), f, k)
		s.Engine.Done(k, *now, err)
		if k == j {
			s.LogsShown("", *now)
			s.LogsOpen("blog", *now)
			apply()
			break
		}
		apply()
	}
	if v := logsText(s, *now, 0, 30); !strings.Contains(v, "does not allow your key this") {
		t.Errorf("a refusal applied after reopening:\n%s", v)
	}
	// Empty gaps in a row, with older lines left or not: each note once.
	f.Set("logs:blog", nil)
	for _, more := range []bool{false, true} {
		s2, f2, now2 := logsOpen(t, "blog", nil)
		for i := range 3 {
			f2.Body("logs/blog.json", logsBody(t0, 0, 0, more, true, fmt.Sprintf("g%d", i)))
			*now2 = now2.Add(model.LogsEvery)
			drive(s2, f2, *now2, 20)
		}
		v := logsText(s2, *now2, 0, 60)
		if n, o := strings.Count(v, "-- a gap"), strings.Count(v, "-- older lines were not sent"); n != 1 || o > 2 {
			t.Errorf("more %v: %d gap notes, %d older-lines notes for three empty gaps in a row:\n%s", more, n, o, v)
		}
	}
}

// TestLogsAgainOnARefusal: ctrl-r held on a refused logs read asks it
// once in AgainAfter, not at the read's own 2 s.
func TestLogsAgainOnARefusal(t *testing.T) {
	s, f, now := logsOpen(t, "blog", func(f *demo.Fake) {
		f.Set("logs:blog", &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this"})
	})
	j := model.Job{Kind: model.KLogs, App: "blog"}
	end := now.Add(10 * time.Second)
	for ; now.Before(end); *now = now.Add(100 * time.Millisecond) {
		s.LogsAgain(*now)
		s.Engine.Again(*now, j)
		drive(s, f, *now, 5)
	}
	if n := len(logsCalls(f)); n > 3 {
		t.Errorf("%d refused reads in 10 s of ctrl-r, want one in 5 s", n)
	}
}

// TestLogsFailureReopened: opened afresh while the engine waits out a
// failed read's backoff, the screen says the failure, not that it asks;
// a refusal followed by another failure says the newer.
func TestLogsFailureReopened(t *testing.T) {
	s, f, now := logsOpen(t, "blog", func(f *demo.Fake) {
		f.Set("logs:blog", &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this"})
	})
	f.Set("logs:blog", &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com did not answer"})
	*now = now.Add(model.AgainAfter)
	s.LogsAgain(*now)
	s.Engine.Again(*now, model.Job{Kind: model.KLogs, App: "blog"})
	for range 3 {
		drive(s, f, *now, 20)
		*now = now.Add(4 * time.Second)
	}
	s.LogsShown("", *now)
	s.LogsOpen("blog", *now)
	drive(s, f, *now, 20)
	v := logsText(s, *now, 0, 30)
	if !strings.Contains(v, "did not answer") || strings.Contains(v, "asking") || strings.Contains(v, "does not allow") {
		t.Errorf("opened afresh during a backoff, after a refusal:\n%s", v)
	}
}

// TestLogsBusyThroughARestart: while moteld restarts, every read gets
// no answer and the logs read the 503 a starting moteld sends with
// Retry-After (FailBusy, as a busy box's): the run of no answer goes on,
// whoami is not asked at each logs read, and the title bar does not say
// back.
func TestLogsBusyThroughARestart(t *testing.T) {
	e := model.NewEngine(t0)
	e.LogsOpen("blog", t0)
	busy := &model.Failure{Kind: model.FailBusy, Message: "deploy.example.com answered 503", RetryAfter: time.Second}
	calls := sim(e, names(2), t0, 2*time.Minute, 10*time.Millisecond, func(j model.Job, _ time.Time) error {
		if j.Kind == model.KLogs {
			return busy
		}
		return fail(model.FailNoAnswer)
	})
	who := 0
	for _, c := range calls {
		if c.j.Kind == model.KWhoAmI {
			who++
		}
	}
	if h := e.Health(t0.Add(2 * time.Minute)); h.Misses == 0 || who > 4 {
		t.Errorf("through a restart: %d misses in the run, whoami asked %d times in 2 minutes", h.Misses, who)
	}
}

// TestLogsStopNotKept: a logs read that stopped every read (a 401) is
// not kept as the logs screen's failure: the stop screen says it.
func TestLogsStopNotKept(t *testing.T) {
	s, f, now := logsOpen(t, "blog", nil)
	f.Set("logs:blog", &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com refused the key (401)"})
	*now = now.Add(model.LogsEvery)
	drive(s, f, *now, 20)
	if s.Engine.Stopped() == model.Running {
		t.Fatal("the 401 did not stop the reads")
	}
	f.Set("logs:blog", nil)
	s.LogsShown("", *now)
	s.LogsOpen("blog", *now)
	if v := logsText(s, *now, 0, 30); strings.Contains(v, "401") {
		t.Errorf("a stop said as the logs read's failure:\n%s", v)
	}
}

// TestLogsRefusedReopened: a logs read the box refused (a 404 from a
// moteld older than the read) is said, and said again when the screen
// is opened afresh, with nothing asked, until ctrl-r asks.
func TestLogsRefusedReopened(t *testing.T) {
	s, f, now := logsOpen(t, "blog", func(f *demo.Fake) {
		f.Set("logs:blog", &model.Failure{Kind: model.FailNotFound, Message: `deploy.example.com answered 404; the box says: "not found"`})
	})
	for range 2 {
		if v := logsText(s, *now, 0, 30); !strings.Contains(v, "answered 404") || !strings.Contains(v, "a moteld older than this read") {
			t.Errorf("the refusal:\n%s", v)
		}
		s.LogsShown("", *now)
		*now = now.Add(time.Minute)
		s.LogsOpen("blog", *now)
		drive(s, f, *now, 20)
	}
	if n := len(logsCalls(f)); n != 1 {
		t.Errorf("%d reads, want the one refused", n)
	}
	f.Set("logs:blog", nil)
	s.LogsAgain(*now)
	s.Engine.Again(*now, model.Job{Kind: model.KLogs, App: "blog"})
	drive(s, f, *now, 20)
	if v := logsText(s, *now, 0, 30); len(logsCalls(f)) != 2 || strings.Contains(v, "404") || !strings.Contains(v, "GET / 200 11ms") {
		t.Errorf("ctrl-r: %d reads:\n%s", len(logsCalls(f)), v)
	}
}
