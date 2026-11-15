package tui

import (
	"context"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

// Options is what Run needs.
type Options struct {
	Session *model.Session
	Client  model.Client
	Clock   model.Clock
	Colour  bool
	// Screen is nil for the terminal; a test gives a simulation screen.
	Screen tcell.Screen
	// Tick redraws the screen so ages move; zero never does.
	Tick time.Duration
	// Started is called once the screen has taken the terminal.
	Started func()
	// Signer, when set, tells the title bar while a token is signed.
	Signer *Signer
}

// Prime makes the first reads before the screen opens, whoami and the
// apps list, so the TUI opens on the right screen: a person with one
// app opens straight on it. A hardware key's first touch is asked for
// on the plain terminal.
func Prime(ctx context.Context, o Options) model.View {
	s := o.Session
	for range 2 {
		now := o.Clock.Now()
		j, ok, _ := s.Engine.Take(now)
		if !ok {
			break
		}
		apply, err := s.Fetch(ctx, o.Client, j)
		apply()
		s.Engine.Done(j, o.Clock.Now(), err)
	}
	if names := s.Names(); len(names) == 1 {
		return model.View{Screen: model.ScreenApp, App: names[0]}
	}
	return model.View{Screen: model.ScreenApps}
}

// Run shows the screens until the person quits or ctx ends.
func Run(ctx context.Context, o Options, start model.View) error {
	u := New(o.Session, o.Colour, start)
	if o.Screen != nil {
		u.App.SetScreen(o.Screen)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		u.Drive(ctx, o.Client, o.Clock)
	}()
	if o.Tick > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(o.Tick)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					u.App.QueueUpdateDraw(func() {})
				}
			}
		}()
	}
	go func() {
		<-ctx.Done()
		u.App.Stop()
	}()
	if o.Started != nil {
		u.App.QueueUpdate(o.Started)
	}
	if o.Signer != nil {
		o.Signer.Notify(func(on bool) { u.App.QueueUpdateDraw(func() { u.S.Signing = on }) })
		defer o.Signer.Notify(nil)
	}
	err := u.App.Run()
	cancel()
	wg.Wait()
	return err
}

// Drive runs the engine, applying each answer on the event goroutine.
func (u *UI) Drive(ctx context.Context, c model.Client, clock model.Clock) {
	u.S.Engine.Run(ctx, clock, func(jctx context.Context, j model.Job) error {
		apply, err := u.S.Fetch(jctx, c, j)
		if jctx.Err() == nil {
			u.App.QueueUpdateDraw(apply)
		}
		return err
	})
}

// RealClock is the wall clock.
type RealClock struct{}

// Now is time.Now.
func (RealClock) Now() time.Time { return time.Now() }

// After is time.After.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Signer tells the screen while a token is being signed, so the title
// bar can ask for a hardware key's touch, and says when the token last
// signed ends.
type Signer struct {
	Inner sshkey.Signer
	Now   func() time.Time
	mu    sync.Mutex
	note  func(bool)
	at    time.Time
}

// Expires is when the token last signed stops being taken: its times
// are sshkey's for a token of MaxLife made when signing began, as the
// client's PersonToken makes them.
func (s *Signer) Expires() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at.IsZero() {
		return time.Time{}
	}
	_, exp := sshkey.TimesLasting(s.at, sshkey.MaxLife)
	return exp
}

// Notify sets what is told; nil tells nothing.
func (s *Signer) Notify(f func(bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note = f
}

// Sign signs with Inner, saying so before and after.
func (s *Signer) Sign(ctx context.Context, key sshkey.Key, namespace string, message []byte) ([]byte, error) {
	s.mu.Lock()
	note, now := s.note, s.Now()
	s.mu.Unlock()
	if note != nil {
		note(true)
		defer note(false)
	}
	out, err := s.Inner.Sign(ctx, key, namespace, message)
	if err == nil {
		s.mu.Lock()
		s.at = now
		s.mu.Unlock()
	}
	return out, err
}
