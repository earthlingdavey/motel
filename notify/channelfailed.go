package notify

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// channel_failed without the metrics package (DESIGN-notify.md,
// "Failed delivery"): once a minute motel reads Alertmanager's own
// alertmanager_notifications_failed_total over the socket, and when it
// has risen tells the operator's other channels, one ChannelFailed alert
// per channel, each routed to that channel alone (notify_channel=).
// With --enable-feature=receiver-name-in-metrics (the drop-in sets it)
// the counter names the receiver, which holds one channel (render): the
// failing channel is that one; without it, every channel of the
// integration.

// channelFailedLife is the alert's endsAt: re-posted while the counter
// keeps rising, gone a quarter-hour after it stops.
const channelFailedLife = 15 * time.Minute

// failWatch is the engine's memory of the counter, and the config it
// reads the counter against.
type failWatch struct {
	prev     map[string]float64 // integration/receiver/reason → value
	operator []string
	kinds    map[string]string   // channel → integration
	channels map[string][]string // receiver → channels
}

// failureAudience is the operator's channels a ChannelFailed may go
// to: those whose severity floor takes a warning, which it is. A
// channel kept for criticals hears no failure of another, from motel
// or from vmalert.
func failureAudience(r *resolved) []string {
	var out []string
	for _, n := range r.operator {
		if r.channels[n].receives(SeverityWarning) {
			out = append(out, n)
		}
	}
	return out
}

func sampleKey(s failedSample) string { return s.integration + "/" + s.receiver + "/" + s.reason }

// rises compares a read of the counter with the last and returns the
// series that rose. The first read is a baseline; a counter lower than
// the last is an Alertmanager restart, and counts from zero.
func (w *failWatch) rises(samples []failedSample) []failedSample {
	first := w.prev == nil
	next := make(map[string]float64, len(samples))
	var out []failedSample
	for _, s := range samples {
		k := sampleKey(s)
		next[k] = s.value
		if first {
			continue
		}
		prev, seen := w.prev[k]
		if (!seen || s.value < prev) && s.value > 0 || seen && s.value > prev {
			out = append(out, s)
		}
	}
	w.prev = next
	return out
}

// suspects is which channels a rise can be: those of the failing
// integration in the named receiver, or in every receiver when the
// counter names none.
func (w *failWatch) suspects(s failedSample) []string {
	var out []string
	add := func(ch string) {
		if w.kinds[ch] == s.integration && !slices.Contains(out, ch) {
			out = append(out, ch)
		}
	}
	if chans, ok := w.channels[s.receiver]; ok && s.receiver != "" {
		for _, ch := range chans {
			add(ch)
		}
	} else {
		for ch := range w.kinds {
			add(ch)
		}
	}
	sort.Strings(out)
	return out
}

// scrapeFailed is one read of the counter; it posts ChannelFailed for
// each rise to every operator channel that is not a suspect.
func (e *engine) scrapeFailed(ctx context.Context, read func(context.Context) ([]byte, error)) {
	body, err := read(ctx)
	var m amMetrics
	if err == nil {
		m, err = parseAMMetrics(body)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		// No reading this minute: the baseline stays the last good one,
		// so the next good read is compared with it, not with nothing.
		e.logger.Debug("monitor: reading Alertmanager's metrics", zap.Error(err))
		return
	}
	if e.active == nil {
		return
	}
	now := e.now()
	// A hold standing ends at a delivery through its suspects; one that
	// has stood heartbeatHoldAfter holds the heartbeat. Before this read's
	// rises, whose own baseline is this read.
	e.watchUntold(m, now)
	for _, s := range e.fail.rises(m.failed) {
		sus := e.fail.suspects(s)
		key := "notify/box/channel_failed/" + strings.Join(sus, ",")
		summary := fmt.Sprintf("Alertmanager could not deliver through %s (%s)", s.integration, s.reason)
		if s.receiver != "" {
			summary += " for receiver " + s.receiver
		}
		if len(sus) > 0 {
			summary += "; failing channel: " + strings.Join(sus, " or ")
		}
		fields := []zap.Field{zap.String("key", key), zap.String("integration", s.integration), zap.String("reason", s.reason),
			zap.String("receiver", s.receiver), zap.Strings("suspects", sus)}
		var to []string
		for _, ch := range e.fail.operator {
			if !slices.Contains(sus, ch) {
				to = append(to, ch)
			}
		}
		// No channel to tell: the heartbeat is what can, by stopping.
		if len(to) == 0 {
			e.untoldFor(key, s.receiver, sus, m, now)
		}
		if !e.bucketFor(key).take(now) {
			e.logger.Warn("monitor: channel failed", append(fields, zap.String("suppressed", "more than four an hour for this key"))...)
			continue
		}
		e.logger.Warn("monitor: channel failed", append(fields, zap.Strings("told", to))...)
		alerts := make([]amAlert, 0, len(to))
		for _, ch := range to {
			alerts = append(alerts, amAlert{
				Labels: map[string]string{
					"alertname": alertChannelFailed, labelChannel: ch, "severity": SeverityWarning,
					"subject": "box", "integration": s.integration, "suspects": strings.Join(sus, ","),
				},
				Annotations: map[string]string{"summary": e.clean(summary)},
				StartsAt:    now, EndsAt: now.Add(channelFailedLife),
			})
		}
		if len(alerts) > 0 {
			e.enqueue(&pending{key: key, alerts: alerts, first: now, journal: true})
		}
	}
}

