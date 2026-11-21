package tui

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/hostile"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// boxAlerts is a box's alerts body of rows, every time pinned: a body
// set with "@" times has them read from when it was set (demo.Fake's
// Body), which would make every alert in it another.
func boxAlerts(rows ...string) string {
	return `{"monitor":"on","alerts":[` + strings.Join(rows, ",") + `],"silences":[]}`
}

// alertRow is an active alert of the box's, firing since start.
func alertRow(name, severity string, start time.Time) string {
	b, _ := json.Marshal(map[string]string{"name": name, "severity": severity, "summary": "s", "state": "active", "starts_at": start.UTC().Format(time.RFC3339)})
	return string(b)
}

// watchNext moves the clock to the watch's next read and makes the
// reads due, a frame with each answer, and one more.
func (h *harness) watchNext() {
	h.now = h.now.Add(model.WatchEvery)
	h.press()
}

// bellTTY opens the TUI on the hostile package's recording terminal,
// the box's alerts as setup sets them, the bell on or off.
func bellTTY(t *testing.T, bell bool, setup func(*demo.Fake)) (*harness, *hostile.TTY) {
	t.Helper()
	tty := hostile.NewTTY()
	scr, err := hostile.Screen(tty)
	if err != nil {
		t.Fatal(err)
	}
	h := startOn(t, scr, setup)
	h.on(func() { h.u.Bell = bell })
	return h, tty
}

// TestBellSteps: the terminal's bell, read as bare BELs in what tcell
// writes: none at opening, though alerts fire; none for a read with
// nothing new; one for a read that brings a new alert; one owed inside
// the floor of a minute is held, and rung once when the floor ends;
// none on a resolve; none while the count is not known, and one when it
// is known again with something new against the last known set.
func TestBellSteps(t *testing.T) {
	old := t0.Add(-time.Hour)
	crit := alertRow("cert_failed", "critical", old)
	h, tty := bellTTY(t, true, func(f *demo.Fake) { f.Body("box-alerts.json", boxAlerts(crit)) })
	bells := func() int { return hostile.BareBELs(tty.Bytes()) }
	want := func(step string, n int) {
		t.Helper()
		if got := bells(); got != n {
			t.Errorf("%s: %d bells, want %d", step, got, n)
		}
	}
	want("at opening", 0)
	h.watchNext()
	want("nothing new", 0)

	disk := alertRow("disk_low", "warning", h.now)
	h.fake.Body("box-alerts.json", boxAlerts(disk, crit))
	h.watchNext()
	want("a new alert", 1)
	rang := h.now

	h.fake.Body("box-alerts.json", boxAlerts(alertRow("backup_stale", "warning", h.now), disk, crit))
	h.watchNext()
	want("another, inside the floor", 1)
	h.now = rang.Add(model.RingFloor - time.Second)
	h.sync()
	want("the floor's last second", 1)
	h.now = rang.Add(model.RingFloor)
	h.sync()
	want("the floor's end", 2)
	h.now = h.now.Add(time.Second)
	h.sync()
	want("a second after", 2)

	h.fake.Body("box-alerts.json", boxAlerts(crit))
	h.watchNext()
	want("a resolve", 2)

	h.now = h.now.Add(model.RingFloor)
	h.fake.Body("box-alerts.json", `{"monitor":"unanswered","alerts":[],"silences":[]}`)
	h.watchNext()
	want("not known", 2)
	h.fake.Set("box-alerts", &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
	h.watchNext()
	want("no answer", 2)
	h.fake.Set("box-alerts", nil)
	h.fake.Body("box-alerts.json", boxAlerts(alertRow("disk_low", "warning", h.now), crit))
	h.now = h.now.Add(10 * time.Second) // past the backoff
	h.watchNext()
	want("known again, one new", 3)
	if found := hostile.Scan(tty.Bytes()); !slices.Equal(found, []string{"bare BEL"}) {
		t.Errorf("signatures in what was written: %v", found)
	}
}

// TestBellOff: with the bell off (MOTEL_BELL=off) a new alert rings
// nothing, and its bell is still taken, so the floor counts the same:
// turned on within a minute, the next new alert's bell waits for the
// floor's end.
func TestBellOff(t *testing.T) {
	crit := alertRow("cert_failed", "critical", t0.Add(-time.Hour))
	h, tty := bellTTY(t, false, func(f *demo.Fake) { f.Body("box-alerts.json", boxAlerts(crit)) })
	disk := alertRow("disk_low", "warning", h.now)
	h.fake.Body("box-alerts.json", boxAlerts(disk, crit))
	h.watchNext()
	rang := h.now
	if n := hostile.BareBELs(tty.Bytes()); n != 0 {
		t.Fatalf("the bell off rang %d times", n)
	}
	h.on(func() { h.u.Bell = true })
	h.fake.Body("box-alerts.json", boxAlerts(alertRow("backup_stale", "warning", h.now), disk, crit))
	h.watchNext()
	if n := hostile.BareBELs(tty.Bytes()); n != 0 {
		t.Errorf("inside the floor of the bell taken while off, %d rang", n)
	}
	h.now = rang.Add(model.RingFloor)
	h.sync()
	if n := hostile.BareBELs(tty.Bytes()); n != 1 {
		t.Errorf("at the floor's end, %d rang", n)
	}
}

// reversed says which of the title bar's cells are drawn reversed, read
// from the screen, as a string of ^ under them.
func reversed(h *harness) string {
	var b strings.Builder
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() {
		defer close(done)
		w, _ := h.scr.Size()
		for x := 0; x < w; {
			_, st, cw := h.scr.Get(x, 0)
			_, _, a := st.Decompose()
			c := " "
			if a&tcell.AttrReverse != 0 {
				c = "^"
			}
			b.WriteString(strings.Repeat(c, max(cw, 1)))
			x += max(cw, 1)
		}
	})
	<-done
	return strings.TrimRight(b.String(), " ")
}

