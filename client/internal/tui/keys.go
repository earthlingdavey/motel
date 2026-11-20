package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// binding is one key of one or more screens: the keys are bound from
// this table and help is written from it, so the two cannot differ
// (TestHelpIsTheKeys).
type binding struct {
	// keys and what are help's words for it.
	keys, what string
	// on are the screens it works on; overlay says it works only with
	// the command or JSON overlay open (help says so), or only with it
	// closed.
	on      []model.Screen
	overlay overlayRule
	match   func(ev *tcell.EventKey) bool
	// do acts; a key it returns goes on to the widget (a table's
	// arrows, a text view's scrolling).
	do func(u *UI, v model.View, ev *tcell.EventKey) *tcell.EventKey
}

type overlayRule int

const (
	either overlayRule = iota
	closed
	open
)

var (
	reading = []model.Screen{model.ScreenApps, model.ScreenApp, model.ScreenRecord, model.ScreenMe, model.ScreenBox, model.ScreenBackups, model.ScreenAlerts, model.ScreenMetrics}
	lists   = []model.Screen{model.ScreenApps, model.ScreenApp, model.ScreenRecord}
	browse  = []model.Screen{model.ScreenApps, model.ScreenApp, model.ScreenRecord, model.ScreenMe, model.ScreenBox, model.ScreenBackups, model.ScreenAlerts, model.ScreenMetrics, model.ScreenHelp}
	every   = append(append([]model.Screen{}, browse...), model.ScreenRefused)
	// stops are a stop screen and help over it, where nothing is asked
	// and so nothing opens: only scrolling, leaving help, r and q.
	stops = []model.Screen{model.ScreenRefused, model.ScreenStopHelp}
	all   = append(append([]model.Screen{}, every...), model.ScreenStopHelp)
)

func rune1(r rune) func(*tcell.EventKey) bool {
	return func(ev *tcell.EventKey) bool { return ev.Key() == tcell.KeyRune && ev.Rune() == r }
}

func keys(ks ...tcell.Key) func(*tcell.EventKey) bool {
	return func(ev *tcell.EventKey) bool {
		for _, k := range ks {
			if ev.Key() == k {
				return true
			}
		}
		return false
	}
}

func pass(_ *UI, _ model.View, ev *tcell.EventKey) *tcell.EventKey { return ev }

func done(f func(u *UI, v model.View)) func(*UI, model.View, *tcell.EventKey) *tcell.EventKey {
	return func(u *UI, v model.View, _ *tcell.EventKey) *tcell.EventKey {
		f(u, v)
		return nil
	}
}

