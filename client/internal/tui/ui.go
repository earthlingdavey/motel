// Package tui is the screens `motel` alone opens: k9s-style, over the
// model. It sees the box only through model, which reads it only
// through the Client it is given, and it hands tview no text but through
// boxtext.go. It acts on nothing: every screen reads.
package tui

import (
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/client/internal/tui/door"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// The smallest screen the TUI lays out; below it, it says what it
// needs.
const (
	minW, minH = 60, 16
)

// UI is the screens. Its fields are touched only on tview's event
// goroutine.
type UI struct {
	App *tview.Application
	S   *model.Session
	// Colour is false with NO_COLOR or a terminal without colour:
	// every meaning is still carried by a mark or a word.
	Colour bool
	// Print is what to print below the prompt once the TUI ends (P), as
	// the command's --json prints it; Shown, the box's answer indented
	// and made safe, for a stdout that is a terminal ("" when P printed
	// only the client's words).
	Print, Shown string

	stack       []model.View
	overlay     int // 0 none, 1 command, 2 JSON
	filter      string
	sort        int
	wrap        bool
	w, h        int
	lastFocused string
	// inFront is the view show last put in front.
	inFront model.View
	// notice is what the : line said of what was typed, shown in the
	// crumb's place until the next key.
	notice model.Line
	// overKey is what the overlay was last made of.
	overKey overKey
	// ended is closed once the event loop has ended.
	ended    chan struct{}
	panicked atomic.Pointer[panicked]
	// Resign drops the token last signed, so r signs afresh: a token
	// the box refused for its times is not sent again.
	Resign func()

	root, body                  *tview.Pages
	title, crumb, foot          *tview.TextView
	prompt                      *tview.InputField
	row2                        *tview.Pages
	strip                       *tview.TextView
	apps                        *tview.Table
	appsFlex, appFlex, recFlex  *tview.Flex
	fields, onDisk              *tview.TextView
	deploys                     *tview.Table
	recHead, recTailTitle, tail *tview.TextView
	page, over                  *tview.TextView
	appRows                     []model.AppRow
	deployRows                  []model.DeployRow
	promptMode                  rune
	// metricsRange is the window the metrics screen last showed: g
	// opens it again.
	metricsRange string
	// Bell rings the terminal's bell for a read that brings a new alert
	// (Session.Ring): on unless MOTEL_BELL=off.
	Bell bool
}

func textView() *tview.TextView {
	return tview.NewTextView().SetDynamicColors(true).SetWrap(false)
}

// New builds the screens, opening on start. An app's screen opens with
// the apps list under it, so esc goes back to the list, as the footer
// says, and an app granted later is reached from there.
func New(s *model.Session, colour bool, start model.View) *UI {
	door.TerminalColours()
	stack := []model.View{start}
	if start.Screen == model.ScreenApp {
		stack = []model.View{{Screen: model.ScreenApps}, start}
	}
	u := &UI{App: tview.NewApplication(), S: s, Colour: colour, stack: stack, ended: make(chan struct{})}
	u.title, u.crumb, u.foot = textView(), textView(), textView()
	u.prompt = tview.NewInputField().SetFieldStyle(tcell.StyleDefault).SetLabelStyle(tcell.StyleDefault).
		SetAutocompleteUseTags(false)
	u.prompt.SetAutocompleteStyles(tcell.ColorDefault, tcell.StyleDefault, tcell.StyleDefault.Reverse(true))
	door.SetCompletions(u.prompt, u.complete)
	u.prompt.SetChangedFunc(func(string) {
		if u.promptMode == '/' {
			u.filter = door.Typed(u.prompt)
		}
	})
	u.prompt.SetDoneFunc(u.promptDone)
	u.row2 = tview.NewPages().AddPage("crumb", u.crumb, true, true).AddPage("prompt", u.prompt, true, false)

	selected := tcell.StyleDefault.Reverse(true)
	u.strip = textView()
	u.apps = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0).SetSelectedStyle(selected)
	u.apps.SetSelectedFunc(func(r, _ int) { u.openApp(r) })
	u.appsFlex = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.strip, 1, 0, false).AddItem(u.apps, 0, 1, true)

	u.fields, u.onDisk = textView(), textView()
	u.deploys = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0).SetSelectedStyle(selected)
	u.deploys.SetSelectedFunc(func(r, _ int) {
		if r >= 1 && r-1 < len(u.deployRows) {
			u.open(model.View{Screen: model.ScreenRecord, App: u.top().App, Version: u.deployRows[r-1].Version})
		}
	})
	u.appFlex = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.fields, 1, 0, false).
		AddItem(u.onDisk, 1, 0, false).AddItem(u.deploys, 0, 1, true)

	u.recHead, u.recTailTitle, u.tail = textView(), textView(), textView()
	u.recFlex = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.recHead, 1, 0, false).
		AddItem(u.recTailTitle, 1, 0, false).AddItem(u.tail, 0, 1, true)

	u.page, u.over = textView(), textView()
	u.page.SetWrap(true).SetWordWrap(true)
	u.over.SetBorder(true)
	u.body = tview.NewPages().
		AddPage("apps", u.appsFlex, true, false).AddPage("app", u.appFlex, true, false).
		AddPage("record", u.recFlex, true, false).AddPage("page", u.page, true, false)
	frame := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.title, 1, 0, false).
		AddItem(u.row2, 1, 0, false).AddItem(u.body, 0, 1, true).AddItem(u.foot, 1, 0, false)
	u.root = tview.NewPages().AddPage("frame", frame, true, true).
		AddPage("overlay", tview.NewFlex().SetDirection(tview.FlexRow).AddItem(nil, 2, 0, false).
			AddItem(u.over, 0, 1, true).AddItem(nil, 1, 0, false), true, false)
	u.App.SetRoot(u.root, true).SetInputCapture(u.key)
	u.App.SetBeforeDrawFunc(func(scr tcell.Screen) bool {
		u.w, u.h = scr.Size()
		now := u.now()
		// The bell owed, at the frame that shows its mark, or says the
		// window is too small for it: taken whether or not it rings, so
		// its floor counts the same with it off. A lone BEL written by
		// tcell, outside the frame's own bytes.
		defer func() {
			if u.S.Ring(now) && u.Bell {
				_ = scr.Beep()
			}
		}()
		if u.w < minW || u.h < minH {
			// Below this nothing lays out; say what is needed instead.
			door.PrintAt(scr, 0, 0, u.w, "window too small: the screen needs "+itoa(minW)+"x"+itoa(minH)+
				", this one is "+itoa(u.w)+"x"+itoa(u.h)+". q quits.")
			return true
		}
		u.render(now)
		return false
	})
	u.show()
	u.stopped()
	return u
}

