package model

import (
	"strings"
	"time"
)

// Screen is a view the TUI shows.
type Screen int

// The screens. ScreenFrame is the title bar, which reads whoami.
const (
	ScreenApps Screen = iota
	ScreenApp
	ScreenRecord
	ScreenFrame
)

// View is the screen open and what it is of.
type View struct {
	Screen       Screen
	App, Version string
}

// Action is a view the TUI offers and the motelctl command that reads
// the same: every view is first a command (TestActionsAreCommands
// walks the command tree for each).
type Action struct {
	Screen  Screen
	Read    Kind
	Command string
}

// Actions is the TUI's whole surface. It acts on nothing: part 1 only
// reads.
var Actions = []Action{
	{ScreenApps, KApps, "apps"},
	{ScreenApp, KStatus, "status <app>"},
	{ScreenRecord, KDeploy, "deploys <app> <version>"},
	{ScreenFrame, KWhoAmI, "whoami"},
}

// Title is the title bar at now: who and where on the left, and the
// freshness word on the right, which is never colour alone.
func (s *Session) Title(now time.Time) (Line, Line) {
	left := Line{ownC("motel", Strong)}
	if s.Demo {
		left = append(left, ownC(" · SAMPLE DATA", Warn))
	}
	left = append(left, own(" · "+s.Host))
	if s.Me != nil {
		if r := s.Me.V.Version; r != "" {
			left = append(left, own(" · "+r))
		}
		left = append(left, own(" │ "+s.Me.V.Name+" · "+s.scopes()))
	} else {
		left = append(left, own(" │ not signed in"))
	}
	return left, s.freshness(now)
}

// scopes says the roles held: "viewer of the box", "viewer of blog, shop".
func (s *Session) scopes() string {
	byRole := map[string][]string{}
	var roles []string
	for _, r := range s.Me.V.Roles {
		if _, ok := byRole[r.Role]; !ok {
			roles = append(roles, r.Role)
		}
		on := "the box"
		if n, ok := strings.CutPrefix(r.Scope, "app:"); ok {
			on = n
		}
		byRole[r.Role] = append(byRole[r.Role], on)
	}
	if len(roles) == 0 {
		return "no grant here"
	}
	var parts []string
	for _, r := range roles {
		parts = append(parts, r+" of "+strings.Join(byRole[r], ", "))
	}
	return strings.Join(parts, "; ")
}

func (s *Session) freshness(now time.Time) Line {
	h := s.Engine.Health(now)
	if h.Back {
		s.backAt = now
	}
	ago := "never"
	if !s.last.IsZero() {
		ago = Ago(now.Sub(s.last)) + " ago"
	}
	switch {
	case s.Signing && s.Key.Hardware:
		return Line{ownC("TOUCH YOUR KEY to sign", Warn)}
	case h.Stop == SignedOut:
		return Line{ownC("SIGNED OUT: the box refused the key (401)", Bad)}
	case h.Stop == NotSigned:
		return Line{ownC("NOT SIGNED: r signs again", Bad)}
	case h.Stop == BadCertificate:
		return Line{ownC("STOPPED: the certificate did not verify", Bad)}
	case h.Paused:
		return Line{ownC("PAUSED (idle 10m): any key resumes", Warn)}
	case h.SlowIn > 0:
		return Line{ownC("slowed by the box (429) · next in "+Ago(h.SlowIn), Warn)}
	case h.Misses >= StaleAfter:
		l := "STALE · " + ago
		if !s.last.IsZero() {
			l += ": no answer since " + hms(s.last)
		}
		return Line{ownC(l+", retry in "+Ago(h.RetryIn), Bad)}
	case h.Misses > 0:
		return Line{ownC("slow · "+ago, Warn)}
	case !s.backAt.IsZero() && now.Sub(s.backAt) < 5*time.Second:
		return Line{ownC("back · "+hms(s.backAt), Good)}
	}
	return Line{own("live · " + ago)}
}
