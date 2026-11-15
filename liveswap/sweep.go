package liveswap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// The start-time sweep of apps no loaded config names. It drives the
// runner, but every question it asks is a module-layer one — what the
// pool holds, whether the process is exiting, which directories an app
// owns — so it lives here rather than in runner_systemd.go.

// unknownAppSweepTimeout bounds the start-time sweep of units whose
// apps are no longer configured.
const unknownAppSweepTimeout = 2 * time.Minute

// appConfigured reports whether any loaded config — the live one, or
// a candidate being provisioned — holds the app. The pool is the
// authority: a candidate that later fails to activate still holds its
// pool references until its Cleanup, and the config it would have
// replaced holds its own throughout, so nothing a sweep judges
// "unknown" can belong to a config that is or may yet be serving.
//
// Tests substitute their own ledger through the seam below. It is an
// atomic rather than a plain var because the sweep runs on a goroutine
// nothing joins: a test restoring the seam can overlap a sweep still
// reading it. nil = the pool.
var appConfiguredSeam atomic.Pointer[func(string) bool]

func appConfigured(app string) bool {
	if f := appConfiguredSeam.Load(); f != nil {
		return (*f)(app)
	}
	refs, ok := appPool.References(poolKey(app))
	return ok && refs > 0
}

// unknownSweepMu serializes start-time sweeps with each other (two
// configs starting back to back), never with config publication.
var unknownSweepMu sync.Mutex

// sweepUnknownApps stops every motel unit whose app the current
// config does not name (invariant 7): an app removed or renamed while
// motel was down has no managedApp left to sweep it, so App.Start
// does it here against the manager's own listing.
func sweepUnknownApps(ctx context.Context, conn systemdConn, root string, logger *zap.Logger) error {
	unknownSweepMu.Lock()
	defer unknownSweepMu.Unlock()
	if caddyExiting() {
		return nil // shutdown drops pool references; the units are meant to survive
	}
	units, err := conn.ListUnits(ctx, unitPrefix+"*.service")
	if err != nil {
		return fmt.Errorf("listing motel units: %w", err)
	}
	r := newSystemdRunner(conn, logger)
	defer r.close()
	var errs []error
	seen := map[string]bool{}
	for _, u := range units {
		app, ok := unitApp(u.Name)
		if !ok || seen[app] {
			continue
		}
		seen[app] = true
		// Judged now, not at listing time: a reload may have added the
		// app back while the listing was in flight.
		if appConfigured(app) {
			continue
		}
		logger.Warn("stopping units of an app no longer in the config", zap.String("app", app), zap.String("unit", u.Name))
		if serr := sweepUnowned(ctx, r, root, app, logger); serr != nil {
			errs = append(errs, serr)
		}
	}
	// An app whose units are all gone — exited cleanly while motel
	// was down — never appears in the listing, yet its socket dirs may
	// remain; if it is also no longer configured, nothing else will
	// ever prune them.
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("listing the liveswap root for apps no longer configured: %w", err))
	}
	for _, e := range entries {
		app := e.Name()
		if !e.IsDir() || seen[app] || !appNameRe.MatchString(app) || appConfigured(app) {
			continue
		}
		if serr := sweepUnowned(ctx, r, root, app, logger); serr != nil {
			errs = append(errs, serr)
		}
	}
	return errors.Join(errs...)
}

// sweepUnowned stops every unit of an app nobody owns and then prunes
// its socket dirs — as one step under the app's ownerLock, so a reload
// acquiring the app cannot slip in between and adopt what is about to
// be removed. The caller's deadline bounds the whole sweep, so a slow
// manager cannot hold the start-time sweep open indefinitely; each
// stop re-checks the pool first, so a reload adopting the app between
// listing and stopping keeps its unit. Ownership is not monotonic — a
// candidate config can own the app for a moment and be cleaned up —
// so a single veto during the sweep disables the prune: disk cleanup
// follows only a sweep that accounted for every unit.
func sweepUnowned(ctx context.Context, r *systemdRunner, root, app string, logger *zap.Logger) error {
	mu := ownerLock(app)
	mu.Lock()
	defer mu.Unlock()
	var vetoed atomic.Bool
	still := func() bool {
		ok := !caddyExiting() && !appConfigured(app)
		if !ok {
			vetoed.Store(true)
		}
		return ok
	}
	if !still() {
		return nil
	}
	if err := r.sweep(ctx, app, nil, still, false); err != nil {
		return err
	}
	if vetoed.Load() {
		return nil
	}
	dirs := newAppDirs(root, app)
	pruneSockets(dirs, nil, logger)
	// What its watchdog had reported is over with it: an app of the name
	// added back later starts with nothing standing (lifecycle.go).
	if err := os.Remove(dirs.alerts); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("removing the alerts file of an app no longer in the config", zap.String("file", dirs.alerts), zap.Error(err))
	}
	return nil
}