// TestMarkReversedInCells: the mark of a new alert, read from the
// screen's cells: reversed on an even second while it moves, plain on
// an odd one; reversed and still after MoveFor; plain after a key. Only
// the mark's own cells are reversed.
func TestMarkReversedInCells(t *testing.T) {
	crit := alertRow("cert_failed", "critical", t0.Add(-time.Hour))
	h := start(t, 80, 24, func(f *demo.Fake) { f.Body("box-alerts.json", boxAlerts(crit)) })
	if r := reversed(h); r != "" {
		t.Fatalf("reversed with nothing new: %q", r)
	}
	if h.now.Add(model.WatchEvery).Unix()%2 != 0 {
		h.now = h.now.Add(time.Second)
	}
	h.fake.Body("box-alerts.json", boxAlerts(alertRow("disk_low", "warning", h.now), crit))
	h.watchNext()
	at := h.now
	mark := "✖ 2 firing, 1 new"
	if top := screenRows(h)[0].text; !strings.HasPrefix(top, mark+" · motel") {
		t.Fatalf("the title bar is %q", top)
	}
	under := strings.Repeat("^", len([]rune(mark)))
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{0, under}, {time.Second, ""}, {2 * time.Second, under}, {3 * time.Second, ""}, {model.MoveFor, under}, {model.MoveFor + time.Second, under}, {model.MoveFor + 2*time.Second, under}} {
		h.now = at.Add(c.d)
		h.sync()
		if got := reversed(h); got != c.want {
			t.Errorf("%s after the arrival: reversed %q, want %q", c.d, got, c.want)
		}
	}
	h.press("x")
	if got := reversed(h); got != "" {
		t.Errorf("after a key: reversed %q", got)
	}
	if top := screenRows(h)[0].text; !strings.HasPrefix(top, "✖ 2 firing · motel") {
		t.Errorf("after a key the title bar is %q", top)
	}
}