// bindings is every key the screens take.
var bindings = []binding{
	{"enter", "open the app", []model.Screen{model.ScreenApps}, closed, keys(tcell.KeyEnter), pass},
	{"enter", "open the deploy's record", []model.Screen{model.ScreenApp}, closed, keys(tcell.KeyEnter), pass},
	{"d", "its newest deploy record if it failed, else the app", []model.Screen{model.ScreenApps}, closed, rune1('d'), done(func(u *UI, _ model.View) { u.openNewest() })},
	{"d", "the newest deploy's record", []model.Screen{model.ScreenApp}, closed, rune1('d'), done(func(u *UI, v model.View) {
		if ver, ok := u.S.Newest(v.App); ok {
			u.open(model.View{Screen: model.ScreenRecord, App: v.App, Version: ver})
		}
	})},
	{"g", "its numbers over time: memory, cpu, requests, 5xx share, p95", []model.Screen{model.ScreenApp}, closed, rune1('g'), done(func(u *UI, v model.View) { u.openMetrics(v.App) })},
	{"t", "another window: 1h, 24h, 7d, 14d", []model.Screen{model.ScreenMetrics}, closed, rune1('t'), done(func(u *UI, _ model.View) { u.nextRange() })},
	{"/", "filter by name; /! keeps only marked rows", []model.Screen{model.ScreenApps}, closed, rune1('/'), done(func(u *UI, _ model.View) { u.ask('/') })},
	{"s", "sort: attention, name, memory, 5xx", []model.Screen{model.ScreenApps}, closed, rune1('s'), done(func(u *UI, _ model.View) { u.sort++ })},
	{"w", "wrap long lines (off: a line stays a line)", []model.Screen{model.ScreenRecord}, closed, rune1('w'), done(func(u *UI, _ model.View) {
		u.wrap = !u.wrap
		u.tail.SetWrap(u.wrap)
	})},
	{"p n", "the previous (older) and next deploy's record", []model.Screen{model.ScreenRecord}, closed, func(ev *tcell.EventKey) bool { return rune1('p')(ev) || rune1('n')(ev) }, func(u *UI, _ model.View, ev *tcell.EventKey) *tcell.EventKey {
		if rune1('p')(ev) {
			u.step(1)
		} else {
			u.step(-1)
		}
		return nil
	}},
	{"j k ↑ ↓", "move (scroll the log tail on a record)", lists, closed, func(ev *tcell.EventKey) bool {
		return rune1('j')(ev) || rune1('k')(ev) || keys(tcell.KeyUp, tcell.KeyDown)(ev)
	}, func(_ *UI, _ model.View, ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case rune1('j')(ev):
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case rune1('k')(ev):
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	}},
	{"PgUp PgDn Home End", "move by a page, to the start or the end", all, either, keys(tcell.KeyPgUp, tcell.KeyPgDn, tcell.KeyHome, tcell.KeyEnd), pass},
	{"↑ ↓", "scroll", []model.Screen{model.ScreenMe, model.ScreenBox, model.ScreenBackups, model.ScreenAlerts, model.ScreenMetrics, model.ScreenHelp, model.ScreenRefused, model.ScreenStopHelp}, closed, keys(tcell.KeyUp, tcell.KeyDown), pass},
	{"↑ ↓", "scroll the overlay", reading, open, keys(tcell.KeyUp, tcell.KeyDown), pass},
	{"c", "the motel command for this view", reading, either, rune1('c'), done(func(u *UI, _ model.View) { u.overlay = 1; u.show() })},
	{"y", "what the box said, as JSON", reading, either, rune1('y'), done(func(u *UI, _ model.View) { u.overlay = 2; u.show() })},
	{"P", "quit and print it (safe on a terminal)", reading, open, rune1('P'), done(func(u *UI, v model.View) { u.printAndQuit(v) })},
	{"P", "quit, and print the person line below the prompt (a refused key)", []model.Screen{model.ScreenRefused}, either, rune1('P'), done(func(u *UI, v model.View) { u.printAndQuit(v) })},
	{"ctrl-r", "ask the box again now, for what this screen shows", reading, either, keys(tcell.KeyCtrlR), done(func(u *UI, v model.View) { u.S.Engine.Again(u.now(), u.shown(v)...) })},
	{"r", "ask again, with a token signed afresh (once in 5 s)", stops, either, rune1('r'), done(func(u *UI, _ model.View) { u.retry() })},
	{":", "go to a view (Tab completes)", browse, either, rune1(':'), done(func(u *UI, _ model.View) { u.ask(':') })},
	{"esc", "back one screen, or close the overlay; never quits", browse, either, keys(tcell.KeyEscape), done(func(u *UI, _ model.View) { u.back() })},
	{"esc", "back to why every read stopped", []model.Screen{model.ScreenStopHelp}, either, keys(tcell.KeyEscape), done(func(u *UI, _ model.View) { u.back() })},
	{"?", "the keys of this screen", every, either, rune1('?'), done(func(u *UI, v model.View) {
		if v.Screen == model.ScreenRefused {
			u.open(model.View{Screen: model.ScreenStopHelp})
			return
		}
		u.open(model.View{Screen: model.ScreenHelp})
	})},
	{"q", "quit", all, either, rune1('q'), done(func(u *UI, _ model.View) { u.App.Stop() })},
}

func (b binding) works(s model.Screen, overlay bool) bool {
	if b.overlay == open && !overlay || b.overlay == closed && overlay {
		return false
	}
	for _, o := range b.on {
		if o == s {
			return true
		}
	}
	return false
}

// keyHelp is help's list of a screen's keys, from the table.
func keyHelp(s model.Screen) string {
	var lines []string
	for _, k := range bindings {
		if k.overlay != open && !k.works(s, false) || k.overlay == open && !k.works(s, true) {
			continue
		}
		what := k.what
		if k.overlay == open {
			what += " (with c or y open)"
		}
		lines = append(lines, " "+pad(k.keys, 19)+" "+what)
	}
	return strings.Join(lines, "\n")
}

func pad(s string, n int) string {
	if r := len([]rune(s)); r < n {
		return s + strings.Repeat(" ", n-r)
	}
	return s
}
