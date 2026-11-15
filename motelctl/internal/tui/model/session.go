package model

import (
	"context"
	"errors"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/client"
)

// Session is what the TUI knows of the box: the last answer to each
// read and when it came, kept when a later read fails, so nothing on
// screen is blanked by a lost connection. It is touched only on the
// UI's goroutine; Fetch makes the read off it and returns the change
// to apply on it.
type Session struct {
	// Host is the box asked, as the person named it (a checked host
	// name, never the box's own words).
	Host string
	// Demo marks every screen as sample data.
	Demo bool
	// Version is motelctl's own release.
	Version string
	Key     Key
	// TokenExpires is when the token last signed stops being taken.
	TokenExpires func() time.Time
	Engine       *Engine
	Now          func() time.Time

	Me     *Got[wire.WhoAmI]
	MeErr  error
	List   *Got[[]string]
	BoxSt  *Got[wire.BoxStatus]
	BoxErr error
	// Signing is set while a token is being signed: a hardware key
	// waits for its touch.
	Signing bool
	// StopErr is the failure that stopped every read.
	StopErr error

	apps    map[string]*appData
	records map[Job]*recData
	// last is when the box last answered anything; backAt when it
	// answered again after a run of no answer.
	last, backAt time.Time
}

// Key is the key the session signs with, as this side knows it.
type Key struct {
	Fingerprint, Shown, Source string
	// Hardware is a key that needs a touch to sign (an sk- type).
	Hardware bool
	// Line is the key as a person line carries it.
	Line string
}

type appData struct {
	st    *Got[wire.Status]
	err   error
	errAt time.Time
}

type recData struct {
	d   *Got[wire.DeployResult]
	err error
}

// Fetch makes the read j with c and returns its error, for the
// engine, and the change to apply on the UI's goroutine; an answer to
// a read the person left (its context cancelled) changes nothing.
func (s *Session) Fetch(ctx context.Context, c Client, j Job) (func(), error) {
	var apply func(at time.Time)
	var err error
	switch j.Kind {
	case KWhoAmI:
		var g Got[wire.WhoAmI]
		g, err = c.WhoAmI(ctx)
		apply = func(at time.Time) {
			if err == nil {
				g.At = at
				s.Me, s.MeErr = &g, nil
			} else {
				s.MeErr = err
			}
		}
	case KApps:
		var g Got[[]string]
		g, err = c.Apps(ctx)
		apply = func(at time.Time) {
			if err == nil {
				g.At = at
				s.List = &g
				s.Engine.SetApps(g.V, at)
			}
		}
	case KStatus:
		var g Got[wire.Status]
		g, err = c.Status(ctx, j.App)
		apply = func(at time.Time) {
			a := s.app(j.App)
			if err == nil {
				g.At = at
				a.st, a.err = &g, nil
			} else {
				a.err, a.errAt = err, at
			}
		}
	case KDeploy:
		var g Got[wire.DeployResult]
		g, err = c.Deploy(ctx, j.App, j.Version)
		apply = func(at time.Time) {
			if s.records == nil {
				s.records = map[Job]*recData{}
			}
			if err == nil {
				g.At = at
				s.records[j] = &recData{d: &g}
			} else {
				s.records[j] = &recData{err: err}
			}
		}
	case KBox:
		var g Got[wire.BoxStatus]
		g, err = c.BoxStatus(ctx)
		apply = func(at time.Time) {
			if err == nil {
				g.At = at
				s.BoxSt, s.BoxErr = &g, nil
			} else {
				s.BoxErr = err
			}
		}
	}
	at := s.Now()
	if errors.Is(err, context.Canceled) {
		return func() {}, err
	}
	return func() {
		switch out, _ := Classify(err); out {
		case Answered:
			s.last, s.StopErr = at, nil
		case Refused:
			s.last = at
		case Stopped:
			s.StopErr = err
		}
		apply(at)
	}, err
}

func (s *Session) app(name string) *appData {
	if s.apps == nil {
		s.apps = map[string]*appData{}
	}
	a, ok := s.apps[name]
	if !ok {
		a = &appData{}
		s.apps[name] = a
	}
	return a
}

// Record is the deploy record read for app and version, if any.
func (s *Session) Record(app, version string) (*Got[wire.DeployResult], error, bool) {
	r, ok := s.records[Job{Kind: KDeploy, App: app, Version: version}]
	if !ok {
		return nil, nil, false
	}
	return r.d, r.err, true
}

// Forbidden says whether err is a 403.
func Forbidden(err error) bool {
	var f *client.Failure
	return errors.As(err, &f) && f.Exit == client.ExitForbidden
}

// NotFound says whether err is a 404.
func NotFound(err error) bool {
	var f *client.Failure
	return errors.As(err, &f) && f.Exit == client.ExitNotFound
}

// Names is the apps the session knows, as the box listed them.
func (s *Session) Names() []string {
	if s.List == nil {
		return nil
	}
	return s.List.V
}