// The heartbeat's hold (DESIGN-notify.md, "Dead-man and uptime"): a
// failure no channel can be told of (failureAudience holds no channel
// but the suspects) would otherwise be heard nowhere while
// the heartbeat's check off the box stays green. So once such a failure
// has stood heartbeatHoldAfter with no delivery through a suspect's
// receivers since, motel stops posting Watchdog; the alert ends within
// heartbeatLife (its receiver sends no resolved), and the check off the
// box reports the box down through its own channels. The next delivery
// through a suspect's receivers (`motel monitor test` makes one) ends
// the hold. It is in memory, as the counter's baseline is: a restart
// of motel ends it.

// heartbeatHoldAfter is how long a failure told to no channel stands
// before the heartbeat is held: a failed notification is not logged,
// so Alertmanager sends it again at the group's next flushes, and a
// passing fault (one 5xx) is delivered meanwhile.
const heartbeatHoldAfter = 5 * time.Minute

// untold is a failure no channel was told of, and the hold it brings.
type untold struct {
	since    time.Time
	suspects []string
	// seen is each suspect receiver's deliveries (attempts that did not
	// fail) at the last read.
	seen map[string]float64
	held bool
}

// receiversOf is every receiver that sends to one of chans.
func (w *failWatch) receiversOf(chans []string) []string {
	var out []string
	for name, cs := range w.channels {
		for _, c := range cs {
			if slices.Contains(chans, c) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func deliveries(m amMetrics, receiver string) float64 {
	return m.requests[receiver] - m.requestsFailed[receiver]
}

// untoldFor records a failure told to no channel; one recorded already
// for the same suspects keeps its start. A failure with no suspect is
// the heartbeat's own, which its check off the box hears already. One
// whose receiver is not this config's (the counter names none, or a
// name from before a reload) has suspects that are a guess, and no
// delivery count by channel to end a hold by. With mu held.
func (e *engine) untoldFor(key, receiver string, sus []string, m amMetrics, now time.Time) {
	if len(sus) == 0 {
		return
	}
	if _, known := e.fail.channels[receiver]; !known {
		return
	}
	if _, ok := e.untold[key]; ok {
		return
	}
	u := &untold{since: now, suspects: sus, seen: map[string]float64{}}
	for _, rc := range e.fail.receiversOf(sus) {
		u.seen[rc] = deliveries(m, rc)
	}
	e.untold[key] = u
}

// watchUntold is one read's part of the hold: a delivery through a
// suspect's receivers ends what it records, and what has stood
// heartbeatHoldAfter without one holds the heartbeat. A receiver with
// no count from an earlier read (a reload renamed it) is counted from
// this one; a count lower than the last (Alertmanager restarted) too.
// With mu held.
func (e *engine) watchUntold(m amMetrics, now time.Time) {
	for key, u := range e.untold {
		through := ""
		for _, rc := range e.fail.receiversOf(u.suspects) {
			d := deliveries(m, rc)
			if prev, ok := u.seen[rc]; ok && d > prev && through == "" {
				through = rc
			}
			u.seen[rc] = d
		}
		if through != "" {
			if u.held {
				e.logger.Info("monitor: heartbeat resumed", zap.Strings("suspects", u.suspects), zap.String("delivered_through", through))
			}
			delete(e.untold, key)
			continue
		}
		// With no heartbeat line there is no check off the box to hear a
		// hold: what stands is recorded, and held once a load adds one.
		if !u.held && now.Sub(u.since) >= heartbeatHoldAfter && e.heartbeatSet() {
			u.held = true
			e.logger.Warn("monitor: heartbeat held: a failed delivery no channel was told of; the heartbeat's check off the box will report this box down",
				zap.Strings("suspects", u.suspects), zap.Time("failed_at", u.since),
				zap.String("resumes", "at the next delivery through the channel: mend it, then motel monitor test "+u.suspects[0]))
		}
	}
}

// heartbeatHeld reports whether a failure no channel was told of holds
// the heartbeat. With mu held.
func (e *engine) heartbeatHeld() bool {
	for _, u := range e.untold {
		if u.held {
			return true
		}
	}
	return false
}

// heartbeatSet reports whether the config the engine serves has a
// heartbeat line. With mu held.
func (e *engine) heartbeatSet() bool {
	return e.active != nil && e.active.cfg != nil && e.active.cfg.heartbeat != ""
}

// keepUntold drops, at a config load, what records only channels the
// new config no longer has: nothing can deliver through them, and the
// hold would stand until a restart. A config with no heartbeat line
// keeps what stands but holds nothing (watchUntold holds it again once
// a load adds the line). With mu held.
func (e *engine) keepUntold(channels map[string]Channel) {
	for _, u := range e.untold {
		if u.held && !e.heartbeatSet() {
			u.held = false
			e.logger.Info("monitor: heartbeat resumed", zap.Strings("suspects", u.suspects), zap.String("why", "the config has no heartbeat line"))
		}
	}
	for key, u := range e.untold {
		if slices.ContainsFunc(u.suspects, func(c string) bool { _, ok := channels[c]; return ok }) {
			continue
		}
		if u.held {
			e.logger.Info("monitor: heartbeat resumed", zap.Strings("suspects", u.suspects), zap.String("why", "the config no longer has the channel"))
		}
		delete(e.untold, key)
	}
}