func itoa(n int) string { return strconv.Itoa(n) }

func (u *UI) top() model.View { return u.stack[len(u.stack)-1] }

func (u *UI) now() time.Time { return u.S.Now() }

// open pushes a view and shows it.
func (u *UI) open(v model.View) {
	if v == u.top() {
		return
	}
	u.stack = append(u.stack, v)
	u.overlay = 0
	u.show()
}

// back is esc: an overlay closes, a prompt closes, or one screen goes
// back. It never quits.
func (u *UI) back() {
	switch {
	case u.overlay != 0:
		u.overlay = 0
	case len(u.stack) > 1:
		v := u.top()
		if v.Screen == model.ScreenRecord {
			u.S.Engine.Drop(model.Job{Kind: model.KDeploy, App: v.App, Version: v.Version})
		}
		u.stack = u.stack[:len(u.stack)-1]
		if b := u.top(); v.Screen == model.ScreenRecord && b.Screen == model.ScreenApp && b.App == v.App {
			// The cursor goes to the record left. d and the : line open
			// the record with its app's screen, never drawn between: the
			// table may still hold another app's rows and cursor.
			// renderApp keeps it on these rows' version.
			rows := u.S.Overview(v.App, u.now()).Deploys
			if i := u.deployRow(rows, v.Version); i >= 0 {
				u.deployRows = rows
				u.deploys.Select(i+1, 0)
			}
		}
	}
	u.show()
}

