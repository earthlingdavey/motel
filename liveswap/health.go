package liveswap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// healthConfig is the deploy gate: the new instance must answer 2xx on
// path continuously for soak before traffic cuts over, and must get
// there within deadline. An empty path means the app has no HTTP
// health endpoint (health_path off): the gate is then simply "the
// process is still alive after soak".
type healthConfig struct {
	path     string
	interval time.Duration
	timeout  time.Duration
	soak     time.Duration
	deadline time.Duration
}

// prober is the health seam; the deploy pipeline's unit tests use a
// fake, httpProber is the real thing. waitHealthy is the
// converge-then-return deploy gate; probeOnce is the steady-state
// single shot the watchdog paces itself.
type prober interface {
	waitHealthy(ctx context.Context, sock *socketRef, alive func() bool, hc healthConfig) error
	probeOnce(ctx context.Context, sock *socketRef, path string, timeout time.Duration) error
}

// httpProber polls the new instance over its unix socket until it has
// been continuously healthy for the soak period. All pacing goes
// through the injected clock so tests advance time instantly.
type httpProber struct {
	clock clock
}

func (p *httpProber) waitHealthy(ctx context.Context, sock *socketRef, alive func() bool, hc healthConfig) error {
	start := p.clock.Now()
	deadline := start.Add(hc.deadline)
	var healthySince time.Time
	lastErr := errors.New("no probe completed")
	// The last probe the app answered, kept apart from lastErr: the
	// final tick before a deadline is often a timeout or a refused
	// dial, and the answer before it is the diagnosis.
	var lastProbe *probeError

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("deploy canceled while waiting for health: %w", err)
		}
		if !alive() {
			return &healthGateError{err: errProcessExited, probe: lastProbe}
		}
		now := p.clock.Now()
		// Past the deadline, a probe is issued only if it can complete
		// the soak: ticks are interval-granular, so a soak that falls
		// due on the first tick past the deadline is met, not refused —
		// and a tick that could not meet it is refused before it spends
		// a probe (up to health_timeout) on an answer that cannot
		// change the verdict.
		pastDeadline := now.After(deadline)
		soakDue := !healthySince.IsZero() && now.Sub(healthySince) >= hc.soak
		if pastDeadline && !soakDue {
			return deadlineVerdict(hc, healthySince, deadline, lastErr, lastProbe)
		}
		wasHealthySince := healthySince // the run a failed probe below resets
		if hc.path == "" {
			// No HTTP check: soak on process liveness alone.
			if healthySince.IsZero() {
				healthySince = now
			}
			lastErr = nil
		} else if err := p.probeOnce(ctx, sock, hc.path, hc.timeout); err != nil {
			healthySince = time.Time{} // health must be continuous
			lastErr = err
			var pe *probeError
			if errors.As(err, &pe) {
				lastProbe = pe
			}
		} else if healthySince.IsZero() {
			healthySince = now
			lastErr = nil
		}

		if !healthySince.IsZero() && now.Sub(healthySince) >= hc.soak {
			return nil
		}
		if pastDeadline {
			// The one probe that could have completed the soak failed:
			// say how long the run was, and what that probe answered.
			return &healthGateError{err: fmt.Errorf("healthy for %v of the %v soak, then the probe past the %v deadline failed: %w",
				now.Sub(wasHealthySince), hc.soak, hc.deadline, lastErr), probe: lastProbe}
		}
		p.clock.Sleep(hc.interval)
	}
}

// deadlineVerdict is the gate's failure at the deadline: an instance
// that was healthy but short of its soak is told how much had accrued
// when the deadline expired (not at the later tick that noticed), and
// no probe from before the healthy run is blamed; one that never got
// there gets the last failure and the last probe the app answered.
func deadlineVerdict(hc healthConfig, healthySince, deadline time.Time, lastErr error, lastProbe *probeError) error {
	if !healthySince.IsZero() {
		return &healthGateError{err: fmt.Errorf("healthy for %v of the %v soak when the %v deadline expired",
			deadline.Sub(healthySince), hc.soak, hc.deadline)}
	}
	return &healthGateError{err: fmt.Errorf("not healthy within deadline %v: %w", hc.deadline, lastErr), probe: lastProbe}
}

// probeOnce issues one GET over the instance's socket and demands a
// 2xx. The per-probe timeout is a real wall-clock context — an
// unresponsive app must not wedge the prober loop.
//
// A client per probe, with keep-alives off: every probe is a fresh
// connect (cheap on a unix socket), and no idle connection to an
// instance that has since died can be handed to a later probe of a
// different one. The probe never follows redirects: it is a control
// signal from supervisor to app, and an app answering 3xx must read as
// "not 2xx", not steer the supervisor's request elsewhere.
func (p *httpProber) probeOnce(ctx context.Context, sock *socketRef, path string, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{
		Transport: &http.Transport{DialContext: dialUnix(sock), DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// dialUnix ignores the host; the app sees it as the Host header,
	// so it is the one value host-allowlisting apps already accept.
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// Read a bounded slice of the body before closing: a huge body is
	// discarded, never buffered.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The first bytes of the body are the diagnosis more often than
		// the status is ("Invalid HTTP_HOST header", "no such table"),
		// and a redirect's target says where the app wanted to go. More
		// is read than is kept: the excerpt is cut after the known
		// values are replaced (failureDetail), so that cut never leaves
		// a fragment of one; the token the read's own bound may have
		// cut goes here (dropUnfinished, journal.go). One byte more is
		// read than kept, to tell a body of exactly probeBodyRead bytes,
		// which is whole, from a longer one.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyRead+1))
		body := string(raw)
		if len(body) > probeBodyRead {
			body = dropUnfinished(body[:probeBodyRead], false)
		}
		return &probeError{status: resp.StatusCode, location: resp.Header.Get("Location"), body: body}
	}
	return nil
}

// probeBodyExcerpt is how much of a failing probe's body is kept in a
// deploy's detail; probeBodyRead is how much is read for it, so that a
// known value straddling the excerpt's end is whole when the filter
// sees it.
const (
	probeBodyExcerpt = 512
	probeBodyRead    = 4096
)

// errProcessExited is the health gate's verdict when the process died
// before it was healthy; the deploy reads the runner's exit for it.
var errProcessExited = errors.New("process exited before becoming healthy")

// healthGateError is why the gate failed, with the last answered probe
// alongside whatever the final tick was — a crash, a timeout, a
// refused dial — so the answer is not lost behind it. Its text is the
// verdict's alone; errors.Is and errors.As see both.
type healthGateError struct {
	err   error
	probe *probeError
}

func (e *healthGateError) Error() string { return e.err.Error() }

func (e *healthGateError) Unwrap() []error {
	if e.probe == nil {
		return []error{e.err}
	}
	return []error{e.err, e.probe}
}

// probeError is a completed probe that was not 2xx: what the app
// answered, kept for the deploy's failure detail. Its text is the same
// line it always was, so nothing that read the error changes.
type probeError struct {
	status   int
	location string
	body     string
}

func (e *probeError) Error() string { return fmt.Sprintf("health check returned %d", e.status) }

var _ prober = (*httpProber)(nil)
