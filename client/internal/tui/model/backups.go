package model

import (
	"strings"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The backups screen: what the box says of its own backups (GET
// /_motel/backups, `motel backup status`), for a viewer of the box.
// Every word of it is the box's claim about itself, and the screen says
// so: a box is only as honest as it is, and docs/backups.md sends the
// operator to look from a machine of their own. The client's own words
// judge nothing the box did not say: healthy is the box's verdict, and
// each mark repeats one.

// OwnClaim is said under every view of the box's backups.
const OwnClaim = "This is the box's own claim, only as honest as the box: look from a machine of your own with restic too (docs/backups.md, \"Look from your own machine\")."

// BackupCols are the backups table's headings.
var BackupCols = []string{"APP", "RESULT", "LAST COMPLETE BACKUP", "FRESH", "RESTORE PROVEN", "LATELY"}

// BackupsAsOf is how old the box's answer about its backups is, for
// the crumb, and that the last read failed when it did: an answer kept
// beside a failure says so at the top, as a record's does (RecordAsOf),
// whatever of the screen is below the fold.
func (s *Session) BackupsAsOf(now time.Time) Line {
	s.seen(now)
	if s.BackupsSt == nil {
		return nil
	}
	l := append(s.asOf(s.BackupsSt.At, now), s.stale(Job{Kind: KBackups})...)
	if s.BackupsErr != nil {
		l = append(l, ownC(" · the last read failed", Warn))
	}
	return l
}

// verdict is the box's verdict on its backups in the client's words:
// the box's, said as the box's.
func verdict(b wire.Backups) Line {
	switch {
	case b.CouldNotTell != "":
		return Line{ownC("could not tell", Warn), own(", the box says")}
	case b.Healthy:
		return Line{ownC("healthy", Good), own(", the box says")}
	}
	return Line{ownC("not healthy", Warn), own(", the box says")}
}

// couldNotTell is the box's why, quoted, under the verdict: laid on
// rows, the first at the value's column (12) and opening with the
// client's quote mark, the rest two further in (Sentence), so no row of
// it begins where a row of the screen's own does (at 0 or 2 for the
// table and the box's lines; at 12, with a word of the client's, for
// the decided line), whatever the box wrote.
func couldNotTell(why string, width int, cells Cells) []Line {
	return Sentence(Line{own(strings.Repeat(" ", 12)), quoted(why)}, width, cells)
}

// BackupsView is the backups screen, its lines whole.
func (s *Session) BackupsView(now time.Time) []Line { return s.BackupsViewFit(now, 0, nil) }

// BackupsViewFit is the backups screen laid to width cells; 0 leaves
// each line one line. A line holding a box's value is cut to the
// width, so a value never begins a row; but for a could not tell,
// which is the client's sentence quoting the box, laid on rows of its
// own under the verdict, each beginning at the value's column or past
// it, where no row of the screen's own begins (couldNotTell).
func (s *Session) BackupsViewFit(now time.Time, width int, cells Cells) []Line {
	s.seen(now)
	switch {
	case s.BackupsSt == nil && s.BackupsErr != nil:
		l := Line{{T: Capped(failureText(s.BackupsErr), 4*LineMax)}}
		if Forbidden(s.BackupsErr) {
			// Held until the person asks, and a grant may come back: say
			// how, as an app's screen does.
			l = append(l, s.askAgain(now)...)
		}
		return Sentence(l, width, cells)
	case s.BackupsSt == nil:
		return Sentence(Line{own("asking " + s.Host + " what it says of its backups…")}, width, cells)
	}
	b := s.BackupsSt.V
	// When the box decided, which its cache may have kept for up to 10 s
	// before this answer: the crumb gives the answer's own age.
	decided := Line{own("decided " + clock(b.At, now, true) + " UTC, " + since(b.At, now) + " · the box's claim")}
	out := []Line{cutTo(field("backups", verdict(b)...), width, cells)}
	if b.CouldNotTell != "" {
		out = append(out, couldNotTell(b.CouldNotTell, width, cells)...)
	}
	out = append(out, cutTo(field("", decided...), width, cells))
	if len(b.Apps) > 0 {
		out = append(out, nil)
		out = append(out, s.backupRows(b, now, width, cells)...)
	}
	if len(b.Lines) > 0 {
		out = append(out, nil, Line{ownC("what the box says", Label)})
		for _, l := range b.Lines {
			mark := "  "
			if l.Level == "problem" {
				mark = MarkLook.Glyph() + " "
			}
			c := Plain
			if l.Level != "info" {
				c = Warn
			}
			out = append(out, cutTo(Line{ownC(mark+pad(l.Level, 8)+" ", c), boxT(LineText(l))}, width, cells))
		}
	}
	out = append(out, nil)
	out = append(out, Sentence(Line{own(OwnClaim)}, width, cells)...)
	if s.BackupsErr != nil {
		out = append(append(out, nil), Sentence(Line{ownC("the last read failed: ", Warn), Seg{T: Capped(failureText(s.BackupsErr), 2*LineMax)}}, width, cells)...)
	}
	return out
}

// backupRows are the table: a heading, then a row an app, each cut to
// width. Every cell is a word of its grammar (an app's name, a result,
// both held by the client package) or the client's own words.
func (s *Session) backupRows(b wire.Backups, now time.Time, width int, cells Cells) []Line {
	at := func(t *time.Time, none string) string {
		if t == nil {
			return none
		}
		return clock(*t, now, false) + " (" + since(*t, now) + ")"
	}
	yes := map[bool]string{true: "yes", false: "no"}
	rows := [][]string{BackupCols}
	marks := []Mark{MarkNone}
	for _, a := range b.Apps {
		result := a.Result
		if result == "" {
			result = "none"
		}
		rows = append(rows, []string{a.Name, result, at(a.LastCompleteBackup, "none"), yes[a.Fresh], at(a.RestoreProven, "never"), yes[a.RestoreProvenLately]})
		m := MarkNone
		if !a.Healthy {
			m = MarkLook
		}
		marks = append(marks, m)
	}
	wide := make([]int, len(BackupCols))
	for _, r := range rows {
		for i, c := range r {
			wide[i] = max(wide[i], len([]rune(c)))
		}
	}
	var out []Line
	for i, r := range rows {
		var cols strings.Builder
		for j, c := range r {
			if j < len(r)-1 {
				c = pad(c, wide[j]+2)
			}
			cols.WriteString(c)
		}
		l := Line{ownC(marks[i].Glyph()+" ", marks[i].Class())}
		if i == 0 {
			l = append(l, ownC(cols.String(), Label))
		} else {
			l = append(l, own(cols.String()))
		}
		out = append(out, cutTo(l, width, cells))
	}
	return out
}

// LineText is a line's text without the `warning: ` status writes
// before a warning's, which the level beside it already says.
func LineText(l wire.BackupLine) string {
	if l.Level == "warning" {
		if t, ok := strings.CutPrefix(l.Text, "warning: "); ok {
			return t
		}
	}
	return l.Text
}

// BoxBackupsLine is the box screen's line of the backups: how old the
// answer is and the box's verdict, or why there is none. The age and
// the pointer come first, so a cut at the window's edge cuts neither;
// the box's why of a could not tell is on :backups alone.
func (s *Session) BoxBackupsLine(now time.Time) Line {
	switch {
	case s.BackupsSt != nil:
		l := Line{own(Ago(now.Sub(s.BackupsSt.At)) + " ago · :backups · ")}
		if s.Engine.Woke(Job{Kind: KBackups}) {
			// After the pointer, as the failed read's words are (stale).
			l = append(l, ownC("stale · ", Warn))
		}
		if s.BackupsErr != nil {
			l = append(l, ownC("the last read failed · ", Warn))
		}
		return field("backups", append(l, verdict(s.BackupsSt.V)...)...)
	case NotFound(s.BackupsErr):
		return field("backups", own("not said here (it answered 404: a moteld older than this read)"))
	case s.BackupsErr != nil:
		return field("backups", ownC("the last read failed", Warn), own(" · :backups says why"))
	}
	return field("backups", own("asking…"))
}
