package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/client"
)

// heard is a writer for a goroutine's writes: first is closed at the
// first.
type heard struct {
	mu    sync.Mutex
	b     strings.Builder
	once  sync.Once
	first chan struct{}
}

func newHeard() *heard { return &heard{first: make(chan struct{})} }

func (h *heard) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.b.Write(p)
	h.once.Do(func() { close(h.first) })
	return len(p), nil
}

func (h *heard) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.b.String()
}

// seconds is sayAsking's clock for a test: each call to after is a
// second of its own, handed to the test on asked, which ends it.
type seconds struct{ asked chan chan time.Time }

func newSeconds() *seconds { return &seconds{asked: make(chan chan time.Time, 8)} }

func (s *seconds) after() <-chan time.Time {
	c := make(chan time.Time, 1)
	s.asked <- c
	return c
}

// TestSayAsking: the line is said once the first reads have waited a
// second; not when they end before; and a signature's time (a hardware
// key's touch) is not counted.
func TestSayAsking(t *testing.T) {
	const line = "motel: asking deploy.example.com …\n"
	var now time.Time

	w, clock := newHeard(), newSeconds()
	stop := sayAsking(context.Background(), w, line, clock.after, nil)
	<-clock.asked <- now
	<-w.first
	stop()
	if w.String() != line {
		t.Errorf("a second waited: %q", w.String())
	}

	w, clock = newHeard(), newSeconds()
	stop = sayAsking(context.Background(), w, line, clock.after, nil)
	second := <-clock.asked
	stop()
	second <- now
	if w.String() != "" {
		t.Errorf("the reads ended within the second: %q", w.String())
	}

	w, clock = newHeard(), newSeconds()
	var note func(bool)
	stop = sayAsking(context.Background(), w, line, clock.after, func(f func(bool)) { note = f })
	second = <-clock.asked
	note(true) // signing: the second asked for no longer counts
	second <- now
	note(false) // signed: a new second
	second = <-clock.asked
	if w.String() != "" {
		t.Errorf("a second spent signing: %q", w.String())
	}
	second <- now
	<-w.first
	stop()
	if w.String() != line {
		t.Errorf("a second waited after signing: %q", w.String())
	}
	if note != nil {
		t.Error("the signer is still told after the first reads")
	}
}

// TestSaidWhileTheBoxWaits: motel started while the box does not
// answer says on stderr, once the first read has waited, what it waits
// for, the box's name made safe; stdout holds nothing. An interrupt or
// a closed terminal then ends it as before: exit 130, nothing more said.
func TestSaidWhileTheBoxWaits(t *testing.T) {
	was := askingAfter
	askingAfter = time.Millisecond
	t.Cleanup(func() { askingAfter = was })
	fb := newFakeBox(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	for _, sig := range []string{"interrupt", "SIGHUP"} {
		t.Run(sig, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			end := cancel
			if sig == "SIGHUP" {
				end = func() { _ = syscall.Kill(os.Getpid(), syscall.SIGHUP) }
			}
			var once sync.Once
			errOut := newHeard()
			// The first read waits until the line is said, then the
			// command is ended, and the read with it.
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				select {
				case <-errOut.first:
				case <-time.After(20 * time.Second): // a guard
					t.Error("nothing said while the first read waited")
				}
				once.Do(end)
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			var out bytes.Buffer
			code := Main(ctx, nil, Env{Terminal: true,
				Getenv: func(k string) string { return env[k] }, Stdout: &out, Stderr: errOut, Dir: t.TempDir(), Home: home,
				Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
				Transport: rt, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test",
			})
			want := "motel: asking 127.0.0.1 who you are and for its apps; the screen opens when it answers, or within a minute if it does not (ctrl-c quits)\n"
			if code != client.ExitInterrupted || errOut.String() != want || out.Len() != 0 {
				t.Errorf("exit %d, stderr %q, stdout %q; want %d, %q and nothing", code, errOut.String(), out.String(), client.ExitInterrupted, want)
			}
		})
	}
}

// TestNotSaidWhileTheClientPauses: a box that refuses the key (401)
// answers at once, and the client pauses before its one retry with the
// same token (retryPerson); that pause is the client's own, not the
// box waited for, so nothing is said before the screen, though it
// lasts longer than askingAfter.
func TestNotSaidWhileTheClientPauses(t *testing.T) {
	was := askingAfter
	askingAfter = 100 * time.Millisecond
	t.Cleanup(func() { askingAfter = was })
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": "https://deploy.example.com/blog"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errOut := newHeard()
	var mu sync.Mutex
	var paused, asked int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		asked++
		if asked == 2 {
			// The retry: the screen would open now, on SIGNED OUT; the
			// test ends before it.
			cancel()
		}
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":"unknown key"}` + "\n")), Request: r}, nil
	})
	var out bytes.Buffer
	code := Main(ctx, nil, Env{Terminal: true,
		Getenv: func(k string) string { return env[k] }, Stdout: &out, Stderr: errOut, Dir: t.TempDir(), Home: home,
		Now: time.Now,
		Sleep: func(context.Context, time.Duration) error {
			mu.Lock()
			paused++
			mu.Unlock()
			// The client's pause, five times askingAfter, or until the
			// line is said.
			select {
			case <-errOut.first:
			case <-time.After(5 * askingAfter):
			}
			return nil
		},
		Transport: rt, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test",
	})
	if paused != 1 || asked != 2 {
		t.Fatalf("asked %d times, paused %d: want the 401's one retry after a pause", asked, paused)
	}
	if code != client.ExitInterrupted || errOut.String() != "" || out.Len() != 0 {
		t.Errorf("exit %d, stderr %q, stdout %q; want %d and nothing said", code, errOut.String(), out.String(), client.ExitInterrupted)
	}
}
