package tui

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
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
	// Resign drops the token last signed, for r.
	Resign func()
}

// Prime makes the first reads before the screen opens, whoami and the
// apps list, so the TUI opens on the right screen: a person with one
// app opens straight on it, and a refused key on the refusal screen. A
// hardware key's first touch is asked for on the plain terminal.
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

// Printed is what P asked to print below the prompt: Raw, the box's
// answer as the command's --json prints it, for a stdout that is not a
// terminal; Shown, when it is the box's answer, indented and made safe
// for one ("" when Raw is the client's words).
type Printed struct{ Raw, Shown string }

// ErrClosed is Run's end when the terminal went away while the screen
// held it: a read of it failed (a terminal window closed reads as an
// end of file, often before its SIGHUP arrives).
var ErrClosed = errors.New("the terminal went away")

// Run shows the screens until the person quits or ctx ends, and
// returns what P asked to print below the prompt.
//
// tview's Run sets its screen to nil, unlocked, as it returns, so Stop
// must never be called from outside its event loop: q stops it from
// inside, and the end of ctx is sent in as ctrl-c, which tview handles
// there too.
func Run(ctx context.Context, o Options, start model.View) (Printed, error) {
	u := New(o.Session, o.Colour, start)
	if o.Screen != nil {
		u.App.SetScreen(o.Screen)
	}
	u.Resign = o.Resign
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer u.guard()
		u.Drive(ctx, o.Client, o.Clock)
	}()
	if o.Tick > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer u.guard()
			t := time.NewTicker(o.Tick)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					u.S.Engine.Tick(o.Clock.Now())
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
	err := u.runLoop()
	close(u.ended)
	cancel()
	wg.Wait()
	if p := u.panicked.Load(); p != nil {
		// Said only now, with the terminal given back.
		return Printed{}, &Panic{Value: p.value, Stack: p.stack}
	}
	// tcell says a failed read of the terminal as an EventError, which
	// tview's Run returns; a screen that could not start returns another.
	var ee *tcell.EventError
	if errors.As(err, &ee) {
		err = ErrClosed
	}
	return Printed{u.Print, u.Shown}, err
}

// Panic is a panic in one of the TUI's goroutines, said once the
// screen has given the terminal back.
type Panic struct {
	Value any
	Stack []byte
}

// Error names the panic only: its value may hold what a box sent, so
// what prints it (cli's runFailure) makes it safe first.
func (p *Panic) Error() string { return "the TUI failed with a panic" }

// runLoop is tview's Run, with a panic on its event loop (in an
// answer applied, a frame drawn, a key) recovered once tview has given
// the terminal back, and returned as a Panic, so Go does not print it
// raw.
func (u *UI) runLoop() (err error) {
	defer func() {
		if p := recover(); p != nil {
			u.panicked.CompareAndSwap(nil, &panicked{p, debug.Stack()})
		}
	}()
	return u.App.Run()
}

type panicked struct {
	value any
	stack []byte
}

// guard ends the screen through its event loop when the goroutine it
// is deferred in panics, so tview gives the terminal back (a panic left
// to run would leave it in raw mode); Run then returns it.
func (u *UI) guard() {
	if p := recover(); p != nil {
		u.panicked.CompareAndSwap(nil, &panicked{p, debug.Stack()})
		u.App.QueueEvent(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone))
	}
}

// post runs f on the event loop and draws, or does nothing once the
// loop has ended: tview's own queue would wait for it for ever.
func (u *UI) post(f func()) {
	done := make(chan struct{})
	go func() {
		defer u.guard()
		u.App.QueueUpdateDraw(f)
		close(done)
	}()
	select {
	case <-done:
	case <-u.ended:
	}
}

// Drive runs the engine, applying each answer on the event goroutine
// once the engine has recorded how the read ended, so a stop it caused
// is a stop when the screen looks (the refusal screen comes at once);
// the answer to a read the person left is dropped.
func (u *UI) Drive(ctx context.Context, c model.Client, clock model.Clock) {
	var apply func()
	u.S.Engine.Run(ctx, clock, func(jctx context.Context, j model.Job) error {
		a, err := u.S.Fetch(jctx, c, j)
		apply = nil
		if jctx.Err() == nil {
			apply = a
		}
		return err
	}, func() {
		if a := apply; a != nil {
			u.post(func() {
				a()
				u.stopped()
			})
		}
	})
}

// RealClock is the wall clock.
type RealClock struct{}

// Now is time.Now on the wall clock alone: a monotonic reading stops
// while a laptop sleeps, so ages and due times taken with it would
// leave the sleep out.
func (RealClock) Now() time.Time { return time.Now().Round(0) }

// After is time.After.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