// show puts the top view's page in front, and tells the engine what to
// read for it.
func (u *UI) show() {
	v, now := u.top(), u.now()
	focus := ""
	switch v.Screen {
	case model.ScreenApps:
		u.body.SwitchToPage("apps")
		u.App.SetFocus(u.apps)
	case model.ScreenApp:
		focus = v.App
		u.body.SwitchToPage("app")
		u.App.SetFocus(u.deploys)
	case model.ScreenMetrics:
		// The app's status goes on being read: its deploys are the
		// plots' marks.
		focus = v.App
		u.body.SwitchToPage("page")
		u.page.ScrollToBeginning()
		u.App.SetFocus(u.page)
	case model.ScreenRecord:
		focus = v.App
		if v != u.inFront {
			// Opened, not shown again for an overlay or the : line: read
			// again, since the box may have rewritten it.
			u.S.Engine.Once(model.Job{Kind: model.KDeploy, App: v.App, Version: v.Version}, now)
		}
		u.body.SwitchToPage("record")
		u.tail.ScrollToBeginning()
		u.App.SetFocus(u.tail)
	default:
		u.body.SwitchToPage("page")
		u.page.ScrollToBeginning()
		u.App.SetFocus(u.page)
	}
	if focus != u.lastFocused {
		u.S.Engine.Focus(focus, now)
		u.lastFocused = focus
	}
	u.inFront = v
	u.S.Engine.BoxOpen(v.Screen == model.ScreenBox, now)
	u.S.Engine.BackupsOpen(v.Screen == model.ScreenBackups || v.Screen == model.ScreenBox, now)
	alerts := ""
	if v.Screen == model.ScreenApp {
		alerts = v.App
	}
	u.S.Engine.AlertsOpen(alerts, now)
	if v.Screen == model.ScreenMetrics {
		u.S.Engine.SeriesOpen(v.App, v.Range, now)
	} else {
		u.S.Engine.SeriesOpen("", "", now)
	}
	u.S.Engine.BoxAlertsOpen(v.Screen == model.ScreenAlerts || v.Screen == model.ScreenBox, now)
	u.S.Engine.BoxRulesOpen(v.Screen == model.ScreenRules, now)
	rules := ""
	if v.Screen == model.ScreenAppRules {
		rules = v.App
	}
	u.S.Engine.RulesOpen(rules, now)
	u.S.Engine.MeOpen(v.Screen == model.ScreenMe, now)
	if u.overlay != 0 {
		u.root.ShowPage("overlay")
		u.over.ScrollToBeginning()
		u.App.SetFocus(u.over)
	} else {
		u.root.HidePage("overlay")
	}
}

// key is every key pressed, before any widget: what it means is the
// same on every screen, but for the screen's own keys.
func (u *UI) key(ev *tcell.EventKey) *tcell.EventKey {
	now := u.now()
	u.S.Engine.Touch(now)
	// Any key sees what arrived a second or more before it, the one that
	// resumes from PAUSED among them.
	u.S.Seen(now)
	u.notice = nil
	if ev.Key() == tcell.KeyCtrlC {
		return ev // tview's own: it quits from inside its event loop
	}
	if u.App.GetFocus() == u.prompt {
		// Enter runs what is typed, exactly: a completion is taken with
		// Tab only, so an open list cannot replace what Enter runs.
		if ev.Key() == tcell.KeyEnter {
			u.promptDone(tcell.KeyEnter)
			return nil
		}
		return ev
	}
	// Only the keys the table binds for this screen do anything: help
	// is written from the same table.
	v := u.top()
	for _, b := range bindings {
		if b.works(v.Screen, u.overlay != 0) && b.match(ev) {
			out := b.do(u, v, ev)
			u.stopped()
			return out
		}
	}
	return nil
}

// openNewest is d: the app's screen, and its newest deploy's record
// when that failed.
func (u *UI) openNewest() {
	if row, _ := u.apps.GetSelection(); row >= 1 && row-1 < len(u.appRows) {
		a := u.appRows[row-1]
		u.open(model.View{Screen: model.ScreenApp, App: a.Name})
		if a.Failed != "" {
			u.open(model.View{Screen: model.ScreenRecord, App: a.Name, Version: a.Failed})
		}
	}
}

// printAndQuit is P: what there is to print, printed once the screen
// is gone; nothing where there is nothing.
func (u *UI) printAndQuit(v model.View) {
	if p, ok := u.S.Printable(v, u.overlay); ok {
		u.Print = string(p)
		u.Shown, _ = u.S.PrintShown(v, u.overlay)
		u.App.Stop()
	}
}

// retry is r on a stop screen: the token forgotten, then one whoami.
// Forgotten first: Retry wakes the reads, and the whoami must not go
// out with the refused token; held to one in RetryEvery.
func (u *UI) retry() {
	if now := u.now(); u.S.Engine.RetryReady(now) {
		if u.Resign != nil {
			u.Resign()
		}
		u.S.Engine.Retry(now)
	}
}

// shown is the reads behind what v shows, for ctrl-r: the list and every
// app's row; an app's status; a record, and the status beside it;
// whoami; the box, its backups and its alerts (the box screen has a
// line of each); the backups; the alerts; the rules, the box's or an
// app's; an app's series over the window shown, and its status; nothing
// on help.
func (u *UI) shown(v model.View) []model.Job {
	st := func(a string) model.Job { return model.Job{Kind: model.KStatus, App: a} }
	switch v.Screen {
	case model.ScreenApps:
		out := []model.Job{{Kind: model.KApps}}
		for _, a := range u.S.Names() {
			out = append(out, st(a))
		}
		return out
	case model.ScreenApp:
		return []model.Job{st(v.App), {Kind: model.KAlerts, App: v.App}}
	case model.ScreenRecord:
		return []model.Job{{Kind: model.KDeploy, App: v.App, Version: v.Version}, st(v.App)}
	case model.ScreenMe:
		return []model.Job{{Kind: model.KWhoAmI}}
	case model.ScreenBox:
		return []model.Job{{Kind: model.KBox}, {Kind: model.KBackups}, {Kind: model.KBoxAlerts}}
	case model.ScreenBackups:
		return []model.Job{{Kind: model.KBackups}}
	case model.ScreenAlerts:
		return []model.Job{{Kind: model.KBoxAlerts}}
	case model.ScreenRules:
		return []model.Job{{Kind: model.KBoxRules}}
	case model.ScreenAppRules:
		return []model.Job{{Kind: model.KRules, App: v.App}}
	case model.ScreenMetrics:
		return []model.Job{{Kind: model.KSeries, App: v.App, Range: v.Range}, st(v.App)}
	}
	return nil
}

