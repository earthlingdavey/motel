package model

import (
	"context"
	"time"
)

// Action is a read the TUI makes, and the motel command that makes
// the same read. Every read goes through this table: a read with no
// action is not made, so the TUI reads nothing a command does not
// (TestActionsAreCommands holds that each names an existing command,
// and that its read calls that command's Client method).
type Action struct {
	Kind    Kind
	Command string
	// What says where the TUI shows it.
	What string
	read func(ctx context.Context, s *Session, c Client, j Job) (func(*Session, time.Time), error)
}

// Actions is the TUI's whole surface. It acts on nothing: it reads.
var Actions = []Action{
	{KWhoAmI, "whoami", "the title bar", readWhoAmI},
	{KApps, "apps", "the apps screen", readApps},
	{KStatus, "status <app>", "an app's row and screen", readStatus},
	{KDeploy, "deploys <app> <version>", "a deploy's record", readDeploy},
	{KBox, "box status", "the box screen", readBox},
	{KBackups, "backup status", "the backups screen, and its line on the box screen", readBackups},
	{KAlerts, "monitor alerts --app <app>", "the alerts on an app's screen", readAlerts},
}

// ActionFor is the action that reads k.
func ActionFor(k Kind) (Action, bool) {
	for _, a := range Actions {
		if a.Kind == k {
			return a, true
		}
	}
	return Action{}, false
}

// Read makes the action's read with c, for a test that watches which
// Client method it calls.
func (a Action) Read(ctx context.Context, c Client, j Job) error {
	_, err := a.read(ctx, &Session{}, c, j)
	return err
}

func readWhoAmI(ctx context.Context, s *Session, c Client, _ Job) (func(*Session, time.Time), error) {
	g, err := c.WhoAmI(ctx)
	if err == nil {
		err = s.aboutAnother(g.V)
	}
	return func(s *Session, at time.Time) {
		if err != nil {
			s.MeErr = err
			return
		}
		g.At = at
		s.Me, s.MeErr = &g, nil
	}, err
}

func readApps(ctx context.Context, _ *Session, c Client, _ Job) (func(*Session, time.Time), error) {
	g, err := c.Apps(ctx)
	return func(s *Session, at time.Time) {
		if err != nil {
			s.ListErr = err
			return
		}
		g.At = at
		s.List, s.ListErr = &g, nil
		s.Engine.SetApps(g.V, at)
	}, err
}

func readStatus(ctx context.Context, _ *Session, c Client, j Job) (func(*Session, time.Time), error) {
	g, err := c.Status(ctx, j.App)
	return func(s *Session, at time.Time) {
		a := s.app(j.App)
		if err != nil {
			a.err, a.errAt = err, at
			if Forbidden(err) || NotFound(err) {
				// The box's last word on it is a refusal: what was held of
				// it goes, so no later failure can bring it back.
				a.st = nil
			}
			return
		}
		g.At = at
		a.st, a.err = &g, nil
	}, err
}

func readDeploy(ctx context.Context, _ *Session, c Client, j Job) (func(*Session, time.Time), error) {
	g, err := c.Deploy(ctx, j.App, j.Version)
	return func(s *Session, at time.Time) {
		if s.records == nil {
			s.records = map[Job]*recData{}
		}
		r := s.records[j]
		if r == nil {
			r = &recData{}
			s.records[j] = r
		}
		if err != nil {
			// A record read before is kept beside a failure, but for a 403
			// or a 404, which drops it as a status's does.
			r.err = err
			if Forbidden(err) || NotFound(err) {
				r.d = nil
			}
			return
		}
		g.At = at
		r.d, r.err = &g, nil
	}, err
}

func readBackups(ctx context.Context, _ *Session, c Client, _ Job) (func(*Session, time.Time), error) {
	g, err := c.Backups(ctx)
	return func(s *Session, at time.Time) {
		if err != nil {
			s.BackupsErr = err
			if Forbidden(err) || NotFound(err) {
				// Dropped, as a status's is (readStatus): a box older than
				// the read answers 404.
				s.BackupsSt = nil
			}
			return
		}
		g.At = at
		s.BackupsSt, s.BackupsErr = &g, nil
	}, err
}

func readAlerts(ctx context.Context, _ *Session, c Client, j Job) (func(*Session, time.Time), error) {
	g, err := c.Alerts(ctx, j.App)
	return func(s *Session, at time.Time) {
		a := s.app(j.App)
		if err != nil {
			a.alertsErr = err
			if Forbidden(err) || NotFound(err) {
				// Dropped, as a status's is (readStatus): a box older than
				// the read answers 404.
				a.alerts = nil
			}
			return
		}
		g.At = at
		a.alerts, a.alertsErr = &g, nil
	}, err
}

func readBox(ctx context.Context, _ *Session, c Client, _ Job) (func(*Session, time.Time), error) {
	g, err := c.BoxStatus(ctx)
	return func(s *Session, at time.Time) {
		if err != nil {
			s.BoxErr = err
			if Forbidden(err) || NotFound(err) {
				// Dropped, as a status's is (readStatus).
				s.BoxSt = nil
			}
			return
		}
		g.At = at
		s.BoxSt, s.BoxErr = &g, nil
	}, err
}