// TestSafeBytesAlertArrives: an alert whose every field the box writes
// is made of the hostile fixture's attacks arrives: the bell rings once,
// the one bare BEL in what tcell writes, the mark carries only the
// client's count, and no attack reaches the terminal, on the screen it
// arrived on nor on the alerts screen, where its text is drawn inert.
func TestSafeBytesAlertArrives(t *testing.T) {
	// Each field one or more of the attacks, the one that names
	// evil.example first, so the alerts screen draws it before its cut at
	// the window's edge.
	var rest []string
	for k, p := range hostile.Payloads {
		if k != "osc8" && k != "urltag" && k != "tags" && k != "dcs" {
			rest = append(rest, p)
		}
	}
	slices.Sort(rest)
	p := hostile.Payloads
	h, tty := bellTTY(t, true, nil)
	b, err := json.Marshal(map[string]string{"name": p["urltag"], "severity": "critical", "summary": p["osc8"] + " " + strings.Join(rest, " "), "app": p["tags"], "instance": p["dcs"], "state": "active", "starts_at": h.now.UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	h.fake.Body("box-alerts.json", boxAlerts(string(b)))
	h.watchNext()
	if top := screenRows(h)[0].text; !strings.HasPrefix(top, "✖ 1 firing, 1 new · motel") {
		t.Errorf("the title bar is %q", top)
	}
	out := tty.Bytes()
	if found := hostile.Scan(out); !slices.Equal(found, []string{"bare BEL"}) || hostile.BareBELs(out) != 1 {
		t.Errorf("on arrival: signatures %v, %d bare BELs", found, hostile.BareBELs(out))
	}
	h.press(":", "alerts", tcell.KeyEnter)
	h.sync()
	out = tty.Bytes()
	if found := hostile.Scan(out); !slices.Equal(found, []string{"bare BEL"}) || hostile.BareBELs(out) != 1 {
		t.Errorf("on the alerts screen: signatures %v, %d bare BELs", found, hostile.BareBELs(out))
	}
	if !bytes.Contains(out, []byte(`evil.example`)) {
		t.Error("the inert attacks are not on the alerts screen")
	}
}

// TestDemoArrivals: --demo's script shows on the title bar at the
// watch's next read after each step, the fixture's own alerts never
// taken for new: a critical alert, then a warning one, both new until
// a key, then the first resolved.
func TestDemoArrivals(t *testing.T) {
	h := start(t, 100, 30, nil)
	bar := func() string { return strings.SplitN(screenRows(h)[0].text, " · motel", 2)[0] }
	if got := bar(); got != "✖ 2 firing" {
		t.Fatalf("at opening the mark is %q", got)
	}
	for i, want := range []string{"✖ 3 firing, 1 new", "✖ 4 firing, 2 new", "✖ 3 firing, 1 new"} {
		h.now = t0.Add(demo.Script[i].At)
		h.fake.Arrive(i)
		h.watchNext()
		if got := bar(); got != want {
			t.Errorf("after step %d the mark is %q, want %q", i, got, want)
		}
	}
	h.now = h.now.Add(2 * time.Second)
	h.press("x")
	if got := bar(); got != "✖ 3 firing" {
		t.Errorf("after a key the mark is %q", got)
	}
}

// beeps is a simulation screen that counts the bells rung on it.
type beeps struct {
	sizedSim
	n int
}

func (b *beeps) Beep() error { b.n++; return nil }

// TestBellWhileTooSmall: in a window under the smallest the screens lay
// out, a new alert still rings the bell.
func TestBellWhileTooSmall(t *testing.T) {
	crit := alertRow("cert_failed", "critical", t0.Add(-time.Hour))
	scr := &beeps{sizedSim: sizedSim{tcell.NewSimulationScreen("UTF-8"), 50, 10}}
	h := startOn(t, scr, func(f *demo.Fake) { f.Body("box-alerts.json", boxAlerts(crit)) })
	h.on(func() { h.u.Bell = true })
	h.fake.Body("box-alerts.json", boxAlerts(alertRow("disk_low", "warning", h.now), crit))
	h.watchNext()
	n := 0
	h.on(func() { n = scr.n })
	if n != 1 {
		t.Errorf("in a 50x10 window, %d bells for a new alert", n)
	}
}