// openMetrics is g on an app's screen: its numbers over the window last
// shown, the hour at first.
func (u *UI) openMetrics(app string) {
	if u.metricsRange == "" {
		u.metricsRange = model.SeriesRanges[0]
	}
	u.open(model.View{Screen: model.ScreenMetrics, App: app, Range: u.metricsRange})
}

// nextRange is t on the metrics screen: the next window, in the screen's
// place, so esc still goes back to the app.
func (u *UI) nextRange() {
	v := u.top()
	v.Range = model.NextRange(v.Range)
	u.metricsRange = v.Range
	u.stack[len(u.stack)-1] = v
	u.overlay = 0
	u.show()
}

// step opens the record next to this one in the app's deploys: p the
// one before (older), n the one after. This one is the deploys table's
// row when it holds this version (several rows can: the filter's
// placeholder), else the first that does; the table's cursor goes to
// the row stepped to, so esc returns there.
func (u *UI) step(by int) {
	v := u.top()
	rows := u.S.Overview(v.App, u.now()).Deploys
	i := u.deployRow(rows, v.Version)
	if i < 0 || i+by < 0 || i+by >= len(rows) {
		return
	}
	// The record left, as esc leaves it: its read is not wanted.
	u.S.Engine.Drop(model.Job{Kind: model.KDeploy, App: v.App, Version: v.Version})
	u.stack[len(u.stack)-1].Version = rows[i+by].Version
	// The cursor is set against these rows, which may not be the ones
	// the table was last drawn with (a status read since): renderApp
	// reads the cursor's version from deployRows.
	u.deployRows = rows
	u.deploys.Select(i+by+1, 0)
	// Opened, though its view may be the one left (one version on two
	// rows): show reads it again.
	u.inFront = model.View{}
	u.show()
}

// deployRow is the index in rows of version's row: the deploys table's
// cursor's when it holds version, else the first that does; -1 when
// none does.
func (u *UI) deployRow(rows []model.DeployRow, version string) int {
	i, _ := u.deploys.GetSelection()
	if i--; i < 0 || i >= len(rows) || rows[i].Version != version {
		i = slices.IndexFunc(rows, func(r model.DeployRow) bool { return r.Version == version })
	}
	return i
}

func (u *UI) openApp(row int) {
	if row >= 1 && row-1 < len(u.appRows) {
		u.open(model.View{Screen: model.ScreenApp, App: u.appRows[row-1].Name})
	}
}

// Stopped shows the refusal screen when the reads stopped, and leaves
// it when they run again.
func (u *UI) stopped() {
	stop := u.S.Engine.Stopped()
	at := slices.IndexFunc(u.stack, func(v model.View) bool { return v.Screen == model.ScreenRefused })
	switch {
	case stop == model.NotThisKey && at != 0:
		// The session dropped all the box said (Session.forget): no
		// screen of it stays under the stop, and nothing drawn from it
		// stays in a widget. After r, the apps screen fills from empty.
		u.stack = []model.View{{Screen: model.ScreenRefused}}
		u.closeLine()
		u.filter, u.appRows, u.deployRows, u.overKey = "", nil, nil, overKey{}
		u.apps.Clear()
		u.deploys.Clear()
		door.SetInput(u.prompt, "")
		u.show()
	case stop != model.Running && at < 0:
		// One refusal screen: help opened over it returns to it, and
		// nothing opens from either (keys.go's stops).
		u.stack = append(u.stack, model.View{Screen: model.ScreenRefused})
		u.closeLine()
		u.show()
	case stop == model.Running && at >= 0 && u.S.Me != nil && u.S.StopErr == nil:
		u.stack = u.stack[:at]
		if len(u.stack) == 0 {
			u.stack = []model.View{{Screen: model.ScreenApps}}
		}
		u.show()
	}
}

// closeLine closes the : line and any overlay: nothing on them is asked
// while every read is stopped.
func (u *UI) closeLine() {
	u.overlay, u.promptMode, u.notice = 0, 0, nil
	u.row2.SwitchToPage("crumb")
}
