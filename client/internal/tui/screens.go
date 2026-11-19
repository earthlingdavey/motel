package tui

import (
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/door"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// render lays out every screen from the session, before each frame: a
// frame is drawn after a key, an answer, or the clock's tick, so every
// age on screen is the age now. A frame reads the clock once: the
// session and the engine take a time earlier than the last they saw
// for a step of the clock back, and two readings in one frame, handed
// to them out of order, would be one at every frame.
//
// It runs inside tview's draw, which holds the application's lock: it
// must not change the focus or show a page (both take the lock), so it
// only sets text and sizes.
func (u *UI) render() {
	now := u.now()
	left, right := u.S.Title(now)
	door.SetText(u.title, door.Line(spread(left, right, u.w), u.Colour))
	v := u.top()
	// Only the refusal and help (over it or over any screen) are prose;
	// the other pages are labelled lines, which a wrap would break under
	// the labels: who am I and the box screen lay a failure's sentence on
	// rows themselves.
	u.page.SetWrap(v.Screen == model.ScreenRefused || v.Screen == model.ScreenStopHelp || v.Screen == model.ScreenHelp)
	crumbL, crumbR := u.crumbs(v, now)
	if u.notice != nil {
		// What the : line said of what was typed, until the next key.
		crumbL = u.notice
	}
	door.SetText(u.crumb, door.Line(spread(crumbL, crumbR, u.w), u.Colour))
	foot := footers[v.Screen]
	if v.Screen == model.ScreenRefused && u.S.CanPrint(v, 0) {
		foot = "↑↓ scroll  r ask again  P print person line  ? keys  q quit"
	}
	if !u.S.Engine.CanAskAgain(now) {
		foot = strings.Replace(foot, "  ctrl-r ask again", "", 1)
	}
	door.SetText(u.foot, door.Lit(foot))
	switch v.Screen {
	case model.ScreenApps:
		u.renderApps(now)
	case model.ScreenApp:
		u.renderApp(v, now)
	case model.ScreenRecord:
		u.renderRecord(v, now)
	case model.ScreenMe:
		door.SetText(u.page, door.Lines(u.S.MeViewFit(now, u.w, door.Width), u.Colour))
	case model.ScreenBox:
		door.SetText(u.page, door.Lines(u.S.BoxViewFit(now, u.w, door.Width), u.Colour))
	case model.ScreenBackups:
		door.SetText(u.page, door.Lines(u.S.BackupsViewFit(now, u.w, door.Width), u.Colour))
	case model.ScreenHelp:
		door.SetText(u.page, door.LitLines(hang(u.help(), u.w)))
	case model.ScreenStopHelp:
		door.SetText(u.page, door.LitLines(hang(stopHelp(), u.w)))
	case model.ScreenRefused:
		door.SetText(u.page, door.Lines(u.S.RefusedView(now), u.Colour))
	}
	if u.overlay != 0 {
		door.SetTitle(u.over.Box, map[int]string{1: "command", 2: "the box's answer"}[u.overlay])
		// Made once per answer and mode: a large body made every frame
		// slow when it was indented and set again at each.
		body, at, have := u.S.Body(v)
		key := overKey{v: v, mode: u.overlay, at: at, n: len(body), w: u.w}
		if !have {
			key.none = u.S.NoAnswer(v)
		}
		if key != u.overKey {
			u.overKey = key
			door.SetText(u.over, door.Lines(u.S.Overlay(v, u.overlay == 2, u.w-2, door.Width, door.AsIs), u.Colour))
		}
		foot := "esc close  c command  y json"
		if u.S.CanPrint(v, u.overlay) {
			foot += "  P quit and print"
		}
		door.SetText(u.foot, door.Lit(foot+"  ↑↓ scroll"))
	}
	if name, _ := u.row2.GetFrontPage(); name == "prompt" {
		// The line takes every key typed but these: the screen's own
		// would be typed into it.
		door.SetText(u.foot, door.Lit(promptFooters[u.promptMode]))
	}
}

// promptFooters are the keys of the : line and the / filter while
// open, whole at 60 columns as the screens' footers are.
var promptFooters = map[rune]string{
	':': "enter run it  tab complete  esc close",
	'/': "enter keep the filter  esc clear it and close",
}

// overKey names what the overlay was last made of: with no answer
// held, what it says of the read in place of one.
type overKey struct {
	v    model.View
	mode int
	at   time.Time
	n, w int
	none string
}

// spread puts left at the start and right at the end of a row w cells
// wide, cutting left first: the right side is the freshness word, or the
// apps' summary, cut only when it alone takes the whole row or more
// (the summary of hundreds of apps at 60 columns).
func spread(left, right model.Line, w int) model.Line {
	right = right.Cut(max(w-1, 0), door.Width)
	room := w - right.Width(door.Width) - 1
	left = left.Cut(max(room, 0), door.Width)
	gap := max(w-left.Width(door.Width)-right.Width(door.Width), 1)
	out := append(model.Line{}, left...)
	out = append(out, model.Seg{T: termtext.From(strings.Repeat(" ", gap))})
	return append(out, right...)
}

func ownLine(s string) model.Line { return model.Line{{T: termtext.From(s)}} }

func (u *UI) crumbs(v model.View, now time.Time) (model.Line, model.Line) {
	switch v.Screen {
	case model.ScreenApps:
		return ownLine("apps"), u.S.AppsView(now, u.filter, u.sort, u.w).Summary
	case model.ScreenApp:
		return ownLine("apps › " + v.App), u.S.Overview(v.App, now).AsOf
	case model.ScreenRecord:
		return ownLine("apps › " + v.App + " › deploy " + v.Version), u.S.RecordAsOf(v.App, v.Version, now)
	case model.ScreenMe:
		return ownLine(":me"), u.S.MeAsOf(now)
	case model.ScreenBox:
		return ownLine(":box"), u.S.BoxAsOf(now)
	case model.ScreenBackups:
		return ownLine(":backups"), u.S.BackupsAsOf(now)
	case model.ScreenHelp, model.ScreenStopHelp:
		return ownLine("help"), ownLine("esc closes")
	}
	if u.S.Answered() {
		return ownLine("stopped"), nil
	}
	return ownLine("first run"), nil
}

// footers are each screen's keys: at most six, then "? more", each
// footer whole at 60 columns, the smallest window
// (TestFootersWholeAtTheSmallest); a key left out is listed by ?.
var footers = map[model.Screen]string{
	model.ScreenApps:     "enter open  / filter  : go to  y json  c cmd  ? more  q quit",
	model.ScreenApp:      "enter record  y json  c command  esc back  ? more",
	model.ScreenRecord:   "↑↓ scroll  w wrap  p previous  n next  esc back  ? more",
	model.ScreenMe:       "↑↓ scroll  y json  c command  esc back  ? more",
	model.ScreenBox:      "↑↓ scroll  y json  c command  esc back  ? more",
	model.ScreenBackups:  "↑↓ scroll  y json  c command  esc back  ? more",
	model.ScreenHelp:     "↑↓ scroll  esc close  q quit",
	model.ScreenRefused:  "↑↓ scroll  r ask again  ? keys  q quit",
	model.ScreenStopHelp: "↑↓ scroll  esc back  r ask again  q quit",
}

// The apps table's columns, dropped in this order when the screen is
// too narrow: AGE first, each row's age then going in its STATE cell,
// then P95, 5xx, OF CAP and MEM. NAME, STATE, VERSION and LAST DEPLOY
// stay, but that two long names shown longer to be told apart (the
// model's shownNames) may take VERSION's and LAST DEPLOY's room: only
// STATE is kept beside them.
var dropOrder = []int{model.ColAge, model.ColP95, model.Col5xx, model.ColOfCap, model.ColMem}

func (u *UI) renderApps(now time.Time) {
	view := u.S.AppsView(now, u.filter, u.sort, u.w)
	n := 4
	if u.h < 30 {
		n = 1
	}
	var strip []model.Line
	switch {
	case len(view.Rows) == 0 && view.Empty != nil:
		strip = []model.Line{view.Empty}
	case len(view.Strip) == 0 && view.AllWell != nil:
		strip = []model.Line{view.AllWell}
	case len(view.Strip) > n && n == 1:
		strip = []model.Line{append(view.Strip[0], ownLine(" · +"+strconv.Itoa(len(view.Strip)-1)+" more (/!ok)")...)}
	case len(view.Strip) > n:
		strip = append(append([]model.Line{}, view.Strip[:n-1]...), ownLine("  +"+strconv.Itoa(len(view.Strip)-n+1)+" more (/!ok to list them)"))
	default:
		strip = view.Strip
	}
	if view.ListFailed != nil {
		strip = append([]model.Line{view.ListFailed}, strip...)
	}
	strip = u.withStop(strip)
	for i := range strip {
		strip[i] = strip[i].Cut(u.w, door.Width)
	}
	strip = append(strip, ownLine(strings.Repeat("─", u.w)))
	door.SetText(u.strip, door.Lines(strip, u.Colour))
	u.appsFlex.ResizeItem(u.strip, len(strip), 0)

	// Each column's width, then the columns that fit.
	var wd [model.NCols]int
	for c := range wd {
		wd[c] = len([]rune(model.ColHeads[c]))
		for _, r := range view.Rows {
			wd[c] = max(wd[c], door.Width(r.Cells[c]))
		}
	}
	keep := map[int]bool{}
	total := 2
	for c := range wd {
		keep[c] = true
		total += wd[c] + 1
	}
	for _, c := range dropOrder {
		if total <= u.w {
			break
		}
		keep[c], total = false, total-wd[c]-1
		if c == model.ColAge {
			// Every value shows its age: STATE takes it, and widens.
			aged := wd[model.ColState]
			for _, r := range view.Rows {
				aged = max(aged, door.Width(r.StateAged))
			}
			total, wd[model.ColState] = total+aged-wd[model.ColState], aged
		}
	}
	selName := ""
	if r, _ := u.apps.GetSelection(); r >= 1 && r-1 < len(u.appRows) {
		selName = u.appRows[r-1].Name
	}
	u.appRows = view.Rows
	u.apps.Clear()
	u.apps.SetCell(0, 0, door.Head(" "))
	col := 1
	for c := 0; c < model.NCols; c++ {
		if keep[c] {
			u.apps.SetCell(0, col, door.Head(model.ColHeads[c]))
			col++
		}
	}
	sel := 1
	for i, r := range view.Rows {
		if r.Name == selName {
			sel = i + 1
		}
		u.apps.SetCell(i+1, 0, u.markCell(r.Mark))
		col := 1
		for c := 0; c < model.NCols; c++ {
			if !keep[c] {
				continue
			}
			t := r.Cells[c]
			if c == model.ColState && !keep[model.ColAge] {
				t = r.StateAged
			}
			cl := door.Cell(t)
			if r.Stale {
				cl.SetAttributes(tcell.AttrDim)
			}
			if c == model.ColState && u.Colour {
				cl.SetTextColor(colourOf(r.Mark.Class()))
			}
			u.apps.SetCell(i+1, col, cl)
			col++
		}
	}
	if len(view.Rows) > 0 {
		u.apps.Select(min(sel, len(view.Rows)), 0)
	}
}

func (u *UI) markCell(m model.Mark) *tview.TableCell {
	c := door.Cell(termtext.From(m.Glyph()))
	if u.Colour {
		c.SetTextColor(colourOf(m.Class()))
	}
	return c
}

func colourOf(c model.Class) tcell.Color {
	switch c {
	case model.Bad:
		return tcell.ColorRed
	case model.Warn:
		return tcell.ColorYellow
	case model.Good:
		return tcell.ColorGreen
	}
	return tcell.ColorDefault
}

func (u *UI) renderApp(v model.View, now time.Time) {
	view := u.S.Overview(v.App, now)
	fs := view.Fields
	if view.Note != nil {
		fs = view.Note
	}
	if view.Failed != nil {
		fs = append(model.Sentence(view.Failed, u.w, door.Width), fs...)
	}
	fs = u.withStop(fs)
	for i := range fs {
		fs[i] = fs[i].Cut(u.w, door.Width)
	}
	fs = fitRows(fs, u.h/2, u.w)
	door.SetText(u.fields, door.Lines(fs, u.Colour))
	u.appFlex.ResizeItem(u.fields, len(fs), 0)
	// The cursor stays on its deploy, by version, as the apps table's
	// stays on its app by name: a new deploy lands above it. Among rows
	// of one version (the filter's placeholder for several), it stays on
	// its row while that row's version is the same, else on the first.
	sel, _ := u.deploys.GetSelection()
	selVersion := ""
	if sel >= 1 && sel-1 < len(u.deployRows) {
		selVersion = u.deployRows[sel-1].Version
	}
	u.deployRows = view.Deploys
	u.deploys.Clear()
	if len(view.Deploys) == 0 {
		door.SetText(u.onDisk, door.Lit(""))
		return
	}
	door.SetText(u.onDisk, door.Line(spread(append(ownLine(strings.Repeat("─", 1)+" "), ownLine("DEPLOYS, newest first")...), view.OnDisk, u.w), u.Colour))
	u.deploys.SetCell(0, 0, door.Head(" "))
	for c, h := range model.DeployCols {
		u.deploys.SetCell(0, c+1, door.Head(h))
	}
	kept := sel >= 1 && sel-1 < len(view.Deploys) && view.Deploys[sel-1].Version == selVersion
	for i, r := range view.Deploys {
		if !kept && r.Version == selVersion {
			sel, kept = i+1, true
		}
		u.deploys.SetCell(i+1, 0, u.markCell(r.Mark))
		for c, t := range r.Cells {
			u.deploys.SetCell(i+1, c+1, door.Cell(t))
		}
	}
	u.deploys.Select(min(max(sel, 1), len(view.Deploys)), 0)
}

func (u *UI) renderRecord(v model.View, now time.Time) {
	view := u.S.RecordView(v.App, v.Version, now)
	hd := view.Head
	if view.Note != nil {
		hd = view.Note
	}
	hd = u.withStop(hd)
	for i := range hd {
		hd[i] = hd[i].Cut(u.w, door.Width)
	}
	hd = fitRows(hd, u.h/2, u.w)
	door.SetText(u.recHead, door.Lines(hd, u.Colour))
	u.recFlex.ResizeItem(u.recHead, len(hd), 0)
	title := model.Line{}
	if view.TailTitle != nil {
		title = append(ownLine("── "), view.TailTitle...).Cut(u.w, door.Width)
		title = append(title, ownLine(" "+strings.Repeat("─", max(u.w-title.Width(door.Width)-1, 0)))...)
	}
	door.SetText(u.recTailTitle, door.Line(title, u.Colour))
	tail := view.Tail
	if !u.wrap {
		// A line stays a line, cut at the window's edge as every other
		// line is, the cut marked.
		tail = make([]termtext.Text, len(view.Tail))
		for i, t := range view.Tail {
			tail[i] = termtext.From(model.Line{{T: t}}.Cut(u.w, door.Width).Text())
		}
	}
	door.SetText(u.tail, door.Texts(tail))
}

// fitRows keeps the lines that fit in rows, the room a screen gives
// its details above a table or a log tail. When some do not fit, the
// last row that shows says how many are not shown (that row's own line
// among them) and where they all are, in the colour the overlay's own
// cut is said in; a line cut at the width is already marked by its "…".
func fitRows(ls []model.Line, rows, w int) []model.Line {
	switch {
	case len(ls) <= rows:
		return ls
	case rows <= 0:
		return nil
	}
	n := len(ls) - rows + 1
	more := model.Line{{T: termtext.From("… " + strconv.Itoa(n) + " more lines: y shows the answer; then P prints it whole"), C: model.Warn}}
	return append(ls[:rows-1:rows-1], more.Cut(w, door.Width))
}

// withStop puts why every read stopped above a screen's lines, so the
// title bar's word has its reason on screen.
func (u *UI) withStop(ls []model.Line) []model.Line {
	if l := u.S.StopLine(); l != nil {
		return append([]model.Line{l}, ls...)
	}
	return ls
}

// hang lays each of help's entries (a name, two spaces or more, its
// words) that is wider than width on rows of its own, the words after
// the first row under the first's, so that no part of one reads as a
// new entry at the left edge. Prose, with no such column, is left for
// the page's own wrap.
func hang(text string, width int) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		col := entryCol(l)
		if door.Width(termtext.From(l)) <= width || col < 0 || width-col < 20 {
			out = append(out, l)
			continue
		}
		indent := strings.Repeat(" ", door.Width(termtext.From(l[:col])))
		// Split at each space, so a run of them between words (the marks'
		// groups) stays a run on its row.
		row, words := l[:col], ""
		for _, w := range strings.Split(l[col:], " ") {
			if words != "" && door.Width(termtext.From(strings.TrimRight(row+words+" "+w, " "))) > width {
				out = append(out, strings.TrimRight(row+words, " "))
				row, words = indent, ""
			}
			if words == "" && w == "" {
				continue
			}
			if words != "" {
				words += " "
			}
			words += w
		}
		out = append(out, row+words)
	}
	return strings.Join(out, "\n")
}

