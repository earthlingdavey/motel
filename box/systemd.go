package box

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/box/proof"
)

// Systemd is what the applier asks of the service manager about
// moteld.service: is it active, and reload it. An interface so the
// Failure-mode table is testable without a manager; production is
// systemctl.
type Systemd interface {
	// IsActive is `systemctl is-active`'s word for the unit: active,
	// activating, inactive, failed, … An error is a question that got
	// no answer, never a state.
	IsActive(ctx context.Context) (string, error)
	// Reload is `systemctl reload`: nil is exit status 0.
	Reload(ctx context.Context) error
}

// Clock is the applier's time: Now for ages and the activating wait,
// Sleep between is-active questions.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

const (
	// activatingWait is step 16's bound on `activating` (DESIGN-box.md,
	// "Caps"): moteld's TimeoutStartSec=240s plus RestartSec, elapsed,
	// since Restart=on-failure can keep a unit activating across
	// attempts.
	activatingWait = 300 * time.Second
	// activatingPoll is how often the wait asks again.
	activatingPoll = time.Second
	// isActiveTimeout bounds one `systemctl is-active`: a question
	// PID 1 answers from memory.
	isActiveTimeout = 30 * time.Second
	// reloadTimeout bounds one `systemctl reload`. The reload is bounded
	// by moteld.service's own 240 s, and the applier sets no shorter
	// one (DESIGN-box.md, "Caps"): this deadline only ends a systemctl
	// that outlives the job it waits for.
	reloadTimeout = 300 * time.Second
	// childWaitDelay is how long a child's leftover pipe holders are
	// waited for once it has exited or been killed.
	childWaitDelay = time.Second
	// maxSystemctlOutput is how much of systemctl's output an error
	// keeps.
	maxSystemctlOutput = 4 << 10
)

// systemctl is Systemd as the unit runs it: `systemctl` on PATH, for the
// one unit. Each call is a direct child with a deadline and a WaitDelay;
// systemctl starts no unit of its own, so there is no grandchild holding
// its pipes to stop (the reload job runs in PID 1, which a killed
// systemctl does not cancel; moteld.service's own timeout bounds it).
type systemctl struct {
	unit string
}

// unitStates are the words `systemctl is-active` prints for a unit
// that exists in any state. Anything else on stdout, or nothing, is an
// unanswered question.
var unitStates = map[string]bool{
	"active": true, "reloading": true, "inactive": true, "failed": true,
	"activating": true, "deactivating": true, "maintenance": true, "refreshing": true,
}

func (s systemctl) IsActive(ctx context.Context) (string, error) {
	out, err := runSystemctl(ctx, isActiveTimeout, "is-active", s.unit)
	// is-active exits non-zero for every state but active, so the word
	// is the answer and the status is not.
	word, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if unitStates[word] {
		return word, nil
	}
	if err == nil {
		err = errors.New("no state")
	}
	return "", fmt.Errorf("systemctl is-active %s: %w: %s", s.unit, err, proof.Bound(strings.TrimSpace(out)))
}

func (s systemctl) Reload(ctx context.Context) error {
	out, err := runSystemctl(ctx, reloadTimeout, "reload", s.unit)
	if err != nil {
		return fmt.Errorf("systemctl reload %s: %w: %s", s.unit, err, proof.Bound(strings.TrimSpace(out)))
	}
	return nil
}

// runSystemctl runs one bounded systemctl with a fixed PATH and nothing
// else in its environment; out is its stdout and stderr together, cut
// at maxSystemctlOutput.
func runSystemctl(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, "systemctl", args...) //nolint:gosec // fixed verbs on a fixed unit
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}
	out := newCappedBuffer(maxSystemctlOutput)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = childWaitDelay
	err := cmd.Run()
	if bounded.Err() != nil && err != nil {
		err = fmt.Errorf("%w (%w)", bounded.Err(), err)
	}
	return string(out.b), err
}

// running is step 16's question (DESIGN-box.md, Glossary "active"):
// `activating` is waited out, and so is `reloading` — a reload in
// flight (the console's, or one a killed applier started) ends in
// `active` or a failure. The run has one budget for waiting, ending
// activatingWait after it first saw either word, so the waiting in a
// run is that long, plus one bounded is-active question per call,
// whatever motel does — a later episode in the same run, once the
// budget is spent, gets no wait (its push is refused as still starting,
// and the workflow's run fails; a re-run pushes again). The answer is
// the last word is-active gave: a push needs `active` (step 16), and
// up classifies it for recovery and init alike. An error is the box's:
// is-active could not be asked.
func (a *Applier) running(ctx context.Context) (string, error) {
	for {
		state, err := a.systemd.IsActive(ctx)
		if err != nil {
			return "", err
		}
		if !transient(state) {
			return state, nil
		}
		now := a.clock.Now()
		if a.waitUntil.IsZero() {
			a.waitUntil = now.Add(activatingWait)
		}
		if !now.Before(a.waitUntil) {
			return state, nil
		}
		if err := a.clock.Sleep(ctx, activatingPoll); err != nil {
			return "", err
		}
	}
}

// transient is a state the wait sits out: moteld on its way to active.
func transient(state string) bool { return state == "activating" || state == "reloading" }

// up is running's answer classified, for every caller alike: moteld
// serves and a reload reaches it — `active`, or a reload still in
// flight after the wait (a reload then queues behind it). Anything else
// — `activating` after the wait (a start, or a restart loop, which
// reads the file on disk when it gets there), inactive, failed — is not
// running. A push is refused on anything but `active` (step 16).
func up(state string) bool { return state == "active" || state == "reloading" }
