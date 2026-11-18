package model

import (
	"context"
	"errors"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
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
	// Addr is the box as --box names it: the host, and :port when it is
	// not 443, so a printed command asks where this session asks.
	Addr string
	// Demo marks every screen as sample data.
	Demo bool
	// Version is the client's own release.
	Version string
	Key     Key
	// TokenExpires is when the token last signed stops being taken.
	TokenExpires func() time.Time
	Engine       *Engine
	Now          func() time.Time

	Me      *Got[wire.WhoAmI]
	MeErr   error
	List    *Got[[]string]
	ListErr error
	BoxSt   *Got[wire.BoxStatus]
	BoxErr  error
	// Signing is set while a token is being signed: a hardware key
	// waits for its touch.
	Signing bool
	// StopErr is the failure that stopped every read.
	StopErr error

	// json is the JSON overlay's lines, made once per answer.
	json struct {
		key   jsonKey
		lines []Line
	}

	apps    map[string]*appData
	records map[Job]*recData
	// last is when the box last answered a read; backAt when it
	// answered again after a run of no answer. refused says the box
	// refused the last read it answered or refused: an order kept
	// without the clock, which may be set back between the two.
	last, backAt time.Time
	refused      bool
	// latest is the latest time the session was given: one before it is
	// the wall clock set back (seen).
	latest time.Time
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

// Fetch makes the read j with c, through the action that names its
// command, and returns its error, for the engine, and the change to
// apply on the UI's goroutine. An answer to a read the person left (its
// context cancelled) changes nothing; a read no action names is not
// made. The answer is stamped when it is applied, on the UI's
// goroutine, as every other time the session reads is, so a step of
// the clock is seen in the order the times were read (seen).
func (s *Session) Fetch(ctx context.Context, c Client, j Job) (func(), error) {
	a, ok := ActionFor(j.Kind)
	if !ok {
		return func() {}, &Failure{Kind: FailNotSent, Message: "no motel command reads this, so the TUI does not"}
	}
	apply, err := a.read(ctx, s, c, j)
	if errors.Is(err, context.Canceled) {
		return func() {}, err
	}
	return func() {
		at := s.Now()
		s.seen(at)
		switch out, stop := Classify(err); out {
		case Answered:
			s.last, s.StopErr, s.refused = at, nil, false
		case Refused:
			switch k, _ := failKind(err); k {
			case FailUnusable:
				s.last, s.refused = at, false // an answer came, if not one to use
			case FailNotSent: // nothing was asked of the box
			default:
				s.refused = true
			}
		case Stopped:
			s.StopErr = err
			if stop == NotThisKey {
				s.forget()
			}
		}
		apply(s, at)
	}, err
}

// seen is the first thing each view given a time does with it, and
// the stamp of each answer. The session is used on the UI's goroutine
// alone, so its times come in the order they were read: one before the
// latest is the wall clock set back, and every time the session keeps
// (each answer's, each failure's, the last answer's) is moved back by
// the step then, once, so no age on screen is less than it is for the
// length of the step. The engine sees the same step on its own (its
// seen), since it is also given times from other goroutines.
func (s *Session) seen(now time.Time) {
	if d := now.Sub(s.latest); d < 0 && !s.latest.IsZero() {
		move := func(ts ...*time.Time) {
			for _, t := range ts {
				if !t.IsZero() {
					*t = t.Add(d)
				}
			}
		}
		move(&s.last, &s.backAt)
		if s.Me != nil {
			move(&s.Me.At)
		}
		if s.List != nil {
			move(&s.List.At)
		}
		if s.BoxSt != nil {
			move(&s.BoxSt.At)
		}
		for _, a := range s.apps {
			if a.st != nil {
				move(&a.st.At)
			}
			move(&a.errAt)
		}
		for _, r := range s.records {
			if r.d != nil {
				move(&r.d.At)
			}
		}
	}
	s.latest = now
}

// forget drops everything the box said, after it answered about another
// key or box: who it said you are, its list, every status and record,
// the box's own answer, and the JSON made of one. Nothing it said before
// can be told from what such a box would say, so none of it is shown;
// after r, the session fills again from empty.
func (s *Session) forget() {
	s.Me, s.MeErr = nil, nil
	s.List, s.ListErr = nil, nil
	s.BoxSt, s.BoxErr = nil, nil
	s.apps, s.records = nil, nil
	s.json.key, s.json.lines = jsonKey{}, nil
}

// Forgotten says whether the session holds nothing the box said: true
// at the start, and after an answer about another key or box.
func (s *Session) Forgotten() bool {
	return s.Me == nil && s.List == nil && s.ListErr == nil && s.BoxSt == nil && s.BoxErr == nil && len(s.apps) == 0 && len(s.records) == 0 && s.json.lines == nil
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

// Record is the deploy record read for app and version, if any, and the
// failure of its last read when that failed: a record read before is
// kept beside it.
func (s *Session) Record(app, version string) (*Got[wire.DeployResult], error, bool) {
	r, ok := s.records[Job{Kind: KDeploy, App: app, Version: version}]
	if !ok {
		return nil, nil, false
	}
	return r.d, r.err, true
}

func failKind(err error) (Fail, bool) {
	var f *Failure
	if errors.As(err, &f) {
		return f.Kind, true
	}
	return 0, false
}

// Forbidden says whether err is a 403.
func Forbidden(err error) bool { k, ok := failKind(err); return ok && k == FailForbidden }

// notSent says whether err is a failure the client made with nothing
// sent.
func notSent(err error) bool { k, ok := failKind(err); return ok && k == FailNotSent }

// NotFound says whether err is a 404.
func NotFound(err error) bool { k, ok := failKind(err); return ok && k == FailNotFound }

// Trusted says whether whoami's answer is about this key on this box,
// by the whoami command's rule (AboutAnother): only then is anything in
// it shown as who you are.
func (s *Session) Trusted() bool {
	if s.Me == nil {
		return false
	}
	k, b := AboutAnother(s.Me.V, s.Key.Fingerprint, s.Host)
	return !k && !b
}

// BoxViewer says whether whoami lists a role on the box, whichever
// role: only then is the box screen offered.
func (s *Session) BoxViewer() bool {
	if !s.Trusted() {
		return false
	}
	for _, r := range s.Me.V.Roles {
		if r.Scope == "box" {
			return true
		}
	}
	return false
}

// Names is the apps the session knows, as the box listed them.
func (s *Session) Names() []string {
	if s.List == nil {
		return nil
	}
	return s.List.V
}
