package tui

import (
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/door"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

// render lays out every screen from the session, before each frame: a
// frame is drawn after a key, an answer, or the clock's tick, so every
// age on screen is the age now.
//
// It runs inside tview's draw, which holds the application's lock: it
// must not change the focus or show a page (both take the lock), so it
// only sets text and sizes.
func (u *UI) render() {
	now := u.now()
	left, right := u.S.Title(now)
	door.SetText(u.title, door.Line(spread(left, right, u.w), u.Colour))
	v := u.top()
	crumbL, crumbR := u.crumbs(v)
	door.SetText(u.crumb, door.Line(spread(crumbL, crumbR, u.w), u.Colour))
	foot := footers[v.Screen]
	if u.S.Engine.Stopped() != model.Running {
		foot = "r ask again  " + foot
	}
	if !u.S.Engine.CanAskAgain(now) {
		foot = strings.Replace(foot, "  ctrl-r ask again", "", 1)
	}
	door.SetText(u.foot, door.Lit(foot))
	switch v.Screen {
	case model.ScreenApps:
		u.renderApps()
	case model.ScreenApp:
		u.renderApp(v)
	case model.ScreenRecord:
		u.renderRecord(v)
	}
}

// spread puts left at the start and right at the end of a row w wide,
// cutting left first: the right side is the freshness word, which is
// never cut.
func spread(left, right model.Line, w int) model.Line {
	room := w - right.Width() - 1
	left = left.Cut(max(room, 0))
	gap := max(w-left.Width()-right.Width(), 1)
	out := append(model.Line{}, left...)
	out = append(out, model.Seg{T: termtext.From(strings.Repeat(" ", gap))})
	return append(out, right...)
}

func ownLine(s string) model.Line { return model.Line{{T: termtext.From(s)}} }

func (u *UI) crumbs(v model.View) (model.Line, model.Line) {
	now := u.now()
	switch v.Screen {
	case model.ScreenApps:
		return ownLine("apps"), u.S.AppsView(now, u.filter, u.sort).Summary
	case model.ScreenApp:
		return ownLine("apps › " + v.App), u.S.Overview(v.App, now).AsOf
	}
	return ownLine("apps › " + v.App + " › deploy " + v.Version), nil
}

// footers are each screen's keys.
var footers = map[model.Screen]string{
	model.ScreenApps:   "enter open  / filter  d record  s sort  ctrl-r ask again  q quit",
	model.ScreenApp:    "enter record  esc back  ctrl-r ask again  q quit",
	model.ScreenRecord: "↑↓ scroll  w wrap  p previous  n next  esc back  q quit",
}

// The apps table's columns, dropped in this order when the screen is
// too narrow: AGE first (the title bar's word says how fresh), then
// P95, 5xx and OF CAP. NAME, STATE, VERSION and LAST DEPLOY stay.
var dropOrder = []int{model.ColAge, model.ColP95, model.Col5xx, model.ColOfCap, model.ColMem}

func (u *UI) renderApps() {
	view := u.S.AppsView(u.now(), u.filter, u.sort)
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
	strip = u.withStop(strip)
	for i := range strip {
		strip[i] = strip[i].Cut(u.w)
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
			cl := door.Cell(r.Cells[c])
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

func (u *UI) renderApp(v model.View) {
	view := u.S.Overview(v.App, u.now())
	fs := view.Fields
	if view.Note != nil {
		fs = view.Note
	}
	fs = u.withStop(fs)
	for i := range fs {
		fs[i] = fs[i].Cut(u.w)
	}
	door.SetText(u.fields, door.Lines(fs, u.Colour))
	u.appFlex.ResizeItem(u.fields, min(len(fs), u.h/2), 0)
	u.deployRows = view.Deploys
	sel, _ := u.deploys.GetSelection()
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
	for i, r := range view.Deploys {
		u.deploys.SetCell(i+1, 0, u.markCell(r.Mark))
		for c, t := range r.Cells {
			u.deploys.SetCell(i+1, c+1, door.Cell(t))
		}
	}
	u.deploys.Select(min(max(sel, 1), len(view.Deploys)), 0)
}

func (u *UI) renderRecord(v model.View) {
	view := u.S.RecordView(v.App, v.Version, u.now())
	hd := view.Head
	if view.Note != nil {
		hd = view.Note
	}
	hd = u.withStop(hd)
	for i := range hd {
		hd[i] = hd[i].Cut(u.w)
	}
	door.SetText(u.recHead, door.Lines(hd, u.Colour))
	u.recFlex.ResizeItem(u.recHead, min(len(hd), u.h/2), 0)
	title := model.Line{}
	if view.TailTitle != nil {
		title = append(ownLine("── "), view.TailTitle...).Cut(u.w)
		title = append(title, ownLine(" "+strings.Repeat("─", max(u.w-title.Width()-1, 0)))...)
	}
	door.SetText(u.recTailTitle, door.Line(title, u.Colour))
	door.SetText(u.tail, door.Texts(view.Tail))
}

// withStop puts why every read stopped above a screen's lines, so the
// title bar's word has its reason on screen.
func (u *UI) withStop(ls []model.Line) []model.Line {
	if l := u.S.StopLine(); l != nil {
		return append([]model.Line{l}, ls...)
	}
	return ls
}
