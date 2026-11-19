package model_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestDeployToABrokenApp: a deploy in flight to an app that is stopped,
// throttled or in backoff (#584, item 4) is said on the apps screen.
// The row keeps its state and its mark, so it sorts where it did, and
// says the deploy as a row in flight does: VERSION the version in
// flight, and its reason in the strip the deploy before it, in the
// words of a row in flight, so that the strip, cut at the screen's
// width, still says it at 80 columns and the state's word after it.
// The app's screen still says the state the row names, with the
// deploying line under it. A box older than "deploying" names no
// version, and VERSION stays the running one (its app's screen has the
// deploying line too); with no deploy in flight, and for a first
// version in flight, the row is as before.
func TestDeployToABrokenApp(t *testing.T) {
	const (
		running  = `"current_version": "7c1e55d0a913", "available_versions": ["7c1e55d0a913"]`
		flying   = `"phase": "soaking", "deploying": {"version": "9a1b2c3d4e5f", "deployed_by": "local:bob-laptop", "started_at": "@-25s"}`
		upload   = `"phase": "idle", "deploying": {"version": "9a1b2c3d4e5f", "deployed_by": "local:bob-laptop", "started_at": "@-25s"}`
		older    = `"phase": "soaking"`
		idle     = `"phase": "idle"`
		throttle = `"watchdog": {"state": "throttled", "restarts_in_window": 5, "last_restart_cause": "probe: timeout"}`
		backoff  = `"watchdog": {"state": "backoff", "consecutive_failures": 3}`
		watching = `"watchdog": {"state": "watching"}`
	)
	const (
		stopped   = `stopped: 7c1e55d0a913 is not running`
		throttled = `throttled: 5 restarts in its window; last cause "probe: timeout"`
		backedOff = `backoff: the watchdog waits to restart it; 3 failed checks in a row`
	)
	for _, c := range []struct {
		name   string
		body   string
		mark   model.Mark
		state  string
		ver    string
		why    string
		screen string
	}{
		{"stopped", flying + `, "running": false, ` + running + `, ` + watching,
			model.MarkBroken, "stopped", "→ 9a1b2c3d4e5f", "deploying 9a1b2c3d4e5f: soaking (seen …); " + stopped, "stopped 7c1e55d0a913"},
		{"stopped, uploading", upload + `, "running": false, ` + running + `, ` + watching,
			model.MarkBroken, "stopped", "→ 9a1b2c3d4e5f", "deploying 9a1b2c3d4e5f (seen …); " + stopped, "stopped 7c1e55d0a913"},
		{"throttled", flying + `, "running": true, ` + running + `, ` + throttle,
			model.MarkBroken, "throttled", "→ 9a1b2c3d4e5f", "deploying 9a1b2c3d4e5f: soaking (seen …); " + throttled, "throttled 7c1e55d0a913"},
		{"backoff", flying + `, "running": true, ` + running + `, ` + backoff,
			model.MarkLook, "backoff", "→ 9a1b2c3d4e5f", "deploying 9a1b2c3d4e5f: soaking (seen …); " + backedOff, "backoff 7c1e55d0a913"},
		{"stopped, an older box", older + `, "running": false, ` + running + `, ` + watching,
			model.MarkBroken, "stopped", "7c1e55d0a913", "deploying: soaking (seen …); " + stopped, "stopped 7c1e55d0a913"},
		{"stopped, nothing in flight", idle + `, "running": false, ` + running + `, ` + watching,
			model.MarkBroken, "stopped", "7c1e55d0a913", stopped, "stopped 7c1e55d0a913"},
		{"throttled, nothing in flight", idle + `, "running": true, ` + running + `, ` + throttle,
			model.MarkBroken, "throttled", "7c1e55d0a913", throttled, "throttled 7c1e55d0a913"},
		{"backoff, nothing in flight", idle + `, "running": true, ` + running + `, ` + backoff,
			model.MarkLook, "backoff", "7c1e55d0a913", backedOff, "backoff 7c1e55d0a913"},
		{"the first version", flying + `, "running": false, "available_versions": [], ` + watching,
			model.MarkMoving, "soaking", "→ 9a1b2c3d4e5f", "deploying 9a1b2c3d4e5f: soaking (seen …)", ""},
	} {
		s, now := brokenSession(t, `{"app": "mailer", `+c.body+`}`)
		r := row(s, now, "mailer")
		why := seen.ReplaceAllString(r.Why.Text(), "(seen …)")
		if r.Mark != c.mark || r.Cells[model.ColState].String() != c.state || r.Cells[model.ColVersion].String() != c.ver || why != c.why {
			t.Errorf("%s: mark %q, STATE %q, VERSION %q, why %q; want %q, %q, %q, %q", c.name,
				r.Mark.Glyph(), r.Cells[model.ColState], r.Cells[model.ColVersion], why, c.mark.Glyph(), c.state, c.ver, c.why)
		}
		v := s.AppsView(now, "", 0, 0)
		strip := ""
		for _, l := range textsOfLines(v.Strip) {
			if strings.Contains(l, " mailer ") {
				strip = seen.ReplaceAllString(l, "(seen …)")
			}
		}
		if want := c.mark.Glyph() + " mailer       " + c.why; strip != want {
			t.Errorf("%s: the strip says %q, want %q", c.name, strip, want)
		}
		// As the screen cuts it at 80 columns: the deploy, and the
		// state's word after it.
		if cut := (model.Line{model.Seg{T: termtext.From(strip)}}).Cut(80, runes).Text(); strings.Contains(c.body, `"soaking"`) &&
			(!strings.Contains(cut, "deploying") || !strings.Contains(cut, ": soaking") || !strings.Contains(cut, c.state)) {
			t.Errorf("%s: cut at 80 columns the strip says %q", c.name, cut)
		}
		// By attention it sorts by its own mark: before a row in flight
		// (⟳) when broken or in backoff.
		if got := v.Rows[0].Name; (c.mark < model.MarkMoving) != (got == "mailer") {
			t.Errorf("%s: by attention %s is first", c.name, got)
		}
		if c.screen == "" {
			continue
		}
		fields := textsOfLines(s.Overview("mailer", now).Fields)
		if len(fields) == 0 {
			t.Errorf("%s: the app's screen has no fields", c.name)
			continue
		}
		deploying := false
		for _, f := range fields {
			deploying = deploying || strings.HasPrefix(f, "deploying ")
		}
		if !strings.HasPrefix(fields[0], "state       "+c.screen) || deploying != strings.Contains(c.why, "deploying") {
			t.Errorf("%s: the app's screen says %q", c.name, fields)
		}
	}
}

var seen = regexp.MustCompile(`\(seen \d+s ago\)`)

// brokenSession is a session whose box lists mailer, of the status
// body given, and api, a deploy of it in flight to an app that runs.
func brokenSession(t *testing.T, status string) (*model.Session, time.Time) {
	t.Helper()
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "api"}, {"name": "mailer"}]}`)
	f.Body("status/mailer.json", status)
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 8 {
		step(t, s, f, &now)
	}
	return s, now
}