// entryCol is where an entry's words begin, after its name and the run
// of two spaces or more that ends it; -1 for a line with none.
func entryCol(l string) int {
	name := strings.TrimLeft(l, " ")
	i := strings.Index(name, "  ")
	if i <= 0 {
		return -1
	}
	i += len(l) - len(name)
	for i < len(l) && l[i] == ' ' {
		i++
	}
	if i == len(l) {
		return -1
	}
	return i
}

// stopHelp is help over a stop screen: its keys, and those of help
// itself, which opens nothing while every read is stopped.
func stopHelp() string {
	return "KEYS OF THE SCREEN BELOW\n" + keyHelp(model.ScreenRefused) + "\n\nKEYS HERE\n" + keyHelp(model.ScreenStopHelp) + `

Every read is stopped, so nothing opens from here: no view, no command or answer overlay, no : line. r asks again; once the box answers, about this key and this box, the screens come back.`
}

// help is the help page: the keys of the screen below it, from the
// table they are bound from, then the views : takes, and the marks.
func (u *UI) help() string {
	below := model.ScreenApps
	if len(u.stack) > 1 {
		below = u.stack[len(u.stack)-2].Screen
	}
	return "KEYS OF THE SCREEN BELOW\n" + keyHelp(below) + `

VIEWS (type after :)
 apps                         every app you may view
 status <app>                 one app's overview (also: app <app>)
 deploys <app> [<version>]    one app's deploys, or one record
 whoami                       who you are here (also: me)
 box                          what the box runs (viewers of the box)
 backups                      what the box says of its backups (viewers of the box)
 help                         this

MARKS   ✖ broken now   ▲ look   ⟳ deploying   ? cannot tell   · nothing to show
AGE     how old each row's answer is; "stale" after three missed reads

This client only reads. Rolling back, silences and logs arrive when the box offers them.
Backups are the box's own claim: look from a machine of your own with restic too.
Deploys come from CI; config and secrets from signed pushes; root's work is over ssh.`
}
