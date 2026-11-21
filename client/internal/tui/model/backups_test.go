package model_test

import (
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

func joined(ls []model.Line) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.Text() + "\n")
	}
	return b.String()
}

// TestBackupsSays: the backups screen and the box screen's line say the
// box's verdict as the box's, and that it is the box's own claim next to
// it; a could not tell's why is laid on rows of its own whole, none
// beginning where a row of the screen's own does, and the box screen's
// line gives the answer's age and the way to the screen before the
// verdict, so a cut at the window's edge cuts neither; a warning's text is not said
// after its level twice; and a 403, held until the person asks, says
// how to ask, and a 404 (a moteld older than the read) does not.
func TestBackupsSays(t *testing.T) {
	s, f, now := session(t)
	s.Engine.BackupsOpen(true, *now)
	why := "backups are not set up: /etc/motel-backup/repository.env is not there (/etc/motel/backup.env is from before this version and is not read: run `motel-backup setup` as root)"
	f.Body("backups.json", `{"at":"@-2s","healthy":false,"could_not_tell":"`+why+`","apps":[],"running":[],"unrecognised_units":[],"lines":[{"level":"warning","text":"warning: the run said so"}]}`)
	for range 4 { // whoami, the list, the watch's alerts, the backups
		step(t, s, f, now)
	}
	if s.BackupsSt == nil {
		t.Fatalf("not read: %v", s.BackupsErr)
	}
	v := s.BackupsViewFit(*now, 60, runes)
	text := joined(v)
	if !strings.Contains(strings.Join(strings.Fields(text), " "), "as root)\"") || !strings.Contains(text, "· the box's claim\n") {
		t.Errorf("at 60 columns the screen says:\n%s", text)
	}
	for _, l := range v {
		if l.Width(runes) > 60 {
			t.Errorf("a row wider than 60: %q", l.Text())
		}
	}
	// The box's why is on rows of its own, none beginning where one of
	// the screen's own does: at the value's column or further in.
	if len(v) < 4 || v[0].Text() != "backups     could not tell, the box says" || !strings.HasPrefix(v[1].Text(), strings.Repeat(" ", 12)+`"`) {
		t.Fatalf("the verdict and the first row of its why:\n%s", text)
	}
	for _, l := range v[2:] {
		if strings.HasPrefix(l.Text(), strings.Repeat(" ", 12)+"decided ") {
			break
		}
		if !strings.HasPrefix(l.Text(), strings.Repeat(" ", 14)) {
			t.Errorf("a row of the box's why begins before column 14: %q", l.Text())
		}
	}
	if strings.Contains(text, "warning: the run said so") || !strings.Contains(text, "warning  the run said so") {
		t.Errorf("a warning's line:\n%s", text)
	}
	box := s.BoxBackupsLine(*now).Cut(60, runes).Text()
	if !strings.HasPrefix(box, "backups     ") || !strings.Contains(box, " ago · :backups · could not tell") {
		t.Errorf("the box screen's line at 60 columns: %q", box)
	}

	// A read that gets no answer keeps the answer before it, and the
	// crumb says the last read failed (an 80x24 screen holds the crumb,
	// and not the failure's line under the box's lines).
	*now = now.Add(model.BoxEvery)
	f.Set("backups", &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
	for range 3 {
		step(t, s, f, now)
	}
	if s.BackupsSt == nil || !strings.HasSuffix(s.BackupsAsOf(*now).Text(), " · the last read failed") {
		t.Errorf("after a read that got no answer: held %v, the crumb says %q", s.BackupsSt != nil, s.BackupsAsOf(*now).Text())
	}
	// An answer again: the crumb says no failure.
	f.Set("backups", nil)
	*now = now.Add(model.BoxEvery)
	for range 3 {
		step(t, s, f, now)
	}
	if l := s.BackupsAsOf(*now).Text(); s.BackupsErr != nil || strings.Contains(l, "failed") {
		t.Errorf("after an answer again: the crumb says %q, err %v", l, s.BackupsErr)
	}

	// A 403 drops the answer, holds the read, and says ctrl-r asks again.
	*now = now.Add(model.BoxEvery)
	f.Set("backups", &model.Failure{Kind: model.FailForbidden, Message: "deploy.example.com does not allow your key this: it needs the viewer role on the box."})
	for range 3 {
		step(t, s, f, now)
	}
	if text := joined(s.BackupsView(*now)); s.BackupsSt != nil || s.BackupsAsOf(*now) != nil || !strings.Contains(text, "viewer role on the box. ctrl-r asks again.\n") {
		t.Errorf("after a 403: held %v, the screen says:\n%s", s.BackupsSt != nil, text)
	}
	// A 404 is a moteld older than the read: asking again mends nothing.
	s.Engine.Again(*now)
	*now = now.Add(model.BoxEvery)
	f.Set("backups", &model.Failure{Kind: model.FailNotFound, Message: `deploy.example.com answered 404; the box says: "not found"`})
	s.Engine.Again(*now, model.Job{Kind: model.KBackups})
	for range 3 {
		step(t, s, f, now)
	}
	if text := joined(s.BackupsView(*now)); !strings.Contains(text, "answered 404") || strings.Contains(text, "ctrl-r") {
		t.Errorf("after a 404 the screen says:\n%s", text)
	}
}
