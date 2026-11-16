package tui

import (
	"context"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

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
	// Signing, when set, is given the function that tells the title
	// bar a token is being signed (a hardware key waits for a touch),
	// and nil when the screen is gone.
	Signing func(func(bool))
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
//
// tview's Run sets its screen to nil, unlocked, as it returns, so Stop
// must never be called from outside its event loop: q stops it from
// inside, and the end of ctx is sent in as ctrl-c, which tview handles
// there too.
func Run(ctx context.Context, o Options, start model.View) error {
	u := New(o.Session, o.Colour, start)
	if o.Screen != nil {
		u.App.SetScreen(o.Screen)
	}
	ctx, cancel := context.WithCancel(ctx)
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
					u.post(func() {})
				}
			}
		}()
	}
	go func() {
		select {
		case <-ctx.Done():
			u.App.QueueEvent(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone))
		case <-u.ended:
		}
	}()
	if o.Started != nil {
		go u.post(o.Started)
	}
	if o.Signing != nil {
		o.Signing(func(on bool) { u.post(func() { u.S.Signing = on }) })
		defer o.Signing(nil)
	}
	err := u.App.Run()
	close(u.ended)
	cancel()
	wg.Wait()
	return err
}

// post runs f on the event loop and draws, or does nothing once the
// loop has ended: tview's own queue would wait for it for ever.
func (u *UI) post(f func()) {
	done := make(chan struct{})
	go func() {
		u.App.QueueUpdateDraw(f)
		close(done)
	}()
	select {
	case <-done:
	case <-u.ended:
	}
}

// Drive runs the engine, applying each answer on the event goroutine;
// the answer to a read the person left is dropped.
func (u *UI) Drive(ctx context.Context, c model.Client, clock model.Clock) {
	u.S.Engine.Run(ctx, clock, func(jctx context.Context, j model.Job) error {
		apply, err := u.S.Fetch(jctx, c, j)
		if jctx.Err() == nil {
			u.post(apply)
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
