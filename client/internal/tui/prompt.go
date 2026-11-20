package tui

import (
	"slices"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/door"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// ask opens row 2's prompt: ':' for the command line, '/' for the
// filter.
func (u *UI) ask(mode rune) {
	u.promptMode = mode
	door.SetLabel(u.prompt, string(mode))
	// Set once: setting the field runs its changed handler, which in /
	// mode makes the filter what the field holds.
	text := ""
	if mode == '/' {
		text = u.filter
	}
	door.SetInput(u.prompt, text)
	u.row2.SwitchToPage("prompt")
	u.App.SetFocus(u.prompt)
}

// promptDone is enter, esc or tab on the prompt.
func (u *UI) promptDone(k tcell.Key) {
	if k == tcell.KeyTab || k == tcell.KeyBacktab {
		return // nothing to complete: what is typed stays
	}
	text := strings.TrimSpace(door.Typed(u.prompt))
	switch {
	case u.promptMode == '/' && k == tcell.KeyEscape:
		u.filter = ""
	case u.promptMode == ':' && k == tcell.KeyEnter:
		u.row2.SwitchToPage("crumb")
		u.show()
		u.run(text)
		return
	}
	u.row2.SwitchToPage("crumb")
	u.show()
}

// run is a command line: the motel command words, which mean here
// what they mean in a shell. What is typed is held to the grammars the
// client holds it to, and a version to the box's own (at most 64
// characters; the command sends a longer one for the box to refuse),
// before anything is asked, and what is wrong is
// said on the prompt's line, the typing echoed only as a quotation.
func (u *UI) run(text string) {
	w := strings.Fields(text)
	if len(w) == 0 {
		return
	}
	say := func(parts ...model.Seg) { u.notice = model.Line(parts) }
	own := func(s string) model.Seg { return model.Seg{T: termtext.From(s), C: model.Warn} }
	quoted := func(s string) model.Seg { return model.Seg{T: termtext.Quote(s), C: model.Warn} }
	app := func() (string, bool) {
		if len(w) < 2 {
			say(own(w[0] + " needs an app: :" + w[0] + " <app>"))
			return "", false
		}
		// One sentence whether the app does not exist or is not yours to
		// view: the list is the box's answer to what you may view.
		if !slices.Contains(u.S.Names(), w[1]) {
			say(own("no app "), quoted(w[1]), own(" that you may view here"))
			return "", false
		}
		return w[1], true
	}
	// Each word takes so many: more is not run, and said.
	most, known := map[string]int{"status": 2, "app": 2, "deploys": 3, "q": 1, "quit": 1, "apps": 1, "whoami": 1, "me": 1, "box": 1, "backups": 1, "alerts": 1, "rules": 2, "metrics": 3, "help": 1, "keys": 1}[w[0]]
	if known && len(w) > most {
		say(own("one word too many: "), quoted(strings.Join(w[most:], " ")), own(" is more than :"+w[0]+" takes"))
		return
	}
	switch w[0] {
	case "q", "quit":
		u.App.Stop()
	case "apps":
		u.open(model.View{Screen: model.ScreenApps})
	case "status", "app", "deploys":
		a, ok := app()
		if !ok {
			return
		}
		if w[0] == "deploys" && len(w) > 2 && !model.ValidVersion(w[2]) {
			say(quoted(w[2]), own(" is not a version (letters, digits, . _ -, at most 64; not starting with .)"))
			return
		}
		u.open(model.View{Screen: model.ScreenApp, App: a})
		if w[0] == "deploys" && len(w) > 2 {
			u.open(model.View{Screen: model.ScreenRecord, App: a, Version: w[2]})
		}
	case "metrics":
		a, ok := app()
		if !ok {
			return
		}
		rng := model.SeriesRanges[0]
		if len(w) > 2 {
			if !slices.Contains(model.SeriesRanges, w[2]) {
				say(quoted(w[2]), own(" is not a window: 1h, 24h, 7d or 14d"))
				return
			}
			rng = w[2]
		}
		u.metricsRange = rng
		mv := model.View{Screen: model.ScreenMetrics, App: a, Range: rng}
		switch top := u.top(); {
		case top.Screen == model.ScreenMetrics && top.App == a:
			// Another window in this one's place, as t gives it.
			u.stack[len(u.stack)-1] = mv
			u.overlay = 0
			u.show()
		case top.Screen == model.ScreenApp && top.App == a:
			u.open(mv)
		default:
			// The app's screen under it, for esc, never shown on the way:
			// shown, it would ask the app's alerts and cancel a series read.
			u.stack = append(u.stack, model.View{Screen: model.ScreenApp, App: a})
			u.open(mv)
		}
	case "whoami", "me":
		u.open(model.View{Screen: model.ScreenMe})
	case "box":
		if !u.S.BoxViewer() {
			say(own("the box screen is for a viewer of the box; :me lists what you hold here"))
			return
		}
		u.open(model.View{Screen: model.ScreenBox})
	case "backups":
		if !u.S.BoxViewer() {
			say(own("the backups screen is for a viewer of the box; :me lists what you hold here"))
			return
		}
		u.open(model.View{Screen: model.ScreenBackups})
	case "alerts":
		if !u.S.BoxViewer() {
			say(own("the alerts screen is for a viewer of the box; :me lists what you hold here"))
			return
		}
		u.open(model.View{Screen: model.ScreenAlerts})
	case "rules":
		if len(w) == 1 {
			if !u.S.BoxViewer() {
				say(own("the box's rules are for a viewer of the box; :rules <app> lists an app's"))
				return
			}
			u.open(model.View{Screen: model.ScreenRules})
			return
		}
		a, ok := app()
		if !ok {
			return
		}
		u.open(model.View{Screen: model.ScreenAppRules, App: a})
	case "help", "keys":
		u.open(model.View{Screen: model.ScreenHelp})
	default:
		// The remedy first: beside the crumb's right side at 60 columns
		// about 20 cells are left, and help lists the views : takes.
		say(own("? lists the : views; no command "), quoted(w[0]), own(" here"))
	}
}

// complete offers only the views this person can reach on this box:
// no box for a viewer of apps alone, and app names from the box's list.
func (u *UI) complete(text string) []string {
	if u.promptMode != ':' || text == "" {
		return nil
	}
	all := []string{"apps", "whoami", "help", "quit"}
	if u.S.BoxViewer() {
		all = append(all, "box", "backups", "alerts", "rules")
	}
	for _, a := range u.S.Names() {
		// Not "metrics <app>": offered for "me", it would stand between
		// esc and closing the line.
		all = append(all, "status "+a, "deploys "+a, "rules "+a)
	}
	var out []string
	for _, c := range all {
		if strings.HasPrefix(c, text) && c != text {
			out = append(out, c)
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}
