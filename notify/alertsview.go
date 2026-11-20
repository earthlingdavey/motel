package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The alerts a person reads (GET /_motel/apps/<app>/alerts, liveswap's
// route): what Alertmanager holds, read over its socket, at most once
// per alertsEvery for the whole process, whatever the number of people
// and configs, within alertsBudget. liveswap picks one app's alerts out
// of the answer by the app label; this side builds no matcher and sends
// none.
const (
	// alertsEvery is how long one answer, or one failure, is given to
	// every request: a token, stolen or not, drives at most two calls to
	// Alertmanager per alertsEvery.
	alertsEvery = 5 * time.Second
	// alertsBudget bounds one fetch, both calls: the route never waits
	// longer on an Alertmanager that does not answer (the client's own
	// 10 s is for the writes, and for socket activation's first start).
	alertsBudget = 2 * time.Second
)

// amHeld is one alert as GET /api/v2/alerts gives it, cut to what the
// route reads. With no parameter Alertmanager gives every alert it
// holds that has not resolved: active, silenced and inhibited alike
// (measured on Debian's 0.28.1, the e2e image's).
type amHeld struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	Status      struct {
		State       string   `json:"state"`
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
}

// amHeldSilence is one silence as GET /api/v2/silences gives it: its
// id and its end, which is all an alert's "silenced until" needs.
type amHeldSilence struct {
	ID     string    `json:"id"`
	EndsAt time.Time `json:"endsAt"`
}

// heldAlerts is every alert Alertmanager holds, as the people's reads
// take them: the heartbeat, the test alert and the deploy mute left out
// (notSilenceable: they say nothing of an app or of the box's health),
// and each alert's silences read for when the last of them ends. The
// silence list is asked for only when an alert names one, and only its
// ids and ends are read.
func (c *amClient) heldAlerts(ctx context.Context) (liveswap.HeldAlerts, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v2/alerts", nil)
	if err != nil {
		return liveswap.HeldAlerts{}, err
	}
	var held []amHeld
	if err := json.Unmarshal(data, &held); err != nil {
		return liveswap.HeldAlerts{}, fmt.Errorf("alertmanager at %s: GET /api/v2/alerts: %w", c.socket, err)
	}
	held = slices.DeleteFunc(held, func(a amHeld) bool { return slices.Contains(notSilenceable, a.Labels["alertname"]) })
	ends := map[string]time.Time{}
	if slices.ContainsFunc(held, func(a amHeld) bool { return len(a.Status.SilencedBy) > 0 }) {
		data, err := c.do(ctx, http.MethodGet, "/api/v2/silences", nil)
		if err != nil {
			return liveswap.HeldAlerts{}, err
		}
		var silences []amHeldSilence
		if err := json.Unmarshal(data, &silences); err != nil {
			return liveswap.HeldAlerts{}, fmt.Errorf("alertmanager at %s: GET /api/v2/silences: %w", c.socket, err)
		}
		for _, s := range silences {
			ends[s.ID] = s.EndsAt
		}
	}
	out := make([]liveswap.AppAlert, 0, len(held))
	for _, a := range held {
		al := liveswap.AppAlert{App: a.Labels["app"], Alert: wire.Alert{
			Name: a.Labels["alertname"], Severity: a.Labels["severity"], Summary: a.Annotations["summary"],
			StartsAt: a.StartsAt.UTC(), State: a.Status.State, Inhibited: len(a.Status.InhibitedBy) > 0,
		}}
		// An id the list does not hold (a silence gone between the two
		// calls) adds no end: the state still says suppressed.
		for _, id := range a.Status.SilencedBy {
			if end, ok := ends[id]; ok && (al.SilencedUntil == nil || end.After(*al.SilencedUntil)) {
				end = end.UTC()
				al.SilencedUntil = &end
			}
		}
		out = append(out, al)
	}
	return liveswap.HeldAlerts{Alerts: out}, nil
}

// alertsFetch is one fetch of what Alertmanager holds: heldAlerts, a
// test seam.
type alertsFetch func(ctx context.Context) (liveswap.HeldAlerts, error)

// alertsEntry is one fetch's answer, for every request until it is
// alertsEvery old; done closes when it is in.
type alertsEntry struct {
	at   time.Time
	held liveswap.HeldAlerts
	ok   bool
	done chan struct{}
}

// alertsView is the process's one fetch at a time and its answer.
type alertsView struct {
	fetch alertsFetch // nil: heldAlerts over theSocket, by am
	now   func() time.Time

	mu   sync.Mutex
	last *alertsEntry
	// am is the client of theSocket the fetches share, so they reuse its
	// idle connection rather than each leaving one open for its 30 s.
	am *amClient
}

// theAlerts is the process's: every config's source reads through it,
// so a reload does not fetch again, and every person shares one answer.
var theAlerts = &alertsView{now: time.Now}

// get answers from the entry under alertsEvery old, or waits for the
// fetch in flight, or makes it. A request that gives up first is told
// Alertmanager did not answer; the fetch it waited for still fills the
// entry for the next.
func (v *alertsView) get(ctx context.Context, logger *zap.Logger) (liveswap.HeldAlerts, bool) {
	v.mu.Lock()
	now := v.now()
	e := v.last
	if e == nil || now.Sub(e.at) >= alertsEvery || now.Before(e.at) {
		e = &alertsEntry{at: now, done: make(chan struct{})}
		v.last = e
		fetch := v.fetch
		if fetch == nil {
			if v.am == nil || v.am.socket != theSocket {
				v.am = newAMClient(theSocket)
			}
			fetch = v.am.heldAlerts
		}
		v.mu.Unlock()
		// The fetch is the box's, not this request's: a person who goes
		// away does not end it for the next.
		// The entry is closed however the fetch ends, a panic too: the
		// requests waiting on it are told it did not answer.
		defer close(e.done)
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), alertsBudget)
		held, err := fetch(fctx)
		cancel()
		if err != nil {
			// A person is told only that it did not answer: the cause is the
			// operator's, at most one line per alertsEvery, as kept.
			logger.Warn("monitor: a person's read of an app's alerts was answered \"monitor\":\"unanswered\": Alertmanager did not answer", zap.Error(err))
			held = liveswap.HeldAlerts{}
		}
		e.held, e.ok = held, err == nil
		return e.held, e.ok
	}
	v.mu.Unlock()
	select {
	case <-e.done:
		return e.held, e.ok
	case <-ctx.Done():
		return liveswap.HeldAlerts{}, false
	}
}

// alertsSource is liveswap's AlertsSource for one config: the process's
// view, and the config's logger for what goes wrong.
type alertsSource struct {
	view   *alertsView
	logger *zap.Logger
}

func (s alertsSource) Alerts(ctx context.Context) (liveswap.HeldAlerts, bool) {
	return s.view.get(ctx, s.logger)
}
