// Package tui is `motelctl tui`: k9s-style screens over the model. It
// sees the box only through model, which reads it only through the
// Client it is given, and it hands tview no text but through
// boxtext.go. It acts on nothing: every screen reads.
package tui

import (
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/motelctl/internal/tui/door"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
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
	Colour      bool
	stack       []model.View
	filter      string
	sort        int
	wrap        bool
	w, h        int
	lastFocused string
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
	appRows                     []model.AppRow
	deployRows                  []model.DeployRow
}

func textView() *tview.TextView {
	return tview.NewTextView().SetDynamicColors(true).SetWrap(false)
}

// New builds the screens, opening on start.
func New(s *model.Session, colour bool, start model.View) *UI {
	door.TerminalColours()
	u := &UI{App: tview.NewApplication(), S: s, Colour: colour, stack: []model.View{start}, ended: make(chan struct{})}
	u.title, u.crumb, u.foot = textView(), textView(), textView()
	u.prompt = tview.NewInputField().SetFieldStyle(tcell.StyleDefault).SetLabelStyle(tcell.StyleDefault).
		SetAutocompleteUseTags(false)
	u.prompt.SetChangedFunc(func(string) { u.filter = door.Typed(u.prompt) })
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

	u.body = tview.NewPages().
		AddPage("apps", u.appsFlex, true, false).AddPage("app", u.appFlex, true, false).
		AddPage("record", u.recFlex, true, false)
	frame := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.title, 1, 0, false).
		AddItem(u.row2, 1, 0, false).AddItem(u.body, 0, 1, true).AddItem(u.foot, 1, 0, false)
	u.root = tview.NewPages().AddPage("frame", frame, true, true)
	u.App.SetRoot(u.root, true).SetInputCapture(u.key)
	u.App.SetBeforeDrawFunc(func(scr tcell.Screen) bool {
		u.w, u.h = scr.Size()
		if u.w < minW || u.h < minH {
			// Below this nothing lays out; say what is needed instead.
			door.PrintAt(scr, 0, 0, u.w, "window too small: motelctl tui needs "+itoa(minW)+"x"+itoa(minH)+
				", this one is "+itoa(u.w)+"x"+itoa(u.h)+". q quits.")
			return true
		}
		u.render()
		return false
	})
	u.show()
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
	u.show()
}

// back is esc: one screen goes back. It never quits.
func (u *UI) back() {
	if len(u.stack) > 1 {
		if v := u.top(); v.Screen == model.ScreenRecord {
			u.S.Engine.Drop(model.Job{Kind: model.KDeploy, App: v.App, Version: v.Version})
		}
		u.stack = u.stack[:len(u.stack)-1]
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
	case model.ScreenRecord:
		focus = v.App
		u.S.Engine.Once(model.Job{Kind: model.KDeploy, App: v.App, Version: v.Version}, now)
		u.body.SwitchToPage("record")
		u.tail.ScrollToBeginning()
		u.App.SetFocus(u.tail)
	}
	if focus != u.lastFocused {
		u.S.Engine.Focus(focus, now)
		u.lastFocused = focus
	}
}

// key is every key pressed, before any widget: what it means is the
// same on every screen, but for the screen's own keys.
func (u *UI) key(ev *tcell.EventKey) *tcell.EventKey {
	u.S.Engine.Touch(u.now())
	if u.App.GetFocus() == u.prompt {
		return ev
	}
	v := u.top()
	r := ev.Rune()
	if ev.Key() != tcell.KeyRune {
		r = 0
	}
	switch {
	case ev.Key() == tcell.KeyEscape:
		u.back()
	case r == 'q':
		u.App.Stop()
	case r == 'r' && u.S.Engine.Stopped() != model.Running:
		// The only way out of a stop: one whoami, as a new start, with a
		// token signed afresh; held to one in RetryEvery.
		// Forgotten before the retry: Retry wakes the reads, and the
		// whoami must not go out with the refused token.
		if now := u.now(); u.S.Engine.RetryReady(now) {
			if u.Resign != nil {
				u.Resign()
			}
			u.S.Engine.Retry(now)
		}
	case r == '/' && v.Screen == model.ScreenApps:
		u.ask()
	case ev.Key() == tcell.KeyCtrlR:
		// Everything on this screen, the record on it too.
		var screen []model.Job
		if v.Screen == model.ScreenRecord {
			screen = append(screen, model.Job{Kind: model.KDeploy, App: v.App, Version: v.Version})
		}
		u.S.Engine.Again(u.now(), screen...)
	case r == 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case r == 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case r == 's' && v.Screen == model.ScreenApps:
		u.sort++
	case r == 'd' && v.Screen == model.ScreenApps:
		if row, _ := u.apps.GetSelection(); row >= 1 && row-1 < len(u.appRows) {
			a := u.appRows[row-1]
			u.open(model.View{Screen: model.ScreenApp, App: a.Name})
			if a.Failed != "" {
				u.open(model.View{Screen: model.ScreenRecord, App: a.Name, Version: a.Failed})
			}
		}
	case r == 'w' && v.Screen == model.ScreenRecord:
		u.wrap = !u.wrap
		u.tail.SetWrap(u.wrap)
	case (r == 'p' || r == 'n') && v.Screen == model.ScreenRecord:
		u.step(map[rune]int{'p': 1, 'n': -1}[r])
	default:
		return ev
	}
	return nil
}

// step opens the record next to this one in the app's deploys: p the
// one before (older), n the one after.
func (u *UI) step(by int) {
	v := u.top()
	rows := u.S.Overview(v.App, u.now()).Deploys
	for i, r := range rows {
		if r.Version == v.Version && i+by >= 0 && i+by < len(rows) {
			u.stack[len(u.stack)-1].Version = rows[i+by].Version
			u.show()
			return
		}
	}
}

func (u *UI) openApp(row int) {
	if row >= 1 && row-1 < len(u.appRows) {
		u.open(model.View{Screen: model.ScreenApp, App: u.appRows[row-1].Name})
	}
}
