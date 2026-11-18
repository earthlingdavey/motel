package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestRefusalDropsTheAnswer: a 403 or a 404 on an app's status and on a
// record drops what was held of them, so a later read that gets no
// answer (ctrl-r in an outage, or the record opened again) cannot bring
// the answer from before the refusal back: not on the row, the app's
// screen, the record's screen, y and P, the strip and summary, or p and
// n. A later answer is shown again.
func TestRefusalDropsTheAnswer(t *testing.T) {
	const version = "e9a0b3f2c118"
	rec := model.Job{Kind: model.KDeploy, App: "blog", Version: version}
	status := model.Job{Kind: model.KStatus, App: "blog"}
	appV := model.View{Screen: model.ScreenApp, App: "blog"}
	recV := model.View{Screen: model.ScreenRecord, App: "blog", Version: version}
	for _, kind := range []model.Fail{model.FailForbidden, model.FailNotFound} {
		for _, again := range []string{"ctrl-r", "opened again"} {
			name := map[model.Fail]string{model.FailForbidden: "403", model.FailNotFound: "404"}[kind] + ", then " + again
			t.Run(name, func(t *testing.T) {
				now := t0
				f := &demo.Fake{Now: func() time.Time { return now }}
				f.Body("apps.json", `{"apps": [{"name": "docs"}, {"name": "blog"}]}`)
				s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
				for range 6 {
					step(t, s, f, &now)
				}
				s.Engine.Focus("blog", now)
				s.Engine.Once(rec, now)
				for step(t, s, f, &now) != rec {
				}
				if len(s.Overview("blog", now).Fields) == 0 || len(s.RecordView("blog", version, now).Head) == 0 {
					t.Fatal("the status and the record were not read")
				}
				refusal := &model.Failure{Kind: kind, Message: "deploy.example.com does not allow your key this: it needs the viewer role on this app."}
				f.Set("blog", refusal)
				f.Set("blog@"+version, refusal)
				now = now.Add(10 * time.Second)
				s.Engine.Again(now, rec, status)
				for seen := map[model.Job]bool{}; !seen[rec] || !seen[status]; {
					seen[step(t, s, f, &now)] = true
				}

				none := &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com timed out on 443: no whole answer in 30 s."}
				f.Set("blog", none)
				f.Set("blog@"+version, none)
				now = now.Add(10 * time.Minute)
				if again == "ctrl-r" {
					s.Engine.Again(now, rec, status)
				} else {
					s.Engine.Drop(rec)
					s.Engine.Again(now, status)
					s.Engine.Once(rec, now)
				}
				for seen := map[model.Job]bool{}; !seen[rec] || !seen[status]; {
					seen[step(t, s, f, &now)] = true
				}

				o := s.Overview("blog", now)
				if len(o.Fields) != 0 || len(o.Deploys) != 0 || o.AsOf != nil {
					t.Errorf("the app's screen shows the status the box refused: %d fields, %d deploys, crumb %q", len(o.Fields), len(o.Deploys), o.AsOf.Text())
				}
				r := s.RecordView("blog", version, now)
				if len(r.Head) != 0 || s.RecordAsOf("blog", version, now) != nil {
					t.Errorf("the record's screen shows the record the box refused: %q", linesText(r.Head...))
				}
				if !strings.Contains(linesText(r.Note...), "timed out") {
					t.Errorf("the record's screen does not say its read failed: %q", linesText(r.Note...))
				}
				for _, v := range []model.View{appV, recV} {
					if _, _, ok := s.Body(v); ok || s.CanPrint(v, 2) {
						t.Errorf("y and P give the answer the box refused on screen %d", v.Screen)
					}
				}
				rw := row(s, now, "blog")
				if st := rw.Cells[model.ColState].String(); strings.Contains(st, "running") || rw.Cells[model.ColVersion].String() != "-" {
					t.Errorf("the row shows the status the box refused: %q, version %q", st, rw.Cells[model.ColVersion])
				}
				v := s.AppsView(now, "", 0, 0)
				if all := linesText(v.Strip...) + v.Summary.Text(); strings.Contains(all, "running") {
					t.Errorf("the strip or summary counts the status the box refused: %q", all)
				}

				// A later answer is shown.
				f.Set("blog", nil)
				f.Set("blog@"+version, nil)
				now = now.Add(time.Minute)
				asked := now
				s.Engine.Again(now, rec, status)
				for seen := map[model.Job]bool{}; !seen[rec] || !seen[status]; {
					seen[step(t, s, f, &now)] = true
				}
				if len(s.Overview("blog", now).Fields) == 0 || len(s.RecordView("blog", version, now).Head) == 0 {
					t.Error("a later answer is not shown")
				}
				if _, at, ok := s.Body(appV); !ok || at.Before(asked) {
					t.Errorf("y does not give the later answer: %v %v", ok, at)
				}
			})
		}
	}
}

func linesText(ls ...model.Line) string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Text())
	}
	return strings.Join(out, "\n")
}
